// Package cli is the samuori command (D29, docs/operators/cli.md). It calls the same API as the web UI with an API
// token, so every command passes the same authorization, validation and execution paths.
package cli

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
	"samuori.invalid/samuori/internal/version"
)

// Exit codes. Anything other than ExitOK means the requested outcome was not reached; blocked and skipped are failures.
const (
	ExitOK      = 0
	ExitFailed  = 1
	ExitUsage   = 2
	ExitError   = 3
	ExitTimeout = 4
)

type env struct {
	api    *apiclient.ClientWithResponses
	out    io.Writer
	errOut io.Writer
	json   bool
	poll   time.Duration
}

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func failf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

const usage = `samuori talks to a Samuori server with an API token.

Usage: samuori [global flags] <command> [flags]

Commands:
  whoami                     show the identity behind the token
  projects                   list the projects the token can see
  upload  --project ID PATH  upload a directory or archive; prints the snapshot ID
  build   --spec ID ...      start a build; --wait follows it through its checks
  status  BUILD              show a build as JSON
  logs    BUILD              print a build's log
  publish --artifact ID --destination ID [--tag T]...  request a publication
  pipeline run PIPELINE --ref REF   start a pipeline run
  version                    print the client version

Global flags (or environment):
  --url URL          server URL (SAMUORI_URL)
  --token-file FILE  file holding the API token, mode 0600 (SAMUORI_TOKEN_FILE); or set SAMUORI_TOKEN
  --ca-file FILE     PEM CA bundle for a private certificate authority (SAMUORI_CA_FILE)
  --json             print JSON results

Exit codes: 0 success, 1 the build, checks, publication or run did not succeed (blocked and skipped included),
2 usage, 3 request or server error, 4 timed out while waiting.
`

// Run executes args and returns the exit code. getenv reads the environment.
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("samuori", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	server := global.String("url", getenv("SAMUORI_URL"), "")
	tokenFile := global.String("token-file", getenv("SAMUORI_TOKEN_FILE"), "")
	caFile := global.String("ca-file", getenv("SAMUORI_CA_FILE"), "")
	asJSON := global.Bool("json", false, "")
	if err := global.Parse(args); err != nil {
		return ExitUsage
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return ExitUsage
	}
	if rest[0] == "version" {
		fmt.Fprintln(stdout, version.Version)
		fmt.Fprintln(stdout, version.Credit)
		return ExitOK
	}
	if rest[0] == "help" {
		global.Usage()
		return ExitOK
	}
	client, err := connect(*server, *tokenFile, *caFile, getenv("SAMUORI_TOKEN"))
	if err != nil {
		fmt.Fprintln(stderr, "samuori:", err)
		return ExitUsage
	}
	e := &env{api: client, out: stdout, errOut: stderr, json: *asJSON, poll: 3 * time.Second}
	cmds := map[string]func(context.Context, []string) error{
		"whoami": e.whoami, "projects": e.projects, "upload": e.upload, "build": e.build,
		"status": e.status, "logs": e.logs, "publish": e.publish, "pipeline": e.pipeline,
	}
	cmd, ok := cmds[rest[0]]
	if !ok {
		fmt.Fprintf(stderr, "samuori: unknown command %q\n", rest[0])
		return ExitUsage
	}
	if err := cmd(ctx, rest[1:]); err != nil {
		fmt.Fprintln(stderr, "samuori:", err)
		var x *exitError
		if errors.As(err, &x) {
			return x.code
		}
		if errors.Is(err, flag.ErrHelp) {
			return ExitUsage
		}
		return ExitError
	}
	return ExitOK
}

// connect refuses to send the token in clear text except to a loopback server.
func connect(server, tokenFile, caFile, token string) (*apiclient.ClientWithResponses, error) {
	if server == "" {
		return nil, errors.New("set --url or SAMUORI_URL")
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, errors.New("the server URL must be https://host[:port]")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("plain http is allowed only to a loopback server; use https")
		}
	}
	if tokenFile != "" {
		raw, err := readPrivate(tokenFile)
		if err != nil {
			return nil, err
		}
		token = raw
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("set SAMUORI_TOKEN or --token-file")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("the CA bundle holds no PEM certificates")
		}
		tlsConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true},
		// The token is never sent to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	auth := func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("User-Agent", "samuori/"+version.Version)
		return nil
	}
	return apiclient.NewClientWithResponses(strings.TrimRight(server, "/")+"/api/v1", apiclient.WithHTTPClient(httpClient), apiclient.WithRequestEditorFn(auth))
}

func readPrivate(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading the token file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("the token file %s must not be readable by group or others (chmod 600)", path)
	}
	raw, err := os.ReadFile(path)
	return string(raw), err
}

// problem turns an unexpected response into an error with the server's code and message.
func problem(what string, status int, body []byte) error {
	var p struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(body, &p) == nil && p.Message != "" {
		msg := fmt.Sprintf("%s: %s (HTTP %d, %s)", what, p.Message, status, p.Code)
		if p.RequestID != "" {
			msg += ", request " + p.RequestID
		}
		return errors.New(msg)
	}
	return fmt.Errorf("%s: HTTP %d", what, status)
}

func newKey(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func (e *env) printJSON(v any) error {
	enc := json.NewEncoder(e.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (e *env) note(format string, args ...any) { fmt.Fprintf(e.errOut, format+"\n", args...) }

func flags(name string, e *env) *flag.FlagSet {
	fs := flag.NewFlagSet("samuori "+name, flag.ContinueOnError)
	fs.SetOutput(e.errOut)
	return fs
}

// parse accepts flags before and after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func (e *env) whoami(ctx context.Context, args []string) error {
	if _, err := parse(flags("whoami", e), args); err != nil {
		return err
	}
	r, err := e.api.GetMeWithResponse(ctx)
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("whoami", r.StatusCode(), r.Body)
	}
	if e.json {
		return e.printJSON(r.JSON200)
	}
	name := r.JSON200.Principal.Id
	if r.JSON200.DisplayName != nil {
		name = *r.JSON200.DisplayName
	}
	fmt.Fprintln(e.out, name)
	for _, b := range r.JSON200.Roles {
		fmt.Fprintf(e.out, "  %s on %s %s\n", b.Role, b.ScopeType, b.ScopeId)
	}
	return nil
}

func (e *env) projects(ctx context.Context, args []string) error {
	if _, err := parse(flags("projects", e), args); err != nil {
		return err
	}
	limit := 200
	r, err := e.api.ListProjectsWithResponse(ctx, &apiclient.ListProjectsParams{Limit: &limit})
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("listing projects", r.StatusCode(), r.Body)
	}
	if e.json {
		return e.printJSON(r.JSON200.Items)
	}
	for _, p := range r.JSON200.Items {
		fmt.Fprintf(e.out, "%s\t%s\n", p.Id, p.Name)
	}
	return nil
}

func (e *env) status(ctx context.Context, args []string) error {
	pos, err := parse(flags("status", e), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori status BUILD")
	}
	r, err := e.api.GetBuildWithResponse(ctx, pos[0])
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("reading the build", r.StatusCode(), r.Body)
	}
	return e.printJSON(r.JSON200)
}

func (e *env) logs(ctx context.Context, args []string) error {
	pos, err := parse(flags("logs", e), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori logs BUILD")
	}
	r, err := e.api.GetBuildLogsWithResponse(ctx, pos[0])
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK {
		return problem("reading the log", r.StatusCode(), r.Body)
	}
	_, err = e.out.Write(r.Body)
	return err
}
