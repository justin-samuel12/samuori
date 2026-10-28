package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/storage"
)

func (a *API) GetStorageStatus(w http.ResponseWriter, r *http.Request) {
	u, err := a.artifacts.Usage(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type route struct {
		Class    string `json:"class"`
		Backend  string `json:"backend"`
		Location string `json:"location,omitempty"`
	}
	out := struct {
		Backend    string    `json:"backend"`
		Location   string    `json:"location,omitempty"`
		Routes     []route   `json:"routes"`
		Blobs      int64     `json:"blobs"`
		Bytes      int64     `json:"bytes"`
		Held       int64     `json:"held"`
		MeasuredAt time.Time `json:"measuredAt"`
	}{Blobs: u.Blobs, Bytes: u.Bytes, Held: u.Held, MeasuredAt: time.Now().UTC()}
	kinds, locations := map[string]bool{}, map[string]bool{}
	for _, c := range storage.Classes {
		rt := route{Class: string(c), Backend: "local"}
		if r := a.artifacts.Route(c); r != nil {
			rt.Backend, rt.Location = storage.Kind(r), r.Describe()
		}
		kinds[rt.Backend], locations[rt.Location] = true, true
		out.Routes = append(out.Routes, rt)
	}
	out.Backend = "mixed"
	if len(kinds) == 1 && len(locations) == 1 {
		out.Backend, out.Location = out.Routes[0].Backend, out.Routes[0].Location
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ListBuildCaches(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	list, err := a.artifacts.Caches(r.Context(), p.OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type item struct {
		Scope       string     `json:"scope"`
		ProjectID   string     `json:"projectId"`
		ProjectName string     `json:"projectName,omitempty"`
		Entries     int        `json:"entries"`
		Bytes       int64      `json:"bytes"`
		NewestAt    time.Time  `json:"newestAt"`
		LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	}
	out := struct {
		TTLHours      int    `json:"ttlHours"`
		MaxScopeBytes int64  `json:"maxScopeBytes"`
		Items         []item `json:"items"`
	}{TTLHours: int(a.artifacts.CacheLimits.TTL.Hours()), MaxScopeBytes: a.artifacts.CacheLimits.MaxScopeBytes, Items: []item{}}
	for _, c := range list {
		var name string
		_ = a.db.SQL.QueryRowContext(r.Context(), a.db.Rebind(`SELECT name FROM projects WHERE id = ? AND org_id = ?`), c.ProjectID, p.OrgID).Scan(&name)
		out.Items = append(out.Items, item{c.Scope, c.ProjectID, name, c.Entries, c.Bytes, c.Newest, c.LastUsed})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) PurgeBuildCache(w http.ResponseWriter, r *http.Request, scope string, _ apigen.PurgeBuildCacheParams) {
	if _, err := a.artifacts.PurgeCache(r.Context(), principal(r), scope, requestID(w)); err != nil {
		if errors.Is(err, artifacts.ErrNoCache) {
			apigen.WriteProblem(w, http.StatusNotFound, "not_found", "the scope has no cache")
			return
		}
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) CollectGarbage(w http.ResponseWriter, r *http.Request, _ apigen.CollectGarbageParams) {
	res, err := a.artifacts.Collect(r.Context(), artifacts.DefaultGCPolicy())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.GarbageCollection{Deleted: res.Deleted, Referenced: res.Referenced, KeptForGrace: res.Kept, DroppedArtifacts: res.DroppedArtifacts})
}

type consumerView struct {
	Name        string `json:"name"`
	Pending     uint64 `json:"pending"`
	AckPending  int    `json:"ackPending"`
	Redelivered int    `json:"redelivered"`
}

func (a *API) GetMessagingStatus(w http.ResponseWriter, r *http.Request) {
	if a.bus == nil {
		apigen.WriteProblem(w, http.StatusServiceUnavailable, "messaging_unavailable", "messaging is not running on this controller")
		return
	}
	st, err := a.bus.Status(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	pending, err := a.db.PendingOutbox(r.Context(), 10_000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Embedded         bool           `json:"embedded"`
		ListensOnNetwork bool           `json:"listensOnNetwork"`
		Connected        bool           `json:"connected"`
		Messages         uint64         `json:"messages"`
		OutboxPending    int            `json:"outboxPending"`
		Consumers        []consumerView `json:"consumers"`
	}{true, a.bus.ListensOnNetwork(), st.Connected, st.Messages, len(pending), []consumerView{}}
	for _, c := range st.Consumers {
		out.Consumers = append(out.Consumers, consumerView{c.Name, c.Pending, c.AckPending, c.Redelivered})
	}
	sort.Slice(out.Consumers, func(i, j int) bool { return out.Consumers[i].Name < out.Consumers[j].Name })
	writeJSON(w, http.StatusOK, out)
}

type engineView struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Workers   int    `json:"workers"`
	Reason    string `json:"reason,omitempty"`
}

// GetEngineStatus reports engines from what connected workers advertise; nothing is assumed to exist.
func (a *API) GetEngineStatus(w http.ResponseWriter, r *http.Request) {
	list, err := a.workers.List(r.Context(), principal(r).OrgID, "", 10_000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	counts := map[string]int{"buildkit": 0}
	connected, profiled := 0, 0
	for _, wk := range list {
		online := a.channel != nil && a.channel.Connected(wk.ID)
		if online {
			connected++
		}
		if len(wk.Capabilities.IsolationProfiles) > 0 {
			profiled++
		}
		for _, e := range wk.Capabilities.Engines {
			if online {
				counts[e]++
			}
		}
	}
	out := struct {
		Engines []engineView `json:"engines"`
		Workers struct {
			Enrolled             int `json:"enrolled"`
			Connected            int `json:"connected"`
			WithIsolationProfile int `json:"withIsolationProfile"`
		} `json:"workers"`
	}{}
	out.Workers.Enrolled, out.Workers.Connected, out.Workers.WithIsolationProfile = len(list), connected, profiled
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := engineView{Name: n, Workers: counts[n], Available: counts[n] > 0}
		if !v.Available {
			v.Reason = "No connected worker offers this engine. Workers need a validated isolation provider and the engine adapter."
		}
		out.Engines = append(out.Engines, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// GetEgressStatus shows the presets, the organization's egress policy and how many connected workers enforce it.
func (a *API) GetEgressStatus(w http.ResponseWriter, r *http.Request) {
	org := principal(r).OrgID
	cat, err := egress.Builtin()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	pols, err := a.policies.List(r.Context(), org)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	list, err := a.workers.List(r.Context(), org, "", 10_000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	type presetView struct {
		ID           string   `json:"id"`
		Version      string   `json:"version"`
		DisplayName  string   `json:"displayName"`
		Destinations []string `json:"destinations"`
		Note         string   `json:"note,omitempty"`
	}
	out := struct {
		Presets []presetView `json:"presets"`
		Policy  struct {
			PolicyID string `json:"policyId"`
			Version  int    `json:"version"`
			egress.Policy
		} `json:"policy"`
		Workers struct {
			Connected    int `json:"connected"`
			StrictEgress int `json:"strictEgress"`
		} `json:"workers"`
	}{Presets: []presetView{}}
	for _, p := range cat.All() {
		out.Presets = append(out.Presets, presetView{p.ID, p.Version, p.DisplayName, p.Destinations, p.Note})
	}
	for _, p := range pols {
		if p.Kind == policy.KindEgress {
			if err := json.Unmarshal(p.Document, &out.Policy.Policy); err != nil {
				a.internalError(w, r, err)
				return
			}
			out.Policy.PolicyID, out.Policy.Version = p.ID, p.Version
		}
	}
	for _, wk := range list {
		if a.channel != nil && a.channel.Connected(wk.ID) {
			out.Workers.Connected++
			if wk.Capabilities.StrictEgress {
				out.Workers.StrictEgress++
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
