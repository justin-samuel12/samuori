package artifacts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// ScopedReuseEnabled stays false until the OD-19 controls pass in CI (SEC-CCH-02..06); until then secret-bearing
// builds never import or export cache (docs/architecture/build-cache.md, rule 3).
const ScopedReuseEnabled = false

type CacheMode string

const (
	CacheNone   CacheMode = "none"
	CacheScoped CacheMode = "scoped"
)

// CacheScope identifies who may share cache records: organization, trust domain, project, the secret-binding
// version IDs and the build-policy version. It never includes secret material or hashes of secret values.
type CacheScope struct {
	OrgID, TrustDomain, ProjectID string
	BindingVersions               []string
	PolicyVersion                 string
}

func (c CacheScope) ID() string {
	versions := append([]string{}, c.BindingVersions...)
	sort.Strings(versions)
	sum := sha256.Sum256([]byte(strings.Join([]string{c.OrgID, c.TrustDomain, c.ProjectID, strings.Join(versions, ","), c.PolicyVersion}, "\x00")))
	return "cs-" + hex.EncodeToString(sum[:16])
}

// CachePolicy decides the cache mode for a build. Secret-bearing builds get none while scoped reuse is disabled;
// a "latest" binding that has not resolved to a concrete version makes any build non-cacheable.
func CachePolicy(secretBearing, cleanBuild bool, scope CacheScope) (CacheMode, string) {
	if cleanBuild || (secretBearing && !ScopedReuseEnabled) {
		return CacheNone, ""
	}
	for _, v := range scope.BindingVersions {
		if v == "" || v == "latest" {
			return CacheNone, ""
		}
	}
	return CacheScoped, scope.ID()
}

// CacheLimits bound shared caches: entries older than TTL or larger than MaxScopeBytes are never imported and are
// removed by garbage collection.
type CacheLimits struct {
	TTL           time.Duration
	MaxScopeBytes int64
}

func DefaultCacheLimits() CacheLimits {
	return CacheLimits{TTL: 7 * 24 * time.Hour, MaxScopeBytes: 10 << 30}
}

type CacheEntry struct {
	ID, OrgID, ProjectID, Scope, Digest string
	Size                                int64
	RunID                               string
	CreatedAt                           time.Time
	LastUsedAt                          *time.Time
}

var ErrNoCache = errors.New("artifacts: the scope has no importable cache")

// importable lists, newest first, the entries a build may import: from runs that succeeded, within the limits.
const importable = `SELECT c.id, c.org_id, c.project_id, c.scope, c.digest, c.size, c.run_id, c.created_at, c.last_used_at FROM cache_entries c
	JOIN build_runs r ON r.id = c.run_id AND r.state = 'succeeded' WHERE c.created_at >= ? AND c.size <= ?`

func scanCache(sc interface{ Scan(...any) error }) (CacheEntry, error) {
	var e CacheEntry
	var used sql.NullTime
	if err := sc.Scan(&e.ID, &e.OrgID, &e.ProjectID, &e.Scope, &e.Digest, &e.Size, &e.RunID, &e.CreatedAt, &used); err != nil {
		return CacheEntry{}, err
	}
	if used.Valid {
		e.LastUsedAt = &used.Time
	}
	return e, nil
}

// LatestCache returns the newest importable cache of exactly this scope; there is no prefix or partial matching.
func (s *Store) LatestCache(ctx context.Context, orgID, scope string) (CacheEntry, error) {
	e, err := scanCache(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(importable+` AND c.org_id = ? AND c.scope = ? ORDER BY c.created_at DESC, c.id DESC LIMIT 1`),
		s.now().UTC().Add(-s.CacheLimits.TTL), s.CacheLimits.MaxScopeBytes, orgID, scope))
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, ErrNoCache
	}
	return e, err
}

func (s *Store) TouchCache(ctx context.Context, id string) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE cache_entries SET last_used_at = ? WHERE id = ?`), s.now().UTC(), id)
	return err
}

type CacheSummary struct {
	Scope, ProjectID string
	Entries          int
	Bytes            int64
	Newest           time.Time
	LastUsed         *time.Time
}

// Caches summarizes an organization's cache scopes.
func (s *Store) Caches(ctx context.Context, orgID string) ([]CacheSummary, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT scope, MIN(project_id), COUNT(*), SUM(size), MAX(created_at), MAX(last_used_at) FROM cache_entries WHERE org_id = ? GROUP BY scope ORDER BY MAX(created_at) DESC`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheSummary{}
	for rows.Next() {
		var c CacheSummary
		var newest any
		var used any
		if err := rows.Scan(&c.Scope, &c.ProjectID, &c.Entries, &c.Bytes, &newest, &used); err != nil {
			return nil, err
		}
		c.Newest = asTime(newest)
		if t := asTime(used); !t.IsZero() {
			c.LastUsed = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// asTime reads an aggregate time, which SQLite returns as text.
func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
			if p, err := time.Parse(layout, t); err == nil {
				return p
			}
		}
	case []byte:
		return asTime(string(t))
	}
	return time.Time{}
}

// PurgeCache removes every entry of a scope and audits it; garbage collection then removes the blobs. The next
// build of the scope runs without cache.
func (s *Store) PurgeCache(ctx context.Context, actor authz.Principal, scope, requestID string) (int64, error) {
	var n int64
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM cache_entries WHERE org_id = ? AND scope = ?`), actor.OrgID, scope)
		if err != nil {
			return err
		}
		if n, _ = res.RowsAffected(); n == 0 {
			return ErrNoCache
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "build-cache.purge", ResourceType: "build-cache", ResourceID: scope, Outcome: "succeeded", Reason: fmt.Sprintf("%d entries", n), RequestID: requestID})
	})
	return n, err
}

// pruneCaches keeps only each scope's newest importable entry and forgets cache artifacts no entry references, so
// that garbage collection reclaims superseded and expired caches.
func (s *Store) pruneCaches(ctx context.Context) error {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(importable+` ORDER BY c.org_id, c.scope, c.created_at DESC, c.id DESC`), s.now().UTC().Add(-s.CacheLimits.TTL), s.CacheLimits.MaxScopeBytes)
	if err != nil {
		return err
	}
	keep, seen := []any{}, map[string]bool{}
	for rows.Next() {
		e, err := scanCache(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if k := e.OrgID + "\x00" + e.Scope; !seen[k] {
			seen[k] = true
			keep = append(keep, e.ID)
		}
	}
	rows.Close()
	// Entries of runs still in progress are kept until their run ends.
	q := `DELETE FROM cache_entries WHERE run_id NOT IN (SELECT id FROM build_runs WHERE state NOT IN ('succeeded', 'failed', 'timed_out', 'cancelled', 'expired', 'blocked', 'discarded'))`
	if len(keep) > 0 {
		q += ` AND id NOT IN (?` + strings.Repeat(", ?", len(keep)-1) + `)`
	}
	if _, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(q), keep...); err != nil {
		return err
	}
	_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`DELETE FROM build_artifacts WHERE kind = ? AND digest NOT IN (SELECT digest FROM cache_entries) AND digest NOT IN (SELECT digest FROM artifact_holds WHERE released_at IS NULL)`), KindCache)
	return err
}
