package notify

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("already exists")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

// Channel is an administrator-configured destination. Its secret (a webhook signing key, or a Slack or Teams URL,
// which carries its own credential) is a controller-only secret.
type Channel struct {
	ID, OrgID, Name, Kind string
	// URL is the webhook endpoint; Recipients are email addresses. Slack and Teams URLs are secrets, never stored here.
	URL                 string   `json:"url,omitempty"`
	Recipients          []string `json:"recipients,omitempty"`
	SecretID            string
	AllowPrivateNetwork bool
	Enabled             bool
	CreatedAt           time.Time
}

// Sender delivers one message over one kind of channel.
type Sender interface {
	Send(ctx context.Context, c Channel, secret []byte, m Message) error
}

// Secrets keeps channel credentials controller-only.
type Secrets interface {
	CreateControllerSecret(ctx context.Context, actor authz.Principal, name, description string, value []byte, requestID string) (string, error)
	ControllerValue(ctx context.Context, orgID, secretID string) ([]byte, error)
}

// Service manages channels, rules, deliveries and inboxes.
type Service struct {
	db      *db.DB
	secrets Secrets
	senders map[string]Sender
	// EmailReady reports whether the controller has an SMTP server; email channels need one.
	EmailReady bool
	now        func() time.Time
}

func NewService(d *db.DB, sec Secrets) *Service {
	return &Service{db: d, secrets: sec, senders: map[string]Sender{}, now: time.Now}
}

// UseSender registers the sender for a channel kind.
func (s *Service) UseSender(kind string, snd Sender) { s.senders[kind] = snd }

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validURL(raw string, allowHTTP bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	return u.Scheme == "https" || (u.Scheme == "http" && (local || allowHTTP))
}

// CreateChannel adds a channel. For webhooks Samuori generates the signing key and returns it once; for Slack and Teams
// secret is the incoming webhook URL. Creation is audited without the secret.
func (s *Service) CreateChannel(ctx context.Context, actor authz.Principal, c Channel, secret, requestID string) (Channel, string, error) {
	if !actor.IsOrgAdmin() {
		return Channel{}, "", ErrForbidden
	}
	if !name.MatchString(c.Name) {
		return Channel{}, "", &Invalid{"names are lower-case letters, digits and dashes"}
	}
	issued := ""
	switch c.Kind {
	case "webhook":
		if !validURL(c.URL, false) || secret != "" {
			return Channel{}, "", &Invalid{"a webhook needs an https URL (http only on loopback); Samuori generates its signing key"}
		}
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		issued, secret = hex.EncodeToString(b), hex.EncodeToString(b)
		c.Recipients = nil
	case "slack", "teams":
		if !validURL(secret, false) || c.URL != "" {
			return Channel{}, "", &Invalid{"give the incoming webhook URL as the secret; it is stored for the controller only"}
		}
		c.Recipients = nil
	case "email":
		if !s.EmailReady {
			return Channel{}, "", &Invalid{"email needs an SMTP server in the controller configuration (smtp.host)"}
		}
		if len(c.Recipients) == 0 || len(c.Recipients) > 50 || secret != "" || c.URL != "" {
			return Channel{}, "", &Invalid{"email needs 1 to 50 recipients and no secret"}
		}
		for _, r := range c.Recipients {
			if a, err := mail.ParseAddress(r); err != nil || a.Name != "" || a.Address != r {
				return Channel{}, "", &Invalid{"recipients are plain addresses such as team@example.com"}
			}
		}
	default:
		return Channel{}, "", &Invalid{"the kind must be webhook, slack, teams or email"}
	}
	c.ID, c.OrgID, c.Enabled, c.CreatedAt = ids.New(), actor.OrgID, true, s.now().UTC()
	if secret != "" {
		id, err := s.secrets.CreateControllerSecret(ctx, actor, "notify-"+c.Name+"-"+strings.ToLower(c.ID[20:]), c.Kind+" channel "+c.Name, []byte(secret), requestID)
		if err != nil {
			return Channel{}, "", err
		}
		c.SecretID = id
	}
	cfg, _ := json.Marshal(struct {
		URL        string   `json:"url,omitempty"`
		Recipients []string `json:"recipients,omitempty"`
	}{c.URL, c.Recipients})
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO notification_channels (id, org_id, name, kind, config, secret_id, allow_private_network, enabled, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			c.ID, c.OrgID, c.Name, c.Kind, string(cfg), nullable(c.SecretID), c.AllowPrivateNetwork, true, actor.ID, c.CreatedAt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "duplicate key") {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-channel.create", ResourceType: "notification-channel", ResourceID: c.ID, Outcome: "succeeded", Reason: c.Kind + " " + c.Name, RequestID: requestID})
	})
	return c, issued, err
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

const channelCols = `id, org_id, name, kind, config, COALESCE(secret_id, ''), allow_private_network, enabled, created_at`

func scanChannel(sc interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	var cfg string
	if err := sc.Scan(&c.ID, &c.OrgID, &c.Name, &c.Kind, &cfg, &c.SecretID, &c.AllowPrivateNetwork, &c.Enabled, &c.CreatedAt); err != nil {
		return Channel{}, err
	}
	_ = json.Unmarshal([]byte(cfg), &c)
	return c, nil
}

func (s *Service) channel(ctx context.Context, orgID, id string) (Channel, error) {
	c, err := scanChannel(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+channelCols+` FROM notification_channels WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	return c, err
}

// Channels lists the organization's channels.
func (s *Service) Channels(ctx context.Context, actor authz.Principal) ([]Channel, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+channelCols+` FROM notification_channels WHERE org_id = ? ORDER BY name`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetEnabled turns a channel on or off; disabled channels receive no new deliveries.
func (s *Service) SetEnabled(ctx context.Context, actor authz.Principal, id string, enabled bool, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE notification_channels SET enabled = ? WHERE id = ? AND org_id = ?`), enabled, id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-channel." + map[bool]string{true: "enable", false: "disable"}[enabled], ResourceType: "notification-channel", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// Rule routes event types, optionally for one team or project, to a channel.
type Rule struct {
	ID, ChannelID     string
	Events            []string
	TeamID, ProjectID string
	CreatedAt         time.Time
}

// CreateRule adds a routing rule.
func (s *Service) CreateRule(ctx context.Context, actor authz.Principal, r Rule, requestID string) (Rule, error) {
	if !actor.IsOrgAdmin() {
		return Rule{}, ErrForbidden
	}
	if err := s.checkRule(ctx, actor, r); err != nil {
		return Rule{}, err
	}
	r.ID, r.CreatedAt = ids.New(), s.now().UTC()
	events, _ := json.Marshal(r.Events)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO notification_rules (id, org_id, channel_id, events, team_id, project_id, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, actor.OrgID, r.ChannelID, string(events), nullable(r.TeamID), nullable(r.ProjectID), actor.ID, r.CreatedAt); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-rule.create", ResourceType: "notification-channel", ResourceID: r.ChannelID, Outcome: "succeeded", Reason: strings.Join(r.Events, ", "), RequestID: requestID})
	})
	return r, err
}

func (s *Service) checkRule(ctx context.Context, actor authz.Principal, r Rule) error {
	if _, err := s.channel(ctx, actor.OrgID, r.ChannelID); err != nil {
		return &Invalid{"the channel does not exist"}
	}
	if len(r.Events) == 0 {
		return &Invalid{"choose at least one event"}
	}
	for _, e := range r.Events {
		if !slices.Contains(Types, e) {
			return &Invalid{"unknown event " + e}
		}
	}
	for table, id := range map[string]string{"teams": r.TeamID, "projects": r.ProjectID} {
		if id == "" {
			continue
		}
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE id = ? AND org_id = ?`), id, actor.OrgID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return &Invalid{"the scope does not exist in this organization"}
		}
	}
	return nil
}

// Rules lists the organization's rules.
func (s *Service) Rules(ctx context.Context, actor authz.Principal) ([]Rule, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, channel_id, events, COALESCE(team_id, ''), COALESCE(project_id, ''), created_at FROM notification_rules WHERE org_id = ? ORDER BY created_at`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var r Rule
		var events string
		if err := rows.Scan(&r.ID, &r.ChannelID, &events, &r.TeamID, &r.ProjectID, &r.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(events), &r.Events)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRule removes a rule; pending deliveries it created still go out.
func (s *Service) DeleteRule(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM notification_rules WHERE id = ? AND org_id = ?`), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-rule.delete", ResourceType: "notification-rule", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// Test sends a test message to a channel now and reports the result; it is audited.
func (s *Service) Test(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	c, err := s.channel(ctx, actor.OrgID, id)
	if err != nil {
		return err
	}
	m := Message{ID: "test-" + ids.New(), Type: "test", Title: "Test from Samuori", Text: "This channel receives Samuori notifications.", OccurredAt: s.now().UTC()}
	m.Resource.Type, m.Resource.ID = "notification-channel", c.ID
	err = s.send(ctx, c, m)
	outcome, reason := "succeeded", ""
	if err != nil {
		outcome, reason = "failed", clip(err.Error())
	}
	_ = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "notification-channel.test", ResourceType: "notification-channel", ResourceID: c.ID, Outcome: outcome, Reason: reason, RequestID: requestID})
	})
	return err
}

func (s *Service) send(ctx context.Context, c Channel, m Message) error {
	snd := s.senders[c.Kind]
	if snd == nil {
		return fmt.Errorf("%s delivery is not available", c.Kind)
	}
	var secret []byte
	if c.SecretID != "" {
		v, err := s.secrets.ControllerValue(ctx, c.OrgID, c.SecretID)
		if err != nil {
			return errors.New("the channel's secret is unavailable")
		}
		secret = v
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return snd.Send(sctx, c, secret, m)
}

func clip(v string) string {
	if len(v) > 500 {
		return v[:500]
	}
	return v
}
