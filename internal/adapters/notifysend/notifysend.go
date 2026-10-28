// Package notifysend delivers notify messages: signed webhooks, Slack and Teams incoming webhooks, and email over
// SMTP. HTTP requests pass the address guard (SEC-NET-03) and never follow redirects.
package notifysend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/netguard"
	"samuori.invalid/samuori/internal/notify"
)

func post(ctx context.Context, c notify.Channel, url string, body []byte, headers map[string]string) error {
	client := netguard.Client(c.AllowPrivateNetwork, 30*time.Second, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "samuori-notify/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) {
			return fmt.Errorf("delivery failed: %v", ue.Unwrap())
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the receiver answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// Sign returns the X-Samuori-Signature value for body at t: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Webhook posts the message as JSON, signed with the channel's key.
type Webhook struct{ Now func() time.Time }

func (w Webhook) Send(ctx context.Context, c notify.Channel, secret []byte, m notify.Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	return post(ctx, c, c.URL, body, map[string]string{"X-Samuori-Event": m.Type, "X-Samuori-Delivery": m.ID, "X-Samuori-Signature": Sign(secret, now, body)})
}

func line(m notify.Message) string {
	s := m.Title
	if m.Text != "" {
		s += ": " + m.Text
	}
	return s
}

// Slack posts to an incoming webhook URL, which is the channel's secret.
type Slack struct{}

func (Slack) Send(ctx context.Context, c notify.Channel, secret []byte, m notify.Message) error {
	text := line(m)
	if m.Link != "" {
		text += " <" + m.Link + "|Open in Samuori>"
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	return post(ctx, c, string(secret), body, nil)
}

// Teams posts an Adaptive Card to a Teams workflow webhook URL, which is the channel's secret.
type Teams struct{}

func (Teams) Send(ctx context.Context, c notify.Channel, secret []byte, m notify.Message) error {
	card := map[string]any{"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "version": "1.4",
		"body": []map[string]any{{"type": "TextBlock", "weight": "Bolder", "text": m.Title, "wrap": true}, {"type": "TextBlock", "text": m.Text, "wrap": true}}}
	if m.Link != "" {
		card["actions"] = []map[string]string{{"type": "Action.OpenUrl", "title": "Open in Samuori", "url": m.Link}}
	}
	body, _ := json.Marshal(map[string]any{"type": "message", "attachments": []map[string]any{{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}}})
	return post(ctx, c, string(secret), body, nil)
}

// SMTP sends email through the controller's SMTP server. STARTTLS is required except to a loopback relay.
type SMTP struct {
	Host     string
	Port     int
	From     string
	Username string
	Password string
}

func (s SMTP) Send(ctx context.Context, c notify.Channel, _ []byte, m notify.Message) error {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connecting to the SMTP server: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	loopback := s.Host == "localhost" || net.ParseIP(s.Host) != nil && net.ParseIP(s.Host).IsLoopback()
	if ok, _ := cl.Extension("STARTTLS"); ok {
		if err := cl.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	} else if !loopback {
		return errors.New("the SMTP server does not offer STARTTLS")
	}
	if s.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("SMTP authentication: %w", err)
		}
	}
	if err := cl.Mail(s.From); err != nil {
		return err
	}
	for _, r := range c.Recipients {
		if err := cl.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", r, err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace("[Samuori] " + m.Title)
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nAuto-Submitted: auto-generated\r\n\r\n%s\r\n\r\n%s\r\n",
		s.From, strings.Join(c.Recipients, ", "), subject, line(m), m.Link)
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
