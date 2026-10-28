package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/backup"
)

func toBackup(r backup.Record) apigen.Backup {
	out := apigen.Backup{Id: r.ID, CreatedAt: r.CreatedAt, File: r.File, Size: r.Size, Sha256: r.SHA256, Dialect: apigen.BackupDialect(r.Dialect),
		SchemaVersion: r.SchemaVersion, Tables: r.Tables, Rows: r.Rows, Blobs: r.Blobs, IncludesKeys: r.IncludesKeys, Trigger: apigen.BackupTrigger(r.Trigger),
		Removed: r.Removed, CheckedAt: r.CheckedAt}
	if r.CreatedBy != "" {
		out.CreatedBy = &r.CreatedBy
	}
	if r.CheckResult != "" {
		res := apigen.BackupCheckResult(r.CheckResult)
		out.CheckResult = &res
	}
	return out
}

func (a *API) ListBackups(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.IsOrgAdmin() {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", backup.ErrForbidden.Error())
		return
	}
	out := apigen.BackupList{Enabled: a.backups.Enabled(), Items: []apigen.Backup{}}
	if !out.Enabled {
		writeJSON(w, http.StatusOK, out)
		return
	}
	dir, keep, hours := a.backups.Dir(), a.backups.Keep(), int(a.backups.Interval.Hours())
	out.Directory, out.Keep, out.IntervalHours = &dir, &keep, &hours
	list, err := a.backups.List(r.Context(), p, 100)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	for _, b := range list {
		out.Items = append(out.Items, toBackup(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateBackup(w http.ResponseWriter, r *http.Request, _ apigen.CreateBackupParams) {
	p := principal(r)
	rec, err := a.backups.Run(r.Context(), &p, requestID(w))
	switch {
	case errors.Is(err, backup.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, backup.ErrDisabled):
		apigen.WriteProblem(w, http.StatusServiceUnavailable, "backups_disabled", err.Error())
	case errors.Is(err, backup.ErrRunning):
		apigen.WriteProblem(w, http.StatusConflict, "backup_running", err.Error())
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, toBackup(rec))
	}
}

func (a *API) CheckBackup(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.CheckBackupParams) {
	rec, err := a.backups.Check(r.Context(), principal(r), id, requestID(w))
	switch {
	case errors.Is(err, backup.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, backup.ErrNotFound), errors.Is(err, backup.ErrDisabled):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, toBackup(rec))
	}
}
