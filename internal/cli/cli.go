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
	"regexp"
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

// PollInterval is how often waiting commands ask the server for progress.
var PollInterval = 3 * time.Second

type env struct {
	api    *apiclient.ClientWithResponses
	stream *apiclient.ClientWithResponses
	raw    *http.Client
	base   string
	auth   apiclient.RequestEditorFn
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	getenv func(string) string
	json   bool
	poll   time.Duration
	global globalFlags
}

type globalFlags struct {
	url, tokenFile, caFile, profile string
}

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func failf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

// Run executes args and returns the exit code. getenv reads the environment; stdin feeds login.
func Run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("samuori", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usageText()) }
	var g globalFlags
	global.StringVar(&g.url, "url", getenv("SAMUORI_URL"), "")
	global.StringVar(&g.tokenFile, "token-file", getenv("SAMUORI_TOKEN_FILE"), "")
	global.StringVar(&g.caFile, "ca-file", getenv("SAMUORI_CA_FILE"), "")
	global.StringVar(&g.profile, "profile", getenv("SAMUORI_PROFILE"), "")
	asJSON := global.Bool("json", false, "")
	if err := global.Parse(args); err != nil {
		return ExitUsage
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return ExitUsage
	}
	e := &env{in: stdin, out: stdout, errOut: stderr, getenv: getenv, json: *asJSON, poll: PollInterval, global: g}
	offline := map[string]func(context.Context, []string) error{
		"version": e.version, "help": e.help, "login": e.login, "logout": e.logout, "profiles": e.profiles,
		"completion": e.completion,
	}
	online := map[string]func(context.Context, []string) error{
		"whoami": e.whoami, "projects": e.projects, "upload": e.upload, "analyze": e.analyze, "spec": e.spec,
		"build": e.build, "builds": e.builds, "status": e.status, "logs": e.logs, "cancel": e.cancel,
		"images": e.images, "download": e.download, "deploy-files": e.deployFiles, "publish": e.publish,
		"promote": e.promote, "requests": e.requests, "request": e.request, "withdraw": e.withdraw,
		"pipeline": e.pipeline, "pipelines": e.pipelines, "api": e.apiCall, "ci": e.ci, "config": e.config,
	}
	cmd, ok := offline[rest[0]]
	if !ok {
		if cmd, ok = online[rest[0]]; !ok {
			fmt.Fprintf(stderr, "samuori: unknown command %q; run samuori help\n", rest[0])
			return ExitUsage
		}
		if !wantsHelp(rest[1:]) {
			s, err := resolveSettings(g, getenv)
			if err == nil {
				err = e.connect(s)
			}
			if err != nil {
				fmt.Fprintln(stderr, "samuori:", err)
				return ExitUsage
			}
		}
	}
	if err := cmd(ctx, rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitUsage
		}
		fmt.Fprintln(stderr, "samuori:", err)
		var x *exitError
		if errors.As(err, &x) {
			return x.code
		}
		return ExitError
	}
	return ExitOK
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "-help" || a == "--help" {
			return true
		}
	}
	return false
}

func (e *env) version(context.Context, []string) error {
	fmt.Fprintln(e.out, version.Version)
	fmt.Fprintln(e.out, version.Credit)
	return nil
}

// connect refuses to send the token in clear text except to a loopback server.
func (e *env) connect(s settings) error {
	if s.url == "" {
		return errors.New("set --url or SAMUORI_URL, or run samuori login")
	}
	if err := checkURL(s.url); err != nil {
		return err
	}
	if s.token == "" {
		return errors.New("set SAMUORI_TOKEN or --token-file, or run samuori login")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.caFile != "" {
		pem, err := os.ReadFile(s.caFile)
		if err != nil {
			return fmt.Errorf("reading the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("the CA bundle holds no PEM certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, ResponseHeaderTimeout: 5 * time.Minute}
	// The token is never sent to another host.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	e.raw = &http.Client{Transport: transport, CheckRedirect: noRedirect}
	timed := &http.Client{Timeout: 5 * time.Minute, Transport: transport, CheckRedirect: noRedirect}
	token := s.token
	e.auth = func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("User-Agent", "samuori/"+version.Version)
		return nil
	}
	e.base = strings.TrimRight(s.url, "/") + "/api/v1"
	var err error
	if e.api, err = apiclient.NewClientWithResponses(e.base, apiclient.WithHTTPClient(timed), apiclient.WithRequestEditorFn(e.auth)); err != nil {
		return err
	}
	e.stream, err = apiclient.NewClientWithResponses(e.base, apiclient.WithHTTPClient(e.raw), apiclient.WithRequestEditorFn(e.auth))
	return err
}

func checkURL(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return errors.New("the server URL must be https://host[:port]")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("plain http is allowed only to a loopback server; use https")
		}
	}
	return nil
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
	return strings.TrimSpace(string(raw)), err
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

// flags returns a flag set for a command whose --help prints the command's usage lines; every command accepts --json.
func flags(name string, e *env) *flag.FlagSet {
	fs := flag.NewFlagSet("samuori "+name, flag.ContinueOnError)
	fs.SetOutput(e.errOut)
	fs.BoolVar(&e.json, "json", e.json, "print JSON results")
	fs.Usage = func() {
		printUsage(e.errOut, name)
		fs.PrintDefaults()
	}
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

// subcommand runs the named subcommand, or def when args start with a flag or are empty.
func (e *env) subcommand(args []string, def string, subs map[string]func([]string) error, group string) error {
	name := def
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	run, ok := subs[name]
	if !ok {
		printUsage(e.errOut, group)
		if name == "" {
			return flag.ErrHelp
		}
		return failf(ExitUsage, "unknown %s command %q", group, name)
	}
	return run(args)
}

var idPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func isID(s string) bool { return idPattern.MatchString(s) }

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
