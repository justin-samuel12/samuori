package setupcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"

	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/clusterjoin"
	"samuori.invalid/samuori/internal/redact"
	"samuori.invalid/samuori/internal/topology"
)

// releasePrincipal is the identity internal/selfupdate verifies release signatures against.
const releasePrincipal = "release@samuori.invalid"

// Prompter reads answers; Secret must not echo what is typed.
type Prompter interface {
	Ask(question, def string) (string, error)
	Secret(question string) (string, error)
}

// Host provides the host's defaults and runs the checks against real services.
type Host interface {
	DefaultHostname(ctx context.Context) string
	AWSRegion(ctx context.Context) string
	OnAWS(ctx context.Context) bool
	Resolve(ctx context.Context, name string) error
	DownloadRDSBundle(ctx context.Context, path string) error
	// CheckDatabase signs in and returns the server version.
	CheckDatabase(ctx context.Context, dsn string) (string, error)
	// CheckStorage writes, reads back and deletes a probe object.
	CheckStorage(ctx context.Context, s S3) error
}

type Installer interface {
	Install(ctx context.Context, a Answers, out io.Writer) error
}

type Setup struct {
	Host       Host
	Installer  Installer
	Out        io.Writer
	ReleaseDir string

	mask *redact.Masker
}

// fatal marks an answer that re-asking cannot fix.
type fatal struct{ error }

type step struct {
	name  string
	skip  func(a *Answers) bool
	ask   func(ctx context.Context, s *Setup, p Prompter, a *Answers) error
	check func(ctx context.Context, s *Setup, a *Answers) error
}

const maxAttempts = 5

// Interactive asks every question, checks each answer before moving on, re-asks after a failed check, and installs
// only after the summary is confirmed.
func (s *Setup) Interactive(ctx context.Context, p Prompter) error {
	out := s.writer()
	var a Answers
	for _, st := range steps {
		if st.skip != nil && st.skip(&a) {
			continue
		}
		for attempt := 1; ; attempt++ {
			err := st.ask(ctx, s, p, &a)
			s.addSecrets(a)
			if err == nil {
				err = st.check(ctx, s, &a)
			}
			if err == nil {
				break
			}
			if errors.Is(err, io.EOF) {
				return errors.New("setup cancelled: no more input")
			}
			fmt.Fprintf(out, "  %s: %v\n", st.name, err)
			var f fatal
			if errors.As(err, &f) || attempt == maxAttempts {
				return fmt.Errorf("%s: %w", st.name, err)
			}
		}
	}
	s.summary(a)
	ok, err := p.Ask("Install Samuori with these settings? (yes/no)", "no")
	if err != nil {
		return err
	}
	if !yes(ok) {
		return errors.New("setup cancelled; nothing was installed")
	}
	return s.Installer.Install(ctx, a, out)
}

// FromFile runs the same checks on the answers file, stops at the first failure and installs without asking.
func (s *Setup) FromFile(ctx context.Context, path string, uid uint32) error {
	a, err := LoadAnswers(path, uid)
	if err != nil {
		return err
	}
	s.addSecrets(a)
	out := s.writer()
	if err := s.applyDefaults(ctx, &a); err != nil {
		return err
	}
	for _, st := range steps {
		if st.skip != nil && st.skip(&a) {
			continue
		}
		if err := st.check(ctx, s, &a); err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	s.summary(a)
	return s.Installer.Install(ctx, a, out)
}

func (s *Setup) applyDefaults(ctx context.Context, a *Answers) error {
	a.Deployment = deployment(a.Deployment)
	if a.Deployment == Cluster {
		a.Controllers = intOr(a.Controllers, 3)
		a.Workers = intOr(a.Workers, 2)
	}
	if a.Host == "" {
		a.Host = s.Host.DefaultHostname(ctx)
	}
	if a.PortalURL == "" && a.Host != "" {
		a.PortalURL = "https://" + a.Host
	}
	if a.Deployment == Cluster {
		d := &a.Database
		d.Port = intOr(d.Port, 5432)
		d.Name = or(d.Name, "samuori")
		d.User = or(d.User, "samuori")
		d.CAFile = or(d.CAFile, DefaultCAFile)
	} else if a.Database != (Database{}) {
		return errors.New("database: a single server uses the built-in SQLite database; remove the database section")
	}
	if a.S3.Bucket != "" && a.S3.Region == "" {
		a.S3.Region = s.Host.AWSRegion(ctx)
	}
	return nil
}

var steps = []step{
	{name: "deployment",
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) (err error) {
			a.Deployment, err = p.Ask("Deployment: cluster or single-server", or(a.Deployment, SingleServer))
			a.Deployment = deployment(a.Deployment)
			return err
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error {
			if a.Deployment != Cluster && a.Deployment != SingleServer {
				return fmt.Errorf("%q: answer cluster or single-server", a.Deployment)
			}
			return nil
		}},
	{name: "planned servers",
		skip: func(a *Answers) bool { return a.Deployment != Cluster },
		ask: func(_ context.Context, s *Setup, p Prompter, a *Answers) error {
			c, err := p.Ask("How many controllers will this cluster have? (3 or 5 survive failures; 1 survives none)", strconv.Itoa(intOr(a.Controllers, 3)))
			if err != nil {
				return err
			}
			if a.Controllers, err = strconv.Atoi(strings.TrimSpace(c)); err != nil {
				return fmt.Errorf("%q is not a number", c)
			}
			if a.Controllers > 1 && a.Controllers%2 == 0 {
				fmt.Fprintf(s.writer(), "  %d controllers survive no more failures than %d: a majority must stay up.\n", a.Controllers, a.Controllers-1)
				v, err := p.Ask(fmt.Sprintf("Plan %d controllers anyway? (yes/no)", a.Controllers), "no")
				if err != nil {
					return err
				}
				if !yes(v) {
					a.Controllers = 0
					return errors.New("choose another number of controllers")
				}
			}
			w, err := p.Ask("How many separate build workers? (2 or more survive a worker failure)", strconv.Itoa(intOr(a.Workers, 2)))
			if err != nil {
				return err
			}
			if a.Workers, err = strconv.Atoi(strings.TrimSpace(w)); err != nil {
				return fmt.Errorf("%q is not a number", w)
			}
			return nil
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error { return a.Plan().Validate() }},
	{name: "dedicated VM",
		skip: func(a *Answers) bool { return a.Deployment != SingleServer },
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) error {
			v, err := p.Ask("A single server builds uploaded source here. Is this a VM that runs nothing but Samuori? (yes/no)", "no")
			a.DedicatedVM = yes(v)
			return err
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error {
			if !a.DedicatedVM {
				return fatal{errors.New("a single server needs a dedicated VM (ADR 0027); use a cluster with separate workers otherwise")}
			}
			return nil
		}},
	{name: "DNS name",
		ask: func(ctx context.Context, s *Setup, p Prompter, a *Answers) (err error) {
			a.Host, err = p.Ask("This host's DNS name (how other servers and workers reach it)", or(a.Host, s.Host.DefaultHostname(ctx)))
			return err
		},
		check: func(ctx context.Context, s *Setup, a *Answers) error {
			if !clusterjoin.ValidHost(a.Host) {
				return fmt.Errorf("%q is not a DNS name or IP address", a.Host)
			}
			if err := s.Host.Resolve(ctx, a.Host); err != nil {
				return fmt.Errorf("%s does not resolve: %w", a.Host, err)
			}
			return nil
		}},
	{name: "portal URL",
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) (err error) {
			a.PortalURL, err = p.Ask("Portal URL (what browsers open)", or(a.PortalURL, "https://"+a.Host))
			return err
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error { return checkPortalURL(a.PortalURL) }},
	{name: "database",
		skip: func(a *Answers) bool { return a.Deployment != Cluster },
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) error {
			d := &a.Database
			var err error
			ask := func(q string, v *string, def string) {
				if err == nil {
					*v, err = p.Ask(q, or(*v, def))
				}
			}
			ask("PostgreSQL host", &d.Host, "")
			port := ""
			ask("PostgreSQL port", &port, strconv.Itoa(intOr(d.Port, 5432)))
			if err == nil {
				if d.Port, err = strconv.Atoi(port); err != nil {
					return fmt.Errorf("port %q is not a number", port)
				}
			}
			ask("Database name", &d.Name, "samuori")
			ask("Database user", &d.User, "samuori")
			if err == nil {
				d.Password, err = p.Secret("Database password")
			}
			ask("CA bundle for the database certificate", &d.CAFile, DefaultCAFile)
			return err
		},
		check: func(ctx context.Context, s *Setup, a *Answers) error {
			d := a.Database
			switch {
			case d.Host == "":
				return errors.New("the PostgreSQL host is required")
			case d.Port < 1 || d.Port > 65535:
				return fmt.Errorf("port %d is out of range", d.Port)
			case d.Password == "":
				return errors.New("the database password is required")
			}
			if err := s.caBundle(ctx, d.CAFile); err != nil {
				return err
			}
			v, err := s.Host.CheckDatabase(ctx, d.DSN())
			if err != nil {
				return err
			}
			fmt.Fprintf(s.writer(), "  database: signed in to %s as %s (%s)\n", d.Host, d.User, v)
			return nil
		}},
	{name: "database replication",
		skip: func(a *Answers) bool { return a.Deployment != Cluster },
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) error {
			v, err := p.Ask("Is this database replicated across zones, such as RDS Multi-AZ? Samuori cannot check this. (yes/no)", map[bool]string{true: "yes", false: "no"}[a.Database.Replicated])
			a.Database.Replicated = yes(v)
			return err
		},
		check: func(context.Context, *Setup, *Answers) error { return nil }},
	{name: "S3",
		ask: func(ctx context.Context, s *Setup, p Prompter, a *Answers) error {
			q := "S3 bucket"
			if a.Deployment == SingleServer {
				q = "S3 bucket (blank keeps blobs on this host)"
			}
			var err error
			if a.S3.Bucket, err = p.Ask(q, a.S3.Bucket); err != nil || a.S3.Bucket == "" {
				return err
			}
			if a.S3.Region, err = p.Ask("S3 region", or(a.S3.Region, s.Host.AWSRegion(ctx))); err != nil {
				return err
			}
			a.S3.Prefix, err = p.Ask("Key prefix in the bucket (blank for none)", a.S3.Prefix)
			return err
		},
		check: func(ctx context.Context, s *Setup, a *Answers) error {
			switch {
			case a.S3.Bucket == "" && a.Deployment == Cluster:
				return errors.New("a cluster keeps shared blobs in S3: the bucket is required")
			case a.S3.Bucket == "":
				return nil
			case a.S3.Region == "":
				return errors.New("the region is required")
			}
			if err := s.Host.CheckStorage(ctx, a.S3); err != nil {
				return err
			}
			fmt.Fprintf(s.writer(), "  S3: wrote, read and deleted a probe object in %s\n", a.S3.Bucket)
			return nil
		}},
	{name: "organization",
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) (err error) {
			a.Organization, err = p.Ask("Organization name", a.Organization)
			return err
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error {
			return plainText("the organization name", a.Organization, 200)
		}},
	{name: "administrator",
		ask: func(_ context.Context, _ *Setup, p Prompter, a *Answers) error {
			var err error
			if a.Admin.Username, err = p.Ask("Administrator username", or(a.Admin.Username, "admin")); err != nil {
				return err
			}
			if a.Admin.Email, err = p.Ask("Administrator email", a.Admin.Email); err != nil {
				return err
			}
			pw, err := p.Secret("Administrator password (12 characters or more)")
			if err != nil {
				return err
			}
			again, err := p.Secret("Administrator password again")
			if err != nil {
				return err
			}
			a.Admin.Password = pw
			if pw != again {
				a.Admin.Password = ""
				return errors.New("the passwords do not match")
			}
			return nil
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error {
			if err := plainText("the username", a.Admin.Username, 64); err != nil {
				return err
			}
			if strings.ContainsFunc(a.Admin.Username, unicode.IsSpace) {
				return errors.New("the username must not contain spaces")
			}
			if m, err := mail.ParseAddress(a.Admin.Email); err != nil || m.Address != a.Admin.Email {
				return fmt.Errorf("%q is not an email address", a.Admin.Email)
			}
			return auth.CheckPasswordPolicy(a.Admin.Username, a.Admin.Password)
		}},
	{name: "release public key",
		ask: func(_ context.Context, s *Setup, p Prompter, a *Answers) (err error) {
			def := a.ReleaseSigners
			if def == "" && s.ReleaseDir != "" {
				if _, err := os.Stat(filepath.Join(s.ReleaseDir, "allowed_signers")); err == nil {
					def = filepath.Join(s.ReleaseDir, "allowed_signers")
				}
			}
			a.ReleaseSigners, err = p.Ask("Release public key, an allowed_signers file (blank: no updates from the portal)", def)
			return err
		},
		check: func(_ context.Context, _ *Setup, a *Answers) error {
			if a.ReleaseSigners == "" {
				return nil
			}
			return checkSigners(a.ReleaseSigners)
		}},
}

func (s *Setup) caBundle(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("the CA bundle path %q must be absolute", path)
	}
	if raw, err := os.ReadFile(path); err == nil {
		if !bytes.Contains(raw, []byte("BEGIN CERTIFICATE")) {
			return fmt.Errorf("%s holds no PEM certificate", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !s.Host.OnAWS(ctx) {
		return fmt.Errorf("%s does not exist; place the CA bundle that signs the database certificate there", path)
	}
	if err := s.Host.DownloadRDSBundle(ctx, path); err != nil {
		return fmt.Errorf("downloading the AWS RDS CA bundle to %s: %w", path, err)
	}
	fmt.Fprintf(s.writer(), "  database: downloaded the AWS RDS global CA bundle to %s\n", path)
	return nil
}

func checkPortalURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
		return fmt.Errorf("%q must be a scheme and host, such as https://samuori.example.com", raw)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "localhost" || strings.HasPrefix(u.Hostname(), "127.") || u.Hostname() == "::1"):
		return nil
	}
	return fmt.Errorf("%q must use https (http only on loopback)", raw)
}

// checkSigners accepts an allowed_signers file with at least one key for the release identity.
func checkSigners(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		principals, rest, _ := strings.Cut(line, " ")
		if !matchesRelease(principals) {
			continue
		}
		rest = strings.TrimSpace(rest)
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest)); err == nil {
			return nil
		}
		if _, opts, ok := strings.Cut(rest, " "); ok {
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(opts)); err == nil {
				return nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s has no public key for %s", path, releasePrincipal)
}

func matchesRelease(principals string) bool {
	for _, p := range strings.Split(principals, ",") {
		if p == releasePrincipal || p == "*" {
			return true
		}
	}
	return false
}

func (s *Setup) summary(a Answers) {
	out := s.writer()
	fmt.Fprintln(out, "\nSummary")
	row := func(k, v string) { fmt.Fprintf(out, "  %-20s %s\n", k, v) }
	row("deployment", a.Deployment)
	row("DNS name", a.Host)
	row("portal URL", a.PortalURL)
	if a.Deployment == Cluster {
		d := a.Database
		row("database", fmt.Sprintf("postgres://%s@%s:%d/%s (password set, CA %s)", d.User, d.Host, d.Port, d.Name, d.CAFile))
	} else {
		row("database", "built-in SQLite")
	}
	if a.S3.Bucket != "" {
		where := "s3://" + a.S3.Bucket
		if a.S3.Prefix != "" {
			where += "/" + strings.Trim(a.S3.Prefix, "/")
		}
		row("S3", where+" in "+a.S3.Region)
	} else {
		row("S3", "none: blobs stay on this host")
	}
	row("organization", a.Organization)
	row("administrator", fmt.Sprintf("%s <%s> (password set)", a.Admin.Username, a.Admin.Email))
	row("release public key", or(a.ReleaseSigners, "none: no updates from the portal"))
	plan := a.Plan()
	row("plan", fmt.Sprintf("%d controller%s, %d worker%s", plan.Controllers, plural(plan.Controllers), plan.Workers, plural(plan.Workers)))
	as := a.Assessment()
	row("resilience", map[topology.Tier]string{topology.Standalone: "standalone: any failure stops Samuori", topology.PartialHA: "partial HA", topology.HA: "highly available"}[as.Tier])
	for _, c := range as.Components {
		state := fmt.Sprintf("survives %d failure%s", c.Tolerates, plural(c.Tolerates))
		if c.Declared {
			state += " (declared)"
		}
		if c.Gap != "" {
			state += "; for HA: " + c.Gap
		}
		row("  "+c.Name, c.Have+", "+state)
	}
	for _, w := range as.Warnings {
		row("  warning", w)
	}
	fmt.Fprintln(out)
}

func (s *Setup) addSecrets(a Answers) {
	if s.mask == nil {
		s.mask = redact.New()
	}
	for _, v := range a.secrets() {
		if v == "" {
			continue
		}
		s.mask.Add("password", []byte(v))
		s.mask.Add("password", []byte(url.QueryEscape(v)))
		s.mask.Add("password", []byte(url.PathEscape(v)))
		s.mask.Add("password", []byte(url.UserPassword("", v).String()[1:]))
	}
}

func (s *Setup) writer() io.Writer { return s.MaskedWriter(s.Out) }

type maskWriter struct {
	w io.Writer
	m *redact.Masker
}

func (m maskWriter) Write(b []byte) (int, error) {
	if _, err := m.w.Write(m.m.Mask(b)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// MaskedWriter replaces every password given so far, so no check error or installer line can print one. Masking
// works per write: a child's output is passed through whole lines.
func (s *Setup) MaskedWriter(w io.Writer) io.Writer {
	if s.mask == nil {
		s.mask = redact.New()
	}
	return maskWriter{w, s.mask}
}

func plainText(what, v string, max int) error {
	switch {
	case strings.TrimSpace(v) == "":
		return fmt.Errorf("%s is required", what)
	case len(v) > max:
		return fmt.Errorf("%s is longer than %d characters", what, max)
	case strings.ContainsFunc(v, unicode.IsControl):
		return fmt.Errorf("%s must not contain control characters", what)
	}
	return nil
}

func deployment(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "single", "single server", "single-server", "server":
		return SingleServer
	case "cluster":
		return Cluster
	}
	return strings.TrimSpace(v)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func yes(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "y" || v == "yes"
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func intOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
