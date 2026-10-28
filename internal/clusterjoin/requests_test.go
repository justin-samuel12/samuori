package clusterjoin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
)

const testSecret = "host-secret-0123456789abcdefghijklmnopqrstuv"

var testSecretHash = hash(testSecret)

func TestJoinRequests(t *testing.T) {
	t.Run("SEC-HA-04", func(t *testing.T) {
		ctx := context.Background()
		s := store(t)
		admin, operator := principal(authz.Administrator), principal(authz.Operator)
		built := 0
		bundle := func(host string) (Bundle, error) {
			built++
			return Bundle{Host: host, DatabaseDSN: "postgres://samuori:pw@db.example/samuori", ClusterKey: make([]byte, 32)}, nil
		}
		open := func(host string) Request {
			t.Helper()
			_, value, err := s.Create(ctx, admin, time.Hour, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Redeem(ctx, value, host, "192.0.2.1", "not-a-hash", ""); !errors.Is(err, ErrInvalid) {
				t.Fatalf("a request without a valid secret hash was opened: %v", err)
			}
			q, err := s.Redeem(ctx, value, host, "192.0.2.1", testSecretHash, "")
			if err != nil {
				t.Fatal(err)
			}
			return q
		}

		q := open("ctrl-2.example")
		if q.State != StatePending || len(q.Code) != 9 || q.Code[4] != '-' || strings.ContainsAny(q.Code, "01IO") {
			t.Fatalf("request = %+v", q)
		}
		var storedHash string
		_ = s.db.SQL.QueryRow(`SELECT secret_hash FROM controller_join_requests WHERE id = ?`, q.ID).Scan(&storedHash)
		if storedHash != testSecretHash {
			t.Fatal("the request does not keep only the secret's hash")
		}

		if got, b, err := s.Poll(ctx, q.ID, testSecret, bundle, ""); err != nil || b != nil || got.State != StatePending {
			t.Fatalf("pending poll = %v, bundle %v, %v", got.State, b != nil, err)
		}
		_, _, wrong := s.Poll(ctx, q.ID, strings.Repeat("x", 43), bundle, "")
		_, _, unknown := s.Poll(ctx, "01JAB3K0000000000000000001", testSecret, bundle, "")
		if !errors.Is(wrong, ErrInvalid) || !errors.Is(unknown, ErrInvalid) || wrong.Error() != unknown.Error() {
			t.Fatalf("wrong secret %v and unknown request %v differ", wrong, unknown)
		}

		if _, err := s.ListRequests(ctx, operator); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator listed join requests: %v", err)
		}
		if _, err := s.Decide(ctx, operator, q.ID, true, "", ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("an operator accepted a join request: %v", err)
		}
		if built != 0 {
			t.Fatal("a bundle was built before an administrator accepted")
		}

		for _, h := range []string{"ctrl-2.example", "ctrl-9.example"} {
			if _, err := s.db.SQL.Exec(s.db.Rebind(`INSERT INTO controller_removals (hostname, host, cert_pins, org_id, reason, requested_by, requested_at, state, updated_at) VALUES (?, ?, 'sha256:old', 'o1', 'gone', 'admin', ?, 'removed', ?)`),
				strings.TrimSuffix(h, ".example"), h, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Decide(ctx, admin, q.ID, true, "codes match", ""); err != nil {
			t.Fatal(err)
		}
		for h, want := range map[string]string{"ctrl-2.example": "rejoined", "ctrl-9.example": "removed"} {
			var got string
			_ = s.db.SQL.QueryRow(s.db.Rebind(`SELECT state FROM controller_removals WHERE host = ?`), h).Scan(&got)
			if got != want {
				t.Errorf("after accepting ctrl-2's join, %s's removal is %q; want %q", h, got, want)
			}
		}
		if _, err := s.Decide(ctx, admin, q.ID, false, "", ""); !errors.Is(err, ErrDecided) {
			t.Fatalf("a decided request was decided again: %v", err)
		}
		if _, b, err := s.Poll(ctx, q.ID, strings.Repeat("x", 43), bundle, ""); !errors.Is(err, ErrInvalid) || b != nil {
			t.Fatalf("an accepted bundle went to a wrong secret: %v", err)
		}
		got, b, err := s.Poll(ctx, q.ID, testSecret, bundle, "")
		if err != nil || b == nil || b.Host != "ctrl-2.example" || got.State != StateCompleted {
			t.Fatalf("accepted poll = %+v, %v", got, err)
		}
		if _, b, err := s.Poll(ctx, q.ID, testSecret, bundle, ""); !errors.Is(err, ErrInvalid) || b != nil {
			t.Fatalf("the bundle was handed out twice: %v", err)
		}

		r := open("ctrl-3.example")
		if _, err := s.Decide(ctx, admin, r.ID, false, "unknown host", ""); err != nil {
			t.Fatal(err)
		}
		if got, b, err := s.Poll(ctx, r.ID, testSecret, bundle, ""); !errors.Is(err, ErrRejected) || b != nil || got.Reason != "unknown host" {
			t.Fatalf("rejected poll = %v, %v", got.Reason, err)
		}

		e := open("ctrl-4.example")
		a := open("ctrl-5.example")
		if _, err := s.Decide(ctx, admin, a.ID, true, "", ""); err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		if _, err := s.Decide(ctx, admin, e.ID, true, "", ""); !errors.Is(err, ErrDecided) {
			t.Fatalf("an expired request was accepted: %v", err)
		}
		for _, id := range []string{e.ID, a.ID} {
			if _, b, err := s.Poll(ctx, id, testSecret, bundle, ""); !errors.Is(err, ErrExpired) || b != nil {
				t.Fatalf("expired poll: %v", err)
			}
		}
		if built != 1 {
			t.Fatalf("%d bundles built, want 1", built)
		}

		list, err := s.ListRequests(ctx, admin)
		if err != nil || len(list) != 4 {
			t.Fatalf("list = %d, %v", len(list), err)
		}
		var actions []string
		rows, _ := s.db.SQL.Query(`SELECT action FROM audit_events WHERE action LIKE 'controller.join%' ORDER BY action`)
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			actions = append(actions, a)
		}
		_ = rows.Close()
		joined := strings.Join(actions, ",")
		for _, want := range []string{"controller.join.accept", "controller.join.reject", "controller.join.request", "controller.join,"} {
			if !strings.Contains(joined+",", want) {
				t.Errorf("audit lacks %s: %s", want, joined)
			}
		}

		url, pin, token, err := DecodeCode(EncodeCode("https://ctrl-1.example:8443", "sha256:abc", "samuorij_x"))
		if err != nil || url != "https://ctrl-1.example:8443" || pin != "sha256:abc" || token != "samuorij_x" {
			t.Fatalf("code round trip = %q %q %q %v", url, pin, token, err)
		}
		for _, bad := range []string{"samuorij_x", "samuoric_!!", EncodeCode("http://ctrl-1.example", "sha256:abc", "samuorij_x")} {
			if _, _, _, err := DecodeCode(bad); err == nil {
				t.Errorf("malformed code %q accepted", bad)
			}
		}
	})
}
