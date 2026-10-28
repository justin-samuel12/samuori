package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/secrets"
)

// InUse refuses deleting a registry or credentials that other records still refer to.
type InUse struct{ Uses []string }

func (e *InUse) Error() string { return "in use: " + strings.Join(e.Uses, "; ") }

// UpdateRegistry changes a registry; the kind is fixed. Where a stored secret is sent (endpoint, plain HTTP, ECR
// account and region) cannot change while any of the registry's profiles holds one, so an edit never redirects a
// credential (docs/architecture/registry-auth.md#editing-and-deleting).
func (s *Service) UpdateRegistry(ctx context.Context, actor authz.Principal, id string, in Registry, requestID string) (Registry, error) {
	if !actor.IsOrgAdmin() {
		return Registry{}, ErrForbidden
	}
	cur, err := s.registry(ctx, actor.OrgID, id)
	if err != nil {
		return Registry{}, err
	}
	in.ID, in.Kind, in.CreatedAt = cur.ID, cur.Kind, cur.CreatedAt
	if in.Kind == "ecr" {
		in.Endpoint = ""
		if in.AccountID == cur.AccountID && in.Region == cur.Region {
			in.Endpoint = cur.Endpoint
		}
	}
	if err := validRegistry(&in); err != nil {
		return Registry{}, err
	}
	var changes []string
	note := func(changed bool, what string) {
		if changed {
			changes = append(changes, what)
		}
	}
	note(in.Name != cur.Name, "name "+cur.Name+" -> "+in.Name)
	note(in.Endpoint != cur.Endpoint, "endpoint "+cur.Endpoint+" -> "+in.Endpoint)
	note(in.PlainHTTP != cur.PlainHTTP, fmt.Sprintf("plain HTTP %t", in.PlainHTTP))
	note(in.AllowPrivate != cur.AllowPrivate, fmt.Sprintf("private network %t", in.AllowPrivate))
	redirects := in.Endpoint != cur.Endpoint || in.PlainHTTP != cur.PlainHTTP || in.AccountID != cur.AccountID || in.Region != cur.Region
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if redirects {
			held, err := names(ctx, s, tx, `SELECT name FROM registry_profiles WHERE registry_id = ? AND org_id = ? AND secret_id IS NOT NULL ORDER BY name`, id, actor.OrgID)
			if err != nil {
				return err
			}
			if len(held) > 0 {
				return &Invalid{"the connection cannot change while credentials " + strings.Join(held, ", ") + " hold a stored secret for this registry; delete those credentials first and add them again afterwards"}
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE registries SET name = ?, endpoint = ?, plain_http = ?, allow_private_network = ?, account_id = ?, region = ? WHERE id = ? AND org_id = ?`),
			in.Name, in.Endpoint, in.PlainHTTP, in.AllowPrivate, in.AccountID, in.Region, id, actor.OrgID); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "registry.update", ResourceType: "registry", ResourceID: id, Outcome: "succeeded",
			Reason: in.Kind + " " + in.Name + "; " + strings.Join(append(changes, "saved"), ", "), RequestID: requestID})
	})
	if err != nil {
		return Registry{}, err
	}
	return in, nil
}

// DeleteRegistry removes a registry nothing refers to any more; the check and the delete share one transaction.
func (s *Service) DeleteRegistry(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	cur, err := s.registry(ctx, actor.OrgID, id)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var uses []string
		for _, ref := range []struct{ query, format string }{
			{`SELECT name FROM registry_profiles WHERE registry_id = ? AND org_id = ? ORDER BY name`, "credentials %s"},
			{`SELECT name FROM destinations WHERE registry_id = ? AND org_id = ? ORDER BY name`, "destination %s"},
			{`SELECT i.name FROM image_sources i JOIN registry_profiles p ON p.id = i.pull_profile_id WHERE p.registry_id = ? AND i.org_id = ? ORDER BY i.name`, "image source %s"},
		} {
			found, err := names(ctx, s, tx, ref.query, id, actor.OrgID)
			if err != nil {
				return err
			}
			for _, n := range found {
				uses = append(uses, fmt.Sprintf(ref.format, n))
			}
		}
		for _, ref := range []struct{ query, format string }{
			{`SELECT COUNT(*) FROM repository_requests WHERE registry_id = ? AND org_id = ?`, "%d repository request(s)"},
			{`SELECT COUNT(*) FROM publications p JOIN destinations d ON d.id = p.destination_id WHERE d.registry_id = ? AND p.org_id = ?`, "%d publication(s)"},
		} {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(ref.query), id, actor.OrgID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				uses = append(uses, fmt.Sprintf(ref.format, n))
			}
		}
		if len(uses) > 0 {
			return &InUse{Uses: uses}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM registries WHERE id = ? AND org_id = ?`), id, actor.OrgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "registry.delete", ResourceType: "registry", ResourceID: id, Outcome: "succeeded",
			Reason: cur.Kind + " " + cur.Name + " (" + cur.Endpoint + ")", RequestID: requestID})
	})
}

// UpdateProfile changes credentials. The registry and purpose are fixed, so a stored secret stays with its registry;
// an ECR profile keeping its stored access key cannot change its API endpoint, where signed requests go.
func (s *Service) UpdateProfile(ctx context.Context, actor authz.Principal, id string, in Profile, requestID string) (Profile, error) {
	if !actor.IsOrgAdmin() {
		return Profile{}, ErrForbidden
	}
	cur, err := s.profile(ctx, actor.OrgID, id)
	if err != nil {
		return Profile{}, err
	}
	reg, err := s.registry(ctx, actor.OrgID, cur.RegistryID)
	if err != nil {
		return Profile{}, err
	}
	in.ID, in.RegistryID, in.Purpose, in.Status = cur.ID, cur.RegistryID, cur.Purpose, cur.Status
	if err := validProfile(&in, reg); err != nil {
		return Profile{}, err
	}
	if cur.SecretID != "" && in.SecretID == cur.SecretID && in.Type == "ecr" && in.ECR.APIEndpoint != cur.ECR.APIEndpoint {
		return Profile{}, &Invalid{"the API endpoint cannot change while the profile keeps its stored access key; choose the secret again under a new profile, or delete this one first"}
	}
	dests, err := s.destinations(ctx, actor.OrgID)
	if err != nil {
		return Profile{}, err
	}
	for _, d := range dests {
		if d.PushProfileID == id && len(in.Scopes) > 0 && !slices.ContainsFunc(in.Scopes, func(sc string) bool { ok, _ := path.Match(sc, d.Repository); return ok }) {
			return Profile{}, &Invalid{"the repository scopes must still include " + d.Repository + ", which destination " + d.Name + " publishes to"}
		}
	}
	var changes []string
	note := func(changed bool, what string) {
		if changed {
			changes = append(changes, what)
		}
	}
	note(in.Name != cur.Name, "name "+cur.Name+" -> "+in.Name)
	note(in.Type != cur.Type, "type "+cur.Type+" -> "+in.Type)
	note(in.Username != cur.Username, "username")
	note(in.SecretID != cur.SecretID, "secret")
	note(!slices.Equal(in.Scopes, cur.Scopes), "scopes "+strings.Join(in.Scopes, ","))
	note(in.ECR.AuthMethod != cur.ECR.AuthMethod, "AWS identity "+in.ECR.AuthMethod)
	note(in.ECR.RoleARN != cur.ECR.RoleARN, "role "+in.ECR.RoleARN)
	note(in.ECR.ExternalID != cur.ECR.ExternalID, "external ID")
	note(in.ECR.APIEndpoint != cur.ECR.APIEndpoint, "API endpoint "+in.ECR.APIEndpoint)
	scopes, _ := json.Marshal(in.Scopes)
	cfg := "{}"
	if in.Type == "ecr" {
		raw, _ := json.Marshal(in.ECR)
		cfg = string(raw)
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var secretRef any
		if in.SecretID != "" {
			if in.SecretID != cur.SecretID {
				if err := s.secrets.MarkControllerOnly(ctx, tx, actor.OrgID, in.SecretID); err != nil {
					return err
				}
			}
			secretRef = in.SecretID
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE registry_profiles SET name = ?, type = ?, username = ?, secret_id = ?, repository_scopes = ?, config = ?, last_test = NULL WHERE id = ? AND org_id = ?`),
			in.Name, in.Type, in.Username, secretRef, string(scopes), cfg, id, actor.OrgID); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "registry-profile.update", ResourceType: "registry-profile", ResourceID: id, Outcome: "succeeded",
			Reason: in.Type + " " + in.Purpose + " " + in.Name + "; " + strings.Join(append(changes, "saved"), ", "), RequestID: requestID})
	})
	if err != nil {
		var inv *secrets.Invalid
		if errors.As(err, &inv) {
			return Profile{}, &Invalid{inv.Reason}
		}
		return Profile{}, err
	}
	return s.profile(ctx, actor.OrgID, id)
}

// DeleteProfile removes credentials no destination or image source uses. Their secret stays reserved for the
// controller, so it never becomes available to builds.
func (s *Service) DeleteProfile(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	cur, err := s.profile(ctx, actor.OrgID, id)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var uses []string
		for _, ref := range []struct{ query, format string }{
			{`SELECT name FROM destinations WHERE push_profile_id = ? AND org_id = ? ORDER BY name`, "destination %s"},
			{`SELECT name FROM image_sources WHERE pull_profile_id = ? AND org_id = ? ORDER BY name`, "image source %s"},
		} {
			found, err := names(ctx, s, tx, ref.query, id, actor.OrgID)
			if err != nil {
				return err
			}
			for _, n := range found {
				uses = append(uses, fmt.Sprintf(ref.format, n))
			}
		}
		if len(uses) > 0 {
			return &InUse{Uses: uses}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM registry_profiles WHERE id = ? AND org_id = ?`), id, actor.OrgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "registry-profile.delete", ResourceType: "registry-profile", ResourceID: id, Outcome: "succeeded",
			Reason: cur.Type + " " + cur.Purpose + " " + cur.Name, RequestID: requestID})
	})
}

func names(ctx context.Context, s *Service, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, s.db.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeleteDestination removes a destination nothing published to or asked for; its publications are history and keep it.
// BuildSpecs that name it by destinationRef are refused at admission afterwards.
func (s *Service) DeleteDestination(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	cur, err := s.destination(ctx, actor.OrgID, id)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var uses []string
		for _, ref := range []struct{ query, format string }{
			{`SELECT COUNT(*) FROM publications WHERE destination_id = ? AND org_id = ?`, "%d publication(s)"},
			{`SELECT COUNT(*) FROM repository_requests WHERE destination_id = ? AND org_id = ?`, "%d repository request(s)"},
		} {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(ref.query), id, actor.OrgID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				uses = append(uses, fmt.Sprintf(ref.format, n))
			}
		}
		if len(uses) > 0 {
			return &InUse{Uses: uses}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM destinations WHERE id = ? AND org_id = ?`), id, actor.OrgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "destination.delete", ResourceType: "destination", ResourceID: id, Outcome: "succeeded",
			Reason: cur.Name + " (" + cur.Repository + ")", RequestID: requestID})
	})
}

// UpdateDestination changes a destination, validated like a new one. The registry is fixed, and the repository is
// fixed once publications or repository requests refer to the destination, so its history keeps its meaning.
func (s *Service) UpdateDestination(ctx context.Context, actor authz.Principal, id string, in Destination, requestID string) (Destination, error) {
	if !actor.IsOrgAdmin() {
		return Destination{}, ErrForbidden
	}
	cur, err := s.destination(ctx, actor.OrgID, id)
	if err != nil {
		return Destination{}, err
	}
	if in.RegistryID != "" && in.RegistryID != cur.RegistryID {
		return Destination{}, &Invalid{"the registry of a destination cannot change; create a new destination instead"}
	}
	in.ID, in.RegistryID = cur.ID, cur.RegistryID
	if err := s.validDestination(ctx, actor.OrgID, &in); err != nil {
		return Destination{}, err
	}
	var changes []string
	note := func(changed bool, what string) {
		if changed {
			changes = append(changes, what)
		}
	}
	note(in.Name != cur.Name, "name "+cur.Name+" -> "+in.Name)
	note(in.Repository != cur.Repository, "repository "+cur.Repository+" -> "+in.Repository)
	note(in.PushProfileID != cur.PushProfileID, "push credentials "+in.PushProfileID)
	note(!slices.Equal(in.AllowedProjects, cur.AllowedProjects), "allowed projects "+strings.Join(in.AllowedProjects, ","))
	note(!slices.Equal(in.ProtectedTags, cur.ProtectedTags), "protected tags "+strings.Join(in.ProtectedTags, ","))
	note(in.ImmutableTags != cur.ImmutableTags, fmt.Sprintf("immutable tags %t", in.ImmutableTags))
	note(in.RequiresApproval != cur.RequiresApproval, fmt.Sprintf("requires approval %t", in.RequiresApproval))
	note(in.AcceptsRestricted != cur.AcceptsRestricted, fmt.Sprintf("accepts restricted %t", in.AcceptsRestricted))
	note(in.SignImages != cur.SignImages, fmt.Sprintf("sign images %t", in.SignImages))
	note(in.AttachProvenance != cur.AttachProvenance, fmt.Sprintf("attach provenance %t", in.AttachProvenance))
	note(in.PromotionOnly != cur.PromotionOnly, fmt.Sprintf("promotion only %t", in.PromotionOnly))
	projects, _ := json.Marshal(in.AllowedProjects)
	tags, _ := json.Marshal(in.ProtectedTags)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if in.Repository != cur.Repository {
			var uses []string
			for _, ref := range []struct{ query, format string }{
				{`SELECT COUNT(*) FROM publications WHERE destination_id = ? AND org_id = ?`, "%d publication(s)"},
				{`SELECT COUNT(*) FROM repository_requests WHERE destination_id = ? AND org_id = ?`, "%d repository request(s)"},
			} {
				var n int
				if err := tx.QueryRowContext(ctx, s.db.Rebind(ref.query), id, actor.OrgID).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					uses = append(uses, fmt.Sprintf(ref.format, n))
				}
			}
			if len(uses) > 0 {
				return &Invalid{"the repository cannot change because " + strings.Join(uses, " and ") + " refer to this destination; create a new destination for the new repository"}
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE destinations SET name = ?, repository = ?, push_profile_id = ?, accepts_restricted = ?, allowed_project_ids = ?, protected_tags = ?, immutable_tags = ?, requires_approval = ?, sign_images = ?, attach_provenance = ?, promotion_only = ? WHERE id = ? AND org_id = ?`),
			in.Name, in.Repository, in.PushProfileID, in.AcceptsRestricted, string(projects), string(tags), in.ImmutableTags, in.RequiresApproval, in.SignImages, in.AttachProvenance, in.PromotionOnly, id, actor.OrgID); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "destination.update", ResourceType: "destination", ResourceID: id, Outcome: "succeeded",
			Reason: in.Name + " (" + in.Repository + "); " + strings.Join(append(changes, "saved"), ", "), RequestID: requestID})
	})
	if err != nil {
		return Destination{}, err
	}
	return in, nil
}
