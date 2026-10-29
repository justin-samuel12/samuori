// Package branding stores the optional operator logo an installation shows in place of the Samuori mark (ADR 0072).
// Image bytes live in the content-addressed blob store; the branding row keeps only the digest, media type and
// intrinsic size. Uploaded SVG is sanitized before it is ever stored or served (see svg.go).
package branding

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/storage"
)

// MaxLogoBytes bounds an uploaded logo (ADR 0072).
const MaxLogoBytes = 512 << 10

const (
	ContentPNG = "image/png"
	ContentSVG = "image/svg+xml"
)

var (
	ErrNotFound    = errors.New("no operator logo set")
	ErrTooLarge    = errors.New("the logo exceeds the size limit")
	ErrUnsupported = errors.New("the logo must be a PNG or SVG image")
	ErrInvalid     = errors.New("the logo could not be read as the declared format")
)

// Variant selects which logo slot an upload or read addresses.
type Variant string

const (
	Primary Variant = "primary"
	Dark    Variant = "dark"
)

// Logo is one stored image.
type Logo struct {
	Digest      string
	ContentType string
	Width       int
	Height      int
}

// Brand is the operator branding for one installation.
type Brand struct {
	Primary   *Logo
	Dark      *Logo
	UpdatedBy string
	UpdatedAt time.Time
}

type Store struct {
	db    *db.DB
	blobs *storage.Blobs
	now   func() time.Time
}

func NewStore(d *db.DB, blobs *storage.Blobs) *Store {
	return &Store{db: d, blobs: blobs, now: time.Now}
}

// Get returns the installation's branding, or ErrNotFound when no logo is set.
func (s *Store) Get(ctx context.Context, orgID string) (Brand, error) {
	var (
		b                Brand
		pd, pct, dd, dct sql.NullString
		pw, ph, dw, dh   sql.NullInt64
	)
	row := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT logo_digest, logo_content_type, logo_width, logo_height,
		logo_dark_digest, logo_dark_content_type, logo_dark_width, logo_dark_height, updated_by, updated_at
		FROM branding WHERE org_id = ?`), orgID)
	err := row.Scan(&pd, &pct, &pw, &ph, &dd, &dct, &dw, &dh, &b.UpdatedBy, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Brand{}, ErrNotFound
	}
	if err != nil {
		return Brand{}, err
	}
	if pd.Valid {
		b.Primary = &Logo{Digest: pd.String, ContentType: pct.String, Width: int(pw.Int64), Height: int(ph.Int64)}
	}
	if dd.Valid {
		b.Dark = &Logo{Digest: dd.String, ContentType: dct.String, Width: int(dw.Int64), Height: int(dh.Int64)}
	}
	return b, nil
}

// PublicLogo returns the installation's logo for a variant without a principal, for the pre-authentication login
// page. One organization per installation (ADR 0071) means at most one branding row, so no identifier is needed.
func (s *Store) PublicLogo(ctx context.Context, v Variant) (*Logo, error) {
	var (
		pd, pct, dd, dct sql.NullString
		pw, ph, dw, dh   sql.NullInt64
	)
	row := s.db.SQL.QueryRowContext(ctx, `SELECT logo_digest, logo_content_type, logo_width, logo_height,
		logo_dark_digest, logo_dark_content_type, logo_dark_width, logo_dark_height FROM branding LIMIT 1`)
	err := row.Scan(&pd, &pct, &pw, &ph, &dd, &dct, &dw, &dh)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	dig, ct, wd, ht := pd, pct, pw, ph
	if v == Dark {
		dig, ct, wd, ht = dd, dct, dw, dh
	}
	if !dig.Valid {
		return nil, ErrNotFound
	}
	return &Logo{Digest: dig.String, ContentType: ct.String, Width: int(wd.Int64), Height: int(ht.Int64)}, nil
}

// Bytes returns a stored logo's verified bytes.
func (s *Store) Bytes(ctx context.Context, l *Logo) ([]byte, error) {
	return s.blobs.ReadAll(l.Digest, MaxLogoBytes)
}

// SetLogo validates and sanitizes an uploaded image, stores it as a blob, and records it for the variant. SVG is
// sanitized before storage; PNG is decoded to confirm the format and read its dimensions.
func (s *Store) SetLogo(ctx context.Context, orgID, actor string, v Variant, declared string, r io.Reader) (Logo, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxLogoBytes+1))
	if err != nil {
		return Logo{}, err
	}
	if int64(len(raw)) > MaxLogoBytes {
		return Logo{}, ErrTooLarge
	}

	var (
		stored      []byte
		contentType string
		w, h        int
	)
	switch declared {
	case ContentSVG:
		clean, fw, fh, serr := SanitizeSVG(raw)
		if serr != nil {
			return Logo{}, fmt.Errorf("%w: %v", ErrInvalid, serr)
		}
		stored, contentType, w, h = clean, ContentSVG, int(fw+0.5), int(fh+0.5)
	case ContentPNG:
		cfg, format, derr := image.DecodeConfig(bytes.NewReader(raw))
		if derr != nil || format != "png" {
			return Logo{}, ErrInvalid
		}
		stored, contentType, w, h = raw, ContentPNG, cfg.Width, cfg.Height
	default:
		return Logo{}, ErrUnsupported
	}

	digest, _, err := s.blobs.PutClass(storage.ClassLogo, bytes.NewReader(stored), MaxLogoBytes)
	if err != nil {
		return Logo{}, err
	}
	l := Logo{Digest: digest, ContentType: contentType, Width: w, Height: h}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return s.upsert(ctx, tx, orgID, actor, now, func(b *row) {
			if v == Dark {
				b.darkDigest, b.darkType, b.darkW, b.darkH = digest, contentType, w, h
			} else {
				b.digest, b.ctype, b.w, b.h = digest, contentType, w, h
			}
		})
	})
	if err != nil {
		return Logo{}, err
	}
	return l, nil
}

// Clear removes the operator logo. Clearing the primary removes the dark variant too, because a dark-only brand has
// no primary to show; clearing the dark variant leaves the primary in place.
func (s *Store) Clear(ctx context.Context, orgID, actor string, v Variant) error {
	if v == Dark {
		return s.db.InTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE branding SET logo_dark_digest = NULL, logo_dark_content_type = NULL,
				logo_dark_width = NULL, logo_dark_height = NULL, updated_by = ?, updated_at = ? WHERE org_id = ?`),
				actor, s.now().UTC(), orgID)
			return err
		})
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM branding WHERE org_id = ?`), orgID)
		return err
	})
}

type row struct {
	digest, ctype        string
	w, h                 int
	darkDigest, darkType string
	darkW, darkH         int
}

func (s *Store) upsert(ctx context.Context, tx *sql.Tx, orgID, actor string, now time.Time, mutate func(*row)) error {
	var cur row
	var (
		pd, pct, dd, dct sql.NullString
		pw, ph, dw, dh   sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT logo_digest, logo_content_type, logo_width, logo_height,
		logo_dark_digest, logo_dark_content_type, logo_dark_width, logo_dark_height FROM branding WHERE org_id = ?`), orgID).
		Scan(&pd, &pct, &pw, &ph, &dd, &dct, &dw, &dh)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		cur = row{pd.String, pct.String, int(pw.Int64), int(ph.Int64), dd.String, dct.String, int(dw.Int64), int(dh.Int64)}
	}
	mutate(&cur)
	_, err = tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM branding WHERE org_id = ?`), orgID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO branding (org_id, logo_digest, logo_content_type, logo_width, logo_height,
		logo_dark_digest, logo_dark_content_type, logo_dark_width, logo_dark_height, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		orgID, nullStr(cur.digest), nullStr(cur.ctype), nullInt(cur.w), nullInt(cur.h),
		nullStr(cur.darkDigest), nullStr(cur.darkType), nullInt(cur.darkW), nullInt(cur.darkH), actor, now)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
