package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/signing"
	"samuori.invalid/samuori/internal/telemetry"
	"samuori.invalid/samuori/internal/validation"
)

const SubjectRequested = "samuori.publications.requested"

const (
	Requested  = "requested"
	Awaiting   = "awaiting_approval"
	Approved   = "approved"
	Rejected   = "rejected"
	Expired    = "expired"
	GateCheck  = "gate_check"
	Publishing = "publishing"
	Verifying  = "verifying"
	Published  = "published"
	Blocked    = "blocked"
	Failed     = "failed"
)

type Gate struct {
	Gate    string `json:"gate"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

type Publication struct {
	ID, OrgID, ProjectID, Kind  string
	ArtifactID, ArtifactDigest  string
	DestinationID               string
	Tags                        []string
	State, Reason               string
	Gates                       []Gate
	ImageDigest, DestinationRef string
	ApprovalID                  string
	RequestedBy                 string
	CreatedAt, UpdatedAt        time.Time
	// A promotion copies an image already published by SourcePublicationID, and a rollback moves tags back to it,
	// after the ValidationRunID revalidation.
	SourcePublicationID, ValidationRunID string
	SignatureRef, AttestationRef         string
	SignatureStorage                     string
	DeprecatedAt, RetiredAt              *time.Time
	DeprecationReason, RetirementReason  string
}

var publishRoles = []authz.Role{authz.Publisher, authz.Maintainer}

// BuildTag is the immutable reference tag every publication carries.
func BuildTag(imageDigest string) string {
	return "samuori-" + strings.TrimPrefix(imageDigest, "sha256:")[:12]
}

func (s *Service) team(ctx context.Context, projectID string) (string, error) {
	var team string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projectID).Scan(&team)
	return team, err
}

// Request records a publication after checking, at request time, that the caller may publish the artifact and
// that the destination is open to its project (SEC-PUB-01). The gate repeats these checks when it runs.
func (s *Service) Request(ctx context.Context, p authz.Principal, roles []authz.Role, artifactID, destinationID string, tags []string, key, requestID string) (Publication, error) {
	art, err := s.validations.Artifact(ctx, p, roles, artifactID)
	if err != nil {
		return Publication{}, ErrNotFound
	}
	team, err := s.team(ctx, art.ProjectID)
	if err != nil {
		return Publication{}, err
	}
	if !authz.Can(p, publishRoles, authz.Resource{OrgID: art.OrgID, TeamID: team, ProjectID: art.ProjectID}) {
		return Publication{}, ErrForbidden
	}
	var existing string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM publications WHERE requested_by = ? AND idempotency_key = ?`), p.ID, key).Scan(&existing); err == nil {
		return s.load(ctx, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	d, err := s.destination(ctx, p.OrgID, destinationID)
	if err != nil || !slices.Contains(d.AllowedProjects, art.ProjectID) {
		return Publication{}, ErrForbidden
	}
	if d.PromotionOnly {
		return Publication{}, &Invalid{"this destination accepts only promotions of images already published elsewhere"}
	}
	if len(tags) == 0 {
		if tags, err = s.DefaultTags(ctx, art.BuildRunID); err != nil {
			return Publication{}, err
		}
	}
	clean, err := cleanTags(tags)
	if err != nil {
		return Publication{}, err
	}
	pub := Publication{ID: ids.New(), OrgID: p.OrgID, ProjectID: art.ProjectID, Kind: "publish", ArtifactID: art.ID, ArtifactDigest: art.Digest, DestinationID: d.ID, Tags: clean,
		State: Requested, Gates: []Gate{}, RequestedBy: p.ID, CreatedAt: s.now().UTC()}
	return s.record(ctx, pub, d, p, existing, key, requestID)
}

// Promote copies an image another publication already published to a further destination, by digest and without
// rebuilding. It first revalidates the image against current policy and exceptions; the gate then requires that
// revalidation to pass and re-verifies the source signature (ADR 0003, D19).
func (s *Service) Promote(ctx context.Context, p authz.Principal, roles []authz.Role, sourceID, destinationID string, tags []string, key, requestID string) (Publication, error) {
	src, err := s.Get(ctx, p, roles, sourceID)
	if err != nil {
		return Publication{}, err
	}
	if src.State != Published {
		return Publication{}, &Invalid{"only a published image can be promoted"}
	}
	team, err := s.team(ctx, src.ProjectID)
	if err != nil {
		return Publication{}, err
	}
	if !authz.Can(p, publishRoles, authz.Resource{OrgID: src.OrgID, TeamID: team, ProjectID: src.ProjectID}) {
		return Publication{}, ErrForbidden
	}
	var existing string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id FROM publications WHERE requested_by = ? AND idempotency_key = ?`), p.ID, key).Scan(&existing); err == nil {
		return s.load(ctx, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	d, err := s.destination(ctx, p.OrgID, destinationID)
	if err != nil || !slices.Contains(d.AllowedProjects, src.ProjectID) {
		return Publication{}, ErrForbidden
	}
	if d.ID == src.DestinationID {
		return Publication{}, &Invalid{"the image is already published to this destination"}
	}
	clean, err := cleanTags(tags)
	if err != nil {
		return Publication{}, err
	}
	v, err := s.validations.Revalidate(ctx, p, roles, src.ArtifactID, "promote-"+key)
	if err != nil {
		return Publication{}, err
	}
	pub := Publication{ID: ids.New(), OrgID: p.OrgID, ProjectID: src.ProjectID, Kind: "promote", ArtifactID: src.ArtifactID, ArtifactDigest: src.ArtifactDigest, DestinationID: d.ID, Tags: clean,
		State: Requested, Gates: []Gate{}, RequestedBy: p.ID, CreatedAt: s.now().UTC(), SourcePublicationID: src.ID, ValidationRunID: v.ID}
	return s.record(ctx, pub, d, p, existing, key, requestID)
}

func (s *Service) record(ctx context.Context, pub Publication, d Destination, p authz.Principal, existing, key, requestID string) (Publication, error) {
	pub.UpdatedAt = pub.CreatedAt
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		return s.insert(ctx, tx, &pub, d, p, key, requestID)
	})
	if err != nil {
		if isUnique(err) {
			return s.load(ctx, existing)
		}
		return Publication{}, err
	}
	s.wake()
	return pub, nil
}

func cleanTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	clean := []string{}
	for _, t := range tags {
		if !tagPattern.MatchString(t) {
			return nil, &Invalid{fmt.Sprintf("tag %q is not a valid OCI tag", t)}
		}
		if strings.HasPrefix(t, "samuori-") {
			return nil, &Invalid{"tags starting with samuori- are reserved for build reference tags"}
		}
		if !seen[t] {
			seen[t] = true
			clean = append(clean, t)
		}
	}
	if len(clean) > 16 {
		return nil, &Invalid{"at most 16 tags"}
	}
	return clean, nil
}

// ApprovalTTL is how long an approval request stays open.
const ApprovalTTL = 72 * time.Hour

// insert records a publication; destinations that require approval hold it in awaiting_approval.
func (s *Service) insert(ctx context.Context, tx *sql.Tx, pub *Publication, d Destination, actor authz.Principal, key, requestID string) error {
	rawTags, _ := json.Marshal(pub.Tags)
	if d.RequiresApproval {
		if s.approvals == nil {
			return errors.New("this destination requires approval, but approvals are not configured")
		}
		pub.State = Awaiting
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO publications (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, gate_decisions, requested_by, idempotency_key, created_at, updated_at, source_publication_id, validation_run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, ?)`), pub.ID, pub.OrgID, pub.ProjectID, pub.Kind, pub.ArtifactID, pub.ArtifactDigest, pub.DestinationID, string(rawTags), pub.State, pub.RequestedBy, key, pub.CreatedAt, pub.UpdatedAt,
		nullable(pub.SourcePublicationID), nullable(pub.ValidationRunID)); err != nil {
		return err
	}
	if pub.State == Awaiting {
		verb := map[string]string{"publish": "Publish", "promote": "Promote", "rollback": "Roll back"}[pub.Kind]
		summary := fmt.Sprintf("%s %s to %s", verb, strings.TrimPrefix(pub.ArtifactDigest, "sha256:")[:12], d.Name)
		if len(pub.Tags) > 0 {
			summary += " as " + strings.Join(pub.Tags, ", ")
		}
		a, err := s.approvals.Create(ctx, tx, pub.OrgID, pub.ProjectID, approvals.SubjectPublication, pub.ID, summary, pub.RequestedBy, ApprovalTTL)
		if err != nil {
			return err
		}
		pub.ApprovalID = a.ID
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE publications SET approval_id = ? WHERE id = ?`), a.ID, pub.ID); err != nil {
			return err
		}
	} else if err := s.enqueue(ctx, tx, pub.ID); err != nil {
		return err
	}
	return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "publication.request", ResourceType: "publication", ResourceID: pub.ID, Outcome: pub.State, Reason: pub.Kind + " to " + d.Name, RequestID: requestID})
}

func (s *Service) enqueue(ctx context.Context, tx *sql.Tx, id string) error {
	payload, _ := json.Marshal(map[string]string{"publicationId": id})
	return s.db.EnqueueOutbox(ctx, tx, ids.New(), SubjectRequested, payload)
}

// UseApprovals connects approval decisions to publications held in awaiting_approval.
func (s *Service) UseApprovals(a *approvals.Service) {
	s.approvals = a
	a.OnDecision(approvals.SubjectPublication, func(ctx context.Context, tx *sql.Tx, ap approvals.Approval) error {
		to := map[string]string{approvals.Approved: Approved, approvals.Rejected: Rejected, approvals.Expired: Expired}[ap.State]
		if to == "" {
			return nil
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE publications SET state = ?, reason = ?, updated_at = ? WHERE id = ? AND state = 'awaiting_approval'`), to, nullable(ap.Reason), s.now().UTC(), ap.SubjectID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 && to == Approved {
			return s.enqueue(ctx, tx, ap.SubjectID)
		}
		return nil
	})
}

func (s *Service) Deliver(ctx context.Context, _ string, payload []byte) error {
	var p struct {
		PublicationID string `json:"publicationId"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.PublicationID == "" {
		return nil
	}
	return s.Process(ctx, p.PublicationID)
}

// Reconcile resumes publications interrupted by a restart and retries ones waiting for a busy destination. Pushes
// are by digest, so resuming is idempotent.
func (s *Service) Reconcile(ctx context.Context) error {
	orphaned, args := s.owner.Orphaned("publishing_by")
	if _, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE destinations SET publishing_id = NULL, publishing_by = NULL WHERE publishing_id IS NOT NULL AND `+orphaned), args...); err != nil {
		return err
	}
	if _, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE publications SET state = 'requested' WHERE state IN ('gate_check', 'publishing', 'verifying')
		AND id NOT IN (SELECT publishing_id FROM destinations WHERE publishing_id IS NOT NULL)`)); err != nil {
		return err
	}
	if s.approvals != nil {
		if err := s.approvals.Expire(ctx); err != nil {
			return err
		}
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM publications WHERE state IN ('requested', 'approved') ORDER BY created_at LIMIT 100`))
	if err != nil {
		return err
	}
	var pending []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, id)
	}
	rows.Close()
	for _, id := range pending {
		if err := s.Process(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// RunReconciler repeats Reconcile until ctx ends.
func (s *Service) RunReconciler(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reconcile(ctx); err != nil {
				onErr(err)
			}
		}
	}
}

// ReleaseStale frees destination locks held by publications that are no longer active.
func (s *Service) ReleaseStale(ctx context.Context) error {
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE destinations SET publishing_id = NULL WHERE publishing_id IS NOT NULL
		AND publishing_id NOT IN (SELECT id FROM publications WHERE state IN ('gate_check', 'publishing', 'verifying'))`))
	return err
}

func (s *Service) setState(ctx context.Context, id, from, to, reason string, gates []Gate) (bool, error) {
	q, args := `UPDATE publications SET state = ?, updated_at = ?`, []any{to, s.now().UTC()}
	if reason != "" {
		q, args = q+`, reason = ?`, append(args, reason)
	}
	if gates != nil {
		raw, _ := json.Marshal(gates)
		q, args = q+`, gate_decisions = ?`, append(args, string(raw))
	}
	res, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(q+` WHERE id = ? AND state = ?`), append(args, id, from)...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Process takes one requested publication through gate_check, publishing and verifying. Publications to the same
// destination are serialized, so a tag check and the tag update are not interleaved with another Samuori publication.
func (s *Service) Process(ctx context.Context, id string) error {
	ctx, span := telemetry.Start(ctx, "publication.process", telemetry.String("samuori.publication.id", id))
	err := s.process(ctx, id)
	span.End(err)
	return err
}

func (s *Service) process(ctx context.Context, id string) error {
	pub, err := s.load(ctx, id)
	if err != nil || (pub.State != Requested && pub.State != Approved) {
		return err
	}
	if pub.ValidationRunID != "" {
		// A promotion waits for its revalidation; the reconciler and the validation's completion retry it.
		v, err := s.validations.ByID(ctx, pub.ValidationRunID)
		if err != nil || v.State == validation.Pending || v.State == validation.Running {
			return err
		}
	}
	res, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE destinations SET publishing_id = ?, publishing_by = ? WHERE id = ? AND publishing_id IS NULL`), id, s.owner.ControllerID(), pub.DestinationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	defer func() {
		_, _ = s.db.SQL.ExecContext(context.WithoutCancel(ctx), s.db.Rebind(`UPDATE destinations SET publishing_id = NULL, publishing_by = NULL WHERE id = ? AND publishing_id = ? AND publishing_by = ?`), pub.DestinationID, id, s.owner.ControllerID())
		s.wake()
	}()
	if ok, err := s.setState(ctx, id, pub.State, GateCheck, "", nil); err != nil || !ok {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	gates, target, image, allTags := s.gate(pctx, pub)
	for _, g := range gates {
		if g.Outcome != "allow" {
			return s.finish(ctx, pub, GateCheck, Blocked, "a publication gate denied it: "+g.Reason, gates, "", "")
		}
	}
	if ok, err := s.setState(ctx, id, GateCheck, Publishing, "", gates); err != nil || !ok {
		return err
	}
	archive, err := s.blobs.File(pub.ArtifactDigest)
	if err != nil {
		return s.finish(ctx, pub, Publishing, Failed, "the image is no longer in internal storage", nil, "", "")
	}
	archivePath := archive.Name()
	archive.Close()
	if err := s.pusher.Push(pctx, target, archivePath, image, allTags); err != nil {
		return s.finish(ctx, pub, Publishing, Failed, "the push failed: "+redact(err, target), nil, "", "")
	}
	if ok, err := s.setState(ctx, id, Publishing, Verifying, "", nil); err != nil || !ok {
		return err
	}
	if err := s.pusher.Verify(pctx, target, image, allTags); err != nil {
		return s.finish(ctx, pub, Verifying, Failed, "the destination did not verify: "+redact(err, target), nil, "", "")
	}
	d, err := s.destination(pctx, pub.OrgID, pub.DestinationID)
	if err != nil {
		return s.finish(ctx, pub, Verifying, Failed, "the destination is gone", nil, "", "")
	}
	sg, err := s.sign(pctx, pub, d, target, image)
	if err != nil {
		return s.finish(ctx, pub, Verifying, Failed, "signing failed: "+redact(err, target), nil, "", "")
	}
	pub.SignatureRef, pub.AttestationRef, pub.SignatureStorage = sg.SignatureRef, sg.AttestationRef, sg.Storage
	return s.finish(ctx, pub, Verifying, Published, "", nil, image, target.Endpoint+"/"+target.Repository+"@"+image)
}

// gate re-evaluates every condition at publish time (TOCTOU), including the live state of tags at the destination.
func (s *Service) gate(ctx context.Context, pub Publication) ([]Gate, Target, string, []string) {
	var gates []Gate
	add := func(gate string, ok bool, reason string) bool {
		outcome := "allow"
		if !ok {
			outcome = "deny"
		}
		gates = append(gates, Gate{Gate: gate, Outcome: outcome, Reason: reason})
		return ok
	}
	d, err := s.destination(ctx, pub.OrgID, pub.DestinationID)
	if !add("destination", err == nil && slices.Contains(d.AllowedProjects, pub.ProjectID), "the destination exists and is open to this project") {
		gates[len(gates)-1].Reason = "the destination no longer exists or is no longer open to this project"
		return gates, Target{}, "", nil
	}
	if d.PromotionOnly && pub.Kind == "publish" {
		add("kind", false, "the destination accepts only promotions")
		return gates, Target{}, "", nil
	}
	if pub.Kind == "promote" {
		src, err := s.load(ctx, pub.SourcePublicationID)
		ok := err == nil && src.State == Published && src.ArtifactDigest == pub.ArtifactDigest
		if !add("promotion-source", ok, "the source publication is published") {
			gates[len(gates)-1].Reason = "the source publication is not published"
			return gates, Target{}, "", nil
		}
		if err := s.verifySource(ctx, src); !add("source-signature", err == nil, "the source image's signature verifies with one of the organization's keys") {
			gates[len(gates)-1].Reason = "the source signature does not verify: " + clipText(err.Error(), 300)
			return gates, Target{}, "", nil
		}
	}
	if pub.Kind == "rollback" {
		src, err := s.load(ctx, pub.SourcePublicationID)
		ok := err == nil && src.State == Published && src.ArtifactDigest == pub.ArtifactDigest && src.DestinationID == pub.DestinationID && !d.ImmutableTags
		if !add("rollback-source", ok, "the earlier publication is still published at this destination and its tags may move") {
			gates[len(gates)-1].Reason = "the earlier publication is no longer published, or this destination's tags are immutable"
			return gates, Target{}, "", nil
		}
	}
	if d.SignImages || d.AttachProvenance {
		var keyErr error = signing.ErrNoKey
		if s.signing != nil {
			_, _, keyErr = s.signing.Active(ctx, pub.OrgID)
		}
		if !add("signing", keyErr == nil, "an active signing key is available") {
			gates[len(gates)-1].Reason = "the destination signs images, but there is no usable signing key: " + clipText(keyErr.Error(), 200)
			return gates, Target{}, "", nil
		}
	}
	if pub.ApprovalID != "" || d.RequiresApproval {
		ok, reason := false, "the publication needs an approval"
		if pub.ApprovalID != "" && s.approvals != nil {
			if a, err := s.approvals.Lookup(ctx, pub.ApprovalID); err == nil {
				ok, reason = a.Usable(s.now()), "the approval is "+a.State
				if a.State == approvals.Approved && !ok {
					reason = "the approval's validity has ended"
				}
			}
		}
		if !add("approval", ok, reason) {
			return gates, Target{}, "", nil
		}
	}
	target, prof, err := s.target(ctx, pub.OrgID, d)
	if !add("credentials", err == nil && prof.Status == "active" && prof.Purpose == "push", "the push profile is active") {
		gates[len(gates)-1].Reason = "the destination's push profile is unavailable or disabled"
		return gates, Target{}, "", nil
	}
	v, ok, err := s.validationOf(ctx, pub)
	verdict := "no validation has completed"
	if err == nil && ok {
		verdict = "the newest validation is " + v.State
		if v.Verdict != "" {
			verdict = "the newest validation verdict is " + v.Verdict
		}
	}
	if !add("validation", err == nil && ok && v.State == validation.Completed && v.Verdict == "passed", verdict) {
		return gates, Target{}, "", nil
	}
	// A restricted image goes only to destinations that accept restricted images, on every channel (ADR 0031).
	if !add("classification", v.Classification != "restricted" || d.AcceptsRestricted, "the image is not restricted, or the destination accepts restricted images") {
		gates[len(gates)-1].Reason = "the image is restricted (it passed only through an approved exception) and this destination does not accept restricted images"
		return gates, Target{}, "", nil
	}
	// An image built by a worker revoked as compromised stays unpublishable until a reviewer releases it (ADR 0026).
	var worker string
	qerr := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT worker_id FROM artifact_quarantines WHERE org_id = ? AND digest = ? AND released_at IS NULL LIMIT 1`), pub.OrgID, pub.ArtifactDigest).Scan(&worker)
	if !add("worker-trust", errors.Is(qerr, sql.ErrNoRows), "the image was not built by a revoked, compromised worker") {
		gates[len(gates)-1].Reason = "the image was built by worker " + worker + ", revoked as compromised; it is quarantined until a reviewer releases it"
		if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
			gates[len(gates)-1].Reason = "the quarantine check failed"
		}
		return gates, Target{}, "", nil
	}
	archive, err := s.blobs.File(pub.ArtifactDigest)
	if err != nil {
		add("artifact", false, "the image is no longer in internal storage")
		return gates, Target{}, "", nil
	}
	image, err := ImageDigest(archive.Name())
	archive.Close()
	if !add("artifact", err == nil, "the stored image has one top-level manifest") {
		gates[len(gates)-1].Reason = "the stored image cannot be published: " + fmt.Sprint(err)
		return gates, Target{}, "", nil
	}
	tags := append([]string{BuildTag(image)}, pub.Tags...)
	for _, tag := range tags {
		current, err := s.pusher.Current(ctx, target, tag)
		if err != nil {
			add("tags", false, "the destination could not be read: "+redact(err, target))
			return gates, Target{}, "", nil
		}
		if current == "" || current == image {
			continue
		}
		protected := d.ImmutableTags || slices.ContainsFunc(d.ProtectedTags, func(p string) bool { ok, _ := path.Match(p, tag); return ok })
		// A rollback may move a protected tag back to the image it was published with; the rollback-source gate
		// already checked that the tag belongs to that publication and that tags are not immutable.
		if protected && !(pub.Kind == "rollback" && slices.Contains(pub.Tags, tag)) {
			add("tags", false, fmt.Sprintf("tag %s is protected and already points to another image", tag))
			return gates, Target{}, "", nil
		}
	}
	add("tags", true, "every tag is new, already points to this image or is not protected")
	return gates, target, image, tags
}

// validationOf returns the validation a publication depends on: a promotion's own revalidation, otherwise the
// image's newest validation.
func (s *Service) validationOf(ctx context.Context, pub Publication) (validation.Run, bool, error) {
	if pub.ValidationRunID != "" {
		v, err := s.validations.ByID(ctx, pub.ValidationRunID)
		return v, err == nil, err
	}
	var runID string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT run_id FROM build_artifacts WHERE id = ?`), pub.ArtifactID).Scan(&runID)
	if err != nil {
		return validation.Run{}, false, err
	}
	return s.validations.Latest(ctx, runID)
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Service) finish(ctx context.Context, pub Publication, from, to, reason string, gates []Gate, image, ref string) error {
	now := s.now().UTC()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		q, args := `UPDATE publications SET state = ?, reason = ?, updated_at = ?`, []any{to, nullable(reason), now}
		if gates != nil {
			raw, _ := json.Marshal(gates)
			q, args = q+`, gate_decisions = ?`, append(args, string(raw))
		}
		if image != "" {
			q, args = q+`, image_digest = ?, destination_ref = ?, signature_ref = ?, attestation_ref = ?, signature_storage = ?`, append(args, image, ref, nullable(pub.SignatureRef), nullable(pub.AttestationRef), nullable(pub.SignatureStorage))
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(q+` WHERE id = ? AND state = ?`), append(args, pub.ID, from)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		action := map[string]string{Published: "publication.published", Blocked: "publication.blocked", Failed: "publication.failed"}[to]
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: pub.OrgID, Kind: authz.System}, Action: action, ResourceType: "publication", ResourceID: pub.ID,
			Outcome: to, Reason: strings.TrimSpace(ref + " " + reason)})
	})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const selectPublication = `SELECT id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, COALESCE(reason, ''), gate_decisions, COALESCE(image_digest, ''), COALESCE(destination_ref, ''), COALESCE(approval_id, ''), requested_by, created_at, updated_at,
	COALESCE(source_publication_id, ''), COALESCE(validation_run_id, ''), COALESCE(signature_ref, ''), COALESCE(attestation_ref, ''), COALESCE(signature_storage, ''),
	deprecated_at, COALESCE(deprecation_reason, ''), retired_at, COALESCE(retirement_reason, '') FROM publications`

func scanPublication(sc interface{ Scan(...any) error }) (Publication, error) {
	var p Publication
	var tags, gates string
	var deprecated, retired sql.NullTime
	if err := sc.Scan(&p.ID, &p.OrgID, &p.ProjectID, &p.Kind, &p.ArtifactID, &p.ArtifactDigest, &p.DestinationID, &tags, &p.State, &p.Reason, &gates, &p.ImageDigest, &p.DestinationRef, &p.ApprovalID, &p.RequestedBy, &p.CreatedAt, &p.UpdatedAt,
		&p.SourcePublicationID, &p.ValidationRunID, &p.SignatureRef, &p.AttestationRef, &p.SignatureStorage, &deprecated, &p.DeprecationReason, &retired, &p.RetirementReason); err != nil {
		return Publication{}, err
	}
	if deprecated.Valid {
		p.DeprecatedAt = &deprecated.Time
	}
	if retired.Valid {
		p.RetiredAt = &retired.Time
	}
	p.Tags, p.Gates = []string{}, []Gate{}
	_ = json.Unmarshal([]byte(tags), &p.Tags)
	_ = json.Unmarshal([]byte(gates), &p.Gates)
	return p, nil
}

func (s *Service) load(ctx context.Context, id string) (Publication, error) {
	p, err := scanPublication(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectPublication+` WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	return p, err
}

func (s *Service) Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Publication, error) {
	pub, err := s.load(ctx, id)
	if err != nil {
		return Publication{}, err
	}
	if pub.OrgID != p.OrgID {
		return Publication{}, ErrNotFound
	}
	if _, err := s.tenancy.GetProject(ctx, p, roles, pub.ProjectID); err != nil {
		return Publication{}, ErrNotFound
	}
	return pub, nil
}

// List returns the publications of projects the caller can read, newest first.
func (s *Service) List(ctx context.Context, p authz.Principal, roles []authz.Role, limit int) ([]Publication, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectPublication+` WHERE org_id = ? ORDER BY created_at DESC LIMIT ?`), p.OrgID, 500)
	if err != nil {
		return nil, err
	}
	var all []Publication
	for rows.Next() {
		pub, err := scanPublication(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, pub)
	}
	rows.Close()
	out := []Publication{}
	visible := map[string]bool{}
	for _, pub := range all {
		ok, seen := visible[pub.ProjectID]
		if !seen {
			_, err := s.tenancy.GetProject(ctx, p, roles, pub.ProjectID)
			ok = err == nil
			visible[pub.ProjectID] = ok
		}
		if ok {
			out = append(out, pub)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// SystemRequester marks publications Samuori requested on its own, such as automatic publication.
const SystemRequester = "system"

// AutoPublish requests a publication when the build's project and its spec both ask for automatic publication. It
// runs inside the transaction that completes the build's validation, so every read uses tx. Approvals and the
// publish-time gate still apply.
func (s *Service) AutoPublish(ctx context.Context, tx *sql.Tx, v validation.Run) error {
	if v.Trigger == "promotion" {
		return s.wakePromotions(ctx, tx, v.ID)
	}
	if v.Trigger != "build" || v.State != validation.Completed || v.Verdict != "passed" || v.BuildRunID == "" {
		return nil
	}
	var mode, specID string
	var rev int
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT p.publication_mode, r.spec_id, r.spec_revision FROM build_runs r JOIN projects p ON p.id = r.project_id WHERE r.id = ?`), v.BuildRunID).Scan(&mode, &specID, &rev); err != nil {
		return err
	}
	if mode != "automatic" {
		return nil
	}
	var raw string
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT document FROM spec_revisions WHERE spec_id = ? AND revision = ?`), specID, rev).Scan(&raw); err != nil {
		return err
	}
	var doc struct {
		Spec struct {
			Publication *struct {
				Mode           string   `json:"mode"`
				DestinationRef string   `json:"destinationRef"`
				RequestedTags  []string `json:"requestedTags"`
			} `json:"publication"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc.Spec.Publication == nil || doc.Spec.Publication.Mode != "automatic" || doc.Spec.Publication.DestinationRef == "" {
		return nil
	}
	intent := doc.Spec.Publication
	actor := authz.Principal{OrgID: v.OrgID, Kind: authz.System}
	skip := func(reason string) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "publication.auto", ResourceType: "artifact", ResourceID: v.ArtifactID, Outcome: "skipped", Reason: reason})
	}
	d, err := scanDestination(tx.QueryRowContext(ctx, s.db.Rebind(`SELECT `+destinationCols+` FROM destinations WHERE org_id = ? AND name = ?`), v.OrgID, intent.DestinationRef))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !slices.Contains(d.AllowedProjects, v.ProjectID)) {
		return skip("destination " + intent.DestinationRef + " does not exist or is not open to this project")
	}
	if err != nil {
		return err
	}
	tags := []string{}
	for _, t := range intent.RequestedTags {
		if !tagPattern.MatchString(t) || strings.HasPrefix(t, "samuori-") {
			return skip(fmt.Sprintf("requested tag %q is not allowed", t))
		}
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if len(tags) > 16 {
		return skip("more than 16 requested tags")
	}
	now := s.now().UTC()
	pub := Publication{ID: ids.New(), OrgID: v.OrgID, ProjectID: v.ProjectID, Kind: "publish", ArtifactID: v.ArtifactID, ArtifactDigest: v.ArtifactDigest, DestinationID: d.ID, Tags: tags,
		State: Requested, Gates: []Gate{}, RequestedBy: SystemRequester, CreatedAt: now, UpdatedAt: now}
	return s.insert(ctx, tx, &pub, d, actor, "auto-"+v.ID, "")
}

type ImagePublication struct {
	ID, DestinationID, DestinationName, State, DestinationRef string
	Tags                                                      []string
}

type Image struct {
	ArtifactID, Digest, BuildRunID string
	Version, App                   string
	Size                           int64
	CreatedAt                      time.Time
	Validation                     *validation.Run
	Publications                   []ImagePublication
}

// Images is the project's Image Library: images its builds produced, with their newest validation and
// publications.
func (s *Service) Images(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, limit int) ([]Image, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT a.id, a.digest, a.run_id, a.size, a.created_at, COALESCE(r.version, ''), COALESCE(sp.name, '') FROM build_artifacts a JOIN build_runs r ON r.id = a.run_id
		LEFT JOIN build_specs sp ON sp.id = r.spec_id
		WHERE r.project_id = ? AND r.org_id = ? AND a.kind = 'image' AND r.state = 'succeeded' AND r.id NOT IN (SELECT run_id FROM build_run_deletions) ORDER BY a.created_at DESC LIMIT ?`), projectID, p.OrgID, limit)
	if err != nil {
		return nil, err
	}
	var out []Image
	for rows.Next() {
		var img Image
		if err := rows.Scan(&img.ArtifactID, &img.Digest, &img.BuildRunID, &img.Size, &img.CreatedAt, &img.Version, &img.App); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, img)
	}
	rows.Close()
	names := map[string]string{}
	all, err := s.destinations(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		names[d.ID] = d.Name
	}
	for i := range out {
		if v, ok, err := s.validations.Latest(ctx, out[i].BuildRunID); err != nil {
			return nil, err
		} else if ok {
			out[i].Validation = &v
		}
		prows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectPublication+` WHERE artifact_id = ? ORDER BY created_at DESC LIMIT 50`), out[i].ArtifactID)
		if err != nil {
			return nil, err
		}
		out[i].Publications = []ImagePublication{}
		for prows.Next() {
			pub, err := scanPublication(prows)
			if err != nil {
				prows.Close()
				return nil, err
			}
			out[i].Publications = append(out[i].Publications, ImagePublication{ID: pub.ID, DestinationID: pub.DestinationID, DestinationName: names[pub.DestinationID], State: pub.State, Tags: pub.Tags, DestinationRef: pub.DestinationRef})
		}
		prows.Close()
	}
	return out, nil
}

// wakePromotions enqueues the promotions that waited for a revalidation, in the transaction that completes it.
func (s *Service) wakePromotions(ctx context.Context, tx *sql.Tx, validationID string) error {
	rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id FROM publications WHERE validation_run_id = ? AND state IN ('requested', 'approved')`), validationID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.enqueue(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
