// Package webrun is samuori-web, the launcher that serves a built frontend and forwards API paths to a backend it starts
// in the same image (ADR 0055).
package webrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Config is what the image's entrypoint passes to the launcher.
type Config struct {
	Listen string
	// Root holds the frontend's built files.
	Root string
	// Prefixes are forwarded to Backend, such as "/api".
	Prefixes []string
	Backend  *url.URL
	// Command is the backend's argv; empty serves files only.
	Command []string
}

// Handler serves files from Root and forwards Prefixes to Backend.
func Handler(c Config) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(c.Backend)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "the backend is not answering", http.StatusBadGateway)
	}
	files := staticFiles{root: c.Root}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		for _, p := range c.Prefixes {
			if r.URL.Path == p || strings.HasPrefix(r.URL.Path, strings.TrimSuffix(p, "/")+"/") {
				proxy.ServeHTTP(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}

type staticFiles struct{ root string }

// ServeHTTP serves a file under root, or index.html for paths that are not files, so client-side routes work. It
// never lists directories or follows a path outside root.
func (s staticFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	name := filepath.Join(s.root, filepath.FromSlash(clean))
	if info, err := os.Stat(name); err == nil && !info.IsDir() && within(s.root, name) {
		http.ServeFile(w, r, name)
		return
	}
	if path.Ext(clean) != "" {
		http.NotFound(w, r)
		return
	}
	index := filepath.Join(s.root, "index.html")
	if _, err := os.Stat(index); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}

func within(root, name string) bool {
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return false
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Run serves until the backend exits or a stop signal arrives, and returns the backend's exit code.
func Run(ctx context.Context, c Config, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv := &http.Server{Addr: c.Listen, Handler: Handler(c), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		fmt.Fprintln(stderr, "samuori-web:", err)
		return 1
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	code := 0
	if len(c.Command) > 0 {
		cmd := exec.Command(c.Command[0], c.Command[1:]...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(stderr, "samuori-web: starting the backend:", err)
			_ = srv.Close()
			return 1
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			code = exitCode(err)
			fmt.Fprintf(stderr, "samuori-web: the backend exited (%d); stopping\n", code)
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-exited:
				code = exitCode(err)
			case <-time.After(20 * time.Second):
				_ = cmd.Process.Kill()
				code = exitCode(<-exited)
			}
		case err := <-served:
			fmt.Fprintln(stderr, "samuori-web:", err)
			_ = cmd.Process.Signal(syscall.SIGTERM)
			<-exited
			return 1
		}
	} else {
		select {
		case <-ctx.Done():
		case err := <-served:
			fmt.Fprintln(stderr, "samuori-web:", err)
			return 1
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return code
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return ee.ExitCode()
	}
	return 1
}
