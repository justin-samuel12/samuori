package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/dbcredentials"
)

type databaseController struct {
	ID        string     `json:"id"`
	Hostname  string     `json:"hostname"`
	Status    string     `json:"status"`
	Detail    string     `json:"detail,omitempty"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

type databaseRevision struct {
	Number    int64     `json:"number"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	Reason    string    `json:"reason"`
}

type databaseStatus struct {
	Engine        string               `json:"engine"`
	Host          string               `json:"host,omitempty"`
	Port          string               `json:"port,omitempty"`
	Database      string               `json:"database,omitempty"`
	User          string               `json:"user,omitempty"`
	TLS           string               `json:"tls,omitempty"`
	ServerVersion string               `json:"serverVersion"`
	LatencyMs     int64                `json:"latencyMs"`
	Pool          map[string]int       `json:"pool"`
	Revision      *databaseRevision    `json:"revision,omitempty"`
	Controllers   []databaseController `json:"controllers"`
	CanUpdate     bool                 `json:"canUpdate"`
}

func (a *API) databaseProblem(w http.ResponseWriter, r *http.Request, err error) {
	var inv *dbcredentials.Invalid
	switch {
	case errors.Is(err, dbcredentials.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, dbcredentials.ErrNotPostgres):
		apigen.WriteProblem(w, http.StatusConflict, "not_postgres", err.Error())
	case errors.Is(err, dbcredentials.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", err.Error())
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) writeDatabaseStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.database.Status(r.Context(), principal(r))
	if err != nil {
		a.databaseProblem(w, r, err)
		return
	}
	out := databaseStatus{Engine: st.Target.Engine, Host: st.Target.Host, Port: st.Target.Port, Database: st.Target.Database, User: st.Target.User,
		TLS: st.Target.TLS, ServerVersion: st.ServerVersion, LatencyMs: st.Latency.Milliseconds(), CanUpdate: st.CanUpdate,
		Pool:        map[string]int{"open": st.Pool.OpenConnections, "inUse": st.Pool.InUse, "idle": st.Pool.Idle, "maxOpen": st.Pool.MaxOpenConnections},
		Controllers: []databaseController{}}
	if rv := st.Revision; rv != nil {
		out.Revision = &databaseRevision{Number: rv.Number, CreatedBy: rv.CreatedBy, CreatedAt: rv.CreatedAt, Reason: rv.Reason}
	}
	for _, c := range st.Controllers {
		out.Controllers = append(out.Controllers, databaseController{ID: c.ID, Hostname: c.Hostname, Status: c.Status, Detail: c.Detail, CheckedAt: c.CheckedAt})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetDatabaseStatus(w http.ResponseWriter, r *http.Request) {
	a.writeDatabaseStatus(w, r)
}

func (a *API) TestDatabaseConnection(w http.ResponseWriter, r *http.Request, _ apigen.TestDatabaseConnectionParams) {
	p, err := a.database.Test(r.Context(), principal(r))
	if err != nil {
		if errors.Is(err, dbcredentials.ErrForbidden) {
			a.databaseProblem(w, r, err)
			return
		}
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "connection_failed", err.Error())
		return
	}
	out := apigen.DatabaseTestResult{ServerVersion: p.ServerVersion, LatencyMs: p.Latency.Milliseconds()}
	if p.User != "" {
		out.User = &p.User
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) UpdateDatabasePassword(w http.ResponseWriter, r *http.Request, _ apigen.UpdateDatabasePasswordParams) {
	var body apigen.DatabasePasswordUpdate
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	password := ""
	if body.Password != nil {
		password = *body.Password
	}
	if err := a.database.Update(r.Context(), principal(r), password, body.Reason, requestID(w)); err != nil {
		a.databaseProblem(w, r, err)
		return
	}
	a.writeDatabaseStatus(w, r)
}
