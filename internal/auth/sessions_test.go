package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

func TestSessionExpiry(t *testing.T) {
	t.Run("SEC-AUTH-11", func(t *testing.T) {
		ctx := context.Background()
		d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		if _, err := d.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		s := NewService(d, tenancy.NewStore(d), DefaultPolicy())
		if err := s.Bootstrap(ctx, BootstrapInput{OrganizationName: "Org", Username: "admin", Password: "a long enough passphrase"}, t.TempDir(), "", false); err != nil {
			t.Fatal(err)
		}
		t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
		now := t0
		s.now = func() time.Time { return now }
		sess, err := s.Login(ctx, "", "admin", "a long enough passphrase", "127.0.0.1", "")
		if err != nil {
			t.Fatal(err)
		}
		get := func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: sess.Token})
			return r
		}
		status := func() SessionStatus {
			t.Helper()
			st, err := s.Session(ctx, sess.Token)
			if err != nil {
				t.Fatal(err)
			}
			return st
		}

		if st := status(); !st.IdleExpiresAt.Equal(t0.Add(30*time.Minute)) || !st.AbsoluteExpiresAt.Equal(t0.Add(12*time.Hour)) {
			t.Fatalf("deadlines = %v / %v", st.IdleExpiresAt, st.AbsoluteExpiresAt)
		}
		now = t0.Add(20 * time.Minute)
		if _, ok, err := s.Authenticate(get()); !ok || err != nil {
			t.Fatalf("live session refused: %v", err)
		}
		if st := status(); !st.IdleExpiresAt.Equal(t0.Add(30 * time.Minute)) {
			t.Fatalf("a status check or read renewed the idle deadline to %v", st.IdleExpiresAt)
		}

		for now.Before(t0.Add(11*time.Hour + 50*time.Minute)) {
			st, err := s.ExtendSession(ctx, sess.Token)
			if err != nil {
				t.Fatalf("extend at %v: %v", now, err)
			}
			if !st.IdleExpiresAt.Equal(now.Add(30*time.Minute)) && !st.IdleExpiresAt.Equal(st.AbsoluteExpiresAt) {
				t.Fatalf("extend at %v gave idle deadline %v", now, st.IdleExpiresAt)
			}
			now = now.Add(20 * time.Minute)
		}
		st, err := s.ExtendSession(ctx, sess.Token)
		if err != nil {
			t.Fatal(err)
		}
		if !st.IdleExpiresAt.Equal(t0.Add(12*time.Hour)) || !st.AbsoluteExpiresAt.Equal(t0.Add(12*time.Hour)) {
			t.Fatalf("extend near the maximum gave %v / %v", st.IdleExpiresAt, st.AbsoluteExpiresAt)
		}
		now = t0.Add(12*time.Hour + time.Second)
		if _, err := s.ExtendSession(ctx, sess.Token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a session past its maximum was extended: %v", err)
		}
		if _, ok, _ := s.Authenticate(get()); ok {
			t.Fatal("a session past its maximum authenticated")
		}

		now = t0.Add(24 * time.Hour)
		idle, err := s.Login(ctx, "", "admin", "a long enough passphrase", "127.0.0.1", "")
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(31 * time.Minute)
		if _, err := s.ExtendSession(ctx, idle.Token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an idle-expired session was extended: %v", err)
		}

		admin := adminOf(t, s)
		dev := authz.Principal{ID: admin.ID, OrgID: admin.OrgID, Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeOrganization, ScopeID: admin.OrgID}}}
		valid := SessionTimeouts{Idle: 15 * time.Minute, Max: 8 * time.Hour, Warning: 60 * time.Second}
		if err := s.SetSessionTimeouts(ctx, dev, valid, ""); !errors.Is(err, tenancy.ErrForbidden) {
			t.Fatalf("a non-administrator changed the session timeouts: %v", err)
		}
		for _, bad := range []SessionTimeouts{
			{Idle: 14 * time.Minute, Max: 8 * time.Hour, Warning: time.Minute},
			{Idle: 121 * time.Minute, Max: 8 * time.Hour, Warning: time.Minute},
			{Idle: 30 * time.Minute, Max: 25 * time.Hour, Warning: time.Minute},
			{Idle: 30 * time.Minute, Max: 0, Warning: time.Minute},
			{Idle: 30 * time.Minute, Max: 8 * time.Hour, Warning: 29 * time.Second},
			{Idle: 30 * time.Minute, Max: 8 * time.Hour, Warning: 601 * time.Second},
			{Idle: 120 * time.Minute, Max: time.Hour, Warning: time.Minute},
		} {
			if err := s.SetSessionTimeouts(ctx, admin, bad, ""); !errors.Is(err, ErrSessionTimeouts) {
				t.Errorf("out-of-bounds timeouts %+v accepted: %v", bad, err)
			}
		}

		now = t0.Add(48 * time.Hour)
		live, err := s.Login(ctx, "", "admin", "a long enough passphrase", "127.0.0.1", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionTimeouts(ctx, admin, valid, "req-1"); err != nil {
			t.Fatal(err)
		}
		var audited int
		if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'identity.session-timeouts' AND request_id = 'req-1'`).Scan(&audited); err != nil || audited != 1 {
			t.Fatalf("timeout change audited %d times: %v", audited, err)
		}
		now = now.Add(16 * time.Minute)
		if _, err := s.Session(ctx, live.Token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a shorter idle timeout did not apply to an existing session: %v", err)
		}
	})
}
