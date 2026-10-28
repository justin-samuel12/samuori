package workers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/isolation"
)

// Location is where pools run (D25). Projects may be restricted to locations for data residency, and a build may
// prefer one of its allowed locations.
type Location struct {
	ID, Name, Description string
	CreatedAt             time.Time
}

var (
	locationName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	ErrInUse     = errors.New("still in use")
	ErrInvalid   = errors.New("invalid")
)

func (s *Store) Locations(ctx context.Context, orgID string) ([]Location, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, name, description, created_at FROM locations WHERE org_id = ? ORDER BY name`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		var l Location
		if err := rows.Scan(&l.ID, &l.Name, &l.Description, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) CreateLocation(ctx context.Context, actor authz.Principal, name, description, requestID string) (Location, error) {
	if !actor.IsOrgAdmin() {
		return Location{}, ErrForbidden
	}
	if !locationName.MatchString(name) {
		return Location{}, ErrInvalid
	}
	l := Location{ID: ids.New(), Name: name, Description: description, CreatedAt: s.now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO locations (id, org_id, name, description, created_at) VALUES (?, ?, ?, ?, ?)`), l.ID, actor.OrgID, name, description, l.CreatedAt); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "location.create", ResourceType: "location", ResourceID: l.ID, Outcome: "succeeded", Reason: name, RequestID: requestID})
	})
	return l, err
}

// InUse refuses deleting a pool or location that something still uses; Uses names each use for the administrator.
type InUse struct{ Uses []string }

func (e *InUse) Error() string { return "still in use by " + strings.Join(e.Uses, "; ") }

func (e *InUse) Is(target error) bool { return target == ErrInUse }

// LocationUses names the pools and projects that use a location; a location they use cannot be deleted.
func LocationUses(ctx context.Context, d *db.DB, tx *sql.Tx, orgID, name string) ([]string, error) {
	var uses []string
	for _, q := range []struct{ query, what string }{
		{`SELECT name FROM worker_pools WHERE org_id = ? AND location = ? ORDER BY name`, "pool "},
		{`SELECT p.name FROM project_locations pl JOIN projects p ON p.id = pl.project_id WHERE p.org_id = ? AND pl.location = ? ORDER BY p.name`, "project "},
	} {
		rows, err := tx.QueryContext(ctx, d.Rebind(q.query), orgID, name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return nil, err
			}
			uses = append(uses, q.what+n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return uses, nil
}

// DeleteLocation refuses a location that pools, projects or a federation route still use.
func (s *Store) DeleteLocation(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT name FROM locations WHERE id = ? AND org_id = ?`), id, actor.OrgID).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		uses, err := LocationUses(ctx, s.db, tx, actor.OrgID, name)
		if err != nil {
			return err
		}
		var route int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM federation_routes WHERE location_id = ?`), id).Scan(&route); err != nil {
			return err
		}
		if route > 0 {
			uses = append(uses, "a federation route (delete the route on the Federation page instead)")
		}
		if len(uses) > 0 {
			return &InUse{Uses: uses}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM locations WHERE id = ?`), id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "location.delete", ResourceType: "location", ResourceID: id, Outcome: "succeeded", Reason: name, RequestID: requestID})
	})
}

// DefaultLocation is the location every organization starts with (ADR 0045).
const DefaultLocation = "default"

// resolveLocation picks the organization's only location, or the default one, when name is empty, then makes sure
// the location is recorded.
func (s *Store) resolveLocation(ctx context.Context, tx *sql.Tx, orgID, name string) (string, error) {
	if name == "" {
		name = DefaultLocation
		var only sql.NullString
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT CASE WHEN COUNT(*) = 1 THEN MIN(name) END FROM locations WHERE org_id = ?`), orgID).Scan(&only); err != nil {
			return "", err
		}
		if only.Valid {
			name = only.String
		}
	}
	return name, s.ensureLocation(ctx, tx, orgID, name)
}

// ensureLocation records a pool's location so every pool location is a managed location.
func (s *Store) ensureLocation(ctx context.Context, tx *sql.Tx, orgID, name string) error {
	if !locationName.MatchString(name) {
		return ErrInvalid
	}
	var n int
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM locations WHERE org_id = ? AND name = ?`), orgID, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO locations (id, org_id, name, description, created_at) VALUES (?, ?, ?, '', ?)`), ids.New(), orgID, name, s.now().UTC())
	return err
}

// PoolUpdate changes a pool's name, location, trust domain or isolation profile. An enabled pool whose profile or
// trust domain changes stays approved, by the administrator who changed it; its workers need a new self-test for a
// new profile.
type PoolUpdate struct {
	Name, Location, TrustDomain, IsolationProfile *string
	TrustedWorkloadsOnly                          *bool
}

func (s *Store) UpdatePool(ctx context.Context, actor authz.Principal, id string, u PoolUpdate, requestID string) (Pool, error) {
	if !actor.IsOrgAdmin() {
		return Pool{}, ErrForbidden
	}
	p, err := s.Pool(ctx, actor.OrgID, id)
	if err != nil {
		return Pool{}, err
	}
	reapprove := false
	if u.Name != nil {
		if !locationName.MatchString(*u.Name) {
			return Pool{}, ErrInvalid
		}
		p.Name = *u.Name
	}
	if u.Location != nil {
		p.Location = *u.Location
	}
	if u.TrustDomain != nil {
		reapprove = reapprove || *u.TrustDomain != p.TrustDomain
		p.TrustDomain = *u.TrustDomain
	}
	if u.IsolationProfile != nil {
		if !isolation.Profile(*u.IsolationProfile).Valid() {
			return Pool{}, ErrInvalid
		}
		reapprove = reapprove || *u.IsolationProfile != p.IsolationProfile
		p.IsolationProfile = *u.IsolationProfile
	}
	if u.TrustedWorkloadsOnly != nil {
		p.TrustedWorkloadsOnly = *u.TrustedWorkloadsOnly
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if p.Location, err = s.resolveLocation(ctx, tx, actor.OrgID, p.Location); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE worker_pools SET name = ?, location = ?, trust_domain = ?, isolation_profile = ?, trusted_workloads_only = ? WHERE id = ? AND org_id = ?`),
			p.Name, p.Location, p.TrustDomain, p.IsolationProfile, p.TrustedWorkloadsOnly, id, actor.OrgID); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		if p.ProfileApproved && reapprove {
			if err := s.recordApproval(ctx, tx, actor, id, true, "approved by changing the isolation profile or trust domain to "+p.IsolationProfile+", "+p.TrustDomain); err != nil {
				return err
			}
			if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pool.profile.approved", ResourceType: "worker_pool", ResourceID: id, Outcome: "succeeded", Reason: p.IsolationProfile + ", trust domain " + p.TrustDomain, RequestID: requestID}); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pool.update", ResourceType: "worker_pool", ResourceID: id, Outcome: "succeeded", Reason: p.Name + " in " + p.Location, RequestID: requestID})
	})
	if err != nil {
		return Pool{}, err
	}
	return s.Pool(ctx, actor.OrgID, id)
}

// DeletePool removes a pool that never had workers.
func (s *Store) DeletePool(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		// Workers keep their history, revoked ones included, so a pool that ever had workers stays.
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM workers WHERE pool_id = ?`), id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return &InUse{Uses: []string{fmt.Sprintf("%d worker(s), whose history is kept; disable the pool instead", n)}}
		}
		for _, q := range []string{`DELETE FROM enrollment_tokens WHERE pool_id = ?`, `DELETE FROM pool_approvals WHERE pool_id = ?`, `DELETE FROM pool_self_tests WHERE pool_id = ?`} {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(q), id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM worker_pools WHERE id = ? AND org_id = ?`), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pool.delete", ResourceType: "worker_pool", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// ProjectLocations returns the locations a project's builds may run in; empty means any.
func (s *Store) ProjectLocations(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT location FROM project_locations WHERE project_id = ? ORDER BY location`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetProjectLocations restricts a project to locations (data residency); an empty list allows any location.
func (s *Store) SetProjectLocations(ctx context.Context, actor authz.Principal, projectID string, locations []string, requestID string) ([]string, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	known, err := s.Locations(ctx, actor.OrgID)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, l := range known {
		names[l.Name] = true
	}
	sort.Strings(locations)
	locations = slices.Compact(locations)
	for _, l := range locations {
		if !names[l] {
			return nil, ErrInvalid
		}
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM projects WHERE id = ? AND org_id = ?`), projectID, actor.OrgID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM project_locations WHERE project_id = ?`), projectID); err != nil {
			return err
		}
		for _, l := range locations {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO project_locations (project_id, location) VALUES (?, ?)`), projectID, l); err != nil {
				return err
			}
		}
		reason := "any location"
		if len(locations) > 0 {
			reason = "restricted to " + strings.Join(locations, ", ")
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "project.locations", ResourceType: "project", ResourceID: projectID, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return nil, err
	}
	return locations, nil
}
