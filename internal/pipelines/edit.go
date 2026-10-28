package pipelines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// ErrActive refuses deleting a pipeline while one of its runs is queued or fetching.
var ErrActive = errors.New("the pipeline has a queued or fetching run; disable it and wait for the run to end")

// Changes are the settings Update may change; nil leaves a setting as it is. Token replaces the repository token and
// is never returned. The webhook URL and secret stay the same.
type Changes struct {
	Name, RepoURL, Username, Token *string
	Refs                           []string
	SpecRevision                   *int
}

// Update changes a pipeline's name, repository, refs, pinned spec revision or credentials in one audited change.
// A replaced token's old secret is no longer used and may be deleted from the secrets page.
func (s *Service) Update(ctx context.Context, actor authz.Principal, id string, c Changes, requestID string) (Pipeline, error) {
	if !actor.IsOrgAdmin() {
		return Pipeline{}, ErrForbidden
	}
	p, err := s.load(ctx, id)
	if err != nil || p.OrgID != actor.OrgID {
		return Pipeline{}, ErrNotFound
	}
	var changed []string
	if c.Name != nil && *c.Name != p.Name {
		if !pipelineName.MatchString(*c.Name) {
			return Pipeline{}, &Invalid{"names are lower-case letters, digits and dashes"}
		}
		changed, p.Name = append(changed, "name "+*c.Name), *c.Name
	}
	if c.RepoURL != nil && *c.RepoURL != p.RepoURL {
		if !validRepoURL(*c.RepoURL) {
			return Pipeline{}, &Invalid{"the repository URL must be https, without credentials, query or fragment (http only on loopback)"}
		}
		changed, p.RepoURL = append(changed, "repository "+*c.RepoURL), *c.RepoURL
	}
	if c.Refs != nil && !slices.Equal(c.Refs, p.Refs) {
		if len(c.Refs) == 0 || len(c.Refs) > 20 {
			return Pipeline{}, &Invalid{"give 1 to 20 ref patterns, such as refs/heads/main or refs/tags/v*"}
		}
		for _, r := range c.Refs {
			if !validPattern(r) {
				return Pipeline{}, &Invalid{"ref pattern " + r + " is not valid; use refs/heads/<branch> or refs/tags/<tag>, with * and ? as wildcards"}
			}
		}
		changed, p.Refs = append(changed, "refs "+strings.Join(c.Refs, " ")), c.Refs
	}
	if c.SpecRevision != nil && *c.SpecRevision != p.SpecRevision {
		if *c.SpecRevision < 0 {
			return Pipeline{}, &Invalid{"the spec revision must be 0 (latest) or a revision number"}
		}
		if *c.SpecRevision > 0 {
			var n int
			if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM spec_revisions WHERE spec_id = ? AND revision = ?`), p.SpecID, *c.SpecRevision).Scan(&n); err != nil {
				return Pipeline{}, err
			}
			if n == 0 {
				return Pipeline{}, &Invalid{"the spec has no such revision"}
			}
		}
		changed, p.SpecRevision = append(changed, fmt.Sprintf("spec revision %d", *c.SpecRevision)), *c.SpecRevision
	}
	if c.Username != nil && *c.Username != p.Username {
		if *c.Username != "" && !username.MatchString(*c.Username) {
			return Pipeline{}, &Invalid{"the username has characters that are not allowed"}
		}
		changed, p.Username = append(changed, "username"), *c.Username
	}
	if c.Token != nil && (*c.Token == "" || len(*c.Token) > 4096 || strings.ContainsAny(*c.Token, "\r\n")) {
		return Pipeline{}, &Invalid{"the token is not valid"}
	}
	if p.Username != "" && !p.HasToken && c.Token == nil {
		return Pipeline{}, &Invalid{"a username needs a token"}
	}
	if len(changed) == 0 && c.Token == nil {
		return p, nil
	}
	if c.Token != nil {
		secretID, err := s.secrets.CreateControllerSecret(ctx, actor, "pipeline-"+p.Name+"-token-"+strings.ToLower(ids.New()[20:]), "repository token of pipeline "+p.Name, []byte(*c.Token), requestID)
		if err != nil {
			return Pipeline{}, err
		}
		changed, p.tokenSecretID, p.HasToken = append(changed, "repository token replaced"), secretID, true
	}
	refs, _ := json.Marshal(p.Refs)
	p.UpdatedAt = s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pipelines SET name = ?, repo_url = ?, refs = ?, spec_revision = ?, username = ?, token_secret_id = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
			p.Name, p.RepoURL, string(refs), p.SpecRevision, nullable(p.Username), nullable(p.tokenSecretID), p.UpdatedAt, id)
		if err != nil {
			if msg := strings.ToLower(err.Error()); strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key") {
				return ErrConflict
			}
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pipeline.update", ResourceType: "pipeline", ResourceID: id, Outcome: "succeeded", Reason: strings.Join(changed, "; "), RequestID: requestID})
	})
	if err != nil {
		return Pipeline{}, err
	}
	return p, nil
}

// Delete takes a pipeline out of use: its webhook stops answering, it is no longer listed and its name is free again.
// The row, its runs and the audit trail are kept (ADR 0062). A queued or fetching run refuses the delete with
// ErrActive. Its webhook and token secrets are left unused, for an administrator to delete from the secrets page.
func (s *Service) Delete(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	p, err := s.load(ctx, id)
	if err != nil || p.OrgID != actor.OrgID {
		return ErrNotFound
	}
	now := s.now().UTC()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pipelines SET name = ?, enabled = ?, token_secret_id = NULL, hook_secret_id = '', deleted_by = ?, deleted_at = ?, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL AND NOT EXISTS (SELECT 1 FROM pipeline_runs WHERE pipeline_id = ? AND state IN (?, ?))`),
			p.Name+"~deleted~"+p.ID, false, actor.ID, now, now, id, id, Queued, Fetching)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var active int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM pipeline_runs WHERE pipeline_id = ? AND state IN (?, ?)`), id, Queued, Fetching).Scan(&active); err != nil {
				return err
			}
			if active > 0 {
				return ErrActive
			}
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pipeline.delete", ResourceType: "pipeline", ResourceID: id, Outcome: "succeeded", Reason: p.Name + " " + p.RepoURL, RequestID: requestID})
	})
}
