package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/mail"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// ChannelChange is an edit of a channel. The kind is fixed. Secret replaces a Slack or Teams incoming webhook URL
// when it is set; the old value stays as an unused controller secret, and a webhook keeps its signing key.
type ChannelChange struct {
	Name                string
	URL                 string
	Recipients          []string
	Secret              string
	AllowPrivateNetwork bool
}

// UpdateChannel edits a channel with the checks of CreateChannel; audited without the secret.
func (s *Service) UpdateChannel(ctx context.Context, actor authz.Principal, id string, ch ChannelChange, requestID string) (Channel, error) {
	if !actor.IsOrgAdmin() {
		return Channel{}, ErrForbidden
	}
	c, err := s.channel(ctx, actor.OrgID, id)
	if err != nil {
		return Channel{}, err
	}
	if !name.MatchString(ch.Name) {
		return Channel{}, &Invalid{"names are lower-case letters, digits and dashes"}
	}
	var changed []string
	switch c.Kind {
	case "webhook":
		if !validURL(ch.URL, false) || ch.Secret != "" || len(ch.Recipients) > 0 {
			return Channel{}, &Invalid{"a webhook needs an https URL (http only on loopback); its signing key does not change"}
		}
		if ch.URL != c.URL {
			changed = append(changed, "url")
		}
		c.URL = ch.URL
	case "slack", "teams":
		if ch.URL != "" || len(ch.Recipients) > 0 || (ch.Secret != "" && !validURL(ch.Secret, false)) {
			return Channel{}, &Invalid{"give a new incoming webhook URL as the secret, or leave it empty to keep the current one"}
		}
	case "email":
		if len(ch.Recipients) == 0 || len(ch.Recipients) > 50 || ch.Secret != "" || ch.URL != "" {
			return Channel{}, &Invalid{"email needs 1 to 50 recipients and no secret"}
		}
		for _, r := range ch.Recipients {
			if a, err := mail.ParseAddress(r); err != nil || a.Name != "" || a.Address != r {
				return Channel{}, &Invalid{"recipients are plain addresses such as team@example.com"}
			}
		}
		if !slices.Equal(ch.Recipients, c.Recipients) {
			changed = append(changed, "recipients")
		}
		c.Recipients = ch.Recipients
	}
	if ch.Name != c.Name {
		changed = append(changed, "name")
	}
	if ch.AllowPrivateNetwork != c.AllowPrivateNetwork {
		changed = append(changed, "private network")
	}
	c.Name, c.AllowPrivateNetwork = ch.Name, ch.AllowPrivateNetwork
	if ch.Secret != "" {
		sid, err := s.secrets.CreateControllerSecret(ctx, actor, "notify-"+c.Name+"-"+strings.ToLower(ids6(ids.New())), c.Kind+" channel "+c.Name, []byte(ch.Secret), requestID)
		if err != nil {
			return Channel{}, err
		}
		c.SecretID = sid
		changed = append(changed, "webhook URL")
	}
	cfg, _ := json.Marshal(struct {
		URL        string   `json:"url,omitempty"`
		Recipients []string `json:"recipients,omitempty"`
	}{c.URL, c.Recipients})
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE notification_channels SET name = ?, config = ?, secret_id = ?, allow_private_network = ? WHERE id = ? AND org_id = ?`),
			c.Name, string(cfg), nullable(c.SecretID), c.AllowPrivateNetwork, id, actor.OrgID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "duplicate key") {
				return ErrConflict
			}
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-channel.update", ResourceType: "notification-channel", ResourceID: id, Outcome: "succeeded", Reason: "changed: " + strings.Join(changed, ", "), RequestID: requestID})
	})
	return c, err
}

func ids6(id string) string {
	if len(id) > 20 {
		return id[20:]
	}
	return id
}

// UpdateRule changes a rule's channel, events and scope with the checks of CreateRule.
func (s *Service) UpdateRule(ctx context.Context, actor authz.Principal, id string, r Rule, requestID string) (Rule, error) {
	if !actor.IsOrgAdmin() {
		return Rule{}, ErrForbidden
	}
	if err := s.checkRule(ctx, actor, r); err != nil {
		return Rule{}, err
	}
	r.ID = id
	events, _ := json.Marshal(r.Events)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE notification_rules SET channel_id = ?, events = ?, team_id = ?, project_id = ? WHERE id = ? AND org_id = ?`),
			r.ChannelID, string(events), nullable(r.TeamID), nullable(r.ProjectID), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-rule.update", ResourceType: "notification-rule", ResourceID: id, Outcome: "succeeded", Reason: strings.Join(r.Events, ", "), RequestID: requestID})
	})
	if err != nil {
		return Rule{}, err
	}
	err = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT created_at FROM notification_rules WHERE id = ?`), id).Scan(&r.CreatedAt)
	return r, err
}
