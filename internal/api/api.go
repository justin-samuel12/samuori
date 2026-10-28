// Package api implements the /api/v1 operations on top of the generated server interface.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"samuori.invalid/samuori/internal/backup"
	"samuori.invalid/samuori/internal/capacity"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/federation"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/dbcredentials"
	"samuori.invalid/samuori/internal/discovery/jobs"
	"samuori.invalid/samuori/internal/health"
	"samuori.invalid/samuori/internal/identity"
	"samuori.invalid/samuori/internal/imagesources"
	"samuori.invalid/samuori/internal/kubeconvert"
	"samuori.invalid/samuori/internal/messaging"
	"samuori.invalid/samuori/internal/notify"
	"samuori.invalid/samuori/internal/observability"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/pipelines"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/publication"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/selfupdate"
	"samuori.invalid/samuori/internal/signing"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/topology"
	"samuori.invalid/samuori/internal/validation"
	"samuori.invalid/samuori/internal/workers"
)

type Deps struct {
	Log           *slog.Logger
	Health        *health.Registry
	Tenancy       *tenancy.Store
	Auth          *auth.Service
	DataDir       string
	SecureCookies bool
	Sources       *sources.Service
	Operations    *operations.Store
	Discovery     *jobs.Service
	Specs         *specs.Service
	Admission     *admission.Checker
	Recipes       *recipes.Statuses
	Policies      *policy.Store
	Workers       *workers.Store
	DB            *db.DB
	Builds        *builds.Service
	Secrets       *secrets.Service
	Validations   *validation.Service
	Publications  *publication.Service
	Approvals     *approvals.Service
	Signing       *signing.Store
	Artifacts     *artifacts.Store
	Bus           *messaging.Bus
	// ConfirmKey signs policy dry-run confirmations; controllers sharing a database must share it.
	ConfirmKey     []byte
	Backups        *backup.Service
	Cluster        *cluster.Member
	Removals       *cluster.Removals
	ClusterJoin    ClusterJoin
	WorkerJoin     WorkerJoin
	Database       *dbcredentials.Service
	SelfUpdate     *selfupdate.Service
	Capacity       *capacity.Service
	Federation     *federation.Store
	FederationInfo FederationInfo
	Channel        Channel
	RateLimit      RateLimit
	Retention      *audit.Retention
	Diagnostics    observability.Source
	Telemetry      TelemetryStatus
	Identity       *identity.Service
	IdentityStore  *identity.Store
	ImageSources   *imagesources.Store
	Notify         *notify.Service
	Pipelines      *pipelines.Service
	Topology       *topology.Store
	Providers      *providers.Store
	Kube           *kubeconvert.Service
	TopologyInfo   TopologyInfo
}

// Channel is the controller side of the worker protocol that API handlers use.
type Channel interface {
	Connected(workerID string) bool
	RequestSelfTest(ctx context.Context, actor authz.Principal, poolID, requestID string) ([]workers.SelfTest, error)
	RequestWorkerSelfTest(ctx context.Context, actor authz.Principal, workerID, requestID string) (workers.SelfTest, error)
}

type API struct {
	apigen.Unimplemented
	log            *slog.Logger
	health         *health.Registry
	tenancy        *tenancy.Store
	auth           *auth.Service
	dataDir        string
	secureCookies  bool
	sources        *sources.Service
	ops            *operations.Store
	discovery      *jobs.Service
	specs          *specs.Service
	admission      *admission.Checker
	recipes        *recipes.Statuses
	policies       *policy.Store
	workers        *workers.Store
	db             *db.DB
	builds         *builds.Service
	secrets        *secrets.Service
	validations    *validation.Service
	publications   *publication.Service
	approvals      *approvals.Service
	signing        *signing.Store
	artifacts      *artifacts.Store
	bus            *messaging.Bus
	channel        Channel
	rateLimit      RateLimit
	retention      *audit.Retention
	diagnostics    observability.Source
	telemetry      TelemetryStatus
	signIn         *identity.Service
	idStore        *identity.Store
	imageSources   *imagesources.Store
	notify         *notify.Service
	pipelines      *pipelines.Service
	topology       *topology.Store
	providers      *providers.Store
	kube           *kubeconvert.Service
	topologyInfo   TopologyInfo
	hookLimit      *limiter
	confirmKey     []byte
	backups        *backup.Service
	cluster        *cluster.Member
	removals       *cluster.Removals
	clusterJoin    ClusterJoin
	workerJoin     WorkerJoin
	database       *dbcredentials.Service
	selfUpdate     *selfupdate.Service
	capacity       *capacity.Service
	joinLimit      *limiter
	federation     *federation.Store
	federationInfo FederationInfo
}

func New(d Deps) *API {
	a := &API{log: d.Log, health: d.Health, tenancy: d.Tenancy, auth: d.Auth, dataDir: d.DataDir, secureCookies: d.SecureCookies, sources: d.Sources, ops: d.Operations, discovery: d.Discovery, specs: d.Specs, admission: d.Admission, recipes: d.Recipes, policies: d.Policies, workers: d.Workers, db: d.DB, builds: d.Builds, secrets: d.Secrets, validations: d.Validations, publications: d.Publications, approvals: d.Approvals, signing: d.Signing, artifacts: d.Artifacts, bus: d.Bus, channel: d.Channel, rateLimit: d.RateLimit, retention: d.Retention, diagnostics: d.Diagnostics, telemetry: d.Telemetry, signIn: d.Identity, idStore: d.IdentityStore, imageSources: d.ImageSources, notify: d.Notify, pipelines: d.Pipelines, topology: d.Topology, providers: d.Providers, kube: d.Kube, topologyInfo: d.TopologyInfo, hookLimit: newLimiter(RateLimit{PerSecond: 1, Burst: 30}), confirmKey: d.ConfirmKey, backups: d.Backups, cluster: d.Cluster, removals: d.Removals, clusterJoin: d.ClusterJoin, workerJoin: d.WorkerJoin, database: d.Database, selfUpdate: d.SelfUpdate, capacity: d.Capacity, joinLimit: newLimiter(RateLimit{PerSecond: 1, Burst: 5}), federation: d.Federation, federationInfo: d.FederationInfo}
	if a.confirmKey == nil {
		a.confirmKey = randomKey()
	}
	return a
}

func Handler(a *API, authn Authenticator) (http.Handler, error) {
	inner := apigen.HandlerWithOptions(a, apigen.StdHTTPServerOptions{
		BaseURL:    "/api/v1",
		BaseRouter: http.NewServeMux(),
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
		},
	})
	return guard(inner, authn, newLimiter(a.rateLimit))
}

func (a *API) GetHealth(w http.ResponseWriter, r *http.Request) {
	a.health.Handler().ServeHTTP(w, r)
}

func (a *API) GetMe(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	_, display, err := a.tenancy.LoadPrincipal(r.Context(), p.ID)
	if err != nil && !errors.Is(err, tenancy.ErrNotFound) {
		a.internalError(w, r, err)
		return
	}
	me := apigen.Me{Principal: apigen.Principal{Id: p.ID, Type: apigen.PrincipalType(p.Kind)}}
	if display != "" {
		me.DisplayName = &display
	}
	if a.policies != nil && p.OrgID != "" {
		if eff, err := a.policies.Effective(r.Context(), p.OrgID); err == nil && eff.Publication.DeveloperDownloads {
			me.DeveloperDownloads = &eff.Publication.DeveloperDownloads
		}
	}
	for _, b := range p.Bindings {
		me.Roles = append(me.Roles, struct {
			Role      apigen.Role             `json:"role"`
			ScopeId   apigen.Id               `json:"scopeId"`
			ScopeType apigen.MeRolesScopeType `json:"scopeType"`
		}{apigen.Role(b.Role), b.ScopeID, apigen.MeRolesScopeType(b.ScopeType)})
	}
	if me.Roles == nil {
		me.Roles = []struct {
			Role      apigen.Role             `json:"role"`
			ScopeId   apigen.Id               `json:"scopeId"`
			ScopeType apigen.MeRolesScopeType `json:"scopeType"`
		}{}
	}
	writeJSON(w, http.StatusOK, me)
}

func (a *API) ListTeams(w http.ResponseWriter, r *http.Request, _ apigen.ListTeamsParams) {
	teams, err := a.tenancy.ListTeams(r.Context(), principal(r))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := apigen.TeamList{Items: make([]apigen.Team, 0, len(teams))}
	for _, t := range teams {
		out.Items = append(out.Items, apigen.Team{Id: t.ID, Name: t.Name})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ListProjects(w http.ResponseWriter, r *http.Request, params apigen.ListProjectsParams) {
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	projects, err := a.tenancy.ListProjects(r.Context(), principal(r), operationRoles(r.Context()), cursor, limit+1)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := apigen.ProjectList{Items: make([]apigen.Project, 0, len(projects))}
	if len(projects) > limit {
		projects = projects[:limit]
		next := projects[limit-1].ID
		out.NextCursor = &next
	}
	for _, p := range projects {
		out.Items = append(out.Items, toProject(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetProject(w http.ResponseWriter, r *http.Request, id apigen.ProjectId) {
	p, err := a.tenancy.GetProject(r.Context(), principal(r), operationRoles(r.Context()), id)
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toProject(p))
}

func (a *API) CreateProject(w http.ResponseWriter, r *http.Request, _ apigen.CreateProjectParams) {
	var body apigen.ProjectCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return
	}
	mode := ""
	if body.PublicationMode != nil {
		mode = string(*body.PublicationMode)
	}
	p, err := a.tenancy.CreateProject(r.Context(), principal(r), body.TeamId, body.Name, mode, w.Header().Get("X-Request-ID"))
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProject(p))
}

func toProject(p tenancy.Project) apigen.Project {
	mode := apigen.ProjectPublicationMode(p.PublicationMode)
	out := apigen.Project{Id: p.ID, TeamId: p.TeamID, Name: p.Name, PublicationMode: &mode, CreatedAt: p.CreatedAt}
	if p.TeamName != "" {
		out.TeamName = &p.TeamName
	}
	return out
}

func principal(r *http.Request) authz.Principal {
	p, _ := authz.FromContext(r.Context())
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) tenancyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tenancy.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, tenancy.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "your roles do not permit this operation here")
	case errors.Is(err, tenancy.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "a resource with that name already exists")
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("request failed", "id", w.Header().Get("X-Request-ID"), "method", r.Method, "path", r.URL.Path, "error", err)
	apigen.WriteProblem(w, http.StatusInternalServerError, "internal", "internal error; quote request ID "+w.Header().Get("X-Request-ID")+" to an operator")
}

func randomKey() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}
