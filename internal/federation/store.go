// Package federation lets two installations run each other's builds in agreed locations (D25, ADR 0037). The
// originating installation owns each run and makes every call; the executing peer applies its own admission,
// policies and workers to a forwarded spec and source, and never receives a build plan.
package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

var (
	ErrForbidden = errors.New("only organization administrators manage federation")
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("the name is already used")
)

// Invalid explains a refused federation setting.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

type Peer struct {
	ID, OrgID, Name, Endpoint, CAPin, RemoteOrgID, CreatedBy string
	CreatedAt                                                time.Time
	Disabled                                                 bool
}

type Grant struct {
	ID, OrgID, PeerID, ProjectID string
	Locations                    []string
	MaxActive                    int
	CreatedBy                    string
	CreatedAt                    time.Time
	Revoked                      bool
}

type Route struct {
	ID, OrgID, PeerID, Location, RemoteLocation, CreatedBy string
	CreatedAt                                              time.Time
}

// Delegation is a run this installation handed to a peer.
type Delegation struct {
	ID, OrgID, RunID, PeerID, RemoteLocation, AttemptID string
	State, RemoteRunID, RemoteState, Detail             string
	ArtifactDigest                                      string
	LastContactAt, UnreachableSince                     *time.Time
	CreatedAt, UpdatedAt                                time.Time
}

// Delegation states. A delegation is pending until the peer accepts it, then accepted until its result is in.
const (
	Pending   = "pending"
	Accepted  = "accepted"
	Completed = "completed"
	Failed    = "failed"
	Cancelled = "cancelled"
	Abandoned = "abandoned"
	// Withdrawn is an abandoned delegation whose peer has been told to stop.
	Withdrawn = "withdrawn"
)

type Store struct {
	db  *db.DB
	now func() time.Time
}

func NewStore(d *db.DB) *Store { return &Store{db: d, now: time.Now} }

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	pinPattern      = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	ulidPattern     = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	locationPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]{0,61}[a-z0-9])?$`)
)

func admin(p authz.Principal) error {
	if !p.IsOrgAdmin() {
		return ErrForbidden
	}
	return nil
}

func (s *Store) audit(ctx context.Context, tx *sql.Tx, p authz.Principal, action, kind, id, reason, requestID string) error {
	return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: action, ResourceType: kind, ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
}

// CreatePeer registers another installation by its endpoint, CA pin and organization.
func (s *Store) CreatePeer(ctx context.Context, p authz.Principal, name, endpoint, pin, remoteOrg, requestID string) (Peer, error) {
	if err := admin(p); err != nil {
		return Peer{}, err
	}
	u, err := url.Parse(endpoint)
	switch {
	case !namePattern.MatchString(name):
		return Peer{}, &Invalid{"the name must be lowercase letters, digits and dashes"}
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "":
		return Peer{}, &Invalid{"the endpoint must be https://host[:port]"}
	case !pinPattern.MatchString(pin):
		return Peer{}, &Invalid{"the CA pin must be sha256: followed by 64 hexadecimal digits"}
	case !ulidPattern.MatchString(remoteOrg):
		return Peer{}, &Invalid{"the peer organization ID is not valid"}
	case remoteOrg == p.OrgID:
		return Peer{}, &Invalid{"an installation cannot federate with itself"}
	}
	peer := Peer{ID: ids.New(), OrgID: p.OrgID, Name: name, Endpoint: strings.TrimRight(endpoint, "/"), CAPin: pin, RemoteOrgID: remoteOrg, CreatedBy: p.ID, CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO federation_peers (id, org_id, name, endpoint, ca_pin, remote_org_id, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			peer.ID, peer.OrgID, peer.Name, peer.Endpoint, peer.CAPin, peer.RemoteOrgID, peer.CreatedBy, peer.CreatedAt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrConflict
			}
			return err
		}
		return s.audit(ctx, tx, p, "federation.peer.create", "federation-peer", peer.ID, name+" "+peer.Endpoint+" "+pin, requestID)
	})
	return peer, err
}

// DisablePeer stops all federation with a peer at once, in both directions.
func (s *Store) DisablePeer(ctx context.Context, p authz.Principal, id, reason, requestID string) error {
	if err := admin(p); err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE federation_peers SET disabled_at = ? WHERE id = ? AND org_id = ? AND disabled_at IS NULL`), s.now().UTC(), id, p.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, p, "federation.peer.disable", "federation-peer", id, reason, requestID)
	})
}

const peerCols = `id, org_id, name, endpoint, ca_pin, remote_org_id, created_by, created_at, disabled_at IS NOT NULL`

func scanPeer(r interface{ Scan(...any) error }) (Peer, error) {
	var x Peer
	err := r.Scan(&x.ID, &x.OrgID, &x.Name, &x.Endpoint, &x.CAPin, &x.RemoteOrgID, &x.CreatedBy, &x.CreatedAt, &x.Disabled)
	return x, err
}

func (s *Store) Peers(ctx context.Context, p authz.Principal) ([]Peer, error) {
	if err := admin(p); err != nil {
		return nil, err
	}
	return s.peers(ctx, `org_id = ?`, p.OrgID)
}

func (s *Store) peers(ctx context.Context, where string, args ...any) ([]Peer, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+peerCols+` FROM federation_peers WHERE `+where+` ORDER BY name`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		x, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) peer(ctx context.Context, orgID, id string) (Peer, error) {
	x, err := scanPeer(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+peerCols+` FROM federation_peers WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, ErrNotFound
	}
	return x, err
}

// ActivePeers returns every enabled peer of every organization, for authenticating inbound calls.
func (s *Store) ActivePeers(ctx context.Context) ([]Peer, error) {
	return s.peers(ctx, `disabled_at IS NULL`)
}

// CreateGrant lets a peer run builds in a hosting project, in the named locations of this installation, with at most
// maxActive at once.
func (s *Store) CreateGrant(ctx context.Context, p authz.Principal, peerID, projectID string, locations []string, maxActive int, requestID string) (Grant, error) {
	if err := admin(p); err != nil {
		return Grant{}, err
	}
	if _, err := s.peer(ctx, p.OrgID, peerID); err != nil {
		return Grant{}, err
	}
	if len(locations) == 0 || maxActive < 1 || maxActive > 1000 {
		return Grant{}, &Invalid{"name at least one location and an active-build limit from 1 to 1000"}
	}
	for _, l := range locations {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM locations l WHERE l.org_id = ? AND l.name = ? AND NOT EXISTS (SELECT 1 FROM federation_routes r WHERE r.location_id = l.id)`), p.OrgID, l).Scan(&n); err != nil {
			return Grant{}, err
		}
		if n == 0 {
			return Grant{}, &Invalid{fmt.Sprintf("%s is not a location of this installation's own workers", l)}
		}
	}
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM projects WHERE id = ? AND org_id = ?`), projectID, p.OrgID).Scan(&n); err != nil || n == 0 {
		return Grant{}, &Invalid{"the hosting project does not exist"}
	}
	raw, _ := json.Marshal(locations)
	g := Grant{ID: ids.New(), OrgID: p.OrgID, PeerID: peerID, ProjectID: projectID, Locations: locations, MaxActive: maxActive, CreatedBy: p.ID, CreatedAt: s.now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO federation_grants (id, org_id, peer_id, project_id, locations, max_active, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			g.ID, g.OrgID, g.PeerID, g.ProjectID, string(raw), g.MaxActive, g.CreatedBy, g.CreatedAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, p, "federation.grant.create", "federation-grant", g.ID, fmt.Sprintf("peer %s, project %s, locations %s, at most %d", peerID, projectID, strings.Join(locations, ","), maxActive), requestID)
	})
	return g, err
}

func (s *Store) RevokeGrant(ctx context.Context, p authz.Principal, id, reason, requestID string) error {
	if err := admin(p); err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE federation_grants SET revoked_at = ? WHERE id = ? AND org_id = ? AND revoked_at IS NULL`), s.now().UTC(), id, p.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, p, "federation.grant.revoke", "federation-grant", id, reason, requestID)
	})
}

func (s *Store) Grants(ctx context.Context, p authz.Principal) ([]Grant, error) {
	if err := admin(p); err != nil {
		return nil, err
	}
	return s.grants(ctx, `org_id = ?`, p.OrgID)
}

func (s *Store) grants(ctx context.Context, where string, args ...any) ([]Grant, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, org_id, peer_id, project_id, locations, max_active, created_by, created_at, revoked_at IS NOT NULL FROM federation_grants WHERE `+where+` ORDER BY created_at`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var locs string
		if err := rows.Scan(&g.ID, &g.OrgID, &g.PeerID, &g.ProjectID, &locs, &g.MaxActive, &g.CreatedBy, &g.CreatedAt, &g.Revoked); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(locs), &g.Locations)
		out = append(out, g)
	}
	return out, rows.Err()
}

// grantFor finds a live grant of peer that covers location.
func (s *Store) grantFor(ctx context.Context, peer Peer, location string) (Grant, error) {
	list, err := s.grants(ctx, `org_id = ? AND peer_id = ? AND revoked_at IS NULL`, peer.OrgID, peer.ID)
	if err != nil {
		return Grant{}, err
	}
	for _, g := range list {
		if slices.Contains(g.Locations, location) {
			return g, nil
		}
	}
	return Grant{}, ErrForbidden
}

// CreateRoute adds a location that sends builds to a peer location; projects use it like any location.
func (s *Store) CreateRoute(ctx context.Context, p authz.Principal, peerID, location, remoteLocation, requestID string) (Route, error) {
	if err := admin(p); err != nil {
		return Route{}, err
	}
	peer, err := s.peer(ctx, p.OrgID, peerID)
	if err != nil {
		return Route{}, err
	}
	if !locationPattern.MatchString(location) || !locationPattern.MatchString(remoteLocation) {
		return Route{}, &Invalid{"locations are lowercase letters, digits, dots, dashes and underscores"}
	}
	r := Route{ID: ids.New(), OrgID: p.OrgID, PeerID: peerID, Location: location, RemoteLocation: remoteLocation, CreatedBy: p.ID, CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		locID := ids.New()
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO locations (id, org_id, name, description, created_at) VALUES (?, ?, ?, ?, ?)`),
			locID, p.OrgID, location, "federation peer "+peer.Name+", location "+remoteLocation, r.CreatedAt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrConflict
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO federation_routes (id, org_id, peer_id, location_id, remote_location, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			r.ID, r.OrgID, r.PeerID, locID, r.RemoteLocation, r.CreatedBy, r.CreatedAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, p, "federation.route.create", "federation-route", r.ID, location+" -> "+peer.Name+"/"+remoteLocation, requestID)
	})
	return r, err
}

func (s *Store) Routes(ctx context.Context, p authz.Principal) ([]Route, error) {
	if err := admin(p); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT r.id, r.org_id, r.peer_id, l.name, r.remote_location, r.created_by, r.created_at FROM federation_routes r JOIN locations l ON l.id = r.location_id WHERE r.org_id = ? ORDER BY l.name`), p.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.ID, &r.OrgID, &r.PeerID, &r.Location, &r.RemoteLocation, &r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// route returns the route a location name stands for.
func (s *Store) route(ctx context.Context, orgID, location string) (Route, error) {
	var r Route
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT r.id, r.org_id, r.peer_id, l.name, r.remote_location, r.created_by, r.created_at FROM federation_routes r JOIN locations l ON l.id = r.location_id WHERE l.org_id = ? AND l.name = ?`), orgID, location).
		Scan(&r.ID, &r.OrgID, &r.PeerID, &r.Location, &r.RemoteLocation, &r.CreatedBy, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Route{}, ErrNotFound
	}
	return r, err
}

// Federated reports whether a location is a route to a peer (builds.PeerRoutes).
func (s *Store) Federated(ctx context.Context, orgID, location string) (bool, error) {
	_, err := s.route(ctx, orgID, location)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

const delegationCols = `id, org_id, run_id, peer_id, remote_location, COALESCE(attempt_id, ''), state, COALESCE(remote_run_id, ''), COALESCE(remote_state, ''), COALESCE(detail, ''), COALESCE(artifact_digest, ''), last_contact_at, unreachable_since, created_at, updated_at`

func scanDelegation(r interface{ Scan(...any) error }) (Delegation, error) {
	var d Delegation
	var contact, unreachable sql.NullTime
	err := r.Scan(&d.ID, &d.OrgID, &d.RunID, &d.PeerID, &d.RemoteLocation, &d.AttemptID, &d.State, &d.RemoteRunID, &d.RemoteState, &d.Detail, &d.ArtifactDigest, &contact, &unreachable, &d.CreatedAt, &d.UpdatedAt)
	if contact.Valid {
		d.LastContactAt = &contact.Time
	}
	if unreachable.Valid {
		d.UnreachableSince = &unreachable.Time
	}
	return d, err
}

func (s *Store) Delegations(ctx context.Context, p authz.Principal, limit int) ([]Delegation, error) {
	if err := admin(p); err != nil {
		return nil, err
	}
	return s.delegations(ctx, `org_id = ? ORDER BY created_at DESC LIMIT ?`, p.OrgID, limit)
}

func (s *Store) delegations(ctx context.Context, where string, args ...any) ([]Delegation, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+delegationCols+` FROM federation_delegations WHERE `+where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delegation
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
