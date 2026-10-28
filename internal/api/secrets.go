package api

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/secrets"
)

func (a *API) secretsError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *secrets.Invalid
	var inUse *secrets.InUse
	switch {
	case errors.As(err, &inUse):
		if inUse.Store {
			apigen.WriteProblem(w, http.StatusConflict, "store_in_use", "This store is still used by "+strings.Join(inUse.Uses, "; ")+". A store that grants, bindings or signing keys ever used is kept for their history; edit it instead.")
			break
		}
		apigen.WriteProblem(w, http.StatusConflict, "secret_in_use", "This secret is still in use: "+strings.Join(inUse.Uses, "; ")+". Revoke or replace those first.")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_secret", inv.Reason)
	case errors.Is(err, secrets.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, secrets.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "you do not have permission for this secret operation")
	case errors.Is(err, secrets.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "the secret changed or already exists")
	default:
		a.internalError(w, r, err)
	}
}

func toSecretProvider(p secrets.Provider) apigen.SecretProvider {
	opt := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	out := apigen.SecretProvider{Id: p.ID, Name: p.Name, Type: p.Type, Health: apigen.ConnectionTest{Status: apigen.ConnectionTestStatusNotTested}}
	if p.Type != secrets.ProviderLocal {
		c := p.Config
		has, long := p.CredentialSecretID != "", c.AuthMethod == "access-key"
		out.Address, out.Mount, out.Namespace, out.Region, out.AuthMethod, out.RoleArn, out.Endpoint = opt(c.Address), opt(c.Mount), opt(c.Namespace), opt(c.Region), opt(c.AuthMethod), opt(c.RoleARN), opt(c.Endpoint)
		out.RoleId, out.ExternalId, out.CaCertPem = opt(c.RoleID), opt(c.ExternalID), opt(c.CACertPEM)
		out.HasCredential, out.LongLivedCredential = &has, &long
		if len(c.AllowedPrefixes) > 0 {
			out.AllowedPrefixes = &c.AllowedPrefixes
		}
	}
	if lc := p.LastCheck; lc != nil {
		at := lc.TestedAt
		out.Health = apigen.ConnectionTest{Status: apigen.ConnectionTestStatus(lc.Status), TestedAt: &at}
		if lc.Detail != "" {
			d := lc.Detail
			out.Health.Detail = &d
		}
	}
	return out
}

func (a *API) ListSecretProviders(w http.ResponseWriter, r *http.Request) {
	list, err := a.secrets.Providers(r.Context(), principal(r))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	out := apigen.SecretProviderList{Items: []apigen.SecretProvider{}}
	for _, p := range list {
		out.Items = append(out.Items, toSecretProvider(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateSecretProvider(w http.ResponseWriter, r *http.Request, _ apigen.CreateSecretProviderParams) {
	var in apigen.SecretProviderCreate
	if !decodeBody(w, r, &in) {
		return
	}
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	cfg := secrets.ExternalConfig{Address: val(in.Address), Mount: val(in.Mount), Namespace: val(in.Namespace), CACertPEM: val(in.CaCertPem), AuthMethod: string(in.AuthMethod),
		RoleID: val(in.RoleId), Region: val(in.Region), RoleARN: val(in.RoleArn), ExternalID: val(in.ExternalId), Endpoint: val(in.Endpoint)}
	if in.AllowedPrefixes != nil {
		cfg.AllowedPrefixes = *in.AllowedPrefixes
	}
	p, err := a.secrets.CreateProvider(r.Context(), principal(r), in.Name, string(in.Type), cfg, []byte(val(in.Credential)), requestID(w))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSecretProvider(p))
}

func (a *API) UpdateSecretProvider(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateSecretProviderParams) {
	var in apigen.SecretProviderUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	cfg := secrets.ExternalConfig{Address: val(in.Address), Mount: val(in.Mount), Namespace: val(in.Namespace), CACertPEM: val(in.CaCertPem), AuthMethod: string(in.AuthMethod),
		RoleID: val(in.RoleId), Region: val(in.Region), RoleARN: val(in.RoleArn), ExternalID: val(in.ExternalId), Endpoint: val(in.Endpoint)}
	if in.AllowedPrefixes != nil {
		cfg.AllowedPrefixes = *in.AllowedPrefixes
	}
	p, err := a.secrets.UpdateProvider(r.Context(), principal(r), id, in.Name, cfg, []byte(val(in.Credential)), requestID(w))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSecretProvider(p))
}

func (a *API) DeleteSecretProvider(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteSecretProviderParams) {
	if err := a.secrets.DeleteProvider(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.secretsError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) TestSecretProvider(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.TestSecretProviderParams) {
	now := time.Now().UTC()
	out := apigen.ConnectionTest{Status: apigen.ConnectionTestStatusOk, TestedAt: &now}
	detail, err := a.secrets.Check(r.Context(), principal(r), id)
	if errors.Is(err, secrets.ErrForbidden) || errors.Is(err, secrets.ErrNotFound) {
		a.secretsError(w, r, err)
		return
	}
	if err != nil {
		out.Status = apigen.ConnectionTestStatusFailed
	}
	if detail != "" {
		out.Detail = &detail
	}
	writeJSON(w, http.StatusOK, out)
}

func toLocalSecret(s secrets.Secret) apigen.LocalSecret {
	out := apigen.LocalSecret{Id: s.ID, Name: s.Name, Type: apigen.LocalSecretType(s.Type), Version: s.Version, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	if s.ControllerOnly {
		out.ControllerOnly, out.SamuoriUses = ptr(true), ptr(append([]string{}, s.SamuoriUses...))
	}
	return out
}

func (a *API) ListLocalSecrets(w http.ResponseWriter, r *http.Request, providerID apigen.Id) {
	list, err := a.secrets.ListSecrets(r.Context(), principal(r))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	local, err := a.secrets.LocalProvider(r.Context(), principal(r).OrgID)
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	if local.ID != providerID {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	out := apigen.LocalSecretList{Items: []apigen.LocalSecret{}}
	for _, s := range list {
		out.Items = append(out.Items, toLocalSecret(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// idempotent runs create once per Idempotency-Key; a replay returns the resource the first request produced.
func (a *API) idempotent(w http.ResponseWriter, r *http.Request, kind, key string, create func() (string, error), replay func(id string)) {
	p := principal(r)
	var op operations.Operation
	var created bool
	err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		var err error
		op, created, err = a.ops.Begin(r.Context(), tx, p, kind, key)
		return err
	})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !created {
		if op.ResourceID == "" {
			apigen.WriteProblem(w, http.StatusConflict, "in_progress", "a request with this idempotency key did not complete; use a new key")
			return
		}
		replay(op.ResourceID)
		return
	}
	id, err := create()
	status, code := operations.Succeeded, ""
	if err != nil {
		status, code = operations.Failed, "failed"
	}
	if ferr := a.db.InTx(r.Context(), func(tx *sql.Tx) error { return a.ops.Finish(r.Context(), tx, op.ID, status, kind, id, code, "") }); ferr != nil && err == nil {
		a.log.Error("finishing operation", "operation", op.ID, "error", ferr)
	}
}

func (a *API) StoreLocalSecret(w http.ResponseWriter, r *http.Request, providerID apigen.Id, params apigen.StoreLocalSecretParams) {
	var in apigen.LocalSecretWrite
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Value == nil {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_secret", "a value is required")
		return
	}
	value := []byte(*in.Value)
	if in.Type == apigen.LocalSecretWriteTypeBinary {
		var err error
		if value, err = base64.StdEncoding.DecodeString(*in.Value); err != nil {
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_secret", "binary values must be base64")
			return
		}
	}
	p := principal(r)
	a.idempotent(w, r, "secret-store", params.IdempotencyKey, func() (string, error) {
		s, err := a.secrets.Store(r.Context(), p, providerID, in.Name, string(in.Type), value, requestID(w))
		clear(value)
		if err != nil {
			a.secretsError(w, r, err)
			return "", err
		}
		writeJSON(w, http.StatusCreated, toLocalSecret(s))
		return s.ID, nil
	}, func(id string) {
		list, err := a.secrets.ListSecrets(r.Context(), p)
		if err != nil {
			a.secretsError(w, r, err)
			return
		}
		for _, s := range list {
			if s.ID == id {
				writeJSON(w, http.StatusOK, toLocalSecret(s))
				return
			}
		}
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	})
}

func toBinding(b secrets.Binding) apigen.SecretBinding {
	out := apigen.SecretBinding{Id: b.ID, ProjectId: b.ProjectID, LogicalId: b.LogicalID, ProviderId: b.ProviderID, ProviderReference: b.ProviderReference,
		Reason: b.Reason, Version: b.Version, Status: apigen.SecretBindingStatus(b.Status), CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, ExpiresAt: b.ExpiresAt}
	if b.Status == secrets.Active && b.Expired(time.Now()) {
		out.Status = apigen.SecretBindingStatusExpired
	}
	if b.SecretName != "" {
		out.SecretName = &b.SecretName
	}
	if b.GrantID != "" {
		grant := b.GrantID
		out.GrantId = &grant
	}
	if b.Encoding != "" {
		e := apigen.SecretBindingEncoding(b.Encoding)
		out.Encoding = &e
	}
	switch b.Usage {
	case "both":
		out.Usage = []apigen.SecretBindingUsage{apigen.SecretBindingUsageBuild, apigen.SecretBindingUsageTest}
	default:
		out.Usage = []apigen.SecretBindingUsage{apigen.SecretBindingUsage(b.Usage)}
	}
	policy := apigen.SecretBindingVersionPolicyLatest
	if b.VersionPolicy != "latest" {
		policy = apigen.SecretBindingVersionPolicyPinned
		if n, err := strconv.Atoi(b.VersionPolicy); err == nil {
			out.PinnedVersion = &n
		} else {
			v := b.VersionPolicy
			out.PinnedVersionId = &v
		}
	}
	out.VersionPolicy = &policy
	pools := append([]string{}, b.PoolIDs...)
	out.EligiblePoolIds = &pools
	return out
}

func (a *API) ListSecretBindings(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	list, err := a.secrets.ListBindings(r.Context(), principal(r), projectID)
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	out := apigen.SecretBindingList{Items: []apigen.SecretBinding{}}
	for _, b := range list {
		out.Items = append(out.Items, toBinding(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateSecretBinding(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.CreateSecretBindingParams) {
	var in apigen.SecretBindingCreate
	if !decodeBody(w, r, &in) {
		return
	}
	req := secrets.BindRequest{ProjectID: projectID, LogicalID: in.LogicalId, ProviderID: in.ProviderId, ProviderReference: in.ProviderReference, Reason: in.Reason, ExpiresAt: in.ExpiresAt}
	if in.Encoding != nil {
		req.Encoding = string(*in.Encoding)
	}
	build, test := slices.Contains(in.Usage, apigen.SecretBindingCreateUsageBuild), slices.Contains(in.Usage, apigen.SecretBindingCreateUsageTest)
	switch {
	case build && test:
		req.Usage = "both"
	case test:
		req.Usage = "test"
	default:
		req.Usage = "build"
	}
	if in.VersionPolicy != nil && *in.VersionPolicy == apigen.SecretBindingCreateVersionPolicyPinned {
		switch {
		case in.PinnedVersion != nil && in.PinnedVersionId == nil:
			req.VersionPolicy = strconv.Itoa(*in.PinnedVersion)
		case in.PinnedVersionId != nil && in.PinnedVersion == nil && *in.PinnedVersionId != "latest":
			req.VersionPolicy = *in.PinnedVersionId
		default:
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_secret", "a pinned version policy needs exactly one of pinnedVersion or pinnedVersionId")
			return
		}
	}
	if in.EligiblePoolIds != nil {
		req.PoolIDs = *in.EligiblePoolIds
	}
	p := principal(r)
	a.idempotent(w, r, "secret-binding", params.IdempotencyKey, func() (string, error) {
		b, err := a.secrets.Bind(r.Context(), p, req, requestID(w))
		if err != nil {
			a.secretsError(w, r, err)
			return "", err
		}
		writeJSON(w, http.StatusCreated, toBinding(b))
		return b.ID, nil
	}, func(id string) {
		a.writeBinding(w, r, projectID, id)
	})
}

func (a *API) writeBinding(w http.ResponseWriter, r *http.Request, projectID, id string) {
	list, err := a.secrets.ListBindings(r.Context(), principal(r), projectID)
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	for _, b := range list {
		if b.ID == id {
			writeJSON(w, http.StatusOK, toBinding(b))
			return
		}
	}
	apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
}

func (a *API) ApplySecretBindingAction(w http.ResponseWriter, r *http.Request, id apigen.Id, params apigen.ApplySecretBindingActionParams) {
	var in apigen.SecretBindingAction
	if !decodeBody(w, r, &in) {
		return
	}
	p := principal(r)
	a.idempotent(w, r, "secret-binding-action", params.IdempotencyKey, func() (string, error) {
		var b secrets.Binding
		var err error
		if in.Action == apigen.Rotate {
			b, err = a.secrets.RotateBinding(r.Context(), p, id, in.Reason, requestID(w))
		} else {
			b, err = a.secrets.Disable(r.Context(), p, id, in.Reason, requestID(w))
		}
		if err != nil {
			a.secretsError(w, r, err)
			return "", err
		}
		writeJSON(w, http.StatusOK, toBinding(b))
		return b.ID, nil
	}, func(string) {
		b, err := a.secrets.Binding(r.Context(), p, id)
		if err != nil {
			a.secretsError(w, r, err)
			return
		}
		a.writeBinding(w, r, b.ProjectID, id)
	})
}

func (a *API) DeleteLocalSecret(w http.ResponseWriter, r *http.Request, providerID apigen.Id, secretID apigen.Id, _ apigen.DeleteLocalSecretParams) {
	if err := a.secrets.DeleteSecret(r.Context(), principal(r), providerID, secretID, requestID(w)); err != nil {
		a.secretsError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
