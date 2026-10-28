// Package messaging runs the controller's embedded NATS JetStream as a wake-up transport. The database stays the
// source of truth: messages carry identifiers only and are relayed from the transactional outbox (ADR 0017).
package messaging

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"samuori.invalid/samuori/internal/db"
)

const (
	Stream    = "Samuori"
	dedupe    = 2 * time.Minute
	ackWait   = 30 * time.Second
	relayTick = 500 * time.Millisecond
)

// Bus is an embedded JetStream with no network listener: only code inside the controller process can connect,
// so workers and other hosts never reach it (SEC-MSG-01).
type Bus struct {
	srv *server.Server
	nc  *nats.Conn
	js  jetstream.JetStream
	log *slog.Logger
	// want is the replicas the stream should reach; GrowReplicas raises it toward this.
	want int
	// ready closes once Samuori's stream exists. An external cluster without JetStream quorum, such as the first
	// controller of a new cluster before a second member joins, leaves it open: the controller serves its API and
	// reports messaging unhealthy, and the database keeps undelivered messages in the outbox.
	ready chan struct{}
}

func readyNow() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func (b *Bus) isReady() bool {
	select {
	case <-b.ready:
		return true
	default:
		return false
	}
}

func Start(ctx context.Context, storeDir string, log *slog.Logger) (*Bus, error) {
	srv, err := server.NewServer(&server.Options{DontListen: true, JetStream: true, StoreDir: storeDir, NoSigs: true, NoLog: true})
	if err != nil {
		return nil, err
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, errors.New("messaging: embedded NATS did not start")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		srv.Shutdown()
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		return nil, err
	}
	if err := ensureStream(ctx, js, 1); err != nil {
		nc.Close()
		srv.Shutdown()
		return nil, err
	}
	return &Bus{srv: srv, nc: nc, js: js, log: log, want: 1, ready: readyNow()}, nil
}

func streamConfig(replicas int) jetstream.StreamConfig {
	return jetstream.StreamConfig{Name: Stream, Subjects: []string{"samuori.>"}, Storage: jetstream.FileStorage, Duplicates: dedupe, Retention: jetstream.WorkQueuePolicy, Replicas: replicas}
}

// ensureStream creates Samuori's stream, or raises an existing stream's replicas toward want. A cluster that is still
// growing gets as many replicas as it has members; replicas are never lowered, so a restart while a member is down
// keeps the stream's redundancy.
func ensureStream(ctx context.Context, js jetstream.JetStream, want int) error {
	s, err := js.Stream(ctx, Stream)
	if err == nil {
		if s.CachedInfo().Config.Replicas < want {
			_, _ = js.UpdateStream(ctx, streamConfig(want))
		}
		return nil
	}
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		return fmt.Errorf("messaging: stream: %w", err)
	}
	for r := want; r >= 1; r-- {
		if _, err = js.CreateStream(ctx, streamConfig(r)); err == nil || errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			return nil
		}
	}
	return fmt.Errorf("messaging: stream: %w", err)
}

// Replicas reports the stream's replicas and the number wanted.
func (b *Bus) Replicas(ctx context.Context) (have, want int, err error) {
	s, err := b.js.Stream(ctx, Stream)
	if err != nil {
		return 0, b.want, err
	}
	return s.CachedInfo().Config.Replicas, b.want, nil
}

// StreamPeer is one replica of the stream, leader included, as JetStream reports it.
type StreamPeer struct {
	Name    string
	Current bool
}

// StreamPeers reports the stream's replicas and how many it is configured for.
func (b *Bus) StreamPeers(ctx context.Context) ([]StreamPeer, int, error) {
	s, err := b.js.Stream(ctx, Stream)
	if err != nil {
		return nil, 0, err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return nil, 0, err
	}
	var peers []StreamPeer
	if c := info.Cluster; c != nil {
		if c.Leader != "" {
			peers = append(peers, StreamPeer{Name: c.Leader, Current: true})
		}
		for _, p := range c.Replicas {
			peers = append(peers, StreamPeer{Name: p.Name, Current: p.Current && !p.Offline})
		}
	}
	return peers, info.Config.Replicas, nil
}

// GrowReplicas raises the stream's replicas as controllers join, until the configured number is reached.
func (b *Bus) GrowReplicas(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		have, want, err := b.Replicas(ctx)
		if err != nil || have >= want {
			continue
		}
		if _, err := b.js.UpdateStream(ctx, streamConfig(want)); err == nil {
			b.log.Info("messaging stream replicas raised", "replicas", want)
		}
	}
}

// External names a NATS cluster that several controllers share (ADR 0034). Only controllers hold its credentials;
// workers never connect to it (ADR 0024).
type External struct {
	URLs                      []string
	CredentialsFile           string
	CAFile, CertFile, KeyFile string
	Replicas                  int
	Name                      string
}

// ErrNoJetStream means NATS answered but JetStream did not, as in a new cluster that has no quorum yet.
var ErrNoJetStream = errors.New("messaging: JetStream is not available")

// Probe connects to an external NATS cluster and reads the JetStream account, changing nothing.
func Probe(ctx context.Context, x External) error {
	nc, js, err := dial(x, slog.New(slog.DiscardHandler), nats.RetryOnFailedConnect(false), nats.MaxReconnects(0))
	if err != nil {
		return err
	}
	defer nc.Close()
	if _, err := js.AccountInfo(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrNoJetStream, err)
	}
	return nil
}

// Connect joins an external NATS cluster and makes sure Samuori's stream exists with the configured replicas. The
// client reconnects across the cluster's servers indefinitely; the database keeps state while it is away.
func Connect(ctx context.Context, x External, log *slog.Logger) (*Bus, error) {
	nc, js, err := dial(x, log, nats.MaxReconnects(-1))
	if err != nil {
		return nil, err
	}
	b := &Bus{nc: nc, js: js, log: log, want: max(x.Replicas, 1), ready: make(chan struct{})}
	if err := ensureStream(ctx, js, b.want); err == nil {
		close(b.ready)
		return b, nil
	} else {
		log.Warn("JetStream is not available yet; retrying", "error", err)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			if err := ensureStream(ctx, js, b.want); err == nil {
				log.Info("JetStream available; stream ready")
				close(b.ready)
				return
			}
		}
	}()
	return b, nil
}

func dial(x External, log *slog.Logger, extra ...nats.Option) (*nats.Conn, jetstream.JetStream, error) {
	opts := []nats.Option{nats.Name(x.Name), nats.ReconnectWait(time.Second), nats.RetryOnFailedConnect(false),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { log.Warn("NATS disconnected", "error", err) }),
		nats.ReconnectHandler(func(nc *nats.Conn) { log.Info("NATS reconnected", "server", nc.ConnectedUrlRedacted()) })}
	opts = append(opts, extra...)
	if x.CredentialsFile != "" {
		if err := private(x.CredentialsFile); err != nil {
			return nil, nil, err
		}
		opts = append(opts, nats.UserCredentials(x.CredentialsFile))
	}
	if x.CAFile != "" {
		pem, err := os.ReadFile(x.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("messaging: CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, errors.New("messaging: the CA file holds no certificates")
		}
		cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		if x.CertFile != "" {
			if err := private(x.KeyFile); err != nil {
				return nil, nil, err
			}
			cert, err := tls.LoadX509KeyPair(x.CertFile, x.KeyFile)
			if err != nil {
				return nil, nil, fmt.Errorf("messaging: client certificate: %w", err)
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
		opts = append(opts, nats.Secure(cfg))
	}
	nc, err := nats.Connect(strings.Join(x.URLs, ","), opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("messaging: connecting to NATS: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return nc, js, nil
}

func private(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("messaging: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("messaging: %s must not be readable by group or others", path)
	}
	return nil
}

func (b *Bus) Close() {
	b.nc.Close()
	if b.srv != nil {
		b.srv.Shutdown()
		b.srv.WaitForShutdown()
	}
}

// ListensOnNetwork reports whether the embedded server has a network listener; it must always be false. An external
// cluster is not this process's listener.
func (b *Bus) ListensOnNetwork() bool { return b.srv != nil && b.srv.Addr() != nil }

// External reports whether the bus is an external cluster.
func (b *Bus) External() bool { return b.srv == nil }

// Healthy reports a lost connection and, on an external cluster, JetStream without quorum: core NATS can stay
// connected while JetStream refuses every operation.
func (b *Bus) Healthy(ctx context.Context) error {
	if !b.nc.IsConnected() {
		return errors.New("messaging: not connected")
	}
	if !b.isReady() {
		return errors.New("messaging: JetStream is not available yet; a new cluster needs a second member for quorum")
	}
	if b.srv == nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		// Account info needs the JetStream meta group, which loses quorum with a minority of members; stream info
		// is still answered from a local replica then.
		if _, err := b.js.AccountInfo(ctx); err != nil {
			return fmt.Errorf("messaging: JetStream is not answering (a majority of NATS members must be up): %w", err)
		}
	}
	return nil
}

// Relay publishes unpublished outbox rows with their message ID, so JetStream drops duplicates within its window
// and consumers' compare-and-swap transitions make any later duplicate a no-op.
type Relay struct {
	db   *db.DB
	bus  *Bus
	wake chan struct{}
}

func NewRelay(d *db.DB, b *Bus) *Relay { return &Relay{db: d, bus: b, wake: make(chan struct{}, 1)} }

// Notify asks the relay to run now instead of waiting for its next tick.
func (r *Relay) Notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Flush publishes everything pending once.
func (r *Relay) Flush(ctx context.Context) (int, error) {
	n := 0
	for {
		msgs, err := r.db.PendingOutbox(ctx, 100)
		if err != nil || len(msgs) == 0 {
			return n, err
		}
		for _, m := range msgs {
			if _, err := r.bus.js.Publish(ctx, m.Subject, m.Payload, jetstream.WithMsgID(m.MsgID)); err != nil {
				return n, err
			}
			if err := r.db.MarkOutboxPublished(ctx, m.MsgID); err != nil {
				return n, err
			}
			n++
		}
	}
}

func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(relayTick)
	defer t.Stop()
	for {
		if _, err := r.Flush(ctx); err != nil && ctx.Err() == nil {
			r.bus.log.Warn("outbox relay", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		}
	}
}

// Handler processes one message; returning an error leaves it for redelivery after the ack wait.
type Handler func(ctx context.Context, subject string, payload []byte) error

// Consume runs a durable consumer for subject until ctx ends. Messages are acknowledged only after h succeeds.
func (b *Bus) Consume(ctx context.Context, durable, subject string, h Handler) error {
	select {
	case <-b.ready:
	case <-ctx.Done():
		return nil
	}
	var c jetstream.Consumer
	for {
		var err error
		c, err = b.js.CreateOrUpdateConsumer(ctx, Stream, jetstream.ConsumerConfig{Durable: durable, FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy, AckWait: ackWait, MaxDeliver: -1})
		if err == nil {
			break
		}
		b.log.Warn("consumer not ready; retrying", "consumer", durable, "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	cc, err := c.Consume(func(m jetstream.Msg) {
		if err := h(ctx, m.Subject(), m.Data()); err != nil {
			b.log.Warn("message handling failed; will redeliver", "subject", m.Subject(), "error", err)
			_ = m.NakWithDelay(time.Second)
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

type ConsumerStatus struct {
	Name                    string
	Pending                 uint64
	AckPending, Redelivered int
}

type Status struct {
	Connected bool
	Messages  uint64
	Consumers []ConsumerStatus
}

// Status reads stream and consumer counters from JetStream.
func (b *Bus) Status(ctx context.Context) (Status, error) {
	st := Status{Connected: b.nc.IsConnected()}
	stream, err := b.js.Stream(ctx, Stream)
	if err != nil {
		return st, err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return st, err
	}
	st.Messages = info.State.Msgs
	lister := stream.ListConsumers(ctx)
	for c := range lister.Info() {
		st.Consumers = append(st.Consumers, ConsumerStatus{Name: c.Name, Pending: c.NumPending, AckPending: c.NumAckPending, Redelivered: c.NumRedelivered})
	}
	return st, lister.Err()
}

// Publish sends a transient message outside the stream, for controller-to-controller signals that the database
// can always reconstruct.
func (b *Bus) Publish(subject string, data []byte) error { return b.nc.Publish(subject, data) }

// Subscribe receives transient messages on subject until the returned function is called.
func (b *Bus) Subscribe(subject string, fn func(data []byte)) (func(), error) {
	sub, err := b.nc.Subscribe(subject, func(m *nats.Msg) { fn(m.Data) })
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
