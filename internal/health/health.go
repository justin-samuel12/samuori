// Package health aggregates component checks for /api/v1/system/health.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

type Status string

const (
	StatusOK          Status = "ok"
	StatusDegraded    Status = "degraded"
	StatusUnavailable Status = "unavailable"
)

// DegradedError marks a failure that leaves the controller able to serve, even from a critical check.
type DegradedError struct{ Err error }

func (e *DegradedError) Error() string { return e.Err.Error() }
func (e *DegradedError) Unwrap() error { return e.Err }

// Degraded reports err as degraded rather than unavailable.
func Degraded(err error) error { return &DegradedError{Err: err} }

type Check struct {
	Name     string
	Critical bool
	Run      func(ctx context.Context) error
}

type Component struct {
	Name      string    `json:"name"`
	Status    Status    `json:"status"`
	CheckedAt time.Time `json:"checkedAt"`
}

type Report struct {
	Status     Status      `json:"status"`
	Components []Component `json:"components"`
}

type Registry struct {
	mu      sync.RWMutex
	checks  []Check
	timeout time.Duration
	now     func() time.Time
}

func NewRegistry(timeout time.Duration) *Registry {
	return &Registry{timeout: timeout, now: time.Now}
}

func (r *Registry) Register(c Check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, c)
}

func (r *Registry) Evaluate(ctx context.Context) Report {
	r.mu.RLock()
	checks := append([]Check(nil), r.checks...)
	r.mu.RUnlock()

	report := Report{Status: StatusOK, Components: make([]Component, len(checks))}
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.Run(cctx) }()
			status := StatusOK
			select {
			case err := <-done:
				var d *DegradedError
				switch {
				case errors.As(err, &d):
					status = StatusDegraded
				case err != nil:
					status = StatusUnavailable
				}
			case <-cctx.Done():
				status = StatusUnavailable
			}
			report.Components[i] = Component{Name: c.Name, Status: status, CheckedAt: r.now().UTC()}
		}()
	}
	wg.Wait()

	for i, c := range checks {
		if report.Components[i].Status == StatusOK {
			continue
		}
		if c.Critical && report.Components[i].Status == StatusUnavailable {
			report.Status = StatusUnavailable
		} else if report.Status == StatusOK {
			report.Status = StatusDegraded
		}
	}
	return report
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		report := r.Evaluate(req.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if report.Status == StatusUnavailable {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(report)
	})
}
