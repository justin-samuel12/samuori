package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/observability"
)

// TelemetryStatus says which optional observability outputs this controller has enabled.
type TelemetryStatus struct {
	MetricsEnabled, TracingEnabled bool
}

const maxAuditExport = 100_000

func auditReader(p authz.Principal) bool {
	return authz.Can(p, []authz.Role{authz.Administrator, authz.SecurityReviewer}, authz.Resource{OrgID: p.OrgID})
}

type auditEventView struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Actor struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	} `json:"actor"`
	Action       string `json:"action"`
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceId,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	RequestID    string `json:"requestId,omitempty"`
}

func toAuditEvent(r audit.Record) auditEventView {
	v := auditEventView{ID: r.ID, At: r.At, Action: r.Action, ResourceType: r.ResourceType, ResourceID: r.ResourceID, Outcome: r.Outcome, Reason: r.Reason, RequestID: r.RequestID}
	v.Actor.Type, v.Actor.ID = r.ActorType, r.ActorID
	return v
}

func auditFilter(orgID string, resourceType, resourceID, actor, action, outcome, request, text *string, since, until *time.Time) audit.Filter {
	val := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return audit.Filter{OrgID: orgID, ResourceType: val(resourceType), ResourceID: val(resourceID), ActorID: val(actor), Action: val(action),
		Outcome: val(outcome), RequestID: val(request), Text: val(text), Since: since, Until: until}
}

func (a *API) ListAuditEvents(w http.ResponseWriter, r *http.Request, params apigen.ListAuditEventsParams) {
	p := principal(r)
	if !auditReader(p) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators and security reviewers can read the audit log")
		return
	}
	f := auditFilter(p.OrgID, params.ResourceType, params.ResourceId, params.ActorId, params.Action, params.Outcome, params.RequestId, params.Q, params.Since, params.Until)
	f.Limit, f.Cursor = pageParams(params.Limit, params.Cursor)
	list, next, err := audit.Search(r.Context(), a.db, f)
	if errors.Is(err, audit.ErrBadCursor) {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_cursor", "the cursor is not valid")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items      []auditEventView `json:"items"`
		NextCursor *string          `json:"nextCursor"`
	}{Items: []auditEventView{}}
	for _, e := range list {
		out.Items = append(out.Items, toAuditEvent(e))
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ExportAuditEvents(w http.ResponseWriter, r *http.Request, params apigen.ExportAuditEventsParams) {
	p := principal(r)
	if !auditReader(p) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators and security reviewers can export the audit log")
		return
	}
	f := auditFilter(p.OrgID, params.ResourceType, params.ResourceId, params.ActorId, params.Action, params.Outcome, params.RequestId, params.Q, params.Since, params.Until)
	var rows []audit.Record
	truncated, err := audit.Export(r.Context(), a.db, f, maxAuditExport, func(rec audit.Record) error {
		rows = append(rows, rec)
		return nil
	})
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	reason := fmt.Sprintf("%d events", len(rows))
	if truncated {
		reason += ", truncated"
	}
	if err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		return audit.Append(r.Context(), a.db, tx, audit.Event{Actor: p, Action: "audit.export", ResourceType: "audit-events", ResourceID: p.OrgID, Outcome: "succeeded", Reason: reason, RequestID: requestID(w)})
	}); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="samuori-audit-`+time.Now().UTC().Format("20060102T150405Z")+`.jsonl"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Samuori-Truncated", fmt.Sprint(truncated))
	enc := json.NewEncoder(w)
	for _, rec := range rows {
		_ = enc.Encode(toAuditEvent(rec))
	}
}

func (a *API) GetAuditPolicy(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !auditReader(p) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators and security reviewers can read the audit policy")
		return
	}
	pol, err := a.retention.Policy(r.Context(), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (a *API) SetAuditPolicy(w http.ResponseWriter, r *http.Request, _ apigen.SetAuditPolicyParams) {
	var in apigen.AuditPolicyUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	pol, err := a.retention.SetPolicy(r.Context(), principal(r), audit.Policy{RetentionDays: in.RetentionDays, ExportEnabled: in.ExportEnabled, RequireExport: in.RequireExport}, requestID(w))
	var inv *audit.Invalid
	switch {
	case errors.Is(err, audit.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators can change the audit policy")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid_policy", inv.Reason)
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, pol)
	}
}

func (a *API) GetDiagnostics(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !authz.Can(p, []authz.Role{authz.Administrator, authz.Operator}, authz.Resource{OrgID: p.OrgID}) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators and operators can read diagnostics")
		return
	}
	snap, err := a.diagnostics.Take(r.Context(), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type telemetryView struct {
		MetricsEnabled       bool `json:"metricsEnabled"`
		TracingEnabled       bool `json:"tracingEnabled"`
		AuditExportDirectory bool `json:"auditExportDirectory"`
	}
	writeJSON(w, http.StatusOK, struct {
		observability.Snapshot
		Telemetry telemetryView `json:"telemetry"`
	}{snap, telemetryView{a.telemetry.MetricsEnabled, a.telemetry.TracingEnabled, a.retention != nil && a.retention.Dir != ""}})
}
