package topology

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
)

const (
	SingleServer = "single-server"
	Cluster      = "cluster"
)

// PlanFile is written by samuori-setup next to server.yml and imported once when no plan is stored.
const PlanFile = "topology-plan.yml"

// Plan is the administrator's target and the resilience they declare for what Samuori cannot measure.
type Plan struct {
	Deployment         string    `json:"deployment" yaml:"deployment"`
	Controllers        int       `json:"controllers" yaml:"controllers"`
	Workers            int       `json:"workers" yaml:"workers"`
	DatabaseReplicated bool      `json:"databaseReplicated" yaml:"databaseReplicated"`
	StorageReplicated  bool      `json:"storageReplicated" yaml:"storageReplicated"`
	UpdatedAt          time.Time `json:"updatedAt" yaml:"-"`
}

var (
	ErrForbidden = errors.New("only administrators change the topology plan")
	ErrNoPlan    = errors.New("no topology plan is stored")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

func (p Plan) Validate() error {
	switch {
	case p.Deployment != SingleServer && p.Deployment != Cluster:
		return &Invalid{"deployment must be single-server or cluster"}
	case p.Controllers < 1 || p.Controllers > 7:
		return &Invalid{"plan 1 to 7 controllers"}
	case p.Workers < 0 || p.Workers > 1000:
		return &Invalid{"plan 0 to 1000 workers"}
	case p.Deployment == SingleServer && p.Controllers != 1:
		return &Invalid{"a single server has one controller; plan a cluster for more"}
	}
	return nil
}

// Facts turns a plan into the facts it would produce, for the planned tier. A single server's own worker counts.
func (p Plan) Facts(storage Storage, portalOnController bool) Facts {
	f := Facts{Controllers: p.Controllers, Pools: map[string]int{}, Database: PostgreSQL, DatabaseReplicated: p.DatabaseReplicated,
		Storage: storage, StorageReplicated: p.StorageReplicated, PortalOnController: portalOnController}
	switch {
	case p.Deployment == SingleServer:
		f.Database, f.Storage = SQLite, Local
	case storage == Local:
		// A cluster needs shared storage; converting a single server moves its blobs to S3.
		f.Storage = S3
	}
	if p.Workers > 0 {
		f.Pools["default"] = p.Workers
	}
	return f
}

type Store struct {
	db  *db.DB
	now func() time.Time
}

func NewStore(d *db.DB) *Store { return &Store{db: d, now: time.Now} }

func (s *Store) Get(ctx context.Context, orgID string) (Plan, error) {
	var p Plan
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT deployment, controllers, workers, database_replicated, storage_replicated, updated_at FROM topology_plans WHERE org_id = ?`), orgID).
		Scan(&p.Deployment, &p.Controllers, &p.Workers, &p.DatabaseReplicated, &p.StorageReplicated, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNoPlan
	}
	return p, err
}

// Set replaces the organization's plan; administrators only, audited.
func (s *Store) Set(ctx context.Context, actor authz.Principal, p Plan, requestID string) (Plan, error) {
	if !actor.IsOrgAdmin() {
		return Plan{}, ErrForbidden
	}
	return s.put(ctx, actor, actor.ID, p, requestID)
}

// ImportFile stores the plan samuori-setup wrote, unless a plan is already stored. A missing file is not an error.
func (s *Store) ImportFile(ctx context.Context, orgID, path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.Get(ctx, orgID); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNoPlan) {
		return false, err
	}
	var p Plan
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	_, err = s.put(ctx, authz.Principal{OrgID: orgID, Kind: authz.System}, "samuori-setup", p, "")
	return err == nil, err
}

func (s *Store) put(ctx context.Context, actor authz.Principal, by string, p Plan, requestID string) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	p.UpdatedAt = s.now().UTC()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO topology_plans (org_id, deployment, controllers, workers, database_replicated, storage_replicated, updated_by, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (org_id) DO UPDATE SET deployment = excluded.deployment, controllers = excluded.controllers, workers = excluded.workers,
			database_replicated = excluded.database_replicated, storage_replicated = excluded.storage_replicated, updated_by = excluded.updated_by, updated_at = excluded.updated_at`),
			actor.OrgID, p.Deployment, p.Controllers, p.Workers, p.DatabaseReplicated, p.StorageReplicated, by, p.UpdatedAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "topology.plan", ResourceType: "topology", ResourceID: actor.OrgID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s, %d controllers, %d workers, database replicated %t, storage replicated %t", p.Deployment, p.Controllers, p.Workers, p.DatabaseReplicated, p.StorageReplicated), RequestID: requestID})
	})
	return p, err
}
