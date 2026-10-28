package setupcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const (
	dbPassword    = "db p@ss/w:rd"
	adminPassword = "correct horse battery"
)

type script struct {
	answers []string
	asked   []string
}

func (s *script) next(q string) (string, error) {
	s.asked = append(s.asked, q)
	if len(s.answers) == 0 {
		return "", io.EOF
	}
	v := s.answers[0]
	s.answers = s.answers[1:]
	return v, nil
}

func (s *script) Ask(q, def string) (string, error) {
	v, err := s.next(q)
	if v == "" {
		v = def
	}
	return v, err
}

func (s *script) Secret(q string) (string, error) { return s.next("secret: " + q) }

type fakeHost struct {
	dbErrs, s3Errs []error
	dbCalls        int
	s3Calls        int
	ca             string
}

func (h *fakeHost) DefaultHostname(context.Context) string    { return "ctrl1.example.test" }
func (h *fakeHost) AWSRegion(context.Context) string          { return "eu-west-1" }
func (h *fakeHost) OnAWS(context.Context) bool                { return false }
func (h *fakeHost) Resolve(_ context.Context, n string) error { return nil }
func (h *fakeHost) DownloadRDSBundle(context.Context, string) error {
	return errors.New("not on AWS")
}

func (h *fakeHost) CheckDatabase(_ context.Context, dsn string) (string, error) {
	h.dbCalls++
	if len(h.dbErrs) > 0 {
		err := h.dbErrs[0]
		h.dbErrs = h.dbErrs[1:]
		if err != nil {
			return "", fmt.Errorf("%w (dsn %s)", err, dsn)
		}
	}
	return "PostgreSQL 17.2", nil
}

func (h *fakeHost) CheckStorage(context.Context, S3) error {
	h.s3Calls++
	if len(h.s3Errs) > 0 {
		err := h.s3Errs[0]
		h.s3Errs = h.s3Errs[1:]
		return err
	}
	return nil
}

type fakeInstaller struct {
	got   *Answers
	print string
}

func (f *fakeInstaller) Install(_ context.Context, a Answers, out io.Writer) error {
	f.got = &a
	fmt.Fprintln(out, f.print)
	return nil
}

func caFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "rds-ca.pem")
	if err := os.WriteFile(p, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func signersFile(t *testing.T, principal string) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(p, []byte(principal+" "+string(ssh.MarshalAuthorizedKey(k))), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func clusterAnswers(ca, signers string) []string {
	return []string{"cluster", "", "", "", "", "db.example.test", "", "", "", dbPassword, ca, "yes",
		"samuori-blobs", "", "", "Example Org", "", "admin@example.test", adminPassword, adminPassword, signers}
}

func TestInteractiveReasksAfterAFailedCheck(t *testing.T) {
	ca, signers := caFile(t), signersFile(t, releasePrincipal)
	in := clusterAnswers(ca, signers)
	// The first database answer fails sign-in; the second round repeats it with the right password.
	retry := []string{"db.example.test", "", "", "", dbPassword, ca}
	in = append(in[:11], append(retry, in[11:]...)...)
	in = append(in, "yes")
	host := &fakeHost{dbErrs: []error{errors.New("password authentication failed"), nil}}
	inst := &fakeInstaller{}
	var out bytes.Buffer
	s := &Setup{Host: host, Installer: inst, Out: &out}
	p := &script{answers: in}
	if err := s.Interactive(context.Background(), p); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if host.dbCalls != 2 {
		t.Fatalf("the database was checked %d times, want 2", host.dbCalls)
	}
	if !strings.Contains(out.String(), "password authentication failed") {
		t.Fatalf("the failed check was not shown:\n%s", out.String())
	}
	if inst.got == nil || inst.got.Database.Password != dbPassword || inst.got.S3.Region != "eu-west-1" || inst.got.Host != "ctrl1.example.test" {
		t.Fatalf("installer got %+v", inst.got)
	}
}

func TestInteractiveInstallsNothingWithoutConfirmation(t *testing.T) {
	ca, signers := caFile(t), signersFile(t, releasePrincipal)
	inst := &fakeInstaller{}
	s := &Setup{Host: &fakeHost{}, Installer: inst, Out: io.Discard}
	err := s.Interactive(context.Background(), &script{answers: append(clusterAnswers(ca, signers), "no")})
	if err == nil || inst.got != nil {
		t.Fatalf("installed without confirmation: %v", err)
	}
}

func TestInteractiveRefusesASharedHostForASingleServer(t *testing.T) {
	inst := &fakeInstaller{}
	s := &Setup{Host: &fakeHost{}, Installer: inst, Out: io.Discard}
	p := &script{answers: []string{"single-server", "no", "no", "no"}}
	if err := s.Interactive(context.Background(), p); err == nil || inst.got != nil {
		t.Fatalf("a non-dedicated host was accepted: %v", err)
	}
	if len(p.asked) != 2 {
		t.Fatalf("a fatal answer was re-asked: %v", p.asked)
	}
}

func writeAnswers(t *testing.T, body string, mode os.FileMode) string {
	p := filepath.Join(t.TempDir(), "answers.yml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func answersYML(ca, signers string) string {
	return fmt.Sprintf(`deployment: cluster
host: ctrl1.example.test
portalUrl: https://samuori.example.test
database:
  host: db.example.test
  password: %q
  caFile: %s
s3:
  bucket: samuori-blobs
  prefix: prod
organization: Example Org
admin:
  username: admin
  email: admin@example.test
  password: %q
releaseSigners: %s
`, dbPassword, ca, adminPassword, signers)
}

func TestLoadAnswers(t *testing.T) {
	uid := uint32(os.Getuid())
	body := answersYML("/etc/samuori/rds-ca.pem", "/etc/samuori/allowed_signers")
	a, err := LoadAnswers(writeAnswers(t, body, 0o600), uid)
	if err != nil {
		t.Fatal(err)
	}
	if a.Deployment != Cluster || a.Database.Host != "db.example.test" || a.Database.Password != dbPassword ||
		a.S3.Prefix != "prod" || a.Admin.Email != "admin@example.test" || a.ReleaseSigners != "/etc/samuori/allowed_signers" {
		t.Fatalf("parsed %+v", a)
	}
	for name, tc := range map[string]struct {
		body string
		mode os.FileMode
		uid  uint32
		want string
	}{
		"group readable": {body, 0o640, uid, "0600"},
		"world readable": {body, 0o604, uid, "0600"},
		"another owner":  {body, 0o600, uid + 1, "owned by"},
		"unknown field":  {body + "adminPassword: x\n", 0o600, uid, "adminPassword"},
		"not yaml":       {"deployment: [", 0o600, uid, "yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadAnswers(writeAnswers(t, tc.body, tc.mode), tc.uid)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), dbPassword) {
				t.Fatalf("the error shows the password: %v", err)
			}
		})
	}
	link := filepath.Join(t.TempDir(), "link.yml")
	if err := os.Symlink(writeAnswers(t, body, 0o600), link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAnswers(link, uid); err == nil {
		t.Fatal("a symbolic link was followed")
	}
}

func TestFromFileStopsAtTheFirstFailedCheck(t *testing.T) {
	ca := caFile(t)
	host := &fakeHost{s3Errs: []error{errors.New("AccessDenied")}}
	inst := &fakeInstaller{}
	s := &Setup{Host: host, Installer: inst, Out: io.Discard}
	// The signers file does not exist, but the S3 failure comes first and ends the run.
	err := s.FromFile(context.Background(), writeAnswers(t, answersYML(ca, "/nonexistent/allowed_signers"), 0o600), uint32(os.Getuid()))
	if err == nil || !strings.Contains(err.Error(), "S3: AccessDenied") {
		t.Fatalf("got %v", err)
	}
	if inst.got != nil || host.s3Calls != 1 || host.dbCalls != 1 {
		t.Fatalf("installed %v, s3 checks %d, database checks %d", inst.got != nil, host.s3Calls, host.dbCalls)
	}
}

func TestFromFileInstallsWithDefaults(t *testing.T) {
	ca, signers := caFile(t), signersFile(t, releasePrincipal)
	inst := &fakeInstaller{}
	s := &Setup{Host: &fakeHost{}, Installer: inst, Out: io.Discard}
	if err := s.FromFile(context.Background(), writeAnswers(t, answersYML(ca, signers), 0o600), uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	d := inst.got.Database
	if d.Port != 5432 || d.Name != "samuori" || d.User != "samuori" || inst.got.S3.Region != "eu-west-1" {
		t.Fatalf("defaults not applied: %+v", inst.got)
	}
	dsn, _ := url.Parse(d.DSN())
	if pw, _ := dsn.User.Password(); pw != dbPassword || dsn.Query().Get("sslmode") != "verify-full" || dsn.Query().Get("sslrootcert") != ca {
		t.Fatalf("DSN %s", d.DSN())
	}
}

func TestSecretsNeverReachTheOutput(t *testing.T) {
	ca, signers := caFile(t), signersFile(t, releasePrincipal)
	host := &fakeHost{dbErrs: []error{errors.New("refused"), nil}}
	in := clusterAnswers(ca, signers)
	in = append(in[:11], append([]string{"", "", "", "", dbPassword, ""}, in[11:]...)...)
	in = append(in, "yes")
	var out bytes.Buffer
	inst := &fakeInstaller{print: "installer echoed " + dbPassword + " and " + adminPassword + " and " + url.QueryEscape(dbPassword)}
	s := &Setup{Host: host, Installer: inst, Out: &out}
	if err := s.Interactive(context.Background(), &script{answers: in}); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{dbPassword, adminPassword, url.QueryEscape(dbPassword), url.PathEscape(dbPassword)} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("output shows %q:\n%s", secret, out.String())
		}
	}
	if !strings.Contains(out.String(), "password set") || !strings.Contains(out.String(), "refused") {
		t.Fatalf("summary or check error missing:\n%s", out.String())
	}
}

func TestChecks(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://samuori.example.test": true, "https://samuori.example.test/": true, "http://127.0.0.1:8080": true,
		"http://samuori.example.test": false, "https://samuori.example.test/portal": false, "samuori.example.test": false,
		"https://user@samuori.example.test": false, "https://samuori.example.test?x=1": false,
	} {
		if err := checkPortalURL(raw); (err == nil) != ok {
			t.Errorf("portal URL %q: %v", raw, err)
		}
	}
	if err := checkSigners(signersFile(t, releasePrincipal)); err != nil {
		t.Error(err)
	}
	if err := checkSigners(signersFile(t, "someone@example.test")); err == nil {
		t.Error("a key for another identity was accepted")
	}
	s := &Setup{Host: &fakeHost{}, Out: io.Discard}
	if err := s.caBundle(context.Background(), filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("a missing CA bundle off AWS was accepted")
	}
}

func TestPlanWarnsOnEvenControllersAndShowsTheTier(t *testing.T) {
	ca, signers := caFile(t), signersFile(t, releasePrincipal)
	in := clusterAnswers(ca, signers)
	// Four controllers, declined, then three; the summary is declined so nothing installs.
	in = append([]string{in[0], "4", "no", "3", "2"}, in[3:]...)
	in = append(in, "no")
	var out bytes.Buffer
	s := &Setup{Host: &fakeHost{}, Installer: &fakeInstaller{}, Out: &out}
	_ = s.Interactive(context.Background(), &script{answers: in})
	got := out.String()
	for _, want := range []string{
		"4 controllers survive no more failures than 3",
		"plan                 3 controllers, 2 workers",
		"controllers        3 controllers, survives 1 failure",
		"database           PostgreSQL, replicated, survives 1 failure (declared)",
		"entry point        the portal URL names a controller, survives 0 failures; for HA: put a load balancer",
		"resilience           partial HA",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
