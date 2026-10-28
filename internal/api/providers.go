package api

import (
	"cmp"
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/providers"
)

type buildProviderView struct {
	providers.Definition
	Config  providers.Config `json:"config"`
	Workers providerWorkers  `json:"workers"`
}

type providerWorkers struct {
	Online              int            `json:"online"`
	WithApprovedVersion int            `json:"withApprovedVersion"`
	Versions            map[string]int `json:"versions"`
}

func (a *API) ListProviders(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !authz.Can(p, []authz.Role{authz.Administrator, authz.Operator}, authz.Resource{OrgID: p.OrgID}) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators and operators see providers")
		return
	}
	a.writeProviders(w, r, p.OrgID)
}

func (a *API) ConfigureProvider(w http.ResponseWriter, r *http.Request, name string, _ apigen.ConfigureProviderParams) {
	var in providers.Config
	if !decodeBody(w, r, &in) {
		return
	}
	if _, err := a.providers.Set(r.Context(), principal(r), name, in, requestID(w)); err != nil {
		a.providerError(w, r, err)
		return
	}
	a.writeProviders(w, r, principal(r).OrgID)
}

func (a *API) SetBuildMethodPolicy(w http.ResponseWriter, r *http.Request, _ apigen.SetBuildMethodPolicyParams) {
	var in providers.Policy
	if !decodeBody(w, r, &in) {
		return
	}
	if _, err := a.providers.SetPolicy(r.Context(), principal(r), in, requestID(w)); err != nil {
		a.providerError(w, r, err)
		return
	}
	a.writeProviders(w, r, principal(r).OrgID)
}

func (a *API) providerError(w http.ResponseWriter, r *http.Request, err error) {
	var bad *providers.Invalid
	switch {
	case errors.Is(err, providers.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, providers.ErrUnknown):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", err.Error())
	case errors.As(err, &bad):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", bad.Reason)
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) writeProviders(w http.ResponseWriter, r *http.Request, orgID string) {
	ctx := r.Context()
	list, err := a.workers.List(ctx, orgID, "", 1000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items  []buildProviderView `json:"items"`
		Policy providers.Policy    `json:"policy"`
	}{Items: []buildProviderView{}}
	for _, d := range providers.Catalog {
		c, err := a.providers.Config(ctx, orgID, d.Name)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		v := buildProviderView{Definition: d, Config: c, Workers: providerWorkers{Versions: map[string]int{}}}
		for _, wk := range list {
			ver, ok := wk.Capabilities.Providers[d.Name]
			if !ok || wk.RevokedAt != nil || a.channel == nil || !a.channel.Connected(wk.ID) {
				continue
			}
			v.Workers.Online++
			v.Workers.Versions[ver]++
			if want := cmp.Or(d.WorkerVersion, c.Version); want != "" && ver == want {
				v.Workers.WithApprovedVersion++
			}
		}
		out.Items = append(out.Items, v)
	}
	if out.Policy, err = a.providers.Policy(ctx, orgID); err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
