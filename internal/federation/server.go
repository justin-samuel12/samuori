package federation

import (
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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/validation"
)

// DelegationRequest is what the originator sends once the source is staged.
type DelegationRequest struct {
	Spec     json.RawMessage `json:"spec"`
	Location string          `json:"location"`
}

// Status is what the executor reports about a delegation.
type Status struct {
	RunID        string             `json:"runId"`
	State        string             `json:"state"`
	FailureClass string             `json:"failureClass,omitempty"`
	Reason       string             `json:"reason,omitempty"`
	Digest       string             `json:"artifactDigest,omitempty"`
	Size         int64              `json:"artifactSize,omitempty"`
	Validation   *ValidationSummary `json:"validation,omitempty"`
	Checks       []validation.Check `json:"checks,omitempty"`
}

type ValidationSummary struct {
	State   string `json:"state"`
	Verdict string `json:"verdict,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Executor serves delegations from peers: it ingests their source under this installation's upload rules and runs
// their builds through its own admission, policies and workers.
type Executor struct {
	Store       *Store
	Sources     *sources.Service
	Specs       *specs.Service
	Builds      *builds.Service
	Validations *validation.Service
	Blobs       *storage.Blobs
	CA          *pki.Authority
	OrgID       string
	Log         *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	hostname []string
}

var delegationID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

type peerKey struct{}

// authenticate matches the client certificate to an enabled peer by its pinned CA and organization.
func (e *Executor) authenticate(r *http.Request) (Peer, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Peer{}, pki.ErrPeerIdentity
	}
	raw := make([][]byte, len(r.TLS.PeerCertificates))
	for i, c := range r.TLS.PeerCertificates {
		raw[i] = c.Raw
	}
	peers, err := e.Store.ActivePeers(r.Context())
	if err != nil {
		return Peer{}, err
	}
	for _, p := range peers {
		if p.OrgID == e.OrgID && pki.VerifyFederationPeer(raw, p.CAPin, p.RemoteOrgID, "", x509.ExtKeyUsageClientAuth, time.Now()) == nil {
			return p, nil
		}
	}
	return Peer{}, pki.ErrPeerIdentity
}

func (e *Executor) certificate(hosts []string) (tls.Certificate, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.cert; c != nil && time.Until(c.Leaf.NotAfter) > 24*time.Hour {
		return *c, nil
	}
	c, err := e.CA.Issuing().FederationCertificate(e.OrgID, hosts, 7*24*time.Hour)
	if err != nil {
		return tls.Certificate{}, err
	}
	e.cert = &c
	return c, nil
}

// Serve accepts peers on ln until ctx ends. The server presents this installation's federation certificate and
// requires a client certificate; which peer it belongs to is decided per request against the registered pins.
func (e *Executor) Serve(ctx context.Context, ln net.Listener, hosts []string) error {
	if _, err := e.certificate(hosts); err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           e.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(e.Log.Handler(), slog.LevelWarn),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				c, err := e.certificate(hosts)
				return &c, err
			}},
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close()
		}
	}()
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler routes the federation API; every route requires an authenticated peer.
func (e *Executor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /federation/v1/delegations/{id}/source", e.source)
	mux.HandleFunc("POST /federation/v1/delegations/{id}", e.create)
	mux.HandleFunc("GET /federation/v1/delegations/{id}", e.status)
	mux.HandleFunc("GET /federation/v1/delegations/{id}/image", e.image)
	mux.HandleFunc("POST /federation/v1/delegations/{id}/cancel", e.cancel)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := e.authenticate(r)
		if err != nil {
			http.Error(w, "this installation does not know the presented certificate", http.StatusUnauthorized)
			return
		}
		if !delegationID.MatchString(r.PathValue("id")) && !delegationID.MatchString(idFromPath(r.URL.Path)) {
			http.Error(w, "invalid delegation ID", http.StatusBadRequest)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer)))
	})
}

func idFromPath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/federation/v1/delegations/"), "/")
	return parts[0]
}

func peerOf(r *http.Request) Peer { return r.Context().Value(peerKey{}).(Peer) }

// principal acts for a peer inside its hosting project, with a developer's rights there and nowhere else.
func principal(peer Peer, g Grant) (authz.Principal, []authz.Role) {
	return authz.Principal{ID: "federation:" + peer.ID, OrgID: g.OrgID, Kind: authz.Service, Bindings: []authz.Binding{{Role: authz.Developer, ScopeType: authz.ScopeProject, ScopeID: g.ProjectID}}},
		[]authz.Role{authz.Developer}
}

func (e *Executor) staged(peer Peer, id string) string {
	return filepath.Join(e.Sources.Staging(), "federation-"+peer.ID+"-"+id+".tar")
}

func (e *Executor) source(w http.ResponseWriter, r *http.Request) {
	peer, id := peerOf(r), r.PathValue("id")
	if _, err := e.inbound(r.Context(), peer, id); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	max := e.Sources.Limits().MaxArchiveBytes
	path := e.staged(peer, id)
	f, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		e.fail(w, err)
		return
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		_ = os.Remove(path + ".partial")
		http.Error(w, fmt.Sprintf("the source is larger than this installation accepts (%d MiB)", max>>20), http.StatusRequestEntityTooLarge)
		return
	}
	if err == nil {
		err = os.Rename(path+".partial", path)
	}
	if err != nil {
		e.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type inboundRow struct {
	grantID, runID string
}

func (e *Executor) inbound(ctx context.Context, peer Peer, id string) (inboundRow, error) {
	var x inboundRow
	var run sql.NullString
	err := e.Store.db.SQL.QueryRowContext(ctx, e.Store.db.Rebind(`SELECT grant_id, run_id FROM federation_inbound WHERE peer_id = ? AND delegation_id = ?`), peer.ID, id).Scan(&x.grantID, &run)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.runID = run.String
	return x, err
}

// create accepts a delegation once: repeating it returns the run it created (idempotent forwarding).
func (e *Executor) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	peer, id := peerOf(r), r.PathValue("id")
	if in, err := e.inbound(ctx, peer, id); err == nil && in.runID != "" {
		e.writeStatus(w, r, in.runID)
		return
	}
	var req DelegationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || len(req.Spec) == 0 || req.Location == "" {
		http.Error(w, "the request needs a spec and a location", http.StatusBadRequest)
		return
	}
	g, err := e.Store.grantFor(ctx, peer, req.Location)
	if err != nil {
		e.refuse(ctx, peer, id, "no grant covers location "+req.Location)
		http.Error(w, "this installation has not granted builds in that location to you", http.StatusForbidden)
		return
	}
	var active int
	if err := e.Store.db.SQL.QueryRowContext(ctx, e.Store.db.Rebind(`SELECT COUNT(*) FROM federation_inbound i JOIN build_runs b ON b.id = i.run_id WHERE i.grant_id = ? AND b.state IN ('queued', 'assigned', 'preparing', 'building', 'committing')`), g.ID).Scan(&active); err != nil {
		e.fail(w, err)
		return
	}
	if active >= g.MaxActive {
		w.Header().Set("Retry-After", "60")
		http.Error(w, fmt.Sprintf("the grant allows %d active builds; try again later", g.MaxActive), http.StatusTooManyRequests)
		return
	}
	archive := e.staged(peer, id)
	if _, err := os.Stat(archive); err != nil {
		http.Error(w, "upload the source first", http.StatusConflict)
		return
	}
	p, roles := principal(peer, g)
	snap, err := e.Sources.Ingest(ctx, p, roles, g.ProjectID, archive, "")
	_ = os.Remove(archive)
	var rej *sources.Rejection
	if errors.As(err, &rej) {
		e.refuse(ctx, peer, id, "source refused: "+rej.Message)
		http.Error(w, "the source was refused: "+rej.Message, http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		e.fail(w, err)
		return
	}
	specID, rev, err := e.spec(ctx, p, roles, g.ProjectID, req.Spec)
	if err != nil {
		var inv *specs.Invalid
		if errors.As(err, &inv) {
			http.Error(w, "the spec was refused: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		e.fail(w, err)
		return
	}
	if _, err := e.Store.db.SQL.ExecContext(ctx, e.Store.db.Rebind(`INSERT INTO federation_inbound (peer_id, delegation_id, org_id, grant_id, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (peer_id, delegation_id) DO NOTHING`),
		peer.ID, id, g.OrgID, g.ID, time.Now().UTC()); err != nil {
		e.fail(w, err)
		return
	}
	run, err := e.Builds.Create(ctx, p, roles, specID, rev, snap.ID, builds.CreateOptions{Location: req.Location}, "federation-"+peer.ID+"-"+id, "")
	if err != nil {
		e.fail(w, err)
		return
	}
	if _, err := e.Store.db.SQL.ExecContext(ctx, e.Store.db.Rebind(`UPDATE federation_inbound SET run_id = ? WHERE peer_id = ? AND delegation_id = ?`), run.ID, peer.ID, id); err != nil {
		e.fail(w, err)
		return
	}
	_ = e.Store.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, e.Store.db, tx, audit.Event{Actor: p, Action: "federation.delegation.accept", ResourceType: "build", ResourceID: run.ID, Outcome: run.State, Reason: "peer " + peer.Name + ", delegation " + id})
	})
	e.writeStatus(w, r, run.ID)
}

func (e *Executor) refuse(ctx context.Context, peer Peer, id, reason string) {
	_ = e.Store.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, e.Store.db, tx, audit.Event{Actor: authz.Principal{ID: "federation:" + peer.ID, OrgID: peer.OrgID, Kind: authz.Service}, Action: "federation.delegation.accept", ResourceType: "federation-peer", ResourceID: peer.ID, Outcome: "denied", Reason: reason + " (delegation " + id + ")"})
	})
}

// spec records the peer's document as a spec of the hosting project, reusing the spec of the same name and adding a
// revision when the document changed.
func (e *Executor) spec(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, doc json.RawMessage) (string, int, error) {
	sp, rev, err := e.Specs.Create(ctx, p, roles, projectID, doc, "federation", "delegated by a federation peer", "")
	if err == nil {
		return sp.ID, rev.Revision, nil
	}
	if !errors.Is(err, specs.ErrConflict) {
		return "", 0, err
	}
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(doc, &meta)
	list, err := e.Specs.List(ctx, p, roles, projectID, meta.Metadata.Name, "", 1)
	if err != nil {
		return "", 0, err
	}
	for _, s := range list {
		if s.Name != meta.Metadata.Name {
			continue
		}
		_, latest, err := e.Specs.Revision(ctx, p, roles, s.ID, s.LatestRevision)
		if err != nil {
			return "", 0, err
		}
		if digest, err := e.Specs.Digest(ctx, p.OrgID, doc); err == nil && digest == latest.Digest {
			return s.ID, s.LatestRevision, nil
		}
		next, err := e.Specs.AddRevision(ctx, p, roles, s.ID, specs.ETag(s.LatestRevision), doc, "federation", "delegated by a federation peer", "")
		return s.ID, next.Revision, err
	}
	return "", 0, specs.ErrConflict
}

func (e *Executor) runFor(r *http.Request) (string, bool) {
	in, err := e.inbound(r.Context(), peerOf(r), r.PathValue("id"))
	return in.runID, err == nil && in.runID != ""
}

func (e *Executor) status(w http.ResponseWriter, r *http.Request) {
	runID, ok := e.runFor(r)
	if !ok {
		http.Error(w, "unknown delegation", http.StatusNotFound)
		return
	}
	e.writeStatus(w, r, runID)
}

func (e *Executor) writeStatus(w http.ResponseWriter, r *http.Request, runID string) {
	run, err := e.Builds.RunByID(r.Context(), runID)
	if err != nil {
		e.fail(w, err)
		return
	}
	st := Status{RunID: run.ID, State: run.State, FailureClass: run.FailureClass, Reason: run.BlockedReason, Digest: run.ArtifactDigest}
	if run.State == builds.Succeeded {
		if size, err := e.Blobs.Size(run.ArtifactDigest); err == nil {
			st.Size = size
		}
		if v, ok, err := e.Validations.Latest(r.Context(), run.ID); err == nil && ok {
			st.Validation = &ValidationSummary{State: v.State, Verdict: v.Verdict, Reason: v.Reason}
			for _, c := range v.Checks {
				if c.Origin == "worker" {
					st.Checks = append(st.Checks, c)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// image streams the built image only after the build and this installation's validation succeeded.
func (e *Executor) image(w http.ResponseWriter, r *http.Request) {
	runID, ok := e.runFor(r)
	if !ok {
		http.Error(w, "unknown delegation", http.StatusNotFound)
		return
	}
	run, err := e.Builds.RunByID(r.Context(), runID)
	if err != nil {
		e.fail(w, err)
		return
	}
	v, ok, err := e.Validations.Latest(r.Context(), run.ID)
	if run.State != builds.Succeeded || err != nil || !ok || v.State != validation.Completed || v.Verdict != "passed" {
		http.Error(w, "the image is not available: the build or its validation has not passed", http.StatusConflict)
		return
	}
	f, err := e.Blobs.Open(run.ArtifactDigest)
	if err != nil {
		e.fail(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Samuori-Digest", run.ArtifactDigest)
	_, _ = io.Copy(w, f)
}

func (e *Executor) cancel(w http.ResponseWriter, r *http.Request) {
	peer, id := peerOf(r), r.PathValue("id")
	in, err := e.inbound(r.Context(), peer, id)
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		e.fail(w, err)
		return
	}
	if in.runID != "" {
		grants, _ := e.Store.grants(r.Context(), `id = ?`, in.grantID)
		if len(grants) == 1 {
			p, roles := principal(peer, grants[0])
			if _, err := e.Builds.Cancel(r.Context(), p, roles, in.runID, ""); err != nil && !errors.Is(err, builds.ErrTerminal) {
				e.fail(w, err)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Executor) fail(w http.ResponseWriter, err error) {
	e.Log.Error("federation request", "error", err)
	http.Error(w, "the request could not be completed", http.StatusInternalServerError)
}
