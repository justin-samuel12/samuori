// Package workeragent is the worker side of the worker protocol: enrollment with a pinned controller CA, then an
// outbound mTLS stream. Workers have no database access and no inbound ports.
package workeragent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/hostcap"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/telemetry"
	"samuori.invalid/samuori/internal/updatestage"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

type Config struct {
	// Controller is the worker endpoint, or several comma-separated endpoints of controllers sharing one cluster;
	// the agent moves to the next one when a session fails (ADR 0034).
	Controller string
	ServerName string
	StateDir   string
	CAPin      string
	TokenFile  string
	Name       string
	Version    string
	// EnvironmentImage is the digest-pinned image providers boot for each build.
	EnvironmentImage string
	// UpdateDir is where releases are staged for the host's samuori-self-update (ADR 0044); empty uses updatestage.Dir.
	UpdateDir string
	// ProvidersDir holds approved conversion tools (convertjob.ProvidersDir when empty).
	ProvidersDir string
}

type Agent struct {
	cfg      Config
	log      *slog.Logger
	host     isolation.Host
	provider isolation.Provider
	manager  *isolation.Manager
	slots    int

	mu          sync.Mutex
	leases      map[string]running
	selfTesting bool
	negotiated  uint32
	renewing    bool
	endpointAt  int
	conn        *grpc.ClientConn
	// client follows the current session's connection, so an attempt that outlives a reconnect, for example to
	// another controller of a cluster, keeps reporting on the new one.
	client workerv1.WorkerServiceClient

	// now replaces the clock in tests of clock skew and certificate lifetimes.
	now func() time.Time
}

type running struct {
	lease      *workerv1.LeaseRef
	cancel     context.CancelFunc
	conversion bool
}

func New(cfg Config, log *slog.Logger) *Agent {
	a := &Agent{cfg: cfg, log: log, host: isolation.DetectHost(), leases: map[string]running{}}
	a.client = workerv1.NewWorkerServiceClient(sessionConn{a})
	return a
}

// sessionConn sends each call on the connection of the session open at that moment.
type sessionConn struct{ a *Agent }

func (c sessionConn) current() (*grpc.ClientConn, error) {
	c.a.mu.Lock()
	defer c.a.mu.Unlock()
	if c.a.conn == nil {
		return nil, status.Error(codes.Unavailable, "no session with a controller")
	}
	return c.a.conn, nil
}

func (c sessionConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	conn, err := c.current()
	if err != nil {
		return err
	}
	return conn.Invoke(ctx, method, args, reply, opts...)
}

func (c sessionConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	conn, err := c.current()
	if err != nil {
		return nil, err
	}
	return conn.NewStream(ctx, desc, method, opts...)
}

// UseProvider attaches the isolation provider whose capabilities the worker advertises and through which it builds.
func (a *Agent) UseProvider(p isolation.Provider) {
	a.provider = p
	if p != nil {
		slots := 1
		if c, err := p.Capabilities(context.Background()); err == nil && c.FreeSlots > 0 {
			slots = int(c.FreeSlots)
		}
		a.manager, a.slots = isolation.NewManager(p, slots), slots
	}
}

func (a *Agent) Host() isolation.Host { return a.host }

func (a *Agent) path(name string) string { return filepath.Join(a.cfg.StateDir, name) }

// Capabilities reports only what this worker can actually do: isolation profiles come from a configured provider,
// never from host detection alone, and engines arrive with the BuildKit adapter (WP14).
func (a *Agent) Capabilities(ctx context.Context) *workerv1.Capabilities {
	c := &workerv1.Capabilities{Os: runtime.GOOS, Architectures: []string{runtime.GOARCH}, Engines: []string{}, IsolationProfiles: []string{}, NestedVirtualization: a.host.NestedVirt}
	if a.provider == nil {
		return c
	}
	pc, err := a.provider.Capabilities(ctx)
	if err != nil {
		a.log.Warn("isolation provider unavailable", "provider", a.provider.Name(), "error", err)
		return c
	}
	if pc.OS != "" {
		c.Os, c.Architectures = pc.OS, pc.Architectures
	}
	for _, p := range pc.Profiles {
		c.IsolationProfiles = append(c.IsolationProfiles, string(p))
	}
	c.FreeDaemonSlots = pc.FreeSlots
	c.StrictEgress = pc.StrictEgress
	c.Engines = append(c.Engines, pc.Engines...)
	c.ConfigDigest = configDigest(a.cfg.Version, a.provider.Name(), pc, c)
	c.Providers = reportedProviders(pc.Providers, a.convertReady(), a.providersDir())
	return c
}

// configDigest identifies what a self-test verified: worker version, provider configuration and capabilities.
// Free slots are left out because they change with load.
func configDigest(version, provider string, pc isolation.Capabilities, c *workerv1.Capabilities) string {
	raw, _ := json.Marshal(struct {
		Version, Provider, ProviderConfig, OS string
		Arch, Engines, Profiles               []string
		Strict                                bool
	}{version, provider, pc.ConfigDigest, c.GetOs(), c.GetArchitectures(), c.GetEngines(), c.GetIsolationProfiles(), c.GetStrictEgress()})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func protocol() *workerv1.ProtocolVersion { return &workerv1.ProtocolVersion{Major: 1, Minor: 7} }

// Enrolled reports whether the state directory holds an identity.
func (a *Agent) Enrolled() bool {
	_, err := os.Stat(a.path("worker.crt"))
	return err == nil
}

// Enroll exchanges the one-time token for a certificate. The controller must chain to the pinned CA.
func (a *Agent) Enroll(ctx context.Context) (string, error) {
	if a.cfg.CAPin == "" {
		return "", errors.New("the controller CA pin is required for enrollment")
	}
	token, err := readPrivate(a.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("enrollment token: %w", err)
	}
	if err := os.MkdirAll(a.cfg.StateDir, 0o700); err != nil {
		return "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return "", err
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13, ServerName: a.cfg.ServerName,
		// Hostname and chain are checked against the pinned CA in VerifyPeerCertificate; system roots are never used.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pki.VerifyPinned(a.cfg.CAPin, a.cfg.ServerName),
	}
	conn, err := grpc.NewClient(a.endpoint(), grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := workerv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &workerv1.EnrollRequest{
		EnrollmentToken: token, CsrDer: csr, Capabilities: a.Capabilities(ctx), Protocol: protocol(), WorkerVersion: a.cfg.Version, WorkerName: a.cfg.Name,
		PreviousWorkerId: previousID(a),
	})
	if err != nil {
		return "", a.diagnose(err)
	}
	if !bundleHasPin(resp.GetTrustBundle(), a.cfg.CAPin) {
		return "", errors.New("the trust bundle does not contain the pinned CA")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"worker.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		{"worker.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.GetCertificateDer()}), 0o644},
		{"worker-id", []byte(resp.GetWorkerId() + "\n"), 0o644},
	}
	_ = os.Remove(a.path("ca-bundle.pem"))
	_ = os.Remove(a.path("worker.key.new"))
	if err := a.storeBundle(resp.GetTrustBundle()); err != nil {
		return "", err
	}
	for _, f := range files {
		if err := writeAtomic(a.path(f.name), f.data, f.mode); err != nil {
			return "", err
		}
	}
	if err := os.Remove(a.cfg.TokenFile); err != nil {
		a.log.Warn("could not delete the used enrollment token file", "path", a.cfg.TokenFile, "error", err)
	}
	return resp.GetWorkerId(), nil
}

func (a *Agent) mtls() (credentials.TransportCredentials, error) {
	cfg, err := a.tlsConfig()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

func (a *Agent) tlsConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(a.path("worker.crt"), a.path("worker.key"))
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(a.path("ca-bundle.pem"))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		return nil, errors.New("empty trust bundle")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ServerName: a.cfg.ServerName, RootCAs: roots, Certificates: []tls.Certificate{cert}, Time: a.clock}, nil
}

// Session runs one Connect stream until it fails or ctx ends; Run reconnects.
func (a *Agent) Session(ctx context.Context) error {
	a.recoverKey()
	if err := a.checkIdentity(); err != nil {
		return err
	}
	creds, err := a.mtls()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(a.endpoint(), grpc.WithTransportCredentials(creds), grpc.WithKeepaliveParams(Keepalive))
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := workerv1.NewWorkerServiceClient(conn).Connect(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&workerv1.WorkerMessage{Kind: &workerv1.WorkerMessage_Hello{Hello: &workerv1.Hello{Protocol: protocol(), WorkerVersion: a.cfg.Version, Capabilities: a.Capabilities(ctx), TrustBundleVersion: a.bundleVersion()}}}); err != nil {
		return a.diagnose(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		return a.diagnose(err)
	}
	if err := a.storeBundle(ack.GetHelloAck().GetTrustBundle()); err != nil {
		return err
	}
	a.checkSkew(ack.GetHelloAck().GetControllerTime().AsTime())
	interval := ack.GetHelloAck().GetHeartbeatInterval().AsDuration()
	if interval <= 0 {
		return errors.New("controller did not acknowledge Hello")
	}
	for _, l := range ack.GetHelloAck().GetAbandonedLeases() {
		a.stop(l.GetAttemptId())
	}
	a.mu.Lock()
	a.conn = conn
	a.negotiated = ack.GetHelloAck().GetNegotiated().GetMinor()
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		a.mu.Unlock()
	}()
	client := a.client
	var sendMu sync.Mutex
	send := func(m *workerv1.WorkerMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}
	a.log.Info("connected to controller", "controller", a.endpoint(), "heartbeat", interval.String())
	go a.reportUpdate(ctx, client)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			switch {
			case msg.GetDrain() != nil:
				a.log.Info("drain requested", "reason", msg.GetDrain().GetReason())
			case msg.GetCancel() != nil:
				a.stop(msg.GetCancel().GetLease().GetAttemptId())
			case msg.GetAssignment() != nil:
				a.assigned(ctx, client, send, msg.GetAssignment())
			case msg.GetConversion() != nil:
				a.convertAssigned(ctx, client, send, msg.GetConversion())
			case msg.GetSelfTest() != nil:
				a.selfTest(ctx, client, msg.GetSelfTest())
			case msg.GetUpdate().GetRollback():
				go a.rollback(ctx, client)
			case msg.GetUpdate() != nil:
				go a.update(ctx, client, msg.GetUpdate().GetVersion())
			case msg.GetRemove() != nil:
				go a.remove(ctx, client)
			case msg.GetTrustBundle() != nil:
				if err := a.storeBundle(msg.GetTrustBundle()); err != nil {
					a.log.Error("trust bundle update", "error", err)
				}
			}
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			return ctx.Err()
		case err := <-recvErr:
			return err
		case <-t.C:
			if err := send(&workerv1.WorkerMessage{Kind: &workerv1.WorkerMessage_Heartbeat{Heartbeat: a.heartbeat()}}); err != nil {
				return err
			}
			a.maybeRenew(ctx, client)
		}
	}
}

func (a *Agent) heartbeat() *workerv1.Heartbeat {
	a.mu.Lock()
	defer a.mu.Unlock()
	hb := &workerv1.Heartbeat{TrustBundleVersion: a.bundleVersion(), HostArch: runtime.GOARCH}
	if free, err := updatestage.FreeBytes(a.updateDir()); err == nil {
		hb.UpdateFreeBytes = free
	}
	hb.Capacity = capacityMessage(hostcap.Measure([]hostcap.Dir{
		{Label: "Builds and worker state", Path: a.cfg.StateDir},
		{Label: "Update staging", Path: a.updateDir()},
		{Label: "System", Path: "/"},
	}))
	for _, r := range a.leases {
		if r.conversion {
			hb.ActiveConversionLeases = append(hb.ActiveConversionLeases, r.lease)
		} else {
			hb.ActiveLeases = append(hb.ActiveLeases, r.lease)
		}
	}
	if a.manager != nil && a.manager.Quarantined() == "" && !a.selfTesting {
		hb.FreeDaemonSlots = uint32(max(a.slots-len(a.leases), 0))
	}
	return hb
}

func (a *Agent) stop(attemptID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.leases[attemptID]; ok {
		r.cancel()
	}
}

func (a *Agent) assigned(ctx context.Context, client workerv1.WorkerServiceClient, send func(*workerv1.WorkerMessage) error, as *workerv1.Assignment) {
	reject := func(reason workerv1.RejectionReason, detail string) {
		_ = send(&workerv1.WorkerMessage{Kind: &workerv1.WorkerMessage_AssignmentRejection{AssignmentRejection: &workerv1.AssignmentRejection{Lease: as.GetLease(), Reason: reason, Detail: detail}}})
	}
	if a.manager == nil {
		reject(workerv1.RejectionReason_REJECTION_REASON_PROFILE_UNVERIFIED, "this worker has no isolation provider")
		return
	}
	a.mu.Lock()
	_, dup := a.leases[as.GetLease().GetAttemptId()]
	if !dup && (len(a.leases) >= a.slots || a.manager.Quarantined() != "" || a.selfTesting) {
		a.mu.Unlock()
		reject(workerv1.RejectionReason_REJECTION_REASON_DAEMON_OCCUPIED, "every daemon slot is busy")
		return
	}
	if dup {
		a.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.leases[as.GetLease().GetAttemptId()] = running{lease: as.GetLease(), cancel: cancel}
	a.mu.Unlock()
	_ = send(&workerv1.WorkerMessage{Kind: &workerv1.WorkerMessage_AssignmentAck{AssignmentAck: &workerv1.AssignmentAck{Lease: as.GetLease()}}})
	go func() {
		defer func() {
			cancel()
			a.mu.Lock()
			delete(a.leases, as.GetLease().GetAttemptId())
			a.mu.Unlock()
		}()
		a.execute(runCtx, client, as)
	}()
}

// execute verifies the plan against the digest bound to the lease before anything runs (SEC-PLN-02).
func (a *Agent) execute(ctx context.Context, client workerv1.WorkerServiceClient, as *workerv1.Assignment) {
	// The attempt continues the controller's trace. Spans carry IDs and outcome classes only, never error text, which
	// can hold build output.
	ctx, span := telemetry.Start(telemetry.WithTraceParent(ctx, as.GetTraceParent()), "worker.attempt",
		telemetry.String("samuori.build.id", as.GetBuildRunId()), telemetry.String("samuori.attempt.id", as.GetLease().GetAttemptId()))
	final := "abandoned"
	defer func() {
		span.Set(telemetry.String("samuori.outcome", final))
		var err error
		if final != "completed" {
			err = errors.New(final)
		}
		span.End(err)
	}()
	step := func(name string, run func(context.Context) error) error {
		sctx, s := telemetry.Start(ctx, name)
		err := run(sctx)
		var class error
		if err != nil {
			class = errors.New("failed")
		}
		s.End(class)
		return err
	}
	var checks []*workerv1.CheckReport
	var egressDenied []*workerv1.EgressDenial
	var egressUnrecorded uint32
	report := func(st workerv1.AttemptState, class, detail, digest string, cleanup workerv1.CleanupOutcome) {
		switch st {
		case workerv1.AttemptState_ATTEMPT_STATE_COMPLETED:
			final = "completed"
		case workerv1.AttemptState_ATTEMPT_STATE_FAILED:
			final = "failed-" + class
		}
		r := &workerv1.AttemptReport{Lease: as.GetLease(), State: st, FailureClass: class, Detail: detail, ArtifactDigest: digest, Cleanup: cleanup}
		if st == workerv1.AttemptState_ATTEMPT_STATE_COMPLETED {
			r.Checks = checks
		}
		if st == workerv1.AttemptState_ATTEMPT_STATE_COMPLETED || st == workerv1.AttemptState_ATTEMPT_STATE_FAILED {
			r.EgressDenied, r.EgressUnrecorded = egressDenied, egressUnrecorded
		}
		ack, err := reportUntilAnswered(client, r)
		if err != nil || !ack.GetAccepted() {
			a.log.Warn("attempt report not accepted", "attempt", as.GetLease().GetAttemptId(), "error", err, "rejection", ack.GetRejection())
		}
	}
	plan, err := client.GetPlan(ctx, &workerv1.GetPlanRequest{Lease: as.GetLease()})
	if err != nil {
		a.log.Warn("plan refused", "attempt", as.GetLease().GetAttemptId(), "error", err)
		return
	}
	if plan.GetPlanDigest() != as.GetPlanDigest() || planDigest(plan.GetPlanJson()) != as.GetPlanDigest() {
		report(workerv1.AttemptState_ATTEMPT_STATE_FAILED, "infrastructure", "the plan does not match the digest bound to the lease", "", workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED)
		return
	}
	report(workerv1.AttemptState_ATTEMPT_STATE_PREPARING, "", "", "", workerv1.CleanupOutcome_CLEANUP_OUTCOME_PENDING)
	var p struct {
		SnapshotDigest string `json:"snapshotDigest"`
		Cache          struct {
			Mode  string `json:"mode"`
			Scope string `json:"scope"`
		} `json:"cache"`
	}
	_ = json.Unmarshal(plan.GetPlanJson(), &p)
	workDir, err := os.MkdirTemp(a.cfg.StateDir, "attempt-")
	if err != nil {
		report(workerv1.AttemptState_ATTEMPT_STATE_FAILED, "infrastructure", "could not create a work directory", "", workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED)
		return
	}
	defer os.RemoveAll(workDir)
	srcDir := filepath.Join(workDir, "source")
	if err := step("worker.fetch-source", func(ctx context.Context) error {
		return fetchSource(ctx, client, as.GetLease(), p.SnapshotDigest, srcDir)
	}); err != nil {
		report(workerv1.AttemptState_ATTEMPT_STATE_FAILED, "infrastructure", "source fetch failed: "+err.Error(), "", workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED)
		return
	}
	lim := as.GetLimits()
	spec := isolation.Spec{AttemptID: as.GetLease().GetAttemptId(), SourceDir: srcDir, OutputDir: workDir, Profile: isolation.Profile(as.GetIsolationProfile()), BaseImage: a.cfg.EnvironmentImage, SecretBearing: as.GetSecretBearing(),
		Limits: isolation.Limits{VCPUs: lim.GetVcpus(), MemoryBytes: lim.GetMemoryBytes(), DiskBytes: lim.GetDiskBytes(), WallClock: lim.GetWallClock().AsDuration()}}
	spec.ReleaseSecret = releaser(client, as.GetLease())
	spec.MirrorToken = mirrorTokens(client, as.GetLease())
	if p.Cache.Mode == "scoped" && p.Cache.Scope != "" {
		dir := filepath.Join(workDir, "cache-in")
		switch err := fetchCache(ctx, client, as.GetLease(), dir); {
		case err == nil:
			spec.CacheDir = dir
		case status.Code(err) != codes.NotFound:
			a.log.Warn("build cache not used", "attempt", as.GetLease().GetAttemptId(), "error", err)
		}
	}
	report(workerv1.AttemptState_ATTEMPT_STATE_BUILDING, "", "", "", workerv1.CleanupOutcome_CLEANUP_OUTCOME_PENDING)
	var res isolation.RunResult
	var cleanup isolation.Cleanup
	a.mu.Lock()
	live := a.negotiated >= 7 && !spec.SecretBearing
	a.mu.Unlock()
	var stream *liveLog
	if live {
		stream = newLiveLog(ctx, client, as.GetLease(), a.log)
		spec.Progress = stream
	}
	err = step("worker.build", func(ctx context.Context) error {
		var err error
		res, cleanup, err = a.manager.Execute(ctx, spec, plan.GetPlanJson())
		return err
	})
	if stream != nil {
		stream.Close()
	}
	outcome := workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED
	if cleanup.Outcome == isolation.CleanupQuarantined {
		outcome = workerv1.CleanupOutcome_CLEANUP_OUTCOME_FAILED_QUARANTINED
	}
	if err == nil {
		report(workerv1.AttemptState_ATTEMPT_STATE_COMMITTING, "", "", "", outcome)
		if uerr := step("worker.upload", func(ctx context.Context) error { return a.uploadImage(ctx, client, as.GetLease(), res) }); uerr != nil {
			err = &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "artifact upload failed: " + uerr.Error()}
		} else {
			if rerr := a.uploadRecords(ctx, client, as.GetLease(), res.Records); rerr != nil {
				err = &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "lineage upload failed: " + rerr.Error()}
			}
			if res.CacheFile != "" {
				if cerr := a.uploadCache(ctx, client, as.GetLease(), res); cerr != nil {
					a.log.Warn("build cache not stored", "attempt", as.GetLease().GetAttemptId(), "error", cerr)
				}
			}
		}
	}
	checks = checkReports(res.Checks)
	egressDenied, egressUnrecorded = egressReport(res.Egress)
	if lerr := a.uploadLog(ctx, client, as.GetLease(), res.Logs); lerr != nil {
		a.log.Warn("log upload failed", "attempt", as.GetLease().GetAttemptId(), "error", lerr)
	}
	switch {
	case err == nil:
		report(workerv1.AttemptState_ATTEMPT_STATE_COMPLETED, "", "", res.ArtifactDigest, outcome)
	case errors.Is(err, isolation.ErrTimeout):
		report(workerv1.AttemptState_ATTEMPT_STATE_FAILED, "timeout", err.Error(), "", outcome)
	case isolation.Requeue(err):
		report(workerv1.AttemptState_ATTEMPT_STATE_FAILED, "infrastructure", err.Error(), "", outcome)
	default:
		report(workerv1.AttemptState_ATTEMPT_STATE_FAILED, "application", err.Error(), "", outcome)
	}
}

// MaxCacheBytes bounds an imported build cache.
const MaxCacheBytes = 10 << 30

// fetchCache unpacks the cache the controller chose for the lease's scope into dir; the archive must match the digest
// and size the controller announced, and every blob its own digest.
func fetchCache(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, dir string) error {
	stream, err := client.GetCache(ctx, &workerv1.GetCacheRequest{Lease: lease})
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.GetSizeBytes() > MaxCacheBytes {
		return errors.New("the controller sent no usable cache header")
	}
	pr, pw := io.Pipe()
	h := sha256.New()
	go func() {
		var n uint64
		for {
			c, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				if n != start.GetSizeBytes() {
					pw.CloseWithError(errors.New("the cache ended early"))
					return
				}
				pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			d := c.GetData()
			if n += uint64(len(d)); n > start.GetSizeBytes() {
				pw.CloseWithError(errors.New("the cache is larger than announced"))
				return
			}
			h.Write(d)
			if _, err := pw.Write(d); err != nil {
				return
			}
		}
	}()
	err = engine.UnpackCache(pr, dir, int64(start.GetSizeBytes()))
	_, _ = io.Copy(io.Discard, pr)
	if err == nil && "sha256:"+hex.EncodeToString(h.Sum(nil)) != start.GetDigest() {
		err = errors.New("the cache does not match its digest")
	}
	if err != nil {
		_ = os.RemoveAll(dir)
	}
	return err
}

// uploadRecords stores the build's lineage files; a build whose lineage cannot be stored does not succeed.
func (a *Agent) uploadRecords(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, records []isolation.Record) error {
	for _, r := range records {
		f, err := os.Open(r.File)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err == nil {
			err = a.upload(ctx, client, lease, r.Kind, r.Digest, f, st.Size())
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) uploadCache(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, res isolation.RunResult) error {
	f, err := os.Open(res.CacheFile)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > MaxCacheBytes {
		return errors.New("the build cache is larger than the limit")
	}
	return a.upload(ctx, client, lease, "cache", res.CacheDigest, f, st.Size())
}

// fetchSource writes the attempt's files under dir, checking the manifest against the plan's snapshot digest and
// every file against the manifest; nothing unverified reaches the build environment.
func fetchSource(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, snapshotDigest, dir string) error {
	stream, err := client.GetSource(ctx, &workerv1.GetSourceRequest{Lease: lease})
	if err != nil {
		return err
	}
	if err := receiveFiles(stream, snapshotDigest, dir); err != nil {
		return err
	}
	return contextModes(dir)
}

// receiveFiles writes a manifest-first file stream under dir, checking the manifest against digest and every file
// against the manifest.
func receiveFiles(stream interface {
	Recv() (*workerv1.SourceChunk, error)
}, snapshotDigest, dir string) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if planDigest(first.GetManifestJson()) != snapshotDigest {
		return errors.New("the source manifest does not match the snapshot digest in the plan")
	}
	var m struct {
		Files []struct {
			Path   string `json:"path"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(first.GetManifestJson(), &m); err != nil {
		return err
	}
	want := map[string]string{}
	for _, f := range m.Files {
		want[f.Path] = f.Digest
	}
	var cur *os.File
	var h hash.Hash
	var curDigest string
	finish := func() error {
		if cur == nil {
			return nil
		}
		cur.Close()
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) != curDigest {
			return errors.New("a source file does not match its manifest digest")
		}
		cur = nil
		return nil
	}
	seen := 0
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch {
		case c.GetFile() != nil:
			if err := finish(); err != nil {
				return err
			}
			f := c.GetFile()
			if want[f.GetPath()] != f.GetDigest() || !safeRelative(f.GetPath()) {
				return errors.New("the controller sent a file that is not in the manifest")
			}
			dst := filepath.Join(dir, filepath.FromSlash(f.GetPath()))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if f.GetExecutable() {
				mode = 0o700
			}
			if cur, err = os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode); err != nil {
				return err
			}
			h, curDigest = sha256.New(), f.GetDigest()
			seen++
		case c.GetData() != nil:
			if cur == nil {
				return errors.New("source data before a file header")
			}
			if _, err := io.MultiWriter(cur, h).Write(c.GetData()); err != nil {
				return err
			}
		}
	}
	if err := finish(); err != nil {
		return err
	}
	if seen != len(m.Files) {
		return errors.New("the source stream ended before every file arrived")
	}
	return nil
}

// uploadImage sends the produced image to internal storage under a grant for this lease.
func (a *Agent) uploadImage(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, res isolation.RunResult) error {
	f, err := os.Open(res.ArtifactFile)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return a.upload(ctx, client, lease, "image", res.ArtifactDigest, f, info.Size())
}

// uploadLog stores the attempt's run log, already masked by the provider, while the lease is still held.
func (a *Agent) uploadLog(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, logs []byte) error {
	if len(logs) == 0 {
		return nil
	}
	if len(logs) > maxLogBytes {
		logs = append(logs[:maxLogBytes:maxLogBytes], []byte("\n[log truncated]\n")...)
	}
	sum := sha256.Sum256(logs)
	return a.upload(ctx, client, lease, "log", "sha256:"+hex.EncodeToString(sum[:]), bytes.NewReader(logs), int64(len(logs)))
}

const maxLogBytes = 8 << 20

func (a *Agent) upload(ctx context.Context, client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef, kind, digest string, f io.Reader, size int64) error {
	grant, err := client.GrantArtifactUpload(ctx, &workerv1.ArtifactUploadRequest{Lease: lease, Kind: kind, Digest: digest, SizeBytes: uint64(size)})
	if err != nil {
		return err
	}
	cfg, err := a.tlsConfig()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+a.endpoint()+grant.GetUploadUrl(), f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	for k, v := range grant.GetHeaders() {
		req.Header.Set(k, v)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}, Timeout: 30 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("controller refused the upload: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func safeRelative(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func planDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Run keeps a session open with capped exponential backoff until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.Reconcile(ctx)
	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.Reconcile(ctx)
			}
		}
	}()
	backoff := time.Second
	for {
		err := a.Session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrIdentityExpired) {
			return err
		}
		if revoked(err) {
			return fmt.Errorf("%w: %v", ErrIdentityRevoked, err)
		}
		a.log.Warn("controller connection lost", "error", err, "retry", backoff.String())
		a.nextEndpoint()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

const reconcileInterval = 10 * time.Minute

// Reconcile destroys environments that no attempt running in this process owns, such as those a crashed or
// restarted worker left behind, with their daemons and any secret material (SEC-SEC-14). It does nothing during a
// self-test, whose environments the provider holds outside the manager.
func (a *Agent) Reconcile(ctx context.Context) []isolation.Cleanup {
	if a.manager == nil {
		return nil
	}
	a.mu.Lock()
	testing := a.selfTesting
	a.mu.Unlock()
	if testing {
		return nil
	}
	cleanups, err := a.manager.Reconcile(ctx, func(attemptID string) bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, running := a.leases[attemptID]
		return running || a.selfTesting
	})
	if err != nil {
		a.log.Warn("environment reconciliation", "error", err)
	}
	for _, c := range cleanups {
		a.log.Info("destroyed an orphaned environment", "attempt", c.AttemptID, "outcome", c.Outcome, "detail", c.Detail)
	}
	return cleanups
}

// reportWindow bounds how long a report is retried while no controller answers; the lease ends sooner or later
// anyway, and the controller then decides.
const reportWindow = 5 * time.Minute

// reportUntilAnswered retries a report across lost sessions until a controller acknowledges or refuses it.
// Keepalive pings the controller so a dead one is noticed within about 20 seconds, even when its host lost power
// and no connection reset arrives; the session then moves to the next controller.
var Keepalive = keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}

func reportUntilAnswered(client workerv1.WorkerServiceClient, r *workerv1.AttemptReport) (*workerv1.AttemptReportAck, error) {
	deadline := time.Now().Add(reportWindow)
	for {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ack, err := client.ReportAttempt(rctx, r)
		cancel()
		if err == nil || time.Now().After(deadline) {
			return ack, err
		}
		switch status.Code(err) {
		case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
			time.Sleep(time.Second)
		default:
			return ack, err
		}
	}
}

func (a *Agent) endpoints() []string {
	var out []string
	for _, e := range strings.Split(a.cfg.Controller, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func (a *Agent) endpoint() string {
	list := a.endpoints()
	if len(list) == 0 {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return list[a.endpointAt%len(list)]
}

func (a *Agent) nextEndpoint() {
	a.mu.Lock()
	a.endpointAt++
	a.mu.Unlock()
}

func readPrivate(p string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must not be accessible by group or others (chmod 600)", p)
	}
	raw, err := os.ReadFile(p)
	return strings.TrimSpace(string(raw)), err
}

// releaser fetches secrets just in time under the attempt's lease. Values stay in memory and are never logged.
func releaser(client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, id string) ([]byte, error) {
		res, err := client.ReleaseSecret(ctx, &workerv1.ReleaseSecretRequest{Lease: lease, LogicalId: id})
		if err != nil {
			return nil, fmt.Errorf("secret %s: release failed", id)
		}
		if d := res.GetDenied(); d != nil {
			if d.GetReason() == workerv1.DenialReason_DENIAL_REASON_OPTIONAL_UNBOUND {
				return nil, isolation.ErrSecretUnbound
			}
			return nil, fmt.Errorf("secret %s: release denied (%s)", id, strings.ToLower(strings.TrimPrefix(d.GetReason().String(), "DENIAL_REASON_")))
		}
		return res.GetSecret().GetValue(), nil
	}
}

// mirrorTokens fetches mirror tokens under the attempt's lease. Tokens are never logged.
func mirrorTokens(client workerv1.WorkerServiceClient, lease *workerv1.LeaseRef) func(context.Context, string, []string) (string, int64, time.Time, error) {
	return func(ctx context.Context, mirror string, scopes []string) (string, int64, time.Time, error) {
		res, err := client.IssueMirrorToken(ctx, &workerv1.MirrorTokenRequest{Lease: lease, Mirror: mirror, Scopes: scopes})
		if err != nil {
			return "", 0, time.Time{}, fmt.Errorf("mirror %s: token request failed", mirror)
		}
		if d := res.GetDenied(); d != nil {
			return "", 0, time.Time{}, fmt.Errorf("mirror %s: %s", mirror, d.GetReason())
		}
		t := res.GetToken()
		return t.GetToken(), t.GetExpiresInSeconds(), t.GetIssuedAt().AsTime(), nil
	}
}

// selfTest runs the worker self-test through the provider only while no build runs, and holds every slot until it
// ends, so its environments never share the worker with a build.
func (a *Agent) selfTest(ctx context.Context, client workerv1.WorkerServiceClient, req *workerv1.SelfTestRequest) {
	report := func(checks []isolation.Check) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		ack, err := client.ReportSelfTest(rctx, &workerv1.SelfTestReport{SelfTestId: req.GetSelfTestId(), Checks: checkReports(checks)})
		if err != nil || !ack.GetAccepted() {
			a.log.Warn("self-test report not accepted", "selfTest", req.GetSelfTestId(), "error", err, "rejection", ack.GetRejection())
		}
	}
	errored := func(reason string) []isolation.Check {
		return []isolation.Check{{ID: "self-test", Category: "self-test", Outcome: "errored", Reason: reason}}
	}
	tester, ok := a.provider.(isolation.SelfTester)
	if !ok {
		go report(errored("this worker's isolation provider cannot run the self-test"))
		return
	}
	if !a.offers(ctx, req.GetProfile()) {
		go report(errored("this worker's provider does not offer the " + req.GetProfile() + " profile"))
		return
	}
	a.mu.Lock()
	if len(a.leases) > 0 || a.selfTesting {
		a.mu.Unlock()
		go report(errored("the worker is busy; run the self-test when it is idle"))
		return
	}
	a.selfTesting = true
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			a.selfTesting = false
			a.mu.Unlock()
		}()
		tctx, cancel := context.WithTimeout(ctx, 40*time.Minute)
		defer cancel()
		a.log.Info("running pool self-test", "selfTest", req.GetSelfTestId(), "profile", req.GetProfile())
		report(tester.SelfTest(tctx))
	}()
}

func (a *Agent) offers(ctx context.Context, profile string) bool {
	c, err := a.provider.Capabilities(ctx)
	if err != nil {
		return false
	}
	for _, p := range c.Profiles {
		if string(p) == profile {
			return true
		}
	}
	return false
}

// maxEgressDenials matches the controller's bound (internal/egress MaxDenials).
const maxEgressDenials = 50

func egressReport(e isolation.EgressReport) ([]*workerv1.EgressDenial, uint32) {
	var out []*workerv1.EgressDenial
	for _, d := range e.Denied {
		if len(out) == maxEgressDenials {
			break
		}
		out = append(out, &workerv1.EgressDenial{Host: d.Host, Port: uint32(max(d.Port, 0)), Count: uint64(max(d.Count, 0)), Reason: d.Reason})
	}
	return out, uint32(min(max(e.Unrecorded, 0), 1<<31))
}

func checkReports(checks []isolation.Check) []*workerv1.CheckReport {
	var out []*workerv1.CheckReport
	for _, c := range checks {
		cr := &workerv1.CheckReport{CheckId: c.ID, Category: c.Category, Outcome: c.Outcome, Reason: c.Reason}
		for _, f := range c.Findings {
			cr.Findings = append(cr.Findings, &workerv1.FindingReport{Rule: f.Rule, Class: f.Class, Path: f.Path, LayerDigest: f.Layer, SizeBytes: uint64(max(f.Size, 0)), LogicalSecretId: f.SecretID})
		}
		out = append(out, cr)
	}
	return out
}

// maybeRenew starts one renewal once the certificate has passed two-thirds of its lifetime.
func (a *Agent) maybeRenew(ctx context.Context, client workerv1.WorkerServiceClient) {
	a.mu.Lock()
	if a.renewing || !a.renewDue() {
		a.mu.Unlock()
		return
	}
	a.renewing = true
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			a.renewing = false
			a.mu.Unlock()
		}()
		if err := a.renew(ctx, client); err != nil {
			a.log.Warn("certificate renewal failed; retrying at the next heartbeat", "error", a.diagnose(err))
		}
	}()
}

func (a *Agent) convertReady() bool { return a.manager != nil && a.manager.ConvertReady() == nil }

// buildProviders are reported by the isolation provider, which knows whether its engine can run them; every other
// installed tool is a conversion provider, reported only when this worker can run conversions.
var buildProviders = map[string]bool{"railpack": true, "buildpacks": true}

func reportedProviders(fromIsolation map[string]string, convertReady bool, dir string) map[string]string {
	out := map[string]string{}
	if convertReady {
		for name, v := range InstalledProviders(dir) {
			if !buildProviders[name] {
				out[name] = v
			}
		}
	}
	for name, v := range fromIsolation {
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func capacityMessage(r hostcap.Report) *workerv1.HostCapacity {
	m := &workerv1.HostCapacity{Cpus: uint32(r.CPUs), MemoryTotalBytes: r.MemoryTotalBytes, MemoryAvailableBytes: r.MemoryAvailableBytes}
	for _, v := range r.Volumes {
		m.Volumes = append(m.Volumes, &workerv1.HostVolume{Label: v.Label, Path: v.Path, TotalBytes: v.TotalBytes, FreeBytes: v.FreeBytes})
	}
	return m
}
