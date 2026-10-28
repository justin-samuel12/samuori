package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/topology"
)

// TopologyInfo is what the controller knows about its own configuration for the topology assessment.
type TopologyInfo struct {
	Storage     topology.Storage
	ExternalURL string
}

type topologyStatus struct {
	Plan            *topology.Plan       `json:"plan,omitempty"`
	Planned         *topology.Assessment `json:"planned,omitempty"`
	Live            topology.Assessment  `json:"live"`
	LiveControllers int                  `json:"liveControllers"`
	LiveWorkers     int                  `json:"liveWorkers"`
}

func (a *API) GetTopology(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !authz.Can(p, []authz.Role{authz.Administrator, authz.Operator}, authz.Resource{OrgID: p.OrgID}) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators and operators see the topology")
		return
	}
	a.writeTopology(w, r, p.OrgID, http.StatusOK)
}

func (a *API) SetTopologyPlan(w http.ResponseWriter, r *http.Request, _ apigen.SetTopologyPlanParams) {
	var in topology.Plan
	if !decodeBody(w, r, &in) {
		return
	}
	p := principal(r)
	if _, err := a.topology.Set(r.Context(), p, in, requestID(w)); err != nil {
		var bad *topology.Invalid
		switch {
		case errors.Is(err, topology.ErrForbidden):
			apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
		case errors.As(err, &bad):
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", bad.Reason)
		default:
			a.internalError(w, r, err)
		}
		return
	}
	a.writeTopology(w, r, p.OrgID, http.StatusOK)
}

func (a *API) writeTopology(w http.ResponseWriter, r *http.Request, orgID string, status int) {
	ctx := r.Context()
	facts := topology.Facts{Pools: map[string]int{}, Database: topology.PostgreSQL, Storage: a.topologyInfo.Storage}
	if a.db.Dialect == db.SQLite {
		facts.Database = topology.SQLite
	}
	var hosts []string
	if a.cluster != nil {
		list, err := a.cluster.Controllers(ctx)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, c := range list {
			if c.Live {
				facts.Controllers++
				hosts = append(hosts, c.Hostname, c.Host)
			}
		}
	} else {
		facts.Controllers = 1
	}
	pools, err := a.workers.ListPools(ctx, orgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	enabled := map[string]string{}
	for _, pl := range pools {
		if pl.ProfileApproved {
			enabled[pl.ID] = pl.Name
			facts.Pools[pl.Name] = 0
		}
	}
	list, err := a.workers.List(ctx, orgID, "", 1000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := topologyStatus{}
	for _, wk := range list {
		name, ok := enabled[wk.PoolID]
		if !ok || wk.RevokedAt != nil || wk.SelfTest.Status != "passed" || a.channel == nil || !a.channel.Connected(wk.ID) {
			continue
		}
		facts.Pools[name]++
		out.LiveWorkers++
	}
	portal := portalHost(a.topologyInfo.ExternalURL)
	facts.PortalOnController = portal == "" || isLoopback(portal)
	for _, h := range hosts {
		if h != "" && strings.EqualFold(h, portal) {
			facts.PortalOnController = true
		}
	}
	plan, err := a.topology.Get(ctx, orgID)
	switch {
	case err == nil:
		facts.DatabaseReplicated, facts.StorageReplicated = plan.DatabaseReplicated, plan.StorageReplicated
		planned := topology.Assess(plan.Facts(a.topologyInfo.Storage, facts.PortalOnController))
		out.Plan, out.Planned = &plan, &planned
	case !errors.Is(err, topology.ErrNoPlan):
		a.internalError(w, r, err)
		return
	}
	out.Live = topology.Assess(facts)
	out.LiveControllers = facts.Controllers
	writeJSON(w, status, out)
}

func portalHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func isLoopback(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }
