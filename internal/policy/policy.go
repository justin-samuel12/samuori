// Package policy stores versioned organization policies. Each change creates an immutable version; edits use the
// version as an optimistic-concurrency token.
package policy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/imagescan"
	"samuori.invalid/samuori/internal/recipes"
)

const (
	KindFrontends   = "frontends"
	KindBaseImages  = "base-images"
	KindBuildArgs   = "build-args"
	KindEgress      = "egress"
	KindValidation  = "validation"
	KindSensitive   = "sensitive-content"
	KindPublication = "publication"
)

var (
	ErrNotFound    = errors.New("policy not found")
	ErrForbidden   = errors.New("forbidden")
	ErrConflict    = errors.New("policy changed since it was read")
	ErrUnsupported = errors.New("policy kind not supported yet")
)

// Invalid reports a document that does not match its kind's schema.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

type Frontends struct {
	Approved []string `json:"approved"`
}

// BaseImages limits base images to approved repositories. With RequireDigest, PinTags accepts a tag and pins it to
// the digest it resolves to at admission instead of refusing it (ADR 0049).
type BaseImages struct {
	AllowedRepositories []string `json:"allowedRepositories"`
	RequireDigest       bool     `json:"requireDigest"`
	PinTags             bool     `json:"pinTags"`
}

type ArgRule struct {
	ID           string `json:"id"`
	NamePattern  string `json:"namePattern,omitempty"`
	ValuePattern string `json:"valuePattern,omitempty"`
	Reason       string `json:"reason"`
}

type BuildArgs struct {
	Rules []ArgRule `json:"rules"`
}

// Validation decides which checks are required for a ValidationRun to pass.
type Validation struct {
	RequireSBOM        bool `json:"requireSbom"`
	RequireRuntimeTest bool `json:"requireRuntimeTest"`
	// RequireVulnerabilityScan makes the vulnerability check required; with no scanner configured it then fails
	// closed. FailOnSeverity (critical, high, medium or low) fails the check for findings at or above it that are not
	// in IgnoreVulnerabilities.
	RequireVulnerabilityScan bool     `json:"requireVulnerabilityScan,omitempty"`
	FailOnSeverity           string   `json:"failOnSeverity,omitempty"`
	IgnoreVulnerabilities    []string `json:"ignoreVulnerabilities,omitempty"`
}

// Severities, most severe first.
var Severities = []string{"critical", "high", "medium", "low"}

// Sensitive sets the outcome per rule class and adds path rules (ADR 0031). Delivered secret bytes always block.
type Sensitive struct {
	Outcomes   map[string]string `json:"outcomes"`
	ExtraRules []SensitiveRule   `json:"extraRules"`
}

type SensitiveRule struct {
	ID     string `json:"id"`
	Class  string `json:"class"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Publication governs how images leave Samuori. DeveloperDownloads lets developers download validated images of their
// projects, besides maintainers, publishers and administrators (ADR 0053).
type Publication struct {
	DeveloperDownloads bool `json:"developerDownloads"`
}

var sensitiveRuleID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type Policy struct {
	ID, Kind  string
	Version   int
	Document  json.RawMessage
	CreatedAt time.Time
	// CreatedBy is the username that saved this version, or "system" for seeded defaults.
	CreatedBy string
}

var pinnedFrontend = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[a-f0-9]{64}$`)

// Validate decodes doc strictly for kind and returns its canonical JSON.
func Validate(kind string, doc []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var v any
	switch kind {
	case KindFrontends:
		var f Frontends
		if err := dec.Decode(&f); err != nil {
			return nil, &Invalid{"frontends: " + err.Error()}
		}
		for _, a := range f.Approved {
			if !pinnedFrontend.MatchString(a) {
				return nil, &Invalid{fmt.Sprintf("frontends: %q must be pinned by digest (name@sha256:<64 hex>)", a)}
			}
		}
		if f.Approved == nil {
			f.Approved = []string{}
		}
		v = f
	case KindBaseImages:
		var b BaseImages
		if err := dec.Decode(&b); err != nil {
			return nil, &Invalid{"base-images: " + err.Error()}
		}
		for _, r := range b.AllowedRepositories {
			if r == "" || strings.ContainsAny(r, "@: ") {
				return nil, &Invalid{fmt.Sprintf("base-images: %q must be a repository such as docker.io/library/node, without tag or digest", r)}
			}
		}
		if b.PinTags && !b.RequireDigest {
			return nil, &Invalid{"base-images: pinTags needs requireDigest; without it tags are already accepted unpinned"}
		}
		if b.AllowedRepositories == nil {
			b.AllowedRepositories = []string{}
		}
		v = b
	case KindBuildArgs:
		var b BuildArgs
		if err := dec.Decode(&b); err != nil {
			return nil, &Invalid{"build-args: " + err.Error()}
		}
		for _, r := range b.Rules {
			if r.ID == "" || r.Reason == "" || (r.NamePattern == "" && r.ValuePattern == "") {
				return nil, &Invalid{"build-args: each rule needs an id, a reason and a name or value pattern"}
			}
			for _, p := range []string{r.NamePattern, r.ValuePattern} {
				if _, err := regexp.Compile(p); err != nil {
					return nil, &Invalid{fmt.Sprintf("build-args: rule %s has an invalid pattern: %v", r.ID, err)}
				}
			}
		}
		if b.Rules == nil {
			b.Rules = []ArgRule{}
		}
		v = b
	case KindEgress:
		var e egress.Policy
		if err := dec.Decode(&e); err != nil {
			return nil, &Invalid{"egress: " + err.Error()}
		}
		cat, err := egress.Builtin()
		if err != nil {
			return nil, err
		}
		if err := cat.ValidatePolicy(e); err != nil {
			return nil, &Invalid{"egress: " + err.Error()}
		}
		e.AllowedPresets, e.DockerfilePresets = nonNil(e.AllowedPresets), nonNil(e.DockerfilePresets)
		if e.Destinations == nil {
			e.Destinations = []egress.Destination{}
		}
		v = e
	case KindValidation:
		var x Validation
		if err := dec.Decode(&x); err != nil {
			return nil, &Invalid{"validation: " + err.Error()}
		}
		if x.FailOnSeverity != "" && !slices.Contains(Severities, x.FailOnSeverity) {
			return nil, &Invalid{"validation: failOnSeverity must be critical, high, medium or low"}
		}
		v = x
	case KindSensitive:
		var x Sensitive
		if err := dec.Decode(&x); err != nil {
			return nil, &Invalid{"sensitive-content: " + err.Error()}
		}
		outcomes := map[string]string{}
		for class, def := range imagescan.DefaultOutcomes {
			outcomes[class] = def
		}
		for class, o := range x.Outcomes {
			if _, ok := imagescan.DefaultOutcomes[class]; !ok {
				return nil, &Invalid{fmt.Sprintf("sensitive-content: unknown rule class %q", class)}
			}
			if o != "warn" && o != "require-exception" && o != "block" {
				return nil, &Invalid{fmt.Sprintf("sensitive-content: outcome %q must be warn, require-exception or block", o)}
			}
			if class == imagescan.ClassBuildSecret && o != "block" {
				return nil, &Invalid{"sensitive-content: delivered secret bytes always block publication"}
			}
			outcomes[class] = o
		}
		x.Outcomes = outcomes
		for _, r := range x.ExtraRules {
			if _, ok := imagescan.DefaultOutcomes[r.Class]; !ok || r.Class == imagescan.ClassBuildSecret || !sensitiveRuleID.MatchString(r.ID) || r.Reason == "" ||
				r.Path == "" || strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "..") {
				return nil, &Invalid{"sensitive-content: each extra rule needs an id, a path class other than build-secret, a relative path pattern and a reason"}
			}
		}
		if x.ExtraRules == nil {
			x.ExtraRules = []SensitiveRule{}
		}
		v = x
	case KindPublication:
		var x Publication
		if err := dec.Decode(&x); err != nil {
			return nil, &Invalid{"publication: " + err.Error()}
		}
		v = x
	default:
		return nil, ErrUnsupported
	}
	out, err := json.Marshal(v)
	return out, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Defaults are seeded for each organization: no custom frontends, the built-in recipes' repositories, digests
// required and tags pinned at admission.
func Defaults(cat *recipes.Catalog) map[string]any {
	repos := map[string]bool{}
	for _, r := range cat.All() {
		for _, rt := range r.Runtimes {
			for _, img := range append([]string{rt.BuildImage, rt.RunImage}, values(rt.RunImages)...) {
				repos[Repository(img)] = true
			}
		}
	}
	presets, err := egress.Builtin()
	if err != nil {
		panic(err)
	}
	allowed := make([]string, 0, len(repos))
	for r := range repos {
		allowed = append(allowed, r)
	}
	sort.Strings(allowed)
	return map[string]any{
		KindFrontends:   Frontends{Approved: []string{}},
		KindBaseImages:  BaseImages{AllowedRepositories: allowed, RequireDigest: true, PinTags: true},
		KindBuildArgs:   BuildArgs{Rules: []ArgRule{}},
		KindEgress:      presets.DefaultPolicy(),
		KindValidation:  Validation{RequireRuntimeTest: true},
		KindSensitive:   Sensitive{Outcomes: maps.Clone(imagescan.DefaultOutcomes), ExtraRules: []SensitiveRule{}},
		KindPublication: Publication{},
	}
}

// Repository strips the tag and digest from an image reference.
func Repository(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// Normalize expands short Docker Hub names so they compare with policy entries ("node" -> "docker.io/library/node").
func Normalize(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found {
		return "docker.io/library/" + ref
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + ref
	}
	return ref
}

type Store struct {
	db       *db.DB
	defaults map[string]any
}

func NewStore(d *db.DB, cat *recipes.Catalog) *Store { return &Store{db: d, defaults: Defaults(cat)} }

// EnsureDefaults creates version 1 of each supported policy for organizations that lack it.
func (s *Store) EnsureDefaults(ctx context.Context) error {
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id FROM organizations`)
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, id)
	}
	rows.Close()
	for _, org := range orgs {
		if err := s.ensureOrg(ctx, org); err != nil {
			return err
		}
	}
	return nil
}

var supportedKinds = []string{KindFrontends, KindBaseImages, KindBuildArgs, KindEgress, KindValidation, KindSensitive, KindPublication}

// EnsureOrg creates version 1 of each policy the organization lacks, outside any caller's transaction.
func (s *Store) EnsureOrg(ctx context.Context, org string) error { return s.ensureOrg(ctx, org) }

func (s *Store) ensureOrg(ctx context.Context, org string) error {
	var exists int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM organizations WHERE id = ?`), org).Scan(&exists); err != nil || exists == 0 {
		return err
	}
	for _, kind := range supportedKinds {
		doc, _ := json.Marshal(s.defaults[kind])
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM policies WHERE org_id = ? AND kind = ?`), org, kind).Scan(&n); err != nil || n > 0 {
				return err
			}
			return s.insert(ctx, tx, authz.Principal{OrgID: org, Kind: authz.System}, ids.New(), kind, 1, doc, true, "", "")
		})
		if err != nil && !isUnique(err) {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

func (s *Store) insert(ctx context.Context, tx *sql.Tx, actor authz.Principal, id, kind string, version int, doc []byte, create bool, reason, requestID string) error {
	if create {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO policies (id, org_id, kind, version) VALUES (?, ?, ?, ?)`), id, actor.OrgID, kind, version); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO policy_versions (policy_id, version, document, created_by, created_at) VALUES (?, ?, ?, ?, ?)`),
		id, version, string(doc), actorID(actor), time.Now().UTC()); err != nil {
		return err
	}
	why := fmt.Sprintf("%s v%d", kind, version)
	if reason != "" {
		why += ": " + reason
	}
	return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "policy.update", ResourceType: "policy", ResourceID: id, Outcome: "succeeded", Reason: why, RequestID: requestID})
}

func actorID(p authz.Principal) string {
	if p.ID == "" {
		return "system"
	}
	return p.ID
}

// List returns the current version of each policy, creating defaults the first time an organization is seen.
func (s *Store) List(ctx context.Context, orgID string) ([]Policy, error) {
	list, err := s.list(ctx, orgID)
	if err != nil || len(list) >= len(supportedKinds) {
		return list, err
	}
	if err := s.ensureOrg(ctx, orgID); err != nil {
		return nil, err
	}
	return s.list(ctx, orgID)
}

func (s *Store) list(ctx context.Context, orgID string) ([]Policy, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT p.id, p.kind, p.version, v.document, v.created_at, COALESCE(u.username, v.created_by) FROM policies p JOIN policy_versions v ON v.policy_id = p.id AND v.version = p.version LEFT JOIN users u ON u.id = v.created_by WHERE p.org_id = ? ORDER BY p.kind`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		var doc string
		if err := rows.Scan(&p.ID, &p.Kind, &p.Version, &doc, &p.CreatedAt, &p.CreatedBy); err != nil {
			return nil, err
		}
		p.Document = json.RawMessage(doc)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ByID(ctx context.Context, orgID, id string) (Policy, error) {
	return s.one(ctx, `p.id = ? AND p.org_id = ?`, id, orgID)
}

func (s *Store) ByKind(ctx context.Context, orgID, kind string) (Policy, error) {
	return s.one(ctx, `p.kind = ? AND p.org_id = ?`, kind, orgID)
}

func (s *Store) one(ctx context.Context, where string, args ...any) (Policy, error) {
	var p Policy
	var doc string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT p.id, p.kind, p.version, v.document, v.created_at, COALESCE(u.username, v.created_by) FROM policies p JOIN policy_versions v ON v.policy_id = p.id AND v.version = p.version LEFT JOIN users u ON u.id = v.created_by WHERE `+where), args...).
		Scan(&p.ID, &p.Kind, &p.Version, &doc, &p.CreatedAt, &p.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	p.Document = json.RawMessage(doc)
	return p, err
}

// Replace writes a new version when ifMatch equals the current version; organization administrators only.
// Replace stores a new version. The reason, when given, is kept in the audit record; the API requires one for changes
// with impact (TM-92).
func (s *Store) Replace(ctx context.Context, actor authz.Principal, id, kind string, ifMatch int, doc []byte, reason, requestID string) (Policy, error) {
	if !actor.IsOrgAdmin() {
		return Policy{}, ErrForbidden
	}
	current, err := s.ByID(ctx, actor.OrgID, id)
	if err != nil {
		return Policy{}, err
	}
	if kind != current.Kind {
		return Policy{}, &Invalid{fmt.Sprintf("this policy is of kind %s", current.Kind)}
	}
	canonical, err := Validate(kind, doc)
	if err != nil {
		return Policy{}, err
	}
	next := current.Version + 1
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE policies SET version = ? WHERE id = ? AND version = ?`), next, id, ifMatch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		return s.insert(ctx, tx, actor, id, kind, next, canonical, false, reason, requestID)
	})
	if err != nil {
		return Policy{}, err
	}
	return s.ByID(ctx, actor.OrgID, id)
}

// Effective is the set of policies admission and discovery evaluate, with the versions that applied.
type Effective struct {
	Frontends     Frontends
	BaseImages    BaseImages
	BuildArgs     BuildArgs
	Egress        egress.Policy
	Validation    Validation
	Sensitive     Sensitive
	Publication   Publication
	IDs, Versions map[string]string
}

func (s *Store) Effective(ctx context.Context, orgID string) (Effective, error) {
	e := Effective{IDs: map[string]string{}, Versions: map[string]string{}}
	list, err := s.List(ctx, orgID)
	if err != nil {
		return e, err
	}
	for _, p := range list {
		if err := e.Set(p.Kind, p.Document); err != nil {
			return e, err
		}
		e.IDs[p.Kind], e.Versions[p.Kind] = p.ID, fmt.Sprint(p.Version)
	}
	return e, nil
}

// Set replaces one kind's document, as a dry run does with a proposed version. Values are decoded fresh so a
// copied Effective never shares slices with the original.
func (e *Effective) Set(kind string, doc json.RawMessage) error {
	switch kind {
	case KindFrontends:
		var v Frontends
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.Frontends = v
	case KindBaseImages:
		var v BaseImages
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.BaseImages = v
	case KindBuildArgs:
		var v BuildArgs
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.BuildArgs = v
	case KindEgress:
		var v egress.Policy
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.Egress = v
	case KindValidation:
		var v Validation
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.Validation = v
	case KindSensitive:
		var v Sensitive
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.Sensitive = v
	case KindPublication:
		var v Publication
		if err := json.Unmarshal(doc, &v); err != nil {
			return err
		}
		e.Publication = v
	}
	return nil
}

// ArgRules converts the build-args policy for the semantic validator.
func (e Effective) ArgRules() []buildspec.ArgRule {
	var out []buildspec.ArgRule
	for _, r := range e.BuildArgs.Rules {
		ar := buildspec.ArgRule{ID: r.ID, Reason: r.Reason}
		if r.NamePattern != "" {
			ar.NamePattern = regexp.MustCompile(r.NamePattern)
		}
		if r.ValuePattern != "" {
			ar.ValuePattern = regexp.MustCompile(r.ValuePattern)
		}
		out = append(out, ar)
	}
	return out
}

// ErrNotPinned is the reason BaseImageAllowed gives for a tag the policy neither accepts nor pins.
const ErrNotPinned = "it is not pinned by digest"

// BaseImageAllowed reports why ref is refused, or "" when the policy allows it. A tag that NeedsPin is allowed here;
// admission must still pin it.
func (e Effective) BaseImageAllowed(ref string) string {
	n := Normalize(ref)
	if e.BaseImages.RequireDigest && !e.BaseImages.PinTags && !strings.Contains(n, "@sha256:") {
		return ErrNotPinned
	}
	repo := Repository(n)
	for _, r := range e.BaseImages.AllowedRepositories {
		if repo == r {
			return ""
		}
	}
	return "its repository " + repo + " is not on the approved list"
}

// NeedsPin reports whether ref is a tag the policy accepts only once it is pinned to a digest.
func (e Effective) NeedsPin(ref string) bool {
	return e.BaseImages.RequireDigest && e.BaseImages.PinTags && !strings.Contains(ref, "@sha256:")
}

// BaseImageRefusal reports why the organization's current base-image policy refuses ref, or "".
// DeveloperDownloads reports whether the organization lets developers download validated images.
func (s *Store) DeveloperDownloads(ctx context.Context, orgID string) (bool, error) {
	e, err := s.Effective(ctx, orgID)
	return e.Publication.DeveloperDownloads, err
}

func (s *Store) BaseImageRefusal(ctx context.Context, orgID, ref string) (string, error) {
	e, err := s.Effective(ctx, orgID)
	if err != nil {
		return "", err
	}
	return e.BaseImageAllowed(ref), nil
}

// UnlistedRepository returns ref's repository when the approved list lacks it, or "".
func (e Effective) UnlistedRepository(ref string) string {
	repo := Repository(Normalize(ref))
	if slices.Contains(e.BaseImages.AllowedRepositories, repo) {
		return ""
	}
	return repo
}

func (e Effective) FrontendApproved(syntax string) bool {
	for _, a := range e.Frontends.Approved {
		if a == syntax {
			return true
		}
	}
	return false
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// BaseImageListed reports whether repository is on the organization's base-image list.
func (s *Store) BaseImageListed(ctx context.Context, orgID, repository string) (bool, error) {
	e, err := s.Effective(ctx, orgID)
	if err != nil {
		return false, err
	}
	return slices.Contains(e.BaseImages.AllowedRepositories, repository), nil
}

// AllowBaseImageRepository adds repository to the base-images policy inside tx and returns the version that lists
// it. The caller has already authorized actor.
func (s *Store) AllowBaseImageRepository(ctx context.Context, tx *sql.Tx, actor authz.Principal, repository, reason string) (int, error) {
	return s.extend(ctx, tx, actor, KindBaseImages, reason, func(doc []byte) (any, bool, error) {
		var b BaseImages
		if err := json.Unmarshal(doc, &b); err != nil {
			return nil, false, err
		}
		if slices.Contains(b.AllowedRepositories, repository) {
			return nil, false, nil
		}
		b.AllowedRepositories = append(b.AllowedRepositories, repository)
		return b, true, nil
	})
}

// ErrDestinationConflict refuses adding a destination the egress policy already lists under different settings.
var ErrDestinationConflict = errors.New("the egress policy already lists this destination with other settings; edit it in the policy")

// AddEgressDestination adds d to the egress policy inside tx, naming it after its host, and returns the version that
// opens it. A destination already listed and at least as open is left as it is. The caller has already authorized
// actor (docs/architecture/egress.md#promoting-project-allowances).
func (s *Store) AddEgressDestination(ctx context.Context, tx *sql.Tx, actor authz.Principal, d egress.Destination, reason string) (int, error) {
	return s.extend(ctx, tx, actor, KindEgress, reason, func(doc []byte) (any, bool, error) {
		var e egress.Policy
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, false, err
		}
		names := map[string]bool{}
		for _, x := range e.Destinations {
			names[x.Name] = true
			if !x.Has(d.Host, d.Port) {
				continue
			}
			if !x.Disabled && (!x.NoDockerfile || d.NoDockerfile) && (x.Internal() || !d.Internal()) {
				return nil, false, nil
			}
			return nil, false, ErrDestinationConflict
		}
		d.Name = destinationName(d.Host, names)
		e.Destinations = append(e.Destinations, d)
		return e, true, nil
	})
}

var nameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

func destinationName(host string, taken map[string]bool) string {
	base := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(host), "-"), "-")
	if base == "" {
		base = "destination"
	}
	if len(base) > 56 {
		base = strings.TrimRight(base[:56], "-")
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// extend writes a new version of the org's kind policy inside tx when change reports a change, and returns the
// version in force. Bumping the version first takes the policy row's write lock, so a concurrent Replace either
// committed before (and its document is the one extended) or fails its version check.
func (s *Store) extend(ctx context.Context, tx *sql.Tx, actor authz.Principal, kind, reason string, change func(doc []byte) (any, bool, error)) (int, error) {
	current := func(behind int) (string, int, []byte, error) {
		var id, doc string
		var version int
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT p.id, p.version, v.document FROM policies p JOIN policy_versions v ON v.policy_id = p.id AND v.version = p.version - ? WHERE p.org_id = ? AND p.kind = ?`), behind, actor.OrgID, kind).
			Scan(&id, &version, &doc)
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, nil, ErrNotFound
		}
		return id, version, []byte(doc), err
	}
	_, version, doc, err := current(0)
	if err != nil {
		return 0, err
	}
	if _, changed, err := change(doc); err != nil || !changed {
		return version, err
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE policies SET version = version + 1 WHERE org_id = ? AND kind = ?`), actor.OrgID, kind); err != nil {
		return 0, err
	}
	id, next, doc, err := current(1)
	if err != nil {
		return 0, err
	}
	v, changed, err := change(doc)
	if err != nil {
		return 0, err
	}
	if changed {
		doc, _ = json.Marshal(v)
	}
	canonical, err := Validate(kind, doc)
	if err != nil {
		return 0, err
	}
	return next, s.insert(ctx, tx, actor, id, kind, next, canonical, false, reason, "")
}
