package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
)

var (
	ErrForbidden = errors.New("only administrators change providers and build methods")
	ErrUnknown   = errors.New("no such provider")
)

type Store struct {
	db  *db.DB
	now func() time.Time
}

func NewStore(d *db.DB) *Store { return &Store{db: d, now: time.Now} }

// Config returns the stored approval for name, or a disabled one when none is stored.
func (s *Store) Config(ctx context.Context, orgID, name string) (Config, error) {
	if _, ok := Lookup(name); !ok {
		return Config{}, ErrUnknown
	}
	var raw string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT config FROM providers WHERE org_id = ? AND name = ?`), orgID, name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Config{Artifacts: map[string]string{}, Pools: []string{}, Projects: []string{}, EgressPresets: []string{}}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, fmt.Errorf("provider %s: %w", name, err)
	}
	return c, nil
}

// Set replaces the approval for name; administrators only, audited.
func (s *Store) Set(ctx context.Context, actor authz.Principal, name string, c Config, requestID string) (Config, error) {
	if !actor.IsOrgAdmin() {
		return Config{}, ErrForbidden
	}
	d, ok := Lookup(name)
	if !ok {
		return Config{}, ErrUnknown
	}
	if err := c.Validate(d); err != nil {
		return Config{}, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO providers (org_id, name, config, updated_by, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (org_id, name) DO UPDATE SET config = excluded.config, updated_by = excluded.updated_by, updated_at = excluded.updated_at`),
			actor.OrgID, name, string(raw), actor.ID, s.now().UTC()); err != nil {
			return err
		}
		state := "disabled"
		if c.Enabled {
			state = "enabled " + c.Version
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "provider.configure", ResourceType: "provider", ResourceID: name, Outcome: "succeeded", Reason: state, RequestID: requestID})
	})
	return c, err
}

func (s *Store) Policy(ctx context.Context, orgID string) (Policy, error) {
	var allowed, def string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT allowed, default_method FROM build_method_policies WHERE org_id = ?`), orgID).Scan(&allowed, &def)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultPolicy(), nil
	}
	if err != nil {
		return Policy{}, err
	}
	p := Policy{Default: def}
	if err := json.Unmarshal([]byte(allowed), &p.Allowed); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (s *Store) SetPolicy(ctx context.Context, actor authz.Principal, p Policy, requestID string) (Policy, error) {
	if !actor.IsOrgAdmin() {
		return Policy{}, ErrForbidden
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	raw, _ := json.Marshal(p.Allowed)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_method_policies (org_id, allowed, default_method, updated_by, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (org_id) DO UPDATE SET allowed = excluded.allowed, default_method = excluded.default_method, updated_by = excluded.updated_by, updated_at = excluded.updated_at`),
			actor.OrgID, string(raw), p.Default, actor.ID, s.now().UTC()); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "build-method-policy.set", ResourceType: "organization", ResourceID: actor.OrgID, Outcome: "succeeded",
			Reason: "allowed " + strings.Join(p.Allowed, ",") + "; default " + or(p.Default, "automatic"), RequestID: requestID})
	})
	return p, err
}

// MethodAllowed is the admission check: the method is allowed by policy and, when it needs a provider, that provider
// is enabled for the project in the pool. The error says what an administrator must change.
func (s *Store) MethodAllowed(ctx context.Context, orgID, method, pool, project string) error {
	p, err := s.Policy(ctx, orgID)
	if err != nil {
		return err
	}
	if !slices.Contains(p.Allowed, method) {
		return &Invalid{fmt.Sprintf("the %s build method is not allowed here; an administrator allows it under Build & conversion providers", method)}
	}
	d, needs := ForMethod(method)
	if !needs {
		return nil
	}
	c, err := s.Config(ctx, orgID, d.Name)
	if err != nil {
		return err
	}
	if !c.Allows(pool, project) {
		return &Invalid{fmt.Sprintf("%s is not enabled for this project and worker pool; an administrator enables it under Build & conversion providers", d.DisplayName)}
	}
	return nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
