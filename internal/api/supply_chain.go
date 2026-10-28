package api

import (
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/signing"
	"samuori.invalid/samuori/internal/validation"
)

type signingKeyView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	PublicKey string     `json:"publicKey"`
	State     string     `json:"state"`
	Backend   string     `json:"backend,omitempty"`
	KeyRef    string     `json:"keyRef,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	RetiredAt *time.Time `json:"retiredAt,omitempty"`
}

func toKey(k signing.KeyInfo) signingKeyView {
	return signingKeyView{k.ID, k.Name, k.PublicKey, k.State, k.Backend, k.KeyRef, k.CreatedAt, k.RetiredAt}
}

func (a *API) signingStore(w http.ResponseWriter) bool {
	if a.signing == nil {
		apigen.WriteProblem(w, http.StatusConflict, "signing_unavailable", "image signing is not configured on this controller")
		return false
	}
	return true
}

func (a *API) ListSigningKeys(w http.ResponseWriter, r *http.Request, params apigen.ListSigningKeysParams) {
	if !a.signingStore(w) {
		return
	}
	list, err := a.signing.List(r.Context(), principal(r).OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items      []signingKeyView `json:"items"`
		NextCursor *string          `json:"nextCursor,omitempty"`
	}{Items: []signingKeyView{}}
	for _, k := range list {
		out.Items = append(out.Items, toKey(k))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x signingKeyView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateSigningKey(w http.ResponseWriter, r *http.Request, _ apigen.CreateSigningKeyParams) {
	var in apigen.CreateSigningKeyJSONBody
	if !decodeBody(w, r, &in) || !a.signingStore(w) {
		return
	}
	var k signing.KeyInfo
	var err error
	if in.ProviderId != nil {
		ref := ""
		if in.KeyRef != nil {
			ref = *in.KeyRef
		}
		k, err = a.signing.CreateRemote(r.Context(), principal(r), in.Name, *in.ProviderId, ref, requestID(w))
	} else {
		k, err = a.signing.Create(r.Context(), principal(r), in.Name, requestID(w))
	}
	var inv *signing.Invalid
	if errors.As(err, &inv) {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_key", inv.Reason)
		return
	}
	if a.signingError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, toKey(k))
}

func (a *API) RetireSigningKey(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RetireSigningKeyParams) {
	if !a.signingStore(w) {
		return
	}
	if a.signingError(w, r, a.signing.Retire(r.Context(), principal(r), id, requestID(w))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) signingError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, signing.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators manage signing keys")
	case errors.Is(err, signing.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "no active key with this ID")
	default:
		a.secretsError(w, r, err)
	}
	return true
}

type exceptionView struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	Classes        []string   `json:"classes"`
	PathPatterns   []string   `json:"pathPatterns"`
	Justification  string     `json:"justification"`
	State          string     `json:"state"`
	RequestedBy    string     `json:"requestedBy"`
	DecidedBy      string     `json:"decidedBy,omitempty"`
	DecisionReason string     `json:"decisionReason,omitempty"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	DecidedAt      *time.Time `json:"decidedAt,omitempty"`
}

func toException(e validation.Exception) exceptionView {
	return exceptionView{e.ID, e.ProjectID, nonNil(e.Classes), nonNil(e.PathPatterns), e.Justification, e.State, e.RequestedBy, e.DecidedBy, e.DecisionReason, e.ExpiresAt, e.CreatedAt, e.DecidedAt}
}

func writeExceptions(w http.ResponseWriter, list []validation.Exception, limit *int, cursor *string) {
	out := struct {
		Items      []exceptionView `json:"items"`
		NextCursor *string         `json:"nextCursor,omitempty"`
	}{Items: []exceptionView{}}
	for _, e := range list {
		out.Items = append(out.Items, toException(e))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, limit, cursor, func(x exceptionView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ListOutputExceptions(w http.ResponseWriter, r *http.Request, id apigen.Id, params apigen.ListOutputExceptionsParams) {
	list, err := a.validations.Exceptions(r.Context(), principal(r), operationRoles(r.Context()), id)
	if a.exceptionError(w, r, err) {
		return
	}
	writeExceptions(w, list, params.Limit, params.Cursor)
}

func (a *API) RequestOutputException(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RequestOutputExceptionParams) {
	var in apigen.OutputExceptionRequest
	if !decodeBody(w, r, &in) {
		return
	}
	var classes []string
	if in.Classes != nil {
		for _, c := range *in.Classes {
			classes = append(classes, string(c))
		}
	}
	e, err := a.validations.RequestException(r.Context(), principal(r), operationRoles(r.Context()), id, classes, in.PathPatterns, in.Justification, time.Duration(in.ExpiresInDays)*24*time.Hour, requestID(w))
	if a.exceptionError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, toException(e))
}

func (a *API) ListPendingOutputExceptions(w http.ResponseWriter, r *http.Request, params apigen.ListPendingOutputExceptionsParams) {
	list, err := a.validations.PendingExceptions(r.Context(), principal(r))
	if a.exceptionError(w, r, err) {
		return
	}
	writeExceptions(w, list, params.Limit, params.Cursor)
}

func (a *API) DecideOutputException(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DecideOutputExceptionParams) {
	var in apigen.Decision
	if !decodeBody(w, r, &in) {
		return
	}
	e, err := a.validations.DecideException(r.Context(), principal(r), id, in.Approve != nil && *in.Approve, in.Reason, requestID(w))
	if a.exceptionError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toException(e))
}

func (a *API) RevokeOutputException(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeOutputExceptionParams) {
	var in apigen.Decision
	if !decodeBody(w, r, &in) {
		return
	}
	e, err := a.validations.RevokeException(r.Context(), principal(r), id, in.Reason, requestID(w))
	if a.exceptionError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toException(e))
}

func (a *API) exceptionError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, validation.ErrExceptionInvalid):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, validation.ErrSelfApproval), errors.Is(err, validation.ErrDecided):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, validation.ErrNotFound), errors.Is(err, validation.ErrForbidden):
		a.validationError(w, r, err)
	default:
		a.tenancyError(w, r, err)
	}
	return true
}
