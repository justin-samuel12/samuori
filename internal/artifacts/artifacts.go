// Package artifacts stores build outputs in internal storage. Workers upload only with a short-lived grant scoped
// to one attempt, kind, digest and size; the stored bytes must hash to the granted digest (ADR 0024).
package artifacts

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/storage"
)

const (
	KindImage = "image"
	KindLog   = "log"
	KindCache = "cache"
	// KindRailpackPlan and KindRailpackInfo are the plan a railpack build ran and Railpack's planning report (ADR 0047).
	KindRailpackPlan = "railpack-plan"
	KindRailpackInfo = "railpack-info"
	MaxRailpackBytes = 4 << 20
	// KindBuildpacksMetadata holds a buildpacks build's pins, buildpacks labels and lifecycle report;
	// KindBuildpacksSBOM is a tar of the lifecycle's SBOM files (ADR 0047).
	KindBuildpacksMetadata = "buildpacks-metadata"
	KindBuildpacksSBOM     = "buildpacks-sbom"
	MaxBuildpacksSBOMBytes = 32 << 20

	MaxImageBytes = 20 << 30
	MaxLogBytes   = 64 << 20
	GrantTTL      = 15 * time.Minute
)

var (
	ErrGrant    = errors.New("artifacts: invalid or expired upload grant")
	ErrMismatch = errors.New("artifacts: content does not match the granted digest or size")
	digestRE    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Grant is what a signed upload token authorizes.
type Grant struct {
	OrgID      string `json:"o"`
	RunID      string `json:"r"`
	AttemptID  string `json:"a"`
	Generation int64  `json:"g"`
	WorkerID   string `json:"w"`
	Kind       string `json:"k"`
	Digest     string `json:"d"`
	Size       int64  `json:"s"`
	Scope      string `json:"c,omitempty"`
	Expires    int64  `json:"e"`
}

type Store struct {
	db    *db.DB
	blobs *storage.Blobs
	key   []byte
	now   func() time.Time
	// CacheLimits bound shared build caches (docs/architecture/build-cache.md).
	CacheLimits CacheLimits
}

// NewStore loads or creates the grant-signing key in keyDir.
func NewStore(d *db.DB, blobs *storage.Blobs, keyDir string) (*Store, error) {
	p := filepath.Join(keyDir, "grant.key")
	key, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(keyDir, 0o700); err != nil {
			return nil, err
		}
		err = os.WriteFile(p, key, 0o600)
	}
	if err != nil {
		return nil, err
	}
	return &Store{db: d, blobs: blobs, key: key, now: time.Now, CacheLimits: DefaultCacheLimits()}, nil
}

func (s *Store) Blobs() *storage.Blobs { return s.blobs }

func limit(kind string) (int64, bool) {
	switch kind {
	case KindImage, KindCache:
		return MaxImageBytes, true
	case KindLog:
		return MaxLogBytes, true
	case KindRailpackPlan, KindRailpackInfo:
		return MaxRailpackBytes, true
	case KindBuildpacksMetadata:
		return MaxRailpackBytes, true
	case KindBuildpacksSBOM:
		return MaxBuildpacksSBOMBytes, true
	}
	return 0, false
}

// Sign issues a token for g, valid for GrantTTL.
func (s *Store) Sign(g Grant) (string, time.Time, error) {
	max, ok := limit(g.Kind)
	if !ok || !digestRE.MatchString(g.Digest) || g.Size <= 0 || g.Size > max {
		return "", time.Time{}, fmt.Errorf("artifacts: refused %s upload of %d bytes", g.Kind, g.Size)
	}
	exp := s.now().Add(GrantTTL)
	g.Expires = exp.Unix()
	body, _ := json.Marshal(g)
	mac := hmac.New(sha256.New, s.key)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), exp, nil
}

// Verify returns the grant a token carries if its signature is valid and it has not expired.
func (s *Store) Verify(token string) (Grant, error) {
	body64, sig64, ok := strings.Cut(token, ".")
	if !ok {
		return Grant{}, ErrGrant
	}
	body, err1 := base64.RawURLEncoding.DecodeString(body64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil {
		return Grant{}, ErrGrant
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Grant{}, ErrGrant
	}
	var g Grant
	if json.Unmarshal(body, &g) != nil || s.now().Unix() > g.Expires {
		return Grant{}, ErrGrant
	}
	return g, nil
}

// Accept stores an upload for g. The content must hash to the granted digest and have the granted size; anything
// else is discarded (SEC-STO-02).
func (s *Store) Accept(ctx context.Context, g Grant, r io.Reader) error {
	class := storage.ClassImage
	if g.Kind == KindCache {
		class = storage.ClassOther
	}
	digest, n, err := s.blobs.PutClass(class, io.LimitReader(r, g.Size+1), g.Size)
	if errors.Is(err, storage.ErrTooLarge) {
		return ErrMismatch
	}
	if err != nil {
		return err
	}
	if digest != g.Digest || n != g.Size {
		return ErrMismatch
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		now := s.now().UTC()
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM build_artifacts WHERE attempt_id = ? AND kind = ? AND digest = ?)`),
			ids.New(), g.OrgID, g.RunID, g.AttemptID, g.Generation, g.Kind, g.Digest, g.Size, now, g.AttemptID, g.Kind, g.Digest); err != nil {
			return err
		}
		if g.Kind != KindCache || g.Scope == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO cache_entries (id, org_id, project_id, scope, digest, size, run_id, attempt_id, created_at)
			SELECT ?, ?, project_id, ?, ?, ?, id, ?, ? FROM build_runs WHERE id = ? AND org_id = ? AND NOT EXISTS (SELECT 1 FROM cache_entries WHERE attempt_id = ? AND digest = ?)`),
			ids.New(), g.OrgID, g.Scope, g.Digest, g.Size, g.AttemptID, now, g.RunID, g.OrgID, g.AttemptID, g.Digest)
		return err
	})
}

// Committed reports whether an attempt stored an artifact of kind with digest, and verifies the stored bytes.
func (s *Store) Committed(ctx context.Context, attemptID, kind, digest string) (bool, error) {
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM build_artifacts WHERE attempt_id = ? AND kind = ? AND digest = ?`), attemptID, kind, digest).Scan(&n); err != nil || n == 0 {
		return false, err
	}
	rc, err := s.blobs.Open(digest)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return false, err
	}
	return true, nil
}

// Hold protects digest from garbage collection until released.
func (s *Store) Hold(ctx context.Context, orgID, digest, reason, actor string) (string, error) {
	id := ids.New()
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`INSERT INTO artifact_holds (id, org_id, digest, reason, actor_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`), id, orgID, digest, reason, actor, s.now().UTC())
	return id, err
}

func (s *Store) Release(ctx context.Context, id string) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE artifact_holds SET released_at = ? WHERE id = ? AND released_at IS NULL`), s.now().UTC(), id)
	return err
}
