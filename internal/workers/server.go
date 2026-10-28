package workers

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"samuori.invalid/samuori/internal/hostcap"
	"samuori.invalid/samuori/internal/pki"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

const (
	ProtocolMajor     = 1
	ProtocolMinor     = 7
	HeartbeatInterval = 15 * time.Second
)

type workerKey struct{}

var workerName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Attempts handles lease-bound worker calls. Implementations reject any lease whose fencing generation is not
// current with ErrStaleLease (docs/architecture/build-lifecycle.md#worker-leases).
type Attempts interface {
	Plan(ctx context.Context, workerID string, lease *workerv1.LeaseRef) (digest string, plan []byte, err error)
	Report(ctx context.Context, workerID string, r *workerv1.AttemptReport) error
	Renew(ctx context.Context, workerID string, leases []*workerv1.LeaseRef) (abandoned []*workerv1.LeaseRef, err error)
	Rejected(ctx context.Context, workerID string, r *workerv1.AssignmentRejection) error
	Source(ctx context.Context, workerID string, lease *workerv1.LeaseRef, send func(*workerv1.SourceChunk) error) error
	// Cache returns ErrNoCache when the lease's plan scope has no importable cache.
	Cache(ctx context.Context, workerID string, lease *workerv1.LeaseRef, send func(*workerv1.CacheChunk) error) error
	Grant(ctx context.Context, workerID string, r *workerv1.ArtifactUploadRequest) (token string, expires time.Time, err error)
	Upload(ctx context.Context, workerID, token string, body io.Reader) error
	Release(ctx context.Context, workerID string, req *workerv1.ReleaseSecretRequest) (*workerv1.ReleaseSecretResponse, error)
	MirrorToken(ctx context.Context, workerID string, req *workerv1.MirrorTokenRequest) (*workerv1.MirrorTokenResponse, error)
	// Resend sends again every assignment leased to the worker that it has not started, after it (re)connects.
	Resend(ctx context.Context, workerID string) error
	// AppendLog stores live build output for the current lease (protocol 1.7); stop asks the worker to send no more.
	AppendLog(ctx context.Context, workerID string, c *workerv1.LogChunk) (stop bool, err error)
}

// Conversions handles the lease-bound calls of Kubernetes conversion runs (protocol 1.4, ADR 0048) with the same
// fencing rule as Attempts.
type Conversions interface {
	Job(ctx context.Context, workerID string, lease *workerv1.LeaseRef) (digest string, job []byte, err error)
	Input(ctx context.Context, workerID string, lease *workerv1.LeaseRef, send func(*workerv1.SourceChunk) error) error
	Report(ctx context.Context, workerID string, r *workerv1.ConversionReport) error
	Renew(ctx context.Context, workerID string, leases []*workerv1.LeaseRef) (abandoned []*workerv1.LeaseRef, err error)
	Resend(ctx context.Context, workerID string) error
}

// ConversionMinor is the first protocol minor version that carries conversions.
const ConversionMinor = 4

// PinMinor is the first protocol minor version whose workers build base-image pins (ADR 0049); older workers would
// build the moving tag, so pinned plans never go to them.
const PinMinor = 5

// ImageMetadataMinor is the first protocol minor version whose workers apply the plan's image labels and annotations;
// older workers would ignore them, so such plans never go to them.
const ImageMetadataMinor = 6

var (
	ErrStaleLease = errors.New("the lease is not current")
	// ErrNoCache is returned by Attempts.Cache when there is nothing to import.
	ErrNoCache = errors.New("no cache for this build")
)

// Server implements the controller side of the worker protocol.
type Server struct {
	workerv1.UnimplementedEnrollmentServiceServer
	workerv1.UnimplementedWorkerServiceServer
	store       *Store
	releases    Releases
	log         *slog.Logger
	attempts    Attempts
	conversions Conversions
	revokeHooks []RevokeHook

	certMu sync.Mutex
	cert   *tls.Certificate

	mu        sync.Mutex
	connected map[string]*connection
	routing   *routing
}

func NewServer(s *Store, log *slog.Logger) *Server {
	return &Server{store: s, log: log, connected: map[string]*connection{}}
}

// UseAttempts attaches the build service; until then lease-bound calls are refused.
func (s *Server) UseAttempts(a Attempts) { s.attempts = a }

// UseConversions attaches the conversion scheduler; until then conversion calls are refused.
func (s *Server) UseConversions(c Conversions) { s.conversions = c }

var ErrNotConnected = errors.New("worker is not connected")

// Send queues a message on the worker's stream, forwarding it to the controller holding the stream when that is
// another one. Assignments are delivered at most once per lease generation and stream, so redelivered wake-ups are
// no-ops (CHA-MSG-01) while a reconnected worker can still receive an assignment it never got.
func (s *Server) Send(workerID string, m *workerv1.ControllerMessage) error {
	err := s.sendHere(workerID, m)
	if errors.Is(err, ErrNotConnected) {
		return s.forward(workerID, m)
	}
	return err
}

func (s *Server) sendHere(workerID string, m *workerv1.ControllerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connected[workerID]
	if !ok {
		return ErrNotConnected
	}
	key := ""
	if a := m.GetAssignment(); a != nil {
		key = fmt.Sprintf("%s/%d", a.GetLease().GetAttemptId(), a.GetLease().GetFencingGeneration())
	} else if a := m.GetConversion(); a != nil {
		key = fmt.Sprintf("%s/%d", a.GetLease().GetAttemptId(), a.GetLease().GetFencingGeneration())
	}
	if key != "" && c.delivered[key] {
		return nil
	}
	select {
	case c.out <- m:
		if key != "" {
			c.delivered[key] = true
		}
		return nil
	default:
		return errors.New("worker stream is congested")
	}
}

// ServerCertTTL bounds the worker endpoint's certificate; it is reissued from the current issuing CA a day before it
// expires and whenever issuance switches to another CA (ADR 0026).
const ServerCertTTL = 7 * 24 * time.Hour

// tlsConfig serves a certificate from the issuing CA, chained with it so a new worker can match its pin, and
// verifies client certificates against every CA in the trust bundle, evaluated per handshake.
func (s *Server) tlsConfig(hosts []string) (*tls.Config, error) {
	if _, err := s.serverCert(hosts); err != nil {
		return nil, err
	}
	base := &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		cert, err := s.serverCert(hosts)
		if err != nil {
			return nil, err
		}
		c := base.Clone()
		c.GetConfigForClient = nil
		c.Certificates, c.ClientCAs = []tls.Certificate{cert}, s.store.auth.Pool()
		return c, nil
	}
	return base, nil
}

func (s *Server) serverCert(hosts []string) (tls.Certificate, error) {
	s.certMu.Lock()
	defer s.certMu.Unlock()
	issuing := s.store.auth.Issuing()
	if c := s.cert; c != nil && len(c.Certificate) > 1 && bytes.Equal(c.Certificate[1], issuing.Cert.Raw) && time.Until(c.Leaf.NotAfter) > 24*time.Hour {
		return *c, nil
	}
	cert, err := issuing.ServerCertificate(hosts, ServerCertTTL)
	if err != nil {
		return tls.Certificate{}, err
	}
	s.cert = &cert
	return cert, nil
}

func (s *Server) grpcServer() *grpc.Server {
	g := grpc.NewServer(grpc.MaxRecvMsgSize(4<<20), grpc.UnaryInterceptor(s.unaryAuth), grpc.StreamInterceptor(s.streamAuth))
	workerv1.RegisterEnrollmentServiceServer(g, s)
	workerv1.RegisterWorkerServiceServer(g, s)
	return g
}

// UploadPath is where workers PUT artifacts with a grant; it shares the worker endpoint's TLS listener.
const UploadPath = "/v1/artifacts"

// Serve serves gRPC and artifact uploads on one TLS listener until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener, hosts []string) error {
	cfg, err := s.tlsConfig(hosts)
	if err != nil {
		return err
	}
	g := s.grpcServer()
	srv := &http.Server{
		// gRPC runs on net/http's HTTP/2 here, so its keepalive settings do not apply. These pings close the
		// connection of a worker whose host vanished without a reset, so it stops counting as connected and gets
		// no more assignments; workers ping too (workeragent.Keepalive).
		HTTP2:             &http.HTTP2Config{SendPingTimeout: 20 * time.Second, PingTimeout: 10 * time.Second},
		TLSConfig:         cfg,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				g.ServeHTTP(w, r)
				return
			}
			s.upload(w, r)
		}),
	}
	go func() {
		<-ctx.Done()
		// Worker streams never end on their own; closing them lets workers move to another controller at once.
		s.mu.Lock()
		for _, c := range s.connected {
			c.close()
		}
		s.mu.Unlock()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close()
		}
	}()
	s.log.Info("worker endpoint listening", "addr", ln.Addr().String(), "caPin", s.store.auth.Issuing().Pin())
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// upload accepts an artifact only from an enrolled worker presenting a grant issued to it for its current lease.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != UploadPath || r.Method != http.MethodPut {
		http.NotFound(w, r)
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "a worker certificate is required", http.StatusUnauthorized)
		return
	}
	leaf := r.TLS.VerifiedChains[0][0]
	id, ok := pki.WorkerID(leaf)
	if !ok {
		http.Error(w, "not a worker certificate", http.StatusUnauthorized)
		return
	}
	if _, err := s.store.Authenticate(r.Context(), id, leaf.SerialNumber.Text(16)); err != nil {
		http.Error(w, "worker is not enrolled with this certificate", http.StatusForbidden)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Samuori-Grant ")
	if !ok || s.attempts == nil {
		http.Error(w, "an upload grant is required", http.StatusUnauthorized)
		return
	}
	if err := s.attempts.Upload(r.Context(), id, token, r.Body); err != nil {
		switch {
		case errors.Is(err, ErrStaleLease), errors.Is(err, ErrGrantRefused):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrUploadMismatch):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		default:
			s.log.Error("artifact upload", "worker", id, "error", err)
			http.Error(w, "upload failed", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
}

var (
	ErrGrantRefused   = errors.New("the upload grant is invalid, expired or not issued to this worker")
	ErrUploadMismatch = errors.New("the upload does not match its grant")
)

func enrollmentMethod(full string) bool {
	return strings.HasPrefix(full, "/"+workerv1.EnrollmentService_ServiceDesc.ServiceName+"/")
}

// authenticate requires a client certificate chaining to the CA that belongs to a currently enrolled worker.
func (s *Server) authenticate(ctx context.Context) (Worker, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Worker{}, status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 {
		return Worker{}, status.Error(codes.Unauthenticated, "a worker certificate is required")
	}
	leaf := info.State.VerifiedChains[0][0]
	id, ok := pki.WorkerID(leaf)
	if !ok {
		return Worker{}, status.Error(codes.Unauthenticated, "certificate does not identify a worker")
	}
	w, err := s.store.Authenticate(ctx, id, leaf.SerialNumber.Text(16))
	if err != nil {
		return Worker{}, status.Error(codes.PermissionDenied, "worker is not enrolled with this certificate")
	}
	return w, nil
}

func (s *Server) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if enrollmentMethod(info.FullMethod) {
		return h(ctx, req)
	}
	w, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return h(context.WithValue(ctx, workerKey{}, w), req)
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a authedStream) Context() context.Context { return a.ctx }

func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	w, err := s.authenticate(ss.Context())
	if err != nil {
		return err
	}
	return h(srv, authedStream{ss, context.WithValue(ss.Context(), workerKey{}, w)})
}

func (s *Server) Enroll(ctx context.Context, req *workerv1.EnrollRequest) (*workerv1.EnrollResponse, error) {
	if req.GetProtocol().GetMajor() != ProtocolMajor {
		return nil, status.Errorf(codes.FailedPrecondition, "protocol %d.x is not supported; this controller speaks %d.x", req.GetProtocol().GetMajor(), ProtocolMajor)
	}
	name := req.GetWorkerName()
	if name != "" && !workerName.MatchString(name) {
		return nil, status.Error(codes.InvalidArgument, "worker names use lowercase letters, digits and dashes (at most 63)")
	}
	w, der, err := s.store.Enroll(ctx, EnrollRequest{Token: req.GetEnrollmentToken(), Name: name, CSR: req.GetCsrDer(), Capabilities: fromProto(req.GetCapabilities()), Version: req.GetWorkerVersion(), ProtocolMinor: req.GetProtocol().GetMinor(), PreviousWorkerID: req.GetPreviousWorkerId()})
	switch {
	case errors.Is(err, ErrToken):
		return nil, status.Error(codes.PermissionDenied, err.Error())
	case err != nil && strings.HasPrefix(err.Error(), "pki:"):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		s.log.Error("enrollment failed", "error", err)
		return nil, status.Error(codes.Internal, "enrollment failed")
	}
	s.log.Info("worker enrolled", "worker", w.ID, "pool", w.PoolID)
	return &workerv1.EnrollResponse{WorkerId: w.ID, CertificateDer: der, TrustBundle: s.bundle(), CertificateExpiresAt: timestamppb.New(w.CertExpiresAt)}, nil
}

func (s *Server) bundle() *workerv1.TrustBundle {
	version, cas := s.store.auth.Bundle()
	b := &workerv1.TrustBundle{BundleVersion: version}
	for _, ca := range cas {
		b.Anchors = append(b.Anchors, &workerv1.TrustAnchor{CaCertificateDer: ca.Raw, NotBefore: timestamppb.New(ca.NotBefore), NotAfter: timestamppb.New(ca.NotAfter)})
	}
	return b
}

// Connect accepts Hello, reconciles the worker's leases, sends the current trust bundle, then relays controller
// messages and records heartbeats until the stream ends or the worker is disconnected (revocation).
func (s *Server) Connect(stream grpc.BidiStreamingServer[workerv1.WorkerMessage, workerv1.ControllerMessage]) error {
	if s.fenced() {
		return status.Error(codes.Unavailable, "this controller cannot reach the database; connect to another controller")
	}
	w := stream.Context().Value(workerKey{}).(Worker)
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "the first message must be Hello")
	}
	if hello.GetProtocol().GetMajor() != ProtocolMajor {
		return status.Errorf(codes.FailedPrecondition, "protocol %d.x is not supported", hello.GetProtocol().GetMajor())
	}
	caps := fromProto(hello.GetCapabilities())
	if err := s.store.Seen(stream.Context(), w.ID, &caps, hello.GetWorkerVersion()); err != nil {
		return status.Error(codes.Internal, "recording worker")
	}
	if err := s.store.AcknowledgeBundle(stream.Context(), w.ID, hello.GetTrustBundleVersion()); err != nil {
		return status.Error(codes.Internal, "recording trust bundle")
	}
	abandoned := hello.GetActiveLeases()
	if s.attempts != nil {
		if abandoned, err = s.attempts.Renew(stream.Context(), w.ID, hello.GetActiveLeases()); err != nil {
			return status.Error(codes.Internal, "reconciling leases")
		}
	}
	abandonedConversions := hello.GetActiveConversionLeases()
	if s.conversions != nil {
		if abandonedConversions, err = s.conversions.Renew(stream.Context(), w.ID, hello.GetActiveConversionLeases()); err != nil {
			return status.Error(codes.Internal, "reconciling conversion leases")
		}
	}
	abandoned = append(abandoned, abandonedConversions...)
	minor := min(hello.GetProtocol().GetMinor(), uint32(ProtocolMinor))
	if _, err := s.store.db.SQL.ExecContext(stream.Context(), s.store.db.Rebind(`UPDATE workers SET protocol_minor = ? WHERE id = ?`), minor, w.ID); err != nil {
		return status.Error(codes.Internal, "recording the protocol version")
	}
	if err := stream.Send(&workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_HelloAck{HelloAck: &workerv1.HelloAck{
		Negotiated:        &workerv1.ProtocolVersion{Major: ProtocolMajor, Minor: minor},
		HeartbeatInterval: durationpb.New(HeartbeatInterval),
		AbandonedLeases:   abandoned,
		TrustBundle:       s.bundle(),
		ControllerTime:    timestamppb.New(s.store.now()),
	}}}); err != nil {
		return err
	}
	conn := &connection{out: make(chan *workerv1.ControllerMessage, 64), done: make(chan struct{}), delivered: map[string]bool{}}
	s.mark(w.ID, conn)
	s.recordSession(stream.Context(), w.ID)
	defer func() {
		if s.unmark(w.ID, conn) {
			s.dropSession(w.ID)
		}
	}()
	s.log.Info("worker connected", "worker", w.ID)
	if s.attempts != nil {
		if err := s.attempts.Resend(stream.Context(), w.ID); err != nil {
			s.log.Error("resending assignments", "worker", w.ID, "error", err)
		}
	}
	if s.conversions != nil {
		if err := s.conversions.Resend(stream.Context(), w.ID); err != nil {
			s.log.Error("resending conversion assignments", "worker", w.ID, "error", err)
		}
	}
	go s.selfTestOnConnect(context.WithoutCancel(stream.Context()), w.ID)
	recv := make(chan *workerv1.WorkerMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case recv <- msg:
			case <-conn.done:
				return
			}
		}
	}()
	for {
		select {
		case <-conn.done:
			if conn.revoked.Load() {
				return status.Error(codes.PermissionDenied, "the worker identity was revoked")
			}
			return status.Error(codes.Unavailable, "the controller closed this stream; reconnect")
		case <-stream.Context().Done():
			return nil
		case <-recvErr:
			return nil
		case m := <-conn.out:
			if err := stream.Send(m); err != nil {
				return err
			}
		case msg := <-recv:
			s.handle(stream.Context(), w, msg)
		}
	}
}

// handle processes one message from the worker's stream. It is bounded in time: the stream loop must stay free to
// close the stream, for example when this controller fences itself after losing the database.
func (s *Server) handle(parent context.Context, w Worker, msg *workerv1.WorkerMessage) {
	ctx, cancel := context.WithTimeout(parent, HeartbeatInterval/2)
	defer cancel()
	switch {
	case msg.GetHeartbeat() != nil:
		if err := s.store.Seen(ctx, w.ID, nil, ""); err != nil {
			s.log.Error("heartbeat", "worker", w.ID, "error", err)
		}
		if hb := msg.GetHeartbeat(); hb.GetHostArch() != "" {
			if err := s.store.RecordHost(ctx, w.ID, hb.GetHostArch(), hb.GetUpdateFreeBytes()); err != nil {
				s.log.Error("heartbeat host facts", "worker", w.ID, "error", err)
			}
		}
		if c := msg.GetHeartbeat().GetCapacity(); c != nil {
			if err := s.store.RecordCapacity(ctx, w.ID, capacityReport(c)); err != nil {
				s.log.Error("heartbeat capacity", "worker", w.ID, "error", err)
			}
		}
		if v := msg.GetHeartbeat().GetTrustBundleVersion(); v > 0 {
			if err := s.store.AcknowledgeBundle(ctx, w.ID, v); err != nil {
				s.log.Error("trust bundle acknowledgement", "worker", w.ID, "error", err)
			}
		}
		if s.attempts != nil {
			abandoned, err := s.attempts.Renew(ctx, w.ID, msg.GetHeartbeat().GetActiveLeases())
			if err != nil {
				s.log.Error("lease renewal", "worker", w.ID, "error", err)
			}
			for _, l := range abandoned {
				_ = s.Send(w.ID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Cancel{Cancel: &workerv1.Cancel{Lease: l, Reason: "lease is no longer current"}}})
			}
		}
		if s.conversions != nil {
			abandoned, err := s.conversions.Renew(ctx, w.ID, msg.GetHeartbeat().GetActiveConversionLeases())
			if err != nil {
				s.log.Error("conversion lease renewal", "worker", w.ID, "error", err)
			}
			for _, l := range abandoned {
				_ = s.Send(w.ID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Cancel{Cancel: &workerv1.Cancel{Lease: l, Reason: "lease is no longer current"}}})
			}
		}
	case msg.GetAssignmentRejection() != nil && s.attempts != nil:
		if err := s.attempts.Rejected(ctx, w.ID, msg.GetAssignmentRejection()); err != nil {
			s.log.Error("assignment rejection", "worker", w.ID, "error", err)
		}
	}
}

type connection struct {
	out       chan *workerv1.ControllerMessage
	done      chan struct{}
	once      sync.Once
	delivered map[string]bool
	revoked   atomic.Bool
}

func (c *connection) close() { c.once.Do(func() { close(c.done) }) }

func (s *Server) mark(id string, c *connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.connected[id]; ok {
		old.close()
	}
	s.connected[id] = c
}

func (s *Server) unmark(id string, c *connection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected[id] == c {
		delete(s.connected, id)
		return true
	}
	return false
}

// Disconnect ends a revoked worker's open stream at once, on whichever controller holds it (ADR 0026).
func (s *Server) Disconnect(id string) {
	s.disconnectHere(id, true)
	if s.routing != nil {
		if err := s.routing.publish(broadcastSubject, envelope{Kind: signalRevoke, WorkerID: id}); err != nil {
			s.log.Error("disconnecting the worker on other controllers", "worker", id, "error", err)
		}
	}
}

// disconnectHere ends the worker's stream on this controller. A revoked worker is told so; otherwise it reconnects.
func (s *Server) disconnectHere(id string, revoked bool) {
	s.mu.Lock()
	c, ok := s.connected[id]
	if ok {
		c.revoked.Store(revoked)
		c.close()
		delete(s.connected, id)
	}
	s.mu.Unlock()
	if ok {
		s.dropSession(id)
	}
}

// Connected reports whether a worker holds an open stream to this controller or to another live one.
func (s *Server) Connected(id string) bool {
	if s.ConnectedHere(id) {
		return true
	}
	_, ok := s.remoteOwner(id)
	return ok
}

// ConnectedHere reports whether a worker holds an open stream to this controller.
func (s *Server) ConnectedHere(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.connected[id]
	return ok
}

func (s *Server) GetPlan(ctx context.Context, req *workerv1.GetPlanRequest) (*workerv1.GetPlanResponse, error) {
	if s.attempts == nil {
		return nil, status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	digest, plan, err := s.attempts.Plan(ctx, ctx.Value(workerKey{}).(Worker).ID, req.GetLease())
	if err != nil {
		return nil, s.callError("GetPlan", err)
	}
	return &workerv1.GetPlanResponse{PlanDigest: digest, PlanJson: plan}, nil
}

// callError maps a failed worker call: a stale lease is the worker's to act on; anything else is logged here with
// its cause and reported to the worker by operation only.
func (s *Server) callError(call string, err error) error {
	if errors.Is(err, ErrStaleLease) {
		return status.Error(codes.FailedPrecondition, "stale lease: this attempt is no longer current")
	}
	s.log.Error("worker call failed", "call", call, "error", err)
	return status.Error(codes.Internal, call+" failed on the controller")
}

// ReleaseSecret is the only call that carries secret material; values are never logged.
func (s *Server) ReleaseSecret(ctx context.Context, req *workerv1.ReleaseSecretRequest) (*workerv1.ReleaseSecretResponse, error) {
	if s.attempts == nil {
		return nil, status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	res, err := s.attempts.Release(ctx, ctx.Value(workerKey{}).(Worker).ID, req)
	if err != nil {
		return nil, status.Error(codes.Internal, "secret release failed")
	}
	return res, nil
}

// IssueMirrorToken returns a mirror token for BuildKit's registry client; tokens are never logged.
func (s *Server) IssueMirrorToken(ctx context.Context, req *workerv1.MirrorTokenRequest) (*workerv1.MirrorTokenResponse, error) {
	if s.attempts == nil {
		return nil, status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	res, err := s.attempts.MirrorToken(ctx, ctx.Value(workerKey{}).(Worker).ID, req)
	if err != nil {
		return nil, status.Error(codes.Internal, "mirror token issuance failed")
	}
	return res, nil
}

func (s *Server) ReportAttempt(ctx context.Context, r *workerv1.AttemptReport) (*workerv1.AttemptReportAck, error) {
	if s.attempts == nil {
		return nil, status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	if err := s.attempts.Report(ctx, ctx.Value(workerKey{}).(Worker).ID, r); err != nil {
		if errors.Is(err, ErrStaleLease) {
			return &workerv1.AttemptReportAck{Accepted: false, Rejection: "stale lease: a newer attempt owns this build"}, nil
		}
		return nil, s.callError("ReportAttempt", err)
	}
	return &workerv1.AttemptReportAck{Accepted: true}, nil
}

func (s *Server) GrantArtifactUpload(ctx context.Context, r *workerv1.ArtifactUploadRequest) (*workerv1.ArtifactUploadGrant, error) {
	if s.attempts == nil {
		return nil, status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	token, exp, err := s.attempts.Grant(ctx, ctx.Value(workerKey{}).(Worker).ID, r)
	if err != nil {
		if errors.Is(err, ErrStaleLease) {
			return nil, s.callError("GrantArtifactUpload", err)
		}
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &workerv1.ArtifactUploadGrant{UploadUrl: UploadPath, Headers: map[string]string{"Authorization": "Samuori-Grant " + token}, ExpiresAt: timestamppb.New(exp)}, nil
}

func (s *Server) AppendLog(ctx context.Context, c *workerv1.LogChunk) (*workerv1.LogAck, error) {
	if s.attempts == nil {
		return nil, status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	stop, err := s.attempts.AppendLog(ctx, ctx.Value(workerKey{}).(Worker).ID, c)
	if err != nil {
		return nil, s.callError("AppendLog", err)
	}
	return &workerv1.LogAck{Stop: stop}, nil
}

func (s *Server) GetSource(req *workerv1.GetSourceRequest, stream grpc.ServerStreamingServer[workerv1.SourceChunk]) error {
	if s.attempts == nil {
		return status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	w := stream.Context().Value(workerKey{}).(Worker)
	if err := s.attempts.Source(stream.Context(), w.ID, req.GetLease(), stream.Send); err != nil {
		return s.callError("GetSource", err)
	}
	return nil
}

func (s *Server) GetCache(req *workerv1.GetCacheRequest, stream grpc.ServerStreamingServer[workerv1.CacheChunk]) error {
	if s.attempts == nil {
		return status.Error(codes.Unavailable, "builds are not enabled on this controller")
	}
	w := stream.Context().Value(workerKey{}).(Worker)
	err := s.attempts.Cache(stream.Context(), w.ID, req.GetLease(), stream.Send)
	if errors.Is(err, ErrNoCache) {
		return status.Error(codes.NotFound, "no cache for this build")
	}
	if err != nil {
		return s.callError("GetCache", err)
	}
	return nil
}

func fromProto(c *workerv1.Capabilities) Capabilities {
	if c == nil {
		return Capabilities{}
	}
	caps := Capabilities{OS: c.GetOs(), Architectures: c.GetArchitectures(), Engines: c.GetEngines(), IsolationProfiles: c.GetIsolationProfiles(), DaemonSlots: c.GetFreeDaemonSlots(), NestedVirt: c.GetNestedVirtualization(), StrictEgress: c.GetStrictEgress(), ConfigDigest: c.GetConfigDigest()}
	for name, v := range c.GetProviders() {
		if len(caps.Providers) < 16 && providerName.MatchString(name) && len(v) <= 64 {
			if caps.Providers == nil {
				caps.Providers = map[string]string{}
			}
			caps.Providers[name] = v
		}
	}
	return caps
}

var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func (s *Server) GetConversionJob(ctx context.Context, req *workerv1.GetConversionJobRequest) (*workerv1.GetConversionJobResponse, error) {
	if s.conversions == nil {
		return nil, status.Error(codes.Unavailable, "conversions are not enabled on this controller")
	}
	digest, job, err := s.conversions.Job(ctx, ctx.Value(workerKey{}).(Worker).ID, req.GetLease())
	if err != nil {
		return nil, s.callError("GetConversionJob", err)
	}
	return &workerv1.GetConversionJobResponse{JobDigest: digest, JobJson: job}, nil
}

func (s *Server) GetConversionInput(req *workerv1.GetConversionInputRequest, stream grpc.ServerStreamingServer[workerv1.SourceChunk]) error {
	if s.conversions == nil {
		return status.Error(codes.Unavailable, "conversions are not enabled on this controller")
	}
	w := stream.Context().Value(workerKey{}).(Worker)
	if err := s.conversions.Input(stream.Context(), w.ID, req.GetLease(), stream.Send); err != nil {
		return s.callError("GetConversionInput", err)
	}
	return nil
}

func (s *Server) ReportConversion(ctx context.Context, r *workerv1.ConversionReport) (*workerv1.ConversionReportAck, error) {
	if s.conversions == nil {
		return nil, status.Error(codes.Unavailable, "conversions are not enabled on this controller")
	}
	if err := s.conversions.Report(ctx, ctx.Value(workerKey{}).(Worker).ID, r); err != nil {
		if errors.Is(err, ErrStaleLease) {
			return &workerv1.ConversionReportAck{Accepted: false, Rejection: "stale lease: this conversion run is no longer current"}, nil
		}
		return nil, s.callError("ReportConversion", err)
	}
	return &workerv1.ConversionReportAck{Accepted: true}, nil
}

func capacityReport(c *workerv1.HostCapacity) hostcap.Report {
	r := hostcap.Report{CPUs: int(min(c.GetCpus(), 4096)), MemoryTotalBytes: c.GetMemoryTotalBytes(), MemoryAvailableBytes: c.GetMemoryAvailableBytes()}
	for i, v := range c.GetVolumes() {
		if i == hostcap.MaxVolumes {
			break
		}
		r.Volumes = append(r.Volumes, hostcap.Volume{Label: v.GetLabel(), Path: v.GetPath(), TotalBytes: v.GetTotalBytes(), FreeBytes: v.GetFreeBytes()})
	}
	return hostcap.Clean(r)
}
