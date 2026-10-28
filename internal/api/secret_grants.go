package api

import (
	"net/http"
	"slices"
	"strconv"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/secrets"
)

func toSecretGrant(g secrets.Grant, withReference bool) apigen.SecretGrant {
	out := apigen.SecretGrant{Id: g.ID, ProjectId: g.ProjectID, Name: g.Name, ProviderName: g.ProviderName, ProviderType: apigen.SecretGrantProviderType(g.ProviderType),
		Reason: g.Reason, Status: apigen.SecretGrantStatus(g.Status), CreatedAt: g.CreatedAt}
	if withReference {
		ref := g.ProviderReference
		out.ProviderReference = &ref
	}
	if len(g.Fields) > 0 {
		fields := append([]string{}, g.Fields...)
		out.Fields = &fields
	}
	switch g.Usage {
	case "both":
		out.Usage = []apigen.SecretGrantUsage{apigen.SecretGrantUsageBuild, apigen.SecretGrantUsageTest}
	default:
		out.Usage = []apigen.SecretGrantUsage{apigen.SecretGrantUsage(g.Usage)}
	}
	return out
}

func toGrantUse(u secrets.GrantUse) apigen.SecretGrantUse {
	out := apigen.SecretGrantUse{LogicalId: u.LogicalID, BindingId: u.BindingID, Version: u.Version}
	if u.Field != "" {
		field := u.Field
		out.Field = &field
	}
	if u.GrantID != "" {
		id := u.GrantID
		out.GrantId = &id
	}
	if u.Encoding != "" {
		e := apigen.SecretGrantUseEncoding(u.Encoding)
		out.Encoding = &e
	}
	return out
}

// ListSecretGrants shows the project's granted keys to its members. Provider references stay with administrators.
func (a *API) ListSecretGrants(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	p := principal(r)
	grants, uses, err := a.secrets.ListGrants(r.Context(), p, projectID)
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	out := apigen.SecretGrantList{Items: []apigen.SecretGrant{}, Uses: []apigen.SecretGrantUse{}}
	for _, g := range grants {
		out.Items = append(out.Items, toSecretGrant(g, p.IsOrgAdmin()))
	}
	for _, u := range uses {
		out.Uses = append(out.Uses, toGrantUse(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateSecretGrant(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.CreateSecretGrantParams) {
	var in apigen.SecretGrantCreate
	if !decodeBody(w, r, &in) {
		return
	}
	req := secrets.GrantRequest{ProjectID: projectID, Reason: in.Reason}
	switch {
	case in.AllowedSecretId != nil && in.ProviderId == nil && in.ProviderReference == nil:
		req.AllowedSecretID = *in.AllowedSecretId
	case in.AllowedSecretId == nil && in.ProviderId != nil && in.ProviderReference != nil:
		req.ProviderID, req.ProviderReference = *in.ProviderId, *in.ProviderReference
	default:
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_secret", "give either allowedSecretId, or providerId and providerReference")
		return
	}
	if in.Name != nil {
		req.Name = *in.Name
	}
	build, test := slices.Contains(in.Usage, apigen.SecretGrantCreateUsageBuild), slices.Contains(in.Usage, apigen.SecretGrantCreateUsageTest)
	switch {
	case build && test:
		req.Usage = "both"
	case test:
		req.Usage = "test"
	default:
		req.Usage = "build"
	}
	if in.VersionPolicy != nil && *in.VersionPolicy == apigen.SecretGrantCreateVersionPolicyPinned {
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
	a.idempotent(w, r, "secret-grant", params.IdempotencyKey, func() (string, error) {
		g, err := a.secrets.CreateGrant(r.Context(), p, req, requestID(w))
		if err != nil {
			a.secretsError(w, r, err)
			return "", err
		}
		writeJSON(w, http.StatusCreated, toSecretGrant(g, true))
		return g.ID, nil
	}, func(id string) {
		grants, _, err := a.secrets.ListGrants(r.Context(), p, projectID)
		if err != nil {
			a.secretsError(w, r, err)
			return
		}
		for _, g := range grants {
			if g.ID == id {
				writeJSON(w, http.StatusOK, toSecretGrant(g, p.IsOrgAdmin()))
				return
			}
		}
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	})
}

func (a *API) RevokeSecretGrant(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, grantID apigen.Id, params apigen.RevokeSecretGrantParams) {
	var in apigen.SecretGrantRevoke
	if !decodeBody(w, r, &in) {
		return
	}
	p := principal(r)
	a.idempotent(w, r, "secret-grant-revoke", params.IdempotencyKey, func() (string, error) {
		if err := a.secrets.RevokeGrant(r.Context(), p, projectID, grantID, in.Reason, requestID(w)); err != nil {
			a.secretsError(w, r, err)
			return "", err
		}
		w.WriteHeader(http.StatusNoContent)
		return grantID, nil
	}, func(string) { w.WriteHeader(http.StatusNoContent) })
}

// BindSecretGrant lets a project's developers and maintainers bind a granted key; the service checks the grant
// belongs to this project.
func (a *API) BindSecretGrant(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.BindSecretGrantParams) {
	var in apigen.SecretGrantBind
	if !decodeBody(w, r, &in) {
		return
	}
	p := principal(r)
	a.idempotent(w, r, "secret-grant-binding", params.IdempotencyKey, func() (string, error) {
		field := ""
		if in.Field != nil {
			field = *in.Field
		}
		encoding := ""
		if in.Encoding != nil {
			encoding = string(*in.Encoding)
		}
		b, err := a.secrets.BindGrant(r.Context(), p, projectID, in.LogicalId, in.GrantId, field, encoding, requestID(w))
		if err != nil {
			a.secretsError(w, r, err)
			return "", err
		}
		writeJSON(w, http.StatusCreated, toGrantUse(secrets.GrantUse{LogicalID: b.LogicalID, GrantID: b.GrantID, Field: field, BindingID: b.ID, Version: b.Version, Encoding: b.Encoding}))
		return b.ID, nil
	}, func(id string) {
		_, uses, err := a.secrets.ListGrants(r.Context(), p, projectID)
		if err != nil {
			a.secretsError(w, r, err)
			return
		}
		for _, u := range uses {
			if u.BindingID == id {
				writeJSON(w, http.StatusOK, toGrantUse(u))
				return
			}
		}
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	})
}

func (a *API) RefreshSecretGrantKeys(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, grantID apigen.Id, _ apigen.RefreshSecretGrantKeysParams) {
	g, err := a.secrets.RefreshGrantKeys(r.Context(), principal(r), projectID, grantID, requestID(w))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSecretGrant(g, true))
}

func toAllowedSecret(x secrets.AllowedSecret) apigen.AllowedSecret {
	out := apigen.AllowedSecret{Id: x.ID, ProviderId: x.ProviderID, ProviderName: x.ProviderName, ProviderType: x.ProviderType, Reference: x.Reference, Label: x.Label, Fields: x.Fields, CreatedBy: x.CreatedBy, CreatedAt: x.CreatedAt}
	if x.Note != "" {
		note := x.Note
		out.Note = &note
	}
	return out
}

func (a *API) ListAllowedSecrets(w http.ResponseWriter, r *http.Request) {
	list, err := a.secrets.AllowedSecrets(r.Context(), principal(r))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	out := apigen.AllowedSecretList{Items: []apigen.AllowedSecret{}}
	for _, x := range list {
		out.Items = append(out.Items, toAllowedSecret(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) AllowSecret(w http.ResponseWriter, r *http.Request, _ apigen.AllowSecretParams) {
	var in apigen.AllowedSecretCreate
	if !decodeBody(w, r, &in) {
		return
	}
	label, note := "", ""
	if in.Label != nil {
		label = *in.Label
	}
	if in.Note != nil {
		note = *in.Note
	}
	x, err := a.secrets.AllowSecret(r.Context(), principal(r), in.ProviderId, in.Reference, label, note, requestID(w))
	if err != nil {
		a.secretsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAllowedSecret(x))
}

func (a *API) RemoveAllowedSecret(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RemoveAllowedSecretParams) {
	if err := a.secrets.RemoveAllowedSecret(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.secretsError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
