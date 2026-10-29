package api

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/recipedef"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/version"
)

// The exported configuration (ADR 0069, api/schemas/configuration.v1alpha1.json). It holds names and settings only:
// no secret value, credential, key, user, role binding or project content. Projects are referred to as team/project.
type configFile struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   configMetadata `json:"metadata"`
	Spec       configSpec     `json:"spec"`
}

type configMetadata struct {
	ExportedAt     string `json:"exportedAt,omitempty"`
	SamuoriVersion string `json:"samuoriVersion,omitempty"`
	Organization   string `json:"organization,omitempty"`
}

type configSpec struct {
	Teams                []string                   `json:"teams,omitempty"`
	Identity             *configIdentity            `json:"identity,omitempty"`
	AuditRetention       *configAudit               `json:"auditRetention,omitempty"`
	Policies             map[string]json.RawMessage `json:"policies,omitempty"`
	BuildMethods         *configBuildMethods        `json:"buildMethods,omitempty"`
	Recipes              *configRecipes             `json:"recipes,omitempty"`
	WorkerPools          []configPool               `json:"workerPools,omitempty"`
	Registries           []configRegistry           `json:"registries,omitempty"`
	CredentialProfiles   []configProfile            `json:"credentialProfiles,omitempty"`
	Destinations         []configDestination        `json:"destinations,omitempty"`
	NotificationChannels []configChannel            `json:"notificationChannels,omitempty"`
	NotificationRules    []configRule               `json:"notificationRules,omitempty"`
}

type configIdentity struct {
	LocalSignIn string        `json:"localSignIn"`
	Session     configSession `json:"session"`
}

type configSession struct {
	IdleTimeoutMinutes int `json:"idleTimeoutMinutes"`
	MaxSessionHours    int `json:"maxSessionHours"`
	WarningSeconds     int `json:"warningSeconds"`
}

type configAudit struct {
	RetentionDays int  `json:"retentionDays"`
	ExportEnabled bool `json:"exportEnabled"`
	RequireExport bool `json:"requireExport"`
}

type configBuildMethods struct {
	Allowed   []string         `json:"allowed"`
	Default   string           `json:"default,omitempty"`
	Providers []configProvider `json:"providers,omitempty"`
}

type configProvider struct {
	Name           string            `json:"name"`
	Enabled        bool              `json:"enabled"`
	Version        string            `json:"version,omitempty"`
	Artifacts      map[string]string `json:"artifacts,omitempty"`
	Pools          []string          `json:"pools,omitempty"`
	Projects       []string          `json:"projects,omitempty"`
	EgressPresets  []string          `json:"egressPresets,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	RunAsRoot      bool              `json:"runAsRoot,omitempty"`
}

type configRecipes struct {
	Statuses             []configRecipeStatus  `json:"statuses,omitempty"`
	ShippedRuntimes      []configShipped       `json:"shippedRuntimes,omitempty"`
	OrganizationRuntimes []configOrgRuntime    `json:"organizationRuntimes,omitempty"`
	Custom               []configCustomVersion `json:"custom,omitempty"`
}

type configRecipeStatus struct {
	Recipe  string `json:"recipe"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
}

type configShipped struct {
	Recipe  string `json:"recipe"`
	Version string `json:"version"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
}

type configOrgRuntime struct {
	Recipe     string `json:"recipe"`
	Version    string `json:"version"`
	Label      string `json:"label"`
	BuildImage string `json:"buildImage"`
	RunImage   string `json:"runImage"`
	Note       string `json:"note,omitempty"`
	Status     string `json:"status"`
}

type configCustomVersion struct {
	Status     string               `json:"status"`
	Reason     string               `json:"reason,omitempty"`
	Definition recipedef.Definition `json:"definition"`
}

type configPool struct {
	Name                 string `json:"name"`
	Location             string `json:"location,omitempty"`
	IsolationProfile     string `json:"isolationProfile"`
	TrustDomain          string `json:"trustDomain"`
	TrustedWorkloadsOnly bool   `json:"trustedWorkloadsOnly,omitempty"`
}

type configRegistry struct {
	Name                string `json:"name"`
	Kind                string `json:"kind"`
	Endpoint            string `json:"endpoint,omitempty"`
	AccountID           string `json:"accountId,omitempty"`
	Region              string `json:"region,omitempty"`
	PlainHTTP           bool   `json:"plainHttp,omitempty"`
	AllowPrivateNetwork bool   `json:"allowPrivateNetwork,omitempty"`
}

// configProfile names registry credentials without their contents, so the importer knows what to add again.
type configProfile struct {
	Name             string   `json:"name"`
	Registry         string   `json:"registry"`
	Type             string   `json:"type"`
	Purpose          string   `json:"purpose"`
	RepositoryScopes []string `json:"repositoryScopes,omitempty"`
}

type configDestination struct {
	Name              string   `json:"name"`
	Registry          string   `json:"registry"`
	Repository        string   `json:"repository"`
	PushProfile       string   `json:"pushProfile"`
	AllowedProjects   []string `json:"allowedProjects,omitempty"`
	ProtectedTags     []string `json:"protectedTags,omitempty"`
	ImmutableTags     bool     `json:"immutableTags,omitempty"`
	RequiresApproval  bool     `json:"requiresApproval,omitempty"`
	AcceptsRestricted bool     `json:"acceptsRestricted,omitempty"`
	SignImages        bool     `json:"signImages,omitempty"`
	AttachProvenance  bool     `json:"attachProvenance,omitempty"`
	PromotionOnly     bool     `json:"promotionOnly,omitempty"`
}

// configChannel leaves out the stored secret: a webhook's signing key, or a Slack or Teams incoming webhook URL.
type configChannel struct {
	Name                string   `json:"name"`
	Kind                string   `json:"kind"`
	URL                 string   `json:"url,omitempty"`
	Recipients          []string `json:"recipients,omitempty"`
	AllowPrivateNetwork bool     `json:"allowPrivateNetwork,omitempty"`
	Enabled             bool     `json:"enabled"`
}

type configRule struct {
	Channel string   `json:"channel"`
	Events  []string `json:"events"`
	Team    string   `json:"team,omitempty"`
	Project string   `json:"project,omitempty"`
}

const configKind = "Configuration"

// configRefs maps the IDs a configuration refers to onto the names it is exported with.
type configRefs struct {
	teams, projects, pools map[string]string
}

func (a *API) configNames(ctx context.Context, p authz.Principal) (configRefs, error) {
	n := configRefs{teams: map[string]string{}, projects: map[string]string{}, pools: map[string]string{}}
	teams, err := a.tenancy.ListTeams(ctx, p)
	if err != nil {
		return n, err
	}
	for _, t := range teams {
		n.teams[t.ID] = t.Name
	}
	projects, err := a.tenancy.ListProjects(ctx, p, []authz.Role{authz.Administrator}, "", 1_000_000, true)
	if err != nil {
		return n, err
	}
	for _, pr := range projects {
		n.projects[pr.ID] = n.teams[pr.TeamID] + "/" + pr.Name
	}
	pools, err := a.workers.ListPools(ctx, p.OrgID)
	if err != nil {
		return n, err
	}
	for _, pl := range pools {
		n.pools[pl.ID] = pl.Name
	}
	return n, nil
}

// named returns the names of ids; an ID with no name stays as it is, so it cannot resolve on import.
func named(ids []string, m map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, cmp.Or(m[id], id))
	}
	slices.Sort(out)
	return out
}

func (a *API) exportConfiguration(ctx context.Context, p authz.Principal) (configFile, error) {
	org, err := a.tenancy.OrganizationName(ctx, p.OrgID)
	if err != nil {
		return configFile{}, err
	}
	f := configFile{APIVersion: "samuori.invalid/v1alpha1", Kind: configKind,
		Metadata: configMetadata{ExportedAt: time.Now().UTC().Format(time.RFC3339), SamuoriVersion: version.Version, Organization: org}}
	s := &f.Spec
	n, err := a.configNames(ctx, p)
	if err != nil {
		return f, err
	}

	login, err := a.auth.LocalLogin(ctx, p.OrgID)
	if err != nil {
		return f, err
	}
	t, err := a.auth.SessionTimeouts(ctx, p.OrgID)
	if err != nil {
		return f, err
	}
	s.Identity = &configIdentity{LocalSignIn: login, Session: configSession{int(t.Idle / time.Minute), int(t.Max / time.Hour), int(t.Warning / time.Second)}}
	ap, err := a.retention.Policy(ctx, p.OrgID)
	if err != nil {
		return f, err
	}
	s.AuditRetention = &configAudit{ap.RetentionDays, ap.ExportEnabled, ap.RequireExport}

	pols, err := a.policies.List(ctx, p.OrgID)
	if err != nil {
		return f, err
	}
	s.Policies = map[string]json.RawMessage{}
	for _, pol := range pols {
		s.Policies[pol.Kind] = pol.Document
	}

	if s.BuildMethods, err = a.exportBuildMethods(ctx, p, n); err != nil {
		return f, err
	}
	if s.Recipes, err = a.exportRecipes(ctx, p); err != nil {
		return f, err
	}

	pools, err := a.workers.ListPools(ctx, p.OrgID)
	if err != nil {
		return f, err
	}
	for _, pl := range pools {
		s.WorkerPools = append(s.WorkerPools, configPool{pl.Name, pl.Location, pl.IsolationProfile, pl.TrustDomain, pl.TrustedWorkloadsOnly})
	}

	if err := a.exportPublication(ctx, p, n, s); err != nil {
		return f, err
	}
	if err := a.exportNotifications(ctx, p, n, s); err != nil {
		return f, err
	}
	return f, nil
}

func (a *API) exportBuildMethods(ctx context.Context, p authz.Principal, n configRefs) (*configBuildMethods, error) {
	pol, err := a.providers.Policy(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := &configBuildMethods{Allowed: slices.Sorted(slices.Values(pol.Allowed)), Default: pol.Default}
	for _, d := range providers.Catalog {
		c, err := a.providers.Config(ctx, p.OrgID, d.Name)
		if err != nil {
			return nil, err
		}
		if !c.Enabled && c.Version == "" && len(c.Artifacts) == 0 && len(c.Pools) == 0 && len(c.Projects) == 0 && len(c.EgressPresets) == 0 && c.TimeoutSeconds == 0 && !c.RunAsRoot {
			continue
		}
		out.Providers = append(out.Providers, configProvider{Name: d.Name, Enabled: c.Enabled, Version: c.Version, Artifacts: c.Artifacts,
			Pools: named(c.Pools, n.pools), Projects: named(c.Projects, n.projects), EgressPresets: slices.Sorted(slices.Values(c.EgressPresets)),
			TimeoutSeconds: c.TimeoutSeconds, RunAsRoot: c.RunAsRoot})
	}
	slices.SortFunc(out.Providers, func(x, y configProvider) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// exportRecipes keeps decisions that differ from a release's defaults, and leaves out withdrawn organization runtimes
// and custom versions: they are history, not configuration.
func (a *API) exportRecipes(ctx context.Context, p authz.Principal) (*configRecipes, error) {
	out := &configRecipes{}
	all, statuses, err := a.recipes.All(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	for _, rc := range all {
		if rc.Custom != nil {
			continue
		}
		if st := statuses[rc.ID+"@"+rc.Version]; st.Status != recipes.Approved {
			out.Statuses = append(out.Statuses, configRecipeStatus{rc.ID, rc.Version, st.Status, st.Reason})
		}
		shipped, org, err := a.recipes.ListRuntimes(ctx, p, rc.ID, rc.Version)
		if err != nil {
			return nil, err
		}
		for _, rt := range shipped {
			if rt.Status != "" && rt.Status != recipes.Approved {
				out.ShippedRuntimes = append(out.ShippedRuntimes, configShipped{rc.ID, rc.Version, rt.Version, rt.Status, rt.Reason})
			}
		}
		for _, o := range org {
			if o.Status == recipes.Draft || o.Status == recipes.Approved {
				out.OrganizationRuntimes = append(out.OrganizationRuntimes, configOrgRuntime{rc.ID, rc.Version, o.Label, o.BuildImage, o.RunImage, o.Note, o.Status})
			}
		}
	}
	custom, err := a.recipes.ListCustom(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, v := range custom {
		if v.Status != recipes.Withdrawn {
			out.Custom = append(out.Custom, configCustomVersion{v.Status, v.Reason, v.Definition})
		}
	}
	slices.SortFunc(out.Statuses, func(x, y configRecipeStatus) int {
		return cmp.Or(strings.Compare(x.Recipe, y.Recipe), strings.Compare(x.Version, y.Version))
	})
	slices.SortFunc(out.ShippedRuntimes, func(x, y configShipped) int {
		return cmp.Or(strings.Compare(x.Recipe, y.Recipe), strings.Compare(x.Version, y.Version), strings.Compare(x.Label, y.Label))
	})
	slices.SortFunc(out.OrganizationRuntimes, func(x, y configOrgRuntime) int {
		return cmp.Or(strings.Compare(x.Recipe, y.Recipe), strings.Compare(x.Version, y.Version), strings.Compare(x.Label, y.Label))
	})
	slices.SortFunc(out.Custom, func(x, y configCustomVersion) int {
		return cmp.Or(strings.Compare(x.Definition.ID, y.Definition.ID), strings.Compare(x.Definition.Version, y.Definition.Version))
	})
	if len(out.Statuses)+len(out.ShippedRuntimes)+len(out.OrganizationRuntimes)+len(out.Custom) == 0 {
		return nil, nil
	}
	return out, nil
}

func (a *API) exportPublication(ctx context.Context, p authz.Principal, n configRefs, s *configSpec) error {
	regs, err := a.publications.Registries(ctx, p)
	if err != nil {
		return err
	}
	regName := map[string]string{}
	for _, r := range regs {
		regName[r.ID] = r.Name
		c := configRegistry{Name: r.Name, Kind: r.Kind, PlainHTTP: r.PlainHTTP, AllowPrivateNetwork: r.AllowPrivate}
		if r.Kind == "ecr" {
			c.AccountID, c.Region = r.AccountID, r.Region
		} else {
			c.Endpoint = r.Endpoint
		}
		s.Registries = append(s.Registries, c)
	}
	profiles, err := a.publications.Profiles(ctx, p)
	if err != nil {
		return err
	}
	profileName := map[string]string{}
	for _, pr := range profiles {
		profileName[pr.ID] = pr.Name
		s.CredentialProfiles = append(s.CredentialProfiles, configProfile{pr.Name, regName[pr.RegistryID], pr.Type, pr.Purpose, slices.Sorted(slices.Values(pr.Scopes))})
	}
	dests, err := a.publications.Destinations(ctx, p)
	if err != nil {
		return err
	}
	for _, d := range dests {
		s.Destinations = append(s.Destinations, configDestination{Name: d.Name, Registry: regName[d.RegistryID], Repository: d.Repository,
			PushProfile: profileName[d.PushProfileID], AllowedProjects: named(d.AllowedProjects, n.projects), ProtectedTags: slices.Sorted(slices.Values(d.ProtectedTags)),
			ImmutableTags: d.ImmutableTags, RequiresApproval: d.RequiresApproval, AcceptsRestricted: d.AcceptsRestricted, SignImages: d.SignImages,
			AttachProvenance: d.AttachProvenance, PromotionOnly: d.PromotionOnly})
	}
	return nil
}

func (a *API) exportNotifications(ctx context.Context, p authz.Principal, n configRefs, s *configSpec) error {
	channels, err := a.notify.Channels(ctx, p)
	if err != nil {
		return err
	}
	channelName := map[string]string{}
	for _, c := range channels {
		channelName[c.ID] = c.Name
		s.NotificationChannels = append(s.NotificationChannels, configChannel{c.Name, c.Kind, c.URL, c.Recipients, c.AllowPrivateNetwork, c.Enabled})
	}
	rules, err := a.notify.Rules(ctx, p)
	if err != nil {
		return err
	}
	teams := map[string]bool{}
	for _, r := range rules {
		c := configRule{Channel: channelName[r.ChannelID], Events: slices.Sorted(slices.Values(r.Events))}
		if r.TeamID != "" {
			c.Team = cmp.Or(n.teams[r.TeamID], r.TeamID)
			teams[c.Team] = true
		}
		if r.ProjectID != "" {
			c.Project = cmp.Or(n.projects[r.ProjectID], r.ProjectID)
		}
		s.NotificationRules = append(s.NotificationRules, c)
	}
	slices.SortFunc(s.NotificationRules, func(x, y configRule) int {
		return cmp.Or(strings.Compare(x.Channel, y.Channel), strings.Compare(x.Team, y.Team), strings.Compare(x.Project, y.Project),
			strings.Compare(strings.Join(x.Events, ","), strings.Join(y.Events, ",")))
	})
	s.Teams = slices.Sorted(func(yield func(string) bool) {
		for t := range teams {
			if !yield(t) {
				return
			}
		}
	})
	return nil
}

// toYAML renders v in block style with the field order of its JSON encoding.
func toYAML(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	var plain func(*yaml.Node)
	plain = func(n *yaml.Node) {
		n.Style = 0
		for _, c := range n.Content {
			plain(c)
		}
	}
	plain(&node)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

func (a *API) ExportConfiguration(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.IsOrgAdmin() {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators export the configuration")
		return
	}
	f, err := a.exportConfiguration(r.Context(), p)
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	asJSON := strings.Contains(r.Header.Get("Accept"), "application/json") && !strings.Contains(r.Header.Get("Accept"), "yaml")
	var body []byte
	if asJSON {
		body, err = json.MarshalIndent(f, "", "  ")
	} else {
		body, err = toYAML(f)
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.db.InTx(r.Context(), func(tx *sql.Tx) error {
		return audit.Append(r.Context(), a.db, tx, audit.Event{Actor: p, Action: "configuration.export", ResourceType: "configuration", ResourceID: p.OrgID, Outcome: "succeeded", RequestID: requestID(w)})
	}); err != nil {
		a.internalError(w, r, err)
		return
	}
	ext, ctype := ".yml", "application/yaml"
	if asJSON {
		ext, ctype = ".json", "application/json"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="samuori-configuration-`+time.Now().UTC().Format("20060102T150405Z")+ext+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
