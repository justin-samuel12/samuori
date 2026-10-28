// Package notify routes events to people and systems: an in-app inbox, and administrator-configured channels
// (webhooks, Slack, Teams, email) with retries and a dead-letter queue (D21, docs/architecture/notifications.md).
// Events come from the audit log, which records every state change in the same transaction. Messages are built from
// structured fields only, never from free text such as failure details, so they carry no secrets or log excerpts
// (SEC-NOT-01).
package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/db"
)

// Types lists the events channels can subscribe to.
var Types = []string{
	"build.succeeded", "build.failed",
	"validation.passed", "validation.failed", "validation.blocked",
	"publication.published", "publication.failed", "publication.blocked",
	"approval.requested", "approval.decided",
	"exception.requested",
	"worker.revoked", "artifact.quarantined",
}

// Message is what a channel or inbox receives: a title, one line of text and a link.
type Message struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Title      string    `json:"title"`
	Text       string    `json:"text"`
	Link       string    `json:"link"`
	OccurredAt time.Time `json:"occurredAt"`
	Resource   struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"resource"`
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project,omitempty"`
	// App is the image definition (BuildSpec metadata.name) the event is about; a project can hold several.
	App string `json:"app,omitempty"`
	// Version is the build's <UTC date>.<n> (ADR 0054), once it succeeded.
	Version string `json:"version,omitempty"`
}

// event is a routed audit record: its message, project and team for rule scopes, and in-app recipients.
type event struct {
	msg        Message
	orgID      string
	projectID  string
	teamID     string
	recipients []string
}

var slug = regexp.MustCompile(`^[a-z][a-z_]{0,40}$`)

// label turns a machine value such as build_error into "build error"; anything else is dropped.
func label(v string) string {
	if !slug.MatchString(v) {
		return ""
	}
	return strings.ReplaceAll(v, "_", " ")
}

func (d *Dispatcher) project(ctx context.Context, tx *sql.Tx, id string) (name, team string) {
	_ = tx.QueryRowContext(ctx, d.db.Rebind(`SELECT name, team_id FROM projects WHERE id = ?`), id).Scan(&name, &team)
	return name, team
}

// classify maps an audit record to an event, or returns nil when nobody subscribes to that kind of record.
func (d *Dispatcher) classify(ctx context.Context, tx *sql.Tx, r audit.Record) (*event, error) {
	e := &event{orgID: r.OrgID}
	e.msg.ID, e.msg.OccurredAt = r.ID, r.At
	e.msg.Resource.Type, e.msg.Resource.ID = r.ResourceType, r.ResourceID
	scan := func(query string, args []any, dest ...any) error {
		err := tx.QueryRowContext(ctx, d.db.Rebind(query), args...).Scan(dest...)
		if errors.Is(err, sql.ErrNoRows) {
			return errSkip
		}
		return err
	}
	var requester string
	switch r.Action {
	case "build.finished":
		var class string
		var created time.Time
		if err := scan(`SELECT r.project_id, r.requested_by, COALESCE(r.failure_class, ''), r.created_at, COALESCE(s.name, ''), COALESCE(r.version, '') FROM build_runs r LEFT JOIN build_specs s ON s.id = r.spec_id WHERE r.id = ?`, []any{r.ResourceID}, &e.projectID, &requester, &class, &created, &e.msg.App, &e.msg.Version); err != nil {
			return nil, err
		}
		defer func() { e.msg.Text += d.buildFacts(ctx, tx, r.ResourceID, requester, created, r.At) }()
		e.msg.Link = "/builds/" + r.ResourceID
		e.recipients = []string{requester}
		if r.Outcome == "succeeded" {
			e.msg.Type, e.msg.Title = "build.succeeded", "Build succeeded"
		} else {
			e.msg.Type, e.msg.Title = "build.failed", "Build "+map[string]string{"timed_out": "timed out", "expired": "expired in the queue"}[r.Outcome]
			if e.msg.Title == "Build " {
				e.msg.Title = "Build failed"
			}
			if l := label(class); l != "" {
				e.msg.Text = "Failure class: " + l + "."
			}
		}
	case "validation.complete":
		var verdict, build sql.NullString
		if err := scan(`SELECT project_id, verdict, build_run_id FROM validation_runs WHERE id = ?`, []any{r.ResourceID}, &e.projectID, &verdict, &build); err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"passed", "failed", "blocked"}, verdict.String) {
			return nil, nil
		}
		e.msg.Type, e.msg.Title = "validation."+verdict.String, "Checks "+verdict.String
		if verdict.String != "passed" {
			if failed := d.failedChecks(ctx, tx, r.ResourceID); failed != "" {
				e.msg.Text = "Failed: " + failed + "."
			}
		}
		if build.Valid {
			e.msg.Link = "/builds/" + build.String
			_ = tx.QueryRowContext(ctx, d.db.Rebind(`SELECT r.requested_by, COALESCE(s.name, ''), COALESCE(r.version, '') FROM build_runs r LEFT JOIN build_specs s ON s.id = r.spec_id WHERE r.id = ?`), build.String).Scan(&requester, &e.msg.App, &e.msg.Version)
			e.recipients = []string{requester}
			if name := d.person(ctx, tx, requester); name != "" {
				e.msg.Text = strings.TrimSpace(e.msg.Text + " Requested by " + name + ".")
			}
		}
	case "publication.published", "publication.failed", "publication.blocked":
		var ref sql.NullString
		if err := scan(`SELECT project_id, requested_by, destination_ref FROM publications WHERE id = ?`, []any{r.ResourceID}, &e.projectID, &requester, &ref); err != nil {
			return nil, err
		}
		state := strings.TrimPrefix(r.Action, "publication.")
		e.msg.Type, e.msg.Title, e.msg.Link = r.Action, "Publication "+state, "/images"
		if state == "published" && ref.Valid {
			e.msg.Text = "Published as " + ref.String + "."
		}
		e.recipients = []string{requester}
	case "approval.request", "approval.decide":
		var state string
		if err := scan(`SELECT project_id, requested_by, state FROM approvals WHERE subject_id = ? ORDER BY created_at DESC LIMIT 1`, []any{r.ResourceID}, &e.projectID, &requester, &state); err != nil {
			return nil, err
		}
		e.msg.Link = "/approvals"
		if r.Action == "approval.request" {
			e.msg.Type, e.msg.Title = "approval.requested", "Approval requested"
			e.recipients = d.approvers(ctx, tx, r.OrgID)
		} else {
			e.msg.Type, e.msg.Title = "approval.decided", "Approval "+label(state)
			e.recipients = []string{requester}
		}
	case "exception.request":
		if err := scan(`SELECT project_id FROM output_exceptions WHERE id = ?`, []any{r.ResourceID}, &e.projectID); err != nil {
			return nil, err
		}
		e.msg.Type, e.msg.Title, e.msg.Link = "exception.requested", "Output exception requested", "/approvals"
		e.recipients = d.approvers(ctx, tx, r.OrgID)
	case "worker.revoke":
		e.msg.Type, e.msg.Title, e.msg.Link = "worker.revoked", "Worker revoked", "/admin/workers"
		e.recipients = d.approvers(ctx, tx, r.OrgID)
	case "artifact.quarantine":
		e.msg.Type, e.msg.Title, e.msg.Link = "artifact.quarantined", "Image quarantined", "/admin/quarantines"
		e.recipients = d.approvers(ctx, tx, r.OrgID)
	default:
		return nil, nil
	}
	if e.projectID != "" {
		name, team := d.project(ctx, tx, e.projectID)
		e.teamID = team
		e.msg.Project = &struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{e.projectID, name}
		head := "Project: " + name + "."
		if !checkID.MatchString(e.msg.App) {
			e.msg.App = ""
		}
		if e.msg.App != "" {
			head += " App: " + e.msg.App + "."
		}
		if !versionPattern.MatchString(e.msg.Version) {
			e.msg.Version = ""
		}
		if e.msg.Version != "" {
			head += " Version: " + e.msg.Version + "."
		}
		e.msg.Text = strings.TrimSpace(head + " " + e.msg.Text)
	}
	if d.baseURL != "" && strings.HasPrefix(e.msg.Link, "/") {
		e.msg.Link = d.baseURL + e.msg.Link
	}
	return e, nil
}

var errSkip = errors.New("the record no longer exists")

var (
	checkID        = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,60}$`)
	versionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.\d{1,6}$`)
)

// person is the display name (or username) of a user; empty for pipelines and service accounts.
func (d *Dispatcher) person(ctx context.Context, tx *sql.Tx, id string) string {
	var name string
	_ = tx.QueryRowContext(ctx, d.db.Rebind(`SELECT CASE WHEN display_name <> '' THEN display_name ELSE username END FROM users WHERE id = ?`), id).Scan(&name)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' || r == '*' || r == '_' || r == '`' {
			return -1
		}
		return r
	}, name)
}

// buildFacts adds who asked for a build, how long it took from the request and the image size: structured fields only.
func (d *Dispatcher) buildFacts(ctx context.Context, tx *sql.Tx, runID, requester string, created, finished time.Time) string {
	var out []string
	if name := d.person(ctx, tx, requester); name != "" {
		out = append(out, "Requested by "+name+".")
	}
	if took := finished.Sub(created); took > 0 {
		out = append(out, "Took "+duration(took)+".")
	}
	var size sql.NullInt64
	_ = tx.QueryRowContext(ctx, d.db.Rebind(`SELECT MAX(size) FROM build_artifacts WHERE run_id = ? AND kind = 'image'`), runID).Scan(&size)
	if size.Valid && size.Int64 > 0 {
		out = append(out, "Image "+byteSize(size.Int64)+".")
	}
	if len(out) == 0 {
		return ""
	}
	return " " + strings.Join(out, " ")
}

// failedChecks lists the IDs of failed, blocked or errored checks, required ones first.
func (d *Dispatcher) failedChecks(ctx context.Context, tx *sql.Tx, validationID string) string {
	rows, err := tx.QueryContext(ctx, d.db.Rebind(`SELECT check_id FROM check_results WHERE validation_run_id = ? AND outcome IN ('failed', 'blocked', 'errored') ORDER BY required DESC, check_id LIMIT 6`), validationID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && checkID.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ", ")
}

func duration(t time.Duration) string {
	t = t.Round(time.Second)
	switch {
	case t < time.Minute:
		return fmt.Sprintf("%d s", int(t.Seconds()))
	case t < time.Hour:
		return fmt.Sprintf("%d min %d s", int(t.Minutes()), int(t.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(t.Hours()), int(t.Minutes())%60)
}

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, u := float64(n)/unit, 0
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for v >= unit && u < len(units)-1 {
		v /= unit
		u++
	}
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, units[u])
	}
	return fmt.Sprintf("%.0f %s", v, units[u])
}

// approvers are the organization's administrators, who decide approvals, exceptions and worker events.
func (d *Dispatcher) approvers(ctx context.Context, tx *sql.Tx, orgID string) []string {
	rows, err := tx.QueryContext(ctx, d.db.Rebind(`SELECT DISTINCT principal_id FROM role_bindings WHERE org_id = ? AND role = 'administrator' AND scope_type = 'organization'`), orgID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// Dispatcher turns new audit records into inbox entries and channel deliveries, exactly once per record.
type Dispatcher struct {
	db      *db.DB
	baseURL string
	now     func() time.Time
}

func NewDispatcher(d *db.DB, baseURL string) *Dispatcher {
	return &Dispatcher{db: d, baseURL: strings.TrimRight(baseURL, "/"), now: time.Now}
}
