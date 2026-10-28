package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/version"
)

type controllerView struct {
	ID          string    `json:"id"`
	Hostname    string    `json:"hostname"`
	Host        string    `json:"host,omitempty"`
	Version     string    `json:"version"`
	StartedAt   time.Time `json:"startedAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
	Live        bool      `json:"live"`
	Leader      bool      `json:"leader"`
	Self        bool      `json:"self"`
	Compatible  bool      `json:"compatible"`
}

type workerVersionView struct {
	Version    string `json:"version"`
	Count      int    `json:"count"`
	Compatible bool   `json:"compatible"`
	Reason     string `json:"reason,omitempty"`
}

type upgradeEvent struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

func (a *API) GetUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !authz.Can(p, []authz.Role{authz.Administrator, authz.Operator}, authz.Resource{OrgID: p.OrgID}) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators and operators see upgrade state")
		return
	}
	ctx := r.Context()
	out := struct {
		Version       string                     `json:"version"`
		SchemaApplied int64                      `json:"schemaApplied"`
		SchemaKnown   int64                      `json:"schemaKnown"`
		Controllers   []controllerView           `json:"controllers"`
		Workers       []workerVersionView        `json:"workers"`
		History       []upgradeEvent             `json:"history"`
		Removals      []apigen.ControllerRemoval `json:"removals"`
	}{Version: version.Version, Controllers: []controllerView{}, Workers: []workerVersionView{}, History: []upgradeEvent{}, Removals: []apigen.ControllerRemoval{}}
	var err error
	if out.SchemaApplied, out.SchemaKnown, err = a.db.Schema(ctx); err != nil {
		a.internalError(w, r, err)
		return
	}
	if a.cluster != nil {
		list, err := a.cluster.Controllers(ctx)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, c := range list {
			out.Controllers = append(out.Controllers, controllerView{c.ID, c.Hostname, c.Host, c.Version, c.StartedAt, c.Heartbeat, c.Live, c.Leader, c.ID == a.cluster.ID,
				version.ControllersCompatible(version.Version, c.Version)})
		}
	}
	if a.removals != nil {
		list, err := a.removals.List(ctx)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, x := range list {
			rm := toControllerRemoval(x)
			rm.RequestedBy = a.username(r, rm.RequestedBy)
			out.Removals = append(out.Removals, rm)
		}
	}
	rows, err := a.db.SQL.QueryContext(ctx, a.db.Rebind(`SELECT version, COUNT(*) FROM workers WHERE org_id = ? AND enrollment_state = 'enrolled' GROUP BY version ORDER BY version`), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var v workerVersionView
		if err := rows.Scan(&v.Version, &v.Count); err != nil {
			a.internalError(w, r, err)
			return
		}
		v.Compatible, v.Reason = version.WorkerCompatible(version.Version, v.Version)
		out.Workers = append(out.Workers, v)
	}
	hist, err := a.db.SQL.QueryContext(ctx, `SELECT at, COALESCE(reason, '') FROM audit_events WHERE action = 'upgrade.migrate' ORDER BY at DESC LIMIT 20`)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	defer hist.Close()
	for hist.Next() {
		var e upgradeEvent
		if err := hist.Scan(&e.At, &e.Reason); err != nil {
			a.internalError(w, r, err)
			return
		}
		out.History = append(out.History, e)
	}
	writeJSON(w, http.StatusOK, out)
}

func toControllerRemoval(x cluster.Removal) apigen.ControllerRemoval {
	out := apigen.ControllerRemoval{Hostname: x.Hostname, State: apigen.ControllerRemovalState(x.State), Reason: x.Reason, RequestedBy: x.RequestedBy,
		RequestedAt: x.RequestedAt, UpdatedAt: x.UpdatedAt}
	if x.Host != "" {
		out.Host = &x.Host
	}
	if x.Outcome != "" {
		o := apigen.ControllerRemovalOutcome(x.Outcome)
		out.Outcome = &o
	}
	if x.Detail != "" {
		out.Detail = &x.Detail
	}
	return out
}

// RemoveController starts removing a controller host (ADR 0045); the refusals carry their reason.
func (a *API) RemoveController(w http.ResponseWriter, r *http.Request, hostname string, _ apigen.RemoveControllerParams) {
	if a.removals == nil {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var body apigen.RemoveControllerJSONRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	x, err := a.removals.Request(r.Context(), principal(r), hostname, body.Reason, requestID(w))
	var refused *cluster.Refused
	switch {
	case errors.Is(err, cluster.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators remove controllers")
	case errors.Is(err, cluster.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "no controller with that host name ran in the last day")
	case errors.As(err, &refused):
		apigen.WriteProblem(w, http.StatusConflict, "removal_refused", refused.Reason)
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusAccepted, toControllerRemoval(x))
	}
}

// username shows a user ID that the caller's organization knows as its username; anything else (a system actor, a
// deleted user) is returned unchanged.
func (a *API) username(r *http.Request, id string) string {
	var name string
	if a.db.SQL.QueryRowContext(r.Context(), a.db.Rebind(`SELECT username FROM users WHERE id = ? AND org_id = ?`), id, principal(r).OrgID).Scan(&name) == nil {
		return name
	}
	return id
}
