package workers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"samuori.invalid/samuori/internal/cluster"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// Transport carries transient signals between controllers. Signals are wake-ups only: losing one delays work but
// never changes state, which the database holds.
type Transport interface {
	Publish(subject string, data []byte) error
	Subscribe(subject string, fn func(data []byte)) (func(), error)
}

// routing forwards messages for workers whose stream another controller holds (ADR 0034). Envelopes are signed with
// a key every controller derives from the cluster key, so holding messaging credentials alone cannot forge a cancel
// or drain.
type routing struct {
	owner cluster.Ownership
	bus   Transport
	key   []byte
}

const (
	signalDeliver    = "deliver"
	signalDisconnect = "disconnect"
	signalRevoke     = "revoke"
	broadcastSubject = "samuorictl.all"
)

func directSubject(controllerID string) string { return "samuorictl.c." + controllerID }

type envelope struct {
	Kind     string `json:"kind"`
	From     string `json:"from"`
	WorkerID string `json:"workerId"`
	Message  []byte `json:"message,omitempty"`
	At       int64  `json:"at"`
	MAC      []byte `json:"mac,omitempty"`
}

func (r *routing) sign(e *envelope) {
	e.MAC = nil
	raw, _ := json.Marshal(e)
	m := hmac.New(sha256.New, r.key)
	m.Write(raw)
	e.MAC = m.Sum(nil)
}

func (r *routing) verify(e envelope) bool {
	got := e.MAC
	r.sign(&e)
	return hmac.Equal(got, e.MAC) && time.Since(time.Unix(0, e.At)).Abs() < time.Minute
}

// UseCluster enables routing across controllers: streams are recorded in worker_sessions, and messages for a
// worker connected elsewhere are forwarded to its controller.
func (s *Server) UseCluster(ctx context.Context, owner cluster.Ownership, bus Transport, key []byte) error {
	r := &routing{owner: owner, bus: bus, key: key}
	handle := func(raw []byte) {
		var e envelope
		if json.Unmarshal(raw, &e) != nil || !r.verify(e) || e.From == owner.ControllerID() {
			return
		}
		switch e.Kind {
		case signalDeliver:
			var m workerv1.ControllerMessage
			if proto.Unmarshal(e.Message, &m) == nil {
				if err := s.sendHere(e.WorkerID, &m); err != nil && !errors.Is(err, ErrNotConnected) {
					s.log.Warn("forwarded worker message", "worker", e.WorkerID, "error", err)
				}
			}
		case signalDisconnect, signalRevoke:
			s.disconnectHere(e.WorkerID, e.Kind == signalRevoke)
		}
	}
	for _, subject := range []string{directSubject(owner.ControllerID()), broadcastSubject} {
		stop, err := bus.Subscribe(subject, handle)
		if err != nil {
			return err
		}
		go func() {
			<-ctx.Done()
			stop()
		}()
	}
	s.mu.Lock()
	s.routing = r
	s.mu.Unlock()
	if f, ok := owner.(freshness); ok {
		go s.fence(ctx, f)
	}
	return nil
}

type freshness interface{ Fresh() bool }

// fence closes every worker stream while this controller cannot record its heartbeat: its peers then treat it as
// gone and its workers must reconnect to a controller that can renew their leases.
func (s *Server) fence(ctx context.Context, f freshness) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if f.Fresh() {
			continue
		}
		s.mu.Lock()
		n := len(s.connected)
		for id, c := range s.connected {
			c.close()
			delete(s.connected, id)
		}
		s.mu.Unlock()
		if n > 0 {
			s.log.Warn("this controller cannot record its heartbeat; worker streams closed so workers move to another controller", "streams", n)
		}
	}
}

// fenced reports whether this controller must refuse worker streams.
func (s *Server) fenced() bool {
	s.mu.Lock()
	r := s.routing
	s.mu.Unlock()
	if r == nil {
		return false
	}
	f, ok := r.owner.(freshness)
	return ok && !f.Fresh()
}

func (r *routing) publish(subject string, e envelope) error {
	e.From, e.At = r.owner.ControllerID(), time.Now().UnixNano()
	r.sign(&e)
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.bus.Publish(subject, raw)
}

// remoteOwner returns the live controller holding workerID's stream, if another one does.
func (s *Server) remoteOwner(workerID string) (string, bool) {
	s.mu.Lock()
	r := s.routing
	s.mu.Unlock()
	if r == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	orphaned, args := r.owner.Orphaned("controller_id")
	var id string
	err := s.store.db.SQL.QueryRowContext(ctx, s.store.db.Rebind(`SELECT controller_id FROM worker_sessions WHERE worker_id = ? AND NOT `+orphaned), append([]any{workerID}, args...)...).Scan(&id)
	if err != nil || id == r.owner.ControllerID() {
		return "", false
	}
	return id, true
}

func (s *Server) forward(workerID string, m *workerv1.ControllerMessage) error {
	controller, ok := s.remoteOwner(workerID)
	if !ok {
		return ErrNotConnected
	}
	raw, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	return s.routing.publish(directSubject(controller), envelope{Kind: signalDeliver, WorkerID: workerID, Message: raw})
}

func (s *Server) recordSession(ctx context.Context, workerID string) {
	if s.routing == nil {
		return
	}
	_, err := s.store.db.SQL.ExecContext(ctx, s.store.db.Rebind(`INSERT INTO worker_sessions (worker_id, controller_id, connected_at) VALUES (?, ?, ?)
		ON CONFLICT (worker_id) DO UPDATE SET controller_id = excluded.controller_id, connected_at = excluded.connected_at`), workerID, s.routing.owner.ControllerID(), s.store.now().UTC())
	if err != nil {
		s.log.Error("recording worker session", "worker", workerID, "error", err)
	}
	// A stream the worker left open on another controller is closed there, so exactly one controller serves it.
	_ = s.routing.publish(broadcastSubject, envelope{Kind: signalDisconnect, WorkerID: workerID})
}

func (s *Server) dropSession(workerID string) {
	if s.routing == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.store.db.SQL.ExecContext(ctx, s.store.db.Rebind(`DELETE FROM worker_sessions WHERE worker_id = ? AND controller_id = ?`), workerID, s.routing.owner.ControllerID())
}
