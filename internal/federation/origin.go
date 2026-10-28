package federation

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/netguard"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/validation"
)

// Origin hands builds bound for federation routes to their peers and brings their results back. It runs on the
// leader; every call it makes is idempotent, so a new leader simply continues.
type Origin struct {
	Store     *Store
	Builds    *builds.Service
	Scheduler *builds.Scheduler
	Sources   *sources.Service
	CA        *pki.Authority
	Log       *slog.Logger
	// AllowPrivatePeers lets peers live on private networks; link-local and metadata addresses are always refused.
	AllowPrivatePeers bool
}

func (o *Origin) client(peer Peer) (*http.Client, error) {
	u, err := url.Parse(peer.Endpoint)
	if err != nil {
		return nil, err
	}
	cert, err := o.CA.Issuing().FederationCertificate(peer.OrgID, nil, time.Hour)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		// The chain and name are checked against the pinned peer CA in VerifyPeerCertificate; system roots are never used.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			return pki.VerifyFederationPeer(raw, peer.CAPin, peer.RemoteOrgID, host, x509.ExtKeyUsageServerAuth, time.Now())
		}}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: netguard.Control(o.AllowPrivatePeers)}
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: cfg, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, ResponseHeaderTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// Step runs one pass: delegate queued runs, then advance every open delegation.
func (o *Origin) Step(ctx context.Context) error {
	queued, err := o.Builds.QueuedForPeers(ctx, 50)
	if err != nil {
		return err
	}
	for _, r := range queued {
		if err := o.delegate(ctx, r); err != nil && !errors.Is(err, builds.ErrNotQueued) {
			o.Log.Error("delegating a build", "run", r.ID, "error", err)
		}
	}
	open, err := o.Store.delegations(ctx, `state IN (?, ?, ?) ORDER BY updated_at LIMIT 100`, Pending, Accepted, Abandoned)
	if err != nil {
		return err
	}
	for _, d := range open {
		if err := o.advance(ctx, d); err != nil {
			o.Log.Warn("federation delegation", "delegation", d.ID, "peer", d.PeerID, "error", err)
		}
	}
	return nil
}

func (o *Origin) delegate(ctx context.Context, r builds.Run) error {
	route, err := o.Store.route(ctx, r.OrgID, r.PreferredLocation)
	if err != nil {
		return err
	}
	peer, err := o.Store.peer(ctx, r.OrgID, route.PeerID)
	if err != nil {
		return err
	}
	return o.Store.db.InTx(ctx, func(tx *sql.Tx) error {
		if peer.Disabled {
			return builds.ErrNotQueued
		}
		attempt, err := o.Builds.Delegate(ctx, tx, r, peer.ID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		id := ids.New()
		if _, err := tx.ExecContext(ctx, o.Store.db.Rebind(`INSERT INTO federation_delegations (id, org_id, run_id, peer_id, remote_location, attempt_id, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, r.OrgID, r.ID, peer.ID, route.RemoteLocation, attempt, Pending, now, now); err != nil {
			return err
		}
		return audit.Append(ctx, o.Store.db, tx, audit.Event{Actor: authz.Principal{OrgID: r.OrgID, Kind: authz.System}, Action: "federation.delegate", ResourceType: "build", ResourceID: r.ID, Outcome: "succeeded", Reason: "peer " + peer.Name + ", location " + route.RemoteLocation + ", delegation " + id})
	})
}

// ErrUnreachable means the peer could not be reached or answered with a server error; the delegation waits.
var ErrUnreachable = errors.New("federation: the peer is unreachable")

type refusal struct {
	status int
	reason string
}

func (e *refusal) Error() string { return fmt.Sprintf("the peer refused (%d): %s", e.status, e.reason) }

func (o *Origin) call(ctx context.Context, c *http.Client, peer Peer, method, path string, body io.Reader, out any) (*http.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, peer.Endpoint+"/federation/v1/delegations/"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		return resp, fmt.Errorf("%w: HTTP %d", ErrUnreachable, resp.StatusCode)
	case resp.StatusCode >= 400:
		return resp, &refusal{resp.StatusCode, strings.TrimSpace(string(raw))}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("%w: unreadable answer", ErrUnreachable)
		}
	}
	return resp, nil
}

func (o *Origin) advance(ctx context.Context, d Delegation) error {
	peer, err := o.Store.peer(ctx, d.OrgID, d.PeerID)
	if err != nil {
		return err
	}
	c, err := o.client(peer)
	if err != nil {
		return err
	}
	run, err := o.Builds.RunByID(ctx, d.RunID)
	if err != nil {
		return err
	}
	if d.State == Abandoned || run.State == builds.Cancelled {
		return o.withdraw(ctx, c, peer, d)
	}
	if d.State == Pending {
		err = o.send(ctx, c, peer, d, run)
	} else {
		err = o.poll(ctx, c, peer, d, run)
	}
	if errors.Is(err, ErrUnreachable) {
		o.mark(ctx, d.ID, `unreachable_since = COALESCE(unreachable_since, ?)`, time.Now().UTC())
	}
	var ref *refusal
	if errors.As(err, &ref) {
		return o.finish(ctx, d, Failed, "the peer refused the build: "+ref.reason)
	}
	return err
}

func (o *Origin) mark(ctx context.Context, id, set string, args ...any) {
	_, _ = o.Store.db.SQL.ExecContext(ctx, o.Store.db.Rebind(`UPDATE federation_delegations SET `+set+`, updated_at = ? WHERE id = ?`), append(args, time.Now().UTC(), id)...)
}

// send stages the snapshot's files at the peer, then asks it to build them with the spec.
func (o *Origin) send(ctx context.Context, c *http.Client, peer Peer, d Delegation, run builds.Run) error {
	archive, err := o.archive(ctx, run.SnapshotID)
	if err != nil {
		return err
	}
	if _, err := o.call(ctx, c, peer, http.MethodPut, d.ID+"/source", bytes.NewReader(archive), nil); err != nil {
		return err
	}
	var doc string
	if err := o.Store.db.SQL.QueryRowContext(ctx, o.Store.db.Rebind(`SELECT document FROM spec_revisions WHERE spec_id = ? AND revision = ?`), run.SpecID, run.SpecRevision).Scan(&doc); err != nil {
		return err
	}
	body, _ := json.Marshal(DelegationRequest{Spec: json.RawMessage(doc), Location: d.RemoteLocation})
	var st Status
	if _, err := o.call(ctx, c, peer, http.MethodPost, d.ID, bytes.NewReader(body), &st); err != nil {
		return err
	}
	now := time.Now().UTC()
	o.mark(ctx, d.ID, `state = ?, remote_run_id = ?, remote_state = ?, last_contact_at = ?, unreachable_since = NULL`, Accepted, st.RunID, st.State, now)
	d.State, d.RemoteRunID = Accepted, st.RunID
	return o.settle(ctx, c, peer, d, st)
}

func (o *Origin) poll(ctx context.Context, c *http.Client, peer Peer, d Delegation, run builds.Run) error {
	var st Status
	if _, err := o.call(ctx, c, peer, http.MethodGet, d.ID, nil, &st); err != nil {
		return err
	}
	o.mark(ctx, d.ID, `remote_state = ?, last_contact_at = ?, unreachable_since = NULL`, st.State, time.Now().UTC())
	return o.settle(ctx, c, peer, d, st)
}

// settle acts on the peer's status: a final failure fails the run; a validated image is fetched and validated here.
func (o *Origin) settle(ctx context.Context, c *http.Client, peer Peer, d Delegation, st Status) error {
	switch st.State {
	case builds.Blocked, builds.Failed, builds.TimedOut, builds.Expired, builds.Cancelled:
		reason := st.Reason
		if reason == "" {
			reason = st.FailureClass
		}
		return o.finish(ctx, d, Failed, fmt.Sprintf("the build %s at peer %s: %s", st.State, peer.Name, reason))
	case builds.Succeeded:
	default:
		return nil
	}
	if st.Validation == nil || st.Validation.State == validation.Pending || st.Validation.State == validation.Running {
		return nil
	}
	if st.Validation.State != validation.Completed || st.Validation.Verdict != "passed" {
		return o.finish(ctx, d, Failed, fmt.Sprintf("the image did not pass validation at peer %s: %s %s", peer.Name, st.Validation.Verdict, st.Validation.Reason))
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, peer.Endpoint+"/federation/v1/delegations/"+d.ID+"/image", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: image HTTP %d", ErrUnreachable, resp.StatusCode)
	}
	checks := make([]validation.Check, 0, len(st.Checks))
	for _, ch := range st.Checks {
		ch.Origin = "worker"
		ch.Reason = strings.TrimSpace("reported by federation peer " + peer.Name + ": " + ch.Reason)
		checks = append(checks, ch)
	}
	err = o.Scheduler.CompleteDelegated(ctx, d.RunID, d.AttemptID, st.Digest, st.Size, resp.Body, checks)
	if errors.Is(err, builds.ErrNotDelegated) {
		return o.finish(ctx, d, Cancelled, "the run here was no longer waiting for the peer")
	}
	if err != nil {
		return err
	}
	o.mark(ctx, d.ID, `state = ?, artifact_digest = ?`, Completed, st.Digest)
	return o.audit(ctx, d, "succeeded", "image "+st.Digest+" from peer "+peer.Name)
}

func (o *Origin) finish(ctx context.Context, d Delegation, state, detail string) error {
	return o.Store.db.InTx(ctx, func(tx *sql.Tx) error {
		if state == Failed {
			if err := o.Builds.FailDelegated(ctx, tx, d.RunID, "application", detail); err != nil && !errors.Is(err, builds.ErrNotDelegated) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, o.Store.db.Rebind(`UPDATE federation_delegations SET state = ?, detail = ?, updated_at = ? WHERE id = ?`), state, clip(detail), time.Now().UTC(), d.ID); err != nil {
			return err
		}
		return audit.Append(ctx, o.Store.db, tx, audit.Event{Actor: authz.Principal{OrgID: d.OrgID, Kind: authz.System}, Action: "federation.delegation", ResourceType: "build", ResourceID: d.RunID, Outcome: state, Reason: clip(detail)})
	})
}

func (o *Origin) audit(ctx context.Context, d Delegation, outcome, reason string) error {
	return o.Store.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, o.Store.db, tx, audit.Event{Actor: authz.Principal{OrgID: d.OrgID, Kind: authz.System}, Action: "federation.delegation", ResourceType: "build", ResourceID: d.RunID, Outcome: outcome, Reason: reason})
	})
}

// withdraw tells the peer to stop a delegation this installation cancelled or abandoned; until the peer confirms,
// the delegation keeps trying.
func (o *Origin) withdraw(ctx context.Context, c *http.Client, peer Peer, d Delegation) error {
	if _, err := o.call(ctx, c, peer, http.MethodPost, d.ID+"/cancel", nil, nil); err != nil {
		var ref *refusal
		if !errors.As(err, &ref) {
			return err
		}
	}
	if d.State == Abandoned {
		o.mark(ctx, d.ID, `state = ?, detail = ?, last_contact_at = ?`, Withdrawn, clip(d.Detail+" The peer was told to stop."), time.Now().UTC())
		return nil
	}
	o.mark(ctx, d.ID, `state = ?`, Cancelled)
	return nil
}

// Abandon gives up on a delegation whose peer cannot finish it: the run fails here at once, the peer is told to stop
// when it can be reached, and any later result from it is refused (ADR 0037).
func (s *Store) Abandon(ctx context.Context, p authz.Principal, b *builds.Service, id, reason, requestID string) error {
	if err := admin(p); err != nil {
		return err
	}
	if len(strings.TrimSpace(reason)) < 10 {
		return &Invalid{"give a reason of at least 10 characters"}
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var runID string
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT run_id FROM federation_delegations WHERE id = ? AND org_id = ? AND state IN (?, ?)`), id, p.OrgID, Pending, Accepted).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		detail := "abandoned by an administrator: " + reason
		if err := b.FailDelegated(ctx, tx, runID, "infrastructure", detail); err != nil && !errors.Is(err, builds.ErrNotDelegated) {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE federation_delegations SET state = ?, detail = ?, updated_at = ? WHERE id = ?`), Abandoned, clip(detail), s.now().UTC(), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, p, "federation.delegation.abandon", "build", runID, reason, requestID)
	})
}

// archive writes the snapshot's files as a tar, as an upload of them would look.
func (o *Origin) archive(ctx context.Context, snapshotID string) ([]byte, error) {
	snap, err := o.Sources.SnapshotByID(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	m, err := o.Sources.Manifest(snap)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range m.Files {
		body, err := o.Sources.ReadFile(f)
		if err != nil {
			return nil, err
		}
		mode := int64(0o644)
		if f.Executable {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	err = tw.Close()
	return buf.Bytes(), err
}

func clip(s string) string {
	if len(s) > 1000 {
		return s[:1000] + "…"
	}
	return s
}
