// Package server hosts the controller's HTTP API.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/config"
	"samuori.invalid/samuori/internal/telemetry"
)

type Server struct {
	cfg     config.Config
	log     *slog.Logger
	mux     *http.ServeMux
	metrics *telemetry.Metrics
}

func New(cfg config.Config, log *slog.Logger, api http.Handler) *Server {
	s := &Server{cfg: cfg, log: log, mux: http.NewServeMux()}
	s.mux.Handle("/api/v1/", api)
	if cfg.UIDir != "" {
		s.mux.Handle("/", webUI(cfg.UIDir))
	}
	return s
}

// pressableStyle is the hash of the one <style> element React Aria injects; the e2e suite fails if it changes.
const pressableStyle = "'sha256-38RhXrc7EdReTKsOm23ZPOCUgniTUUcjky8QOOrQx6o='"

// webUI serves the built single-page app; unknown non-asset paths fall back to index.html for client routing.
func webUI(dir string) http.Handler {
	root := os.DirFS(dir)
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' "+pressableStyle+"; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "api" || strings.HasPrefix(name, "api/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}
		if info, err := fs.Stat(root, name); name == "" || err != nil || info.IsDir() {
			h.Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, root, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// UseMetrics records API request metrics in m.
func (s *Server) UseMetrics(m *telemetry.Metrics) { s.metrics = m }

// Serve runs until ctx is cancelled, then drains in-flight requests within the shutdown timeout.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.middleware(s.mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		if s.cfg.TLSEnabled() {
			errc <- srv.ServeTLS(ln, s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	s.log.Info("listening", "addr", ln.Addr().String(), "tls", s.cfg.TLSEnabled(), "role", s.cfg.Role)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) middleware(next http.Handler) http.Handler {
	logged := telemetry.Instrument(next, s.metrics, func(r *http.Request, info telemetry.RequestInfo, status int, d time.Duration, traceID string) {
		attrs := []any{"id", r.Header.Get("X-Request-ID"), "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", d.Milliseconds()}
		if info.Operation != "" {
			attrs = append(attrs, "operation", info.Operation)
		}
		if traceID != "" {
			attrs = append(attrs, "trace_id", traceID)
		}
		s.log.Info("request", attrs...)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		logged.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
