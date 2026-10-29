package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/branding"
)

func (a *API) GetBranding(w http.ResponseWriter, r *http.Request) {
	b, err := a.branding.Get(r.Context(), principal(r).OrgID)
	if errors.Is(err, branding.ErrNotFound) {
		writeJSON(w, http.StatusOK, apigen.BrandingStatus{CustomLogo: false})
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, brandingStatus(b))
}

// GetBrandingLogo and GetBrandingLogoDark are unauthenticated: the login page needs the logo before sign-in. They
// serve only the image bytes and expose no other data (ADR 0072, docs/architecture/security.md#operator-logos).
func (a *API) GetBrandingLogo(w http.ResponseWriter, r *http.Request) {
	a.serveLogo(w, r, branding.Primary)
}

func (a *API) GetBrandingLogoDark(w http.ResponseWriter, r *http.Request) {
	a.serveLogo(w, r, branding.Dark)
}

func (a *API) serveLogo(w http.ResponseWriter, r *http.Request, v branding.Variant) {
	l, err := a.branding.PublicLogo(r.Context(), v)
	if errors.Is(err, branding.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "no operator logo is set")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	etag := `"` + l.Digest + `"`
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=300, must-revalidate")
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, err := a.branding.Bytes(r.Context(), l)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	h.Set("Content-Type", l.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	// Defence in depth should the image be opened directly: no scripts, no fetches, sandboxed (TM, sanitiser is
	// the primary control for stored SVG).
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	h.Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *API) UploadBrandingLogo(w http.ResponseWriter, r *http.Request, params apigen.UploadBrandingLogoParams) {
	p := principal(r)
	if !p.IsOrgAdmin() {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators set the logo")
		return
	}
	v := branding.Primary
	if params.Variant != nil && *params.Variant == apigen.Dark {
		v = branding.Dark
	}
	ctype := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	_, err := a.branding.SetLogo(r.Context(), p.OrgID, p.ID, v, ctype, r.Body)
	switch {
	case errors.Is(err, branding.ErrTooLarge):
		apigen.WriteProblem(w, http.StatusRequestEntityTooLarge, "too_large", "the logo must be 512 KB or smaller")
		return
	case errors.Is(err, branding.ErrUnsupported):
		apigen.WriteProblem(w, http.StatusUnsupportedMediaType, "unsupported_media", "the logo must be a PNG or SVG image")
		return
	case errors.Is(err, branding.ErrInvalid):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "the file could not be read as the declared image format")
		return
	case err != nil:
		a.internalError(w, r, err)
		return
	}
	if err := a.auditBranding(r.Context(), w, p, "branding.logo.set"); err != nil {
		a.internalError(w, r, err)
		return
	}
	b, err := a.branding.Get(r.Context(), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, brandingStatus(b))
}

func (a *API) ClearBranding(w http.ResponseWriter, r *http.Request, _ apigen.ClearBrandingParams) {
	a.clearBranding(w, r, branding.Primary, "branding.cleared")
}

func (a *API) ClearBrandingDark(w http.ResponseWriter, r *http.Request, _ apigen.ClearBrandingDarkParams) {
	a.clearBranding(w, r, branding.Dark, "branding.logo.dark.cleared")
}

func (a *API) clearBranding(w http.ResponseWriter, r *http.Request, v branding.Variant, action string) {
	p := principal(r)
	if !p.IsOrgAdmin() {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators clear the logo")
		return
	}
	if err := a.branding.Clear(r.Context(), p.OrgID, p.ID, v); err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.auditBranding(r.Context(), w, p, action); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) auditBranding(ctx context.Context, w http.ResponseWriter, p authz.Principal, action string) error {
	return a.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, a.db, tx, audit.Event{Actor: p, Action: action, ResourceType: "branding", ResourceID: p.OrgID, Outcome: "succeeded", RequestID: requestID(w)})
	})
}

func brandingStatus(b branding.Brand) apigen.BrandingStatus {
	out := apigen.BrandingStatus{CustomLogo: b.Primary != nil}
	if !b.UpdatedAt.IsZero() {
		t := b.UpdatedAt
		out.UpdatedAt = &t
	}
	if b.Primary != nil {
		l := toBrandingLogo("/api/v1/branding/logo", b.Primary)
		out.Logo = &l
	}
	if b.Dark != nil {
		l := toBrandingLogo("/api/v1/branding/logo/dark", b.Dark)
		out.LogoDark = &l
	}
	return out
}

func toBrandingLogo(path string, l *branding.Logo) apigen.BrandingLogo {
	var ratio float32
	if l.Height > 0 {
		ratio = float32(l.Width) / float32(l.Height)
	}
	token := l.Digest
	if cut, ok := strings.CutPrefix(token, "sha256:"); ok && len(cut) >= 12 {
		token = cut[:12]
	}
	return apigen.BrandingLogo{
		Url:         path + "?v=" + token,
		ContentType: apigen.BrandingLogoContentType(l.ContentType),
		Width:       float32(l.Width),
		Height:      float32(l.Height),
		AspectRatio: ratio,
	}
}
