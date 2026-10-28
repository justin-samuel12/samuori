package kubeconvert

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/imagescan"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/tenancy"
)

var (
	ViewRoles  = []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.Operator, authz.SecurityReviewer, authz.Administrator}
	WriteRoles = []authz.Role{authz.Developer, authz.Maintainer, authz.Administrator}
)

const (
	SubjectQueued = "samuori.conversions.queued"
	SubjectAssign = "samuori.conversions.assign"
	SubjectCancel = "samuori.conversions.cancel"
)

// Limits on what users send and what a revision may hold.
const (
	MaxEditBytes    = 512 << 10
	MaxRevisionSize = convertjob.MaxOutputBytes + 256<<10
)

type Service struct {
	db        *db.DB
	tenancy   *tenancy.Store
	sources   *sources.Service
	blobs     *storage.Blobs
	providers *providers.Store
	ops       *operations.Store
	wake      func()
	now       func() time.Time
	// LeaseTTL bounds how long a worker may go without renewing a conversion lease.
	LeaseTTL time.Duration
}

func NewService(d *db.DB, t *tenancy.Store, src *sources.Service, blobs *storage.Blobs, p *providers.Store, ops *operations.Store) *Service {
	return &Service{db: d, tenancy: t, sources: src, blobs: blobs, providers: p, ops: ops, wake: func() {}, now: time.Now, LeaseTTL: 2 * time.Minute}
}

// OnCommit registers the function that wakes the outbox relay after a transition commits.
func (s *Service) OnCommit(f func()) { s.wake = f }

type CreateRequest struct {
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	Input    Input   `json:"input"`
	Options  Options `json:"options"`
}

// runContext is stored with each run: how to read the tool's output and post-process it.
type runContext struct {
	Plan        ToolPlan       `json:"plan"`
	ComposeFile string         `json:"composeFile"`
	Image       *ImageEvidence `json:"image,omitempty"`
	Lineage     Lineage        `json:"lineage"`
	Notes       []Finding      `json:"notes,omitempty"`
	Questions   []Question     `json:"questions,omitempty"`
}

// project authorizes p on a project: ErrNotFound unless p may see it, ErrForbidden when p may see but not change it.
func (s *Service) project(ctx context.Context, p authz.Principal, projectID string, write bool) (tenancy.Project, error) {
	pr, err := s.tenancy.GetProject(ctx, p, ViewRoles, projectID)
	if errors.Is(err, tenancy.ErrNotFound) {
		return tenancy.Project{}, ErrNotFound
	}
	if err != nil {
		return tenancy.Project{}, err
	}
	if write && (p.ReadOnly || !authz.Can(p, WriteRoles, authz.Resource{OrgID: pr.OrgID, TeamID: pr.TeamID, ProjectID: pr.ID})) {
		return tenancy.Project{}, ErrForbidden
	}
	return pr, nil
}

// Create records a conversion and queues its first run. A repeated idempotency key returns the first result.
func (s *Service) Create(ctx context.Context, p authz.Principal, projectID string, req CreateRequest, key, requestID string) (Conversion, error) {
	pr, err := s.project(ctx, p, projectID, true)
	if err != nil {
		return Conversion{}, err
	}
	if !dnsLabel.MatchString(req.Name) {
		return Conversion{}, invalid("the name %q must be a DNS label (lowercase letters, digits and dashes, at most 63)", req.Name)
	}
	prov, cfg, err := s.provider(ctx, p.OrgID, pr.ID, req.Provider)
	if err != nil {
		return Conversion{}, err
	}
	if err := req.Input.normalize(); err != nil {
		return Conversion{}, err
	}
	if !supports(prov, req.Input.Kind) {
		return Conversion{}, invalid("%s cannot convert a %s input", req.Provider, req.Input.Kind)
	}
	if err := req.Options.normalize(prov); err != nil {
		return Conversion{}, err
	}
	c := Conversion{ID: ids.New(), OrgID: p.OrgID, ProjectID: pr.ID, Name: req.Name, Provider: req.Provider, Input: req.Input, Options: req.Options, State: StateQueued, CreatedBy: p.ID}
	run, err := s.prepareRun(ctx, c, cfg, prov, req.Options, nil, nil, 1)
	if err != nil {
		return Conversion{}, err
	}
	now := s.now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	var existing string
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		op, fresh, err := s.ops.Begin(ctx, tx, p, "kube-conversion.create", key)
		if err != nil {
			return err
		}
		if !fresh {
			existing = op.ResourceID
			return nil
		}
		in, _ := json.Marshal(c.Input)
		opts, _ := json.Marshal(c.Options)
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO kube_conversions (id, org_id, project_id, name, provider, input, options, state, current_revision, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`),
			c.ID, c.OrgID, c.ProjectID, c.Name, c.Provider, string(in), string(opts), StateQueued, p.ID, now, now); err != nil {
			return err
		}
		run.OperationID = op.ID
		if err := s.insertRun(ctx, tx, c, run, p.ID, now); err != nil {
			return err
		}
		if err := s.ops.Finish(ctx, tx, op.ID, operations.Succeeded, "kube-conversion", c.ID, "", ""); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "kube-conversion.create", ResourceType: "kube-conversion", ResourceID: c.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("project %s, %s %s, %s input, %s", c.ProjectID, c.Provider, run.version, c.Input.Kind, c.Options.Format), RequestID: requestID})
	})
	if err != nil {
		return Conversion{}, err
	}
	s.wake()
	if existing != "" {
		return s.Get(ctx, p, existing)
	}
	return s.Get(ctx, p, c.ID)
}

// provider checks that a provider is in the catalog, implemented, and enabled by an administrator for the project.
func (s *Service) provider(ctx context.Context, orgID, projectID, name string) (Provider, providers.Config, error) {
	prov, ok := Lookup(name)
	if !ok {
		return nil, providers.Config{}, invalid("%q is not a conversion provider Samuori can run", name)
	}
	cfg, err := s.providers.Config(ctx, orgID, name)
	if err != nil {
		return nil, providers.Config{}, err
	}
	if !cfg.Allows("", projectID) {
		d, _ := providers.Lookup(name)
		return nil, providers.Config{}, invalid("%s is not enabled for this project; an administrator enables it under Build & conversion providers", d.DisplayName)
	}
	return prov, cfg, nil
}

// ProviderOption is what a project's members may know about a conversion provider: whether they can use it, what
// it reads and writes, and its maintenance warning; never its pins, pools or project list.
type ProviderOption struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName"`
	Available     bool     `json:"available"`
	Reason        string   `json:"reason,omitempty"`
	Maintenance   string   `json:"maintenance,omitempty"`
	AsksQuestions bool     `json:"asksQuestions"`
	Inputs        []string `json:"inputs"`
	Formats       []string `json:"formats"`
}

// Providers lists the conversion providers Samuori can run and whether p may use each in the project.
func (s *Service) Providers(ctx context.Context, p authz.Principal, projectID string) ([]ProviderOption, error) {
	pr, err := s.project(ctx, p, projectID, false)
	if err != nil {
		return nil, err
	}
	out := []ProviderOption{}
	for _, d := range providers.Catalog {
		prov, ok := registry[d.Name]
		if d.Kind != providers.Conversion || !ok {
			continue
		}
		cfg, err := s.providers.Config(ctx, p.OrgID, d.Name)
		if err != nil {
			return nil, err
		}
		_, asks := prov.(Asker)
		o := ProviderOption{Name: d.Name, DisplayName: d.DisplayName, Available: cfg.Allows("", pr.ID), Maintenance: d.Maintenance, AsksQuestions: asks, Inputs: prov.Inputs(), Formats: prov.Formats()}
		if !o.Available {
			o.Reason = d.DisplayName + " is not enabled for this project; an administrator enables it under Build & conversion providers"
		}
		out = append(out, o)
	}
	return out, nil
}

type preparedRun struct {
	id, version string
	options     Options
	answers     map[string]string
	job         convertjob.Job
	inline      map[string]string
	context     runContext
	number      int
	OperationID string
}

// prepareRun resolves the input, plans the tool run and pins the job to the approved version and checksums.
func (s *Service) prepareRun(ctx context.Context, c Conversion, cfg providers.Config, prov Provider, opts Options, answers map[string]string, questions []Question, number int) (preparedRun, error) {
	r := preparedRun{id: ids.New(), version: cfg.Version, options: opts, answers: answers, number: number, inline: map[string]string{}}
	if r.answers == nil {
		r.answers = map[string]string{}
	}
	lin := Lineage{ProjectID: c.ProjectID, Input: c.Input, Provider: c.Provider, ProviderVersion: cfg.Version, Options: opts, KubernetesVersion: opts.KubernetesVersion, RunID: r.id}
	var inputDigest string
	switch c.Input.Kind {
	case InputCompose, InputSource:
		snap, m, err := s.snapshot(ctx, c.ProjectID, c.Input.SnapshotID)
		if err != nil {
			return r, err
		}
		if c.Input.Kind == InputCompose && !hasFile(m, c.Input.ComposePath) {
			return r, invalid("the upload has no file %s", c.Input.ComposePath)
		}
		if check, ok := prov.(ComposeChecker); ok && c.Input.Kind == InputCompose {
			raw, err := s.snapshotFile(m, c.Input.ComposePath)
			if err != nil {
				return r, err
			}
			notes, err := check.CheckCompose(raw)
			if err != nil {
				return r, err
			}
			r.context.Notes = append(r.context.Notes, notes...)
		}
		inputDigest, lin.SnapshotDigest, r.context.ComposeFile = snap.Digest, snap.Digest, c.Input.ComposePath
		if c.Input.BuildRunID != "" || c.Input.PublicationID != "" {
			img, err := s.image(ctx, c, &lin)
			if err != nil {
				return r, err
			}
			r.context.Image = img
		}
	case InputBuild, InputPublication:
		img, err := s.image(ctx, c, &lin)
		if err != nil {
			return r, err
		}
		r.context.Image = img
		_, versioned := prov.(ComposeChecker)
		compose, note := syntheticCompose(c.Input.Service, img, versioned)
		r.inline["compose.yml"] = compose
		r.context.ComposeFile = "compose.yml"
		r.context.Notes = append(r.context.Notes, note)
		inputDigest = inlineManifest(r.inline).Digest()
	}
	plan, err := prov.Plan(PlanInput{Name: c.Name, Input: c.Input, Options: opts, Answers: r.answers, Questions: questions, ComposeFile: r.context.ComposeFile})
	if err != nil {
		return r, err
	}
	r.context.Questions = questions
	r.context.Notes = append(r.context.Notes, plan.Notes...)
	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = convertjob.DefaultTimeout
	}
	sums := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		if v := cfg.Artifacts["binary-"+arch]; v != "" {
			sums[arch] = v
		}
	}
	r.job = convertjob.Job{Version: 1, ConversionID: c.ID, RunID: r.id, Provider: c.Provider, ProviderVersion: cfg.Version, ToolSHA256: sums, Args: plan.Args, Mkdirs: plan.Mkdirs,
		Files: plan.Files, Keep: plan.Keep, InputDigest: inputDigest, TimeoutSeconds: timeout}
	if err := r.job.Validate(); err != nil {
		return r, err
	}
	r.context.Plan, r.context.Lineage = plan, lin
	return r, nil
}

const maxComposeBytes = 256 << 10

// snapshotFile reads one file of an upload, for checks that parse it; nothing in it is executed.
func (s *Service) snapshotFile(m sources.Manifest, p string) ([]byte, error) {
	for _, f := range m.Files {
		if f.Path != p {
			continue
		}
		if f.Size > maxComposeBytes {
			return nil, invalid("%s is larger than %d KiB", p, maxComposeBytes>>10)
		}
		rc, err := s.blobs.Open(f.Digest)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxComposeBytes))
	}
	return nil, invalid("the upload has no file %s", p)
}

func hasFile(m sources.Manifest, p string) bool {
	for _, f := range m.Files {
		if f.Path == p {
			return true
		}
	}
	return false
}

func (s *Service) snapshot(ctx context.Context, projectID, id string) (sources.Snapshot, sources.Manifest, error) {
	snap, err := s.sources.SnapshotByID(ctx, id)
	if errors.Is(err, sources.ErrNotFound) || (err == nil && snap.ProjectID != projectID) {
		return sources.Snapshot{}, sources.Manifest{}, invalid("the upload %s is not in this project", id)
	}
	if err != nil {
		return sources.Snapshot{}, sources.Manifest{}, err
	}
	m, err := s.sources.Manifest(snap)
	if errors.Is(err, sources.ErrDeleted) {
		return sources.Snapshot{}, sources.Manifest{}, invalid("the upload %s was deleted", id)
	}
	return snap, m, err
}

// image collects what Samuori's records show about a build's or publication's image in the conversion's project.
func (s *Service) image(ctx context.Context, c Conversion, lin *Lineage) (*ImageEvidence, error) {
	img := &ImageEvidence{Service: c.Input.Service}
	var artifactID string
	if c.Input.PublicationID != "" {
		var project, state, ref, digest string
		err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT project_id, state, COALESCE(destination_ref, ''), COALESCE(image_digest, ''), artifact_id FROM publications WHERE id = ? AND org_id = ?`),
			c.Input.PublicationID, c.OrgID).Scan(&project, &state, &ref, &digest, &artifactID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && project != c.ProjectID) {
			return nil, invalid("the publication %s is not in this project", c.Input.PublicationID)
		}
		if err != nil {
			return nil, err
		}
		if state != "published" || digest == "" {
			return nil, invalid("the publication %s is %s, not published", c.Input.PublicationID, state)
		}
		img.Reference, img.Digest = repository(ref)+"@"+digest, digest
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT run_id FROM build_artifacts WHERE id = ?`), artifactID).Scan(&img.BuildRunID); err != nil {
			return nil, err
		}
	} else {
		img.BuildRunID = c.Input.BuildRunID
	}
	var deleted int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM build_run_deletions WHERE run_id = ?`), img.BuildRunID).Scan(&deleted); err != nil {
		return nil, err
	}
	if deleted > 0 && c.Input.PublicationID == "" {
		return nil, invalid("the build %s was deleted; convert a published image or a newer build", img.BuildRunID)
	}
	var project, state, archive, specID string
	var planDigest sql.NullString
	var specRev int
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT project_id, state, COALESCE(artifact_digest, ''), plan_digest, spec_id, spec_revision FROM build_runs WHERE id = ? AND org_id = ?`),
		img.BuildRunID, c.OrgID).Scan(&project, &state, &archive, &planDigest, &specID, &specRev)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project != c.ProjectID) {
		return nil, invalid("the build %s is not in this project", img.BuildRunID)
	}
	if err != nil {
		return nil, err
	}
	if state != "succeeded" || archive == "" {
		return nil, invalid("the build %s has no image (it is %s)", img.BuildRunID, state)
	}
	if img.Reference == "" {
		var ref, digest string
		err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(p.destination_ref, ''), COALESCE(p.image_digest, '') FROM publications p JOIN build_artifacts a ON a.id = p.artifact_id
			WHERE a.run_id = ? AND p.state = 'published' AND p.image_digest IS NOT NULL ORDER BY p.updated_at DESC LIMIT 1`), img.BuildRunID).Scan(&ref, &digest)
		if err == nil {
			img.Reference, img.Digest = repository(ref)+"@"+digest, digest
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		} else {
			img.Digest = archive
		}
	}
	if planDigest.Valid {
		var raw string
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), planDigest.String).Scan(&raw); err == nil {
			if plan, err := buildplan.Decode([]byte(raw)); err == nil {
				img.Profile, img.Port, img.Path = plan.Validation.Profile, plan.Validation.Port, plan.Validation.Path
			}
		}
	}
	if img.Port == 0 && (img.Profile == buildplan.ProfileHTTPStartup || img.Profile == buildplan.ProfileStaticServe) {
		img.Port = s.exposedPort(archive)
	}
	lin.SpecID, lin.SpecRevision, lin.BuildRunID, lin.ImageDigest, lin.ImageReference = specID, specRev, img.BuildRunID, img.Digest, img.Reference
	return img, nil
}

// exposedPort reads the first TCP port the image exposes, as the runtime check does when its plan names none.
func (s *Service) exposedPort(archive string) int {
	if s.blobs == nil {
		return 0
	}
	f, err := s.blobs.File(archive)
	if err != nil {
		return 0
	}
	defer f.Close()
	a, err := imagescan.Open(f, imagescan.DefaultLimits().MaxEntries)
	if err != nil {
		return 0
	}
	imgs, err := a.Images()
	if err != nil {
		return 0
	}
	var ports []int
	for _, im := range imgs {
		for exposed := range im.Config.Config.ExposedPorts {
			if n, proto, _ := strings.Cut(exposed, "/"); proto == "" || proto == "tcp" {
				if v, err := strconv.Atoi(n); err == nil {
					ports = append(ports, v)
				}
			}
		}
	}
	sort.Ints(ports)
	if len(ports) == 0 {
		return 0
	}
	return ports[0]
}

var tagSuffix = regexp.MustCompile(`:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// repository strips a tag and digest from a reference, keeping a registry port.
func repository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, "/"); tagSuffix.MatchString(ref[i+1:]) {
		ref = ref[:i+1] + tagSuffix.ReplaceAllString(ref[i+1:], "")
	}
	return ref
}

// syntheticCompose describes an image-only input as a one-service Compose file from Samuori's records alone.
func syntheticCompose(service string, img *ImageEvidence, versioned bool) (string, Finding) {
	ref := img.Reference
	if ref == "" {
		ref = "samuori-unpublished/" + service + "@" + img.Digest
	}
	var b strings.Builder
	if versioned {
		b.WriteString("version: \"3.8\"\n")
	}
	fmt.Fprintf(&b, "services:\n  %s:\n    image: %q\n", service, ref)
	if img.Port > 0 {
		fmt.Fprintf(&b, "    ports:\n      - \"%d:%d\"\n", img.Port, img.Port)
	}
	note := Finding{Severity: SeverityReview, Code: "image-only-input", Source: SourceProvider,
		Message: "Samuori described the image as a one-service Compose file from its records; environment, volumes and other services are not known and must be added by hand"}
	if img.Port == 0 {
		note.Message += "; no port is known, so no Service was generated"
	}
	return b.String(), note
}

func inlineManifest(files map[string]string) sources.Manifest {
	m := sources.Manifest{Version: 1, Files: []sources.File{}, Directories: []string{}}
	for p, c := range files {
		sum := sha256.Sum256([]byte(c))
		m.Files = append(m.Files, sources.File{Path: p, Size: int64(len(c)), Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m
}

const maxAttempts = 3

func (s *Service) insertRun(ctx context.Context, tx *sql.Tx, c Conversion, r preparedRun, actor string, now time.Time) error {
	opts, _ := json.Marshal(r.options)
	answers, _ := json.Marshal(r.answers)
	inline, _ := json.Marshal(r.inline)
	rc, _ := json.Marshal(r.context)
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO kube_conversion_runs (id, conversion_id, org_id, number, options, answers, job, job_digest, inline_files, context, provider_version, state, max_attempts, operation_id, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.id, c.ID, c.OrgID, r.number, string(opts), string(answers), string(r.job.Canonical()), r.job.Digest(), string(inline), string(rc), r.version, StateQueued, maxAttempts, nullable(r.OperationID), actor, now, now); err != nil {
		return err
	}
	return s.db.EnqueueOutbox(ctx, tx, ids.New(), SubjectQueued, []byte(`{"runId":"`+r.id+`"}`))
}

// Rerun queues a new run with new options or answers. Only one run of a conversion is active at a time.
func (s *Service) Rerun(ctx context.Context, p authz.Principal, id string, opts *Options, answers map[string]string, key, requestID string) (Conversion, error) {
	c, err := s.load(ctx, p, id, true)
	if err != nil {
		return Conversion{}, err
	}
	if c.LatestRun != nil && activeState(c.LatestRun.State) {
		return Conversion{}, ErrConflict
	}
	prov, cfg, err := s.provider(ctx, c.OrgID, c.ProjectID, c.Provider)
	if err != nil {
		return Conversion{}, err
	}
	o := c.Options
	if opts != nil {
		o = *opts
	}
	if err := o.normalize(prov); err != nil {
		return Conversion{}, err
	}
	number := 1
	if c.LatestRun != nil {
		number = c.LatestRun.Number + 1
	}
	questions, answers, err := s.answers(ctx, c, prov, answers)
	if err != nil {
		return Conversion{}, err
	}
	run, err := s.prepareRun(ctx, c, cfg, prov, o, answers, questions, number)
	if err != nil {
		return Conversion{}, err
	}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		op, fresh, err := s.ops.Begin(ctx, tx, p, "kube-conversion.rerun", key)
		if err != nil {
			return err
		}
		if !fresh {
			return nil
		}
		raw, _ := json.Marshal(o)
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE kube_conversions SET options = ?, state = ?, updated_at = ? WHERE id = ? AND state NOT IN ('queued', 'assigned', 'running')`), string(raw), StateQueued, now, c.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		run.OperationID = op.ID
		if err := s.insertRun(ctx, tx, c, run, p.ID, now); err != nil {
			return err
		}
		if err := s.ops.Finish(ctx, tx, op.ID, operations.Succeeded, "kube-conversion", c.ID, "", ""); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "kube-conversion.rerun", ResourceType: "kube-conversion", ResourceID: c.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("run %d, %s %s, %d answers", number, c.Provider, cfg.Version, len(answers)), RequestID: requestID})
	})
	if err != nil {
		return Conversion{}, err
	}
	s.wake()
	return s.Get(ctx, p, c.ID)
}

// answers returns the questions of the latest run that asked any and the answers a re-run uses: the given ones,
// validated by the provider against those questions, or with none given, the previous answers still asked.
func (s *Service) answers(ctx context.Context, c Conversion, prov Provider, given map[string]string) ([]Question, map[string]string, error) {
	runs, err := s.runs(ctx, c.ID, 100)
	if err != nil {
		return nil, nil, err
	}
	var asked *Run
	for i := range runs {
		if len(runs[i].Questions) > 0 {
			asked = &runs[i]
			break
		}
	}
	if asked == nil {
		if len(given) > 0 {
			return nil, nil, invalid("this conversion has no questions to answer")
		}
		return nil, nil, nil
	}
	if given == nil {
		kept := map[string]string{}
		for id, v := range asked.Answers {
			if _, ok := findQuestion(asked.Questions, id); ok {
				kept[id] = v
			}
		}
		return asked.Questions, kept, nil
	}
	a, ok := prov.(Asker)
	if !ok {
		return nil, nil, invalid("%s asks no questions", c.Provider)
	}
	if err := a.CheckAnswers(asked.Questions, given); err != nil {
		return nil, nil, err
	}
	return asked.Questions, given, nil
}

const selectConversion = `SELECT id, org_id, project_id, name, provider, input, options, state, current_revision, created_by, created_at, updated_at FROM kube_conversions`

func scanConversion(r interface{ Scan(...any) error }) (Conversion, error) {
	var c Conversion
	var in, opts string
	if err := r.Scan(&c.ID, &c.OrgID, &c.ProjectID, &c.Name, &c.Provider, &in, &opts, &c.State, &c.CurrentRevision, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Conversion{}, err
	}
	_ = json.Unmarshal([]byte(in), &c.Input)
	_ = json.Unmarshal([]byte(opts), &c.Options)
	return c, nil
}

// load returns a conversion p may see (and change, with write), with its latest run.
func (s *Service) load(ctx context.Context, p authz.Principal, id string, write bool) (Conversion, error) {
	c, err := scanConversion(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectConversion+` WHERE id = ? AND org_id = ?`), id, p.OrgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversion{}, ErrNotFound
	}
	if err != nil {
		return Conversion{}, err
	}
	if _, err := s.project(ctx, p, c.ProjectID, write); err != nil {
		return Conversion{}, err
	}
	runs, err := s.runs(ctx, c.ID, 1)
	if err != nil {
		return Conversion{}, err
	}
	if len(runs) > 0 {
		c.LatestRun = &runs[0]
	}
	return c, nil
}

func (s *Service) Get(ctx context.Context, p authz.Principal, id string) (Conversion, error) {
	return s.load(ctx, p, id, false)
}

func (s *Service) List(ctx context.Context, p authz.Principal, projectID string, limit int) ([]Conversion, error) {
	if _, err := s.project(ctx, p, projectID, false); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectConversion+` WHERE project_id = ? AND org_id = ? ORDER BY created_at DESC LIMIT ?`), projectID, p.OrgID, limit)
	if err != nil {
		return nil, err
	}
	var out []Conversion
	for rows.Next() {
		c, err := scanConversion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	for i := range out {
		runs, err := s.runs(ctx, out[i].ID, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			out[i].LatestRun = &runs[0]
		}
	}
	return out, rows.Err()
}

const selectRun = `SELECT id, conversion_id, number, options, answers, provider_version, state, COALESCE(failure_class, ''), COALESCE(detail, ''), questions, COALESCE(revision, 0), COALESCE(tool_digest, ''), COALESCE(operation_id, ''), created_by, created_at, updated_at, ended_at FROM kube_conversion_runs`

func (s *Service) runs(ctx context.Context, conversionID string, limit int) ([]Run, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectRun+` WHERE conversion_id = ? ORDER BY number DESC LIMIT ?`), conversionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var opts, answers, questions string
		var ended sql.NullTime
		if err := rows.Scan(&r.ID, &r.ConversionID, &r.Number, &opts, &answers, &r.ProviderVersion, &r.State, &r.FailureClass, &r.Detail, &questions, &r.Revision, &r.ToolDigest, &r.OperationID, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &ended); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(opts), &r.Options)
		_ = json.Unmarshal([]byte(answers), &r.Answers)
		_ = json.Unmarshal([]byte(questions), &r.Questions)
		if ended.Valid {
			r.EndedAt = &ended.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) Runs(ctx context.Context, p authz.Principal, id string) ([]Run, error) {
	if _, err := s.load(ctx, p, id, false); err != nil {
		return nil, err
	}
	return s.runs(ctx, id, 100)
}

// Revisions lists a conversion's revisions, newest first, without file contents.
func (s *Service) Revisions(ctx context.Context, p authz.Principal, id string) ([]Revision, error) {
	if _, err := s.load(ctx, p, id, false); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectRevision+` WHERE conversion_id = ? ORDER BY number DESC`), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const selectRevision = `SELECT number, COALESCE(run_id, ''), COALESCE(parent, 0), origin, findings, lineage, errors, created_by, created_at FROM kube_conversion_revisions`

func scanRevision(r interface{ Scan(...any) error }) (Revision, error) {
	var rev Revision
	var findings, lineage string
	if err := r.Scan(&rev.Number, &rev.RunID, &rev.Parent, &rev.Origin, &findings, &lineage, &rev.Errors, &rev.CreatedBy, &rev.CreatedAt); err != nil {
		return Revision{}, err
	}
	_ = json.Unmarshal([]byte(findings), &rev.Findings)
	_ = json.Unmarshal([]byte(lineage), &rev.Lineage)
	return rev, nil
}

// Revision returns one revision with its files; number 0 is the current one.
func (s *Service) Revision(ctx context.Context, p authz.Principal, id string, number int) (Conversion, Revision, error) {
	c, err := s.load(ctx, p, id, false)
	if err != nil {
		return Conversion{}, Revision{}, err
	}
	rev, err := s.revision(ctx, c, number)
	return c, rev, err
}

func (s *Service) revision(ctx context.Context, c Conversion, number int) (Revision, error) {
	if number == 0 {
		number = c.CurrentRevision
	}
	rev, err := scanRevision(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectRevision+` WHERE conversion_id = ? AND number = ?`), c.ID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT path, digest, size, content FROM kube_conversion_files WHERE conversion_id = ? AND revision = ? ORDER BY path`), c.ID, number)
	if err != nil {
		return Revision{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Digest, &f.Size, &f.Content); err != nil {
			return Revision{}, err
		}
		rev.Files = append(rev.Files, f)
	}
	return rev, rows.Err()
}

// Download returns a revision for a ZIP bundle and audits the download.
func (s *Service) Download(ctx context.Context, p authz.Principal, id string, number int, requestID string) (Conversion, Revision, error) {
	c, rev, err := s.Revision(ctx, p, id, number)
	if err != nil {
		return Conversion{}, Revision{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "kube-conversion.download", ResourceType: "kube-conversion", ResourceID: c.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("revision %d", rev.Number), RequestID: requestID})
	})
	return c, rev, err
}

// Edit stores a changed or added file as a new revision of the given one and validates every file again. Literal
// secrets in the edit are replaced like the provider's, so no revision ever holds one.
func (s *Service) Edit(ctx context.Context, p authz.Principal, id string, base int, filePath, newContent, requestID string) (Revision, error) {
	c, err := s.load(ctx, p, id, true)
	if err != nil {
		return Revision{}, err
	}
	if !convertjob.LocalPath(filePath) || !manifestFile(filePath) && !strings.HasSuffix(filePath, ".md") && !helmMeta(filePath) {
		return Revision{}, invalid("the file must be a .yml, .yaml, .json or .md path inside the bundle")
	}
	if len(newContent) > MaxEditBytes {
		return Revision{}, invalid("the file is larger than %d KiB", MaxEditBytes>>10)
	}
	prev, err := s.revision(ctx, c, base)
	if err != nil {
		return Revision{}, err
	}
	files := append([]File(nil), prev.Files...)
	replaced := false
	for i := range files {
		if files[i].Path == filePath {
			files[i], replaced = newFile(filePath, []byte(newContent)), true
		}
	}
	if !replaced {
		files = append(files, newFile(filePath, []byte(newContent)))
	}
	out, fresh := Process(PostContext{Name: c.Name, Options: prev.Lineage.Options, Review: true}, files)
	var findings []Finding
	for _, f := range prev.Findings {
		if f.Source == SourceProvider || f.Source == SourceOptions {
			findings = append(findings, f)
		}
	}
	findings = append(findings, fresh...)
	lin := prev.Lineage
	lin.EditedFrom = prev.Number
	return s.storeRevision(ctx, c, "", prev.Number, OriginEdit, out, findings, lin, p, requestID)
}

func countErrors(fs []Finding) int {
	n := 0
	for _, f := range fs {
		if f.Severity == SeverityError {
			n++
		}
	}
	return n
}

func (s *Service) storeRevision(ctx context.Context, c Conversion, runID string, parent int, origin string, files []File, findings []Finding, lin Lineage, actor authz.Principal, requestID string) (Revision, error) {
	total := 0
	for _, f := range files {
		total += f.Size
	}
	if total > MaxRevisionSize {
		return Revision{}, invalid("the revision is larger than %d MiB", MaxRevisionSize>>20)
	}
	now := s.now().UTC()
	var rev Revision
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COALESCE(MAX(number), 0) + 1 FROM kube_conversion_revisions WHERE conversion_id = ?`), c.ID).Scan(&n); err != nil {
			return err
		}
		rev = Revision{Number: n, RunID: runID, Parent: parent, Origin: origin, Findings: findings, Lineage: lin, Errors: countErrors(findings), CreatedBy: actor.ID, CreatedAt: now, Files: files}
		if err := s.insertRevision(ctx, tx, c.ID, rev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE kube_conversions SET current_revision = ?, updated_at = ? WHERE id = ?`), n, now, c.ID); err != nil {
			return err
		}
		if origin != OriginEdit {
			return nil
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "kube-conversion.edit", ResourceType: "kube-conversion", ResourceID: c.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("revision %d from %d, %d errors", n, parent, rev.Errors), RequestID: requestID})
	})
	return rev, err
}

func (s *Service) insertRevision(ctx context.Context, tx *sql.Tx, conversionID string, rev Revision) error {
	findings, _ := json.Marshal(rev.Findings)
	lineage, _ := json.Marshal(rev.Lineage)
	if findings == nil || string(findings) == "null" {
		findings = []byte("[]")
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO kube_conversion_revisions (conversion_id, number, run_id, parent, origin, findings, lineage, errors, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		conversionID, rev.Number, nullable(rev.RunID), nullableInt(rev.Parent), rev.Origin, string(findings), string(lineage), rev.Errors, rev.CreatedBy, rev.CreatedAt); err != nil {
		return err
	}
	for _, f := range rev.Files {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO kube_conversion_files (conversion_id, revision, path, digest, size, content) VALUES (?, ?, ?, ?, ?, ?)`),
			conversionID, rev.Number, f.Path, f.Digest, f.Size, f.Content); err != nil {
			return err
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// Prefix of paths a provider may write; bundle paths are checked again before a revision is stored.
func cleanBundlePath(p string) bool {
	return convertjob.LocalPath(p) && path.Clean(p) == p && !strings.HasPrefix(p, convertjob.MetaDir+"/")
}
