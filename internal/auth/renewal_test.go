package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

func TestSessionRenewal(t *testing.T) {
	t.Run("SEC-AUTH-13", func(t *testing.T) {
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
		const pass, bobPass = "a long enough passphrase", "a different long passphrase"
		if err := s.Bootstrap(ctx, BootstrapInput{OrganizationName: "Org", Username: "admin", Password: pass}, t.TempDir(), "", false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateLocalUser(ctx, adminOf(t, s), "bob", "", bobPass, ""); err != nil {
			t.Fatal(err)
		}
		t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
		now := t0
		s.now = func() time.Time { return now }
		authenticated := func(token string) bool {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
			_, ok, _ := s.Authenticate(r)
			return ok
		}
		old, err := s.Login(ctx, "", "admin", pass, "10.0.0.1", "")
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 28; i++ {
			now = now.Add(25 * time.Minute)
			if _, err := s.ExtendSession(ctx, old.Token); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.RenewSession(ctx, old.Token, bobPass, "10.0.0.1", ""); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("another user's password renewed the session: %v", err)
		}
		fresh, st, err := s.RenewSession(ctx, old.Token, pass, "10.0.0.1", "renew-1")
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Token == old.Token || fresh.CSRF == old.CSRF || fresh.UserID != old.UserID {
			t.Fatal("renewal kept the session token or CSRF token, or changed the user")
		}
		if !st.AbsoluteExpiresAt.Equal(now.Add(12*time.Hour)) || !st.IdleExpiresAt.Equal(now.Add(30*time.Minute)) {
			t.Fatalf("renewed deadlines = %v / %v", st.IdleExpiresAt, st.AbsoluteExpiresAt)
		}
		if authenticated(old.Token) || !authenticated(fresh.Token) {
			t.Fatal("the old session still works or the new one does not")
		}
		if _, _, err := s.RenewSession(ctx, old.Token, pass, "10.0.0.1", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a replaced session was renewed again: %v", err)
		}
		var audited int
		if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'session.renew' AND outcome = 'succeeded' AND request_id = 'renew-1'`).Scan(&audited); err != nil || audited != 1 {
			t.Fatalf("renewal audited %d times: %v", audited, err)
		}

		now = now.Add(12*time.Hour + time.Second)
		if _, _, err := s.RenewSession(ctx, fresh.Token, pass, "10.0.0.1", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an expired session was renewed: %v", err)
		}

		bob, err := s.Login(ctx, "", "bob", bobPass, "10.0.0.2", "")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < DefaultPolicy().MaxFailures; i++ {
			if _, _, err := s.RenewSession(ctx, bob.Token, "wrong", "10.0.0.2", ""); !errors.Is(err, ErrWrongPassword) {
				t.Fatalf("wrong password %d: %v", i, err)
			}
		}
		if _, _, err := s.RenewSession(ctx, bob.Token, bobPass, "10.0.0.2", ""); !errors.Is(err, ErrLocked) {
			t.Fatalf("renewal after repeated wrong passwords was not locked out: %v", err)
		}
		if _, err := s.Login(ctx, "", "bob", bobPass, "10.0.0.3", ""); !errors.Is(err, ErrLocked) {
			t.Fatalf("wrong renewal passwords did not count toward the sign-in lockout: %v", err)
		}
		if !authenticated(bob.Token) {
			t.Fatal("failed renewals ended the session")
		}

		now = now.Add(16 * time.Minute)
		for i := 0; i < DefaultPolicy().PerIPPerMinute; i++ {
			s.allowIP("10.0.0.9", now)
		}
		if _, _, err := s.RenewSession(ctx, bob.Token, bobPass, "10.0.0.9", ""); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("renewal ignored the per-address rate limit: %v", err)
		}

		if _, err := d.SQL.Exec(`UPDATE users SET disabled = ? WHERE username = 'bob'`, true); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.RenewSession(ctx, bob.Token, bobPass, "10.0.0.4", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("a disabled user renewed a session: %v", err)
		}
		if _, err := d.SQL.Exec(`UPDATE users SET disabled = ? WHERE username = 'bob'`, false); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SQL.Exec(`UPDATE user_credentials SET temporary_until = ? WHERE user_id = (SELECT id FROM users WHERE username = 'bob')`, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.RenewSession(ctx, bob.Token, bobPass, "10.0.0.4", ""); !errors.Is(err, ErrPasswordChangeRequired) {
			t.Fatalf("a user who must change their password renewed a session: %v", err)
		}
	})
}

func TestSafeReturnTo(t *testing.T) {
	for _, ok := range []string{"/", "/projects/abc", "/builds?status=failed#top", "/a/@b"} {
		if got, safe := SafeReturnTo(ok); !safe || got != ok {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "https://evil.example/", "//evil.example/x", "/\\evil.example", "\\\\evil", "javascript:alert(1)",
		"evil.example", "/api/v1/auth/logout", "/api", "/x\r\nLocation: https://evil.example", "/x\ty", "http:/evil.example"} {
		if got, safe := SafeReturnTo(bad); safe || got != "/" {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}
