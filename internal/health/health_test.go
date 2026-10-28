package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCriticalDegraded(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want Status
		code int
	}{
		{"a critical check that only degrades keeps the controller serving", Degraded(errors.New("waiting for a second controller")), StatusDegraded, http.StatusOK},
		{"a critical failure makes it unavailable", errors.New("no NATS majority"), StatusUnavailable, http.StatusServiceUnavailable},
	} {
		r := NewRegistry(time.Second)
		r.Register(Check{Name: "database", Critical: true, Run: func(context.Context) error { return nil }})
		r.Register(Check{Name: "messaging", Critical: true, Run: func(context.Context) error { return c.err }})
		if got := r.Evaluate(context.Background()).Status; got != c.want {
			t.Errorf("%s: status %s, want %s", c.name, got, c.want)
		}
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != c.code {
			t.Errorf("%s: HTTP %d, want %d", c.name, w.Code, c.code)
		}
	}
}
