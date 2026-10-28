package builds

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
)

type fakeProviders struct{ c providers.Config }

func (f fakeProviders) Config(context.Context, string, string) (providers.Config, error) {
	return f.c, nil
}

func (f fakeProviders) MethodAllowed(_ context.Context, _, _, pool, project string) error {
	if !f.c.Allows(pool, project) {
		return &providers.Invalid{Reason: "not here"}
	}
	return nil
}

// TestRailpackDispatch: a railpack run goes only to a worker with the approved Railpack version, in a pool the
// administrator allows for the project; the plan pins the provider, and Railpack cache keys come from the
// per-project cache scope (SEC-RPK-04, SEC-RPK-03).
func TestRailpackDispatch(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := providers.Config{Enabled: true, Version: "v0.43.0", Pools: []string{"pool-a"}, EgressPresets: []string{}, TimeoutSeconds: 0,
		Artifacts: map[string]string{"binary-arm64": "sha256:b", "frontend": "ghcr.io/railwayapp/railpack-frontend:v0.43.0@sha256:f", "builder": "b@sha256:1", "runtime": "r@sha256:2"}}
	s := &Service{db: d, providers: fakeProviders{cfg}, now: time.Now}
	sc := &Scheduler{s: s}

	doc := &buildspec.Document{Spec: buildspec.Spec{Build: buildspec.Build{Method: "railpack", Railpack: &buildspec.Railpack{Context: "api"}}}}
	if _, err := railpackFor(doc, sources.Manifest{Files: []sources.File{{Path: "web/index.html"}}}, cfg); err == nil {
		t.Error("a Railpack context missing from the upload was planned")
	}
	rp, err := railpackFor(doc, sources.Manifest{Files: []sources.File{{Path: "api/package.json"}}}, cfg)
	if err != nil || rp.Version != "v0.43.0" || rp.Frontend != cfg.Artifacts["frontend"] || len(rp.Images) != 2 || rp.TimeoutSeconds != DefaultRailpackTimeout {
		t.Fatalf("railpack pin = %+v, %v", rp, err)
	}
	if rp.User != buildplan.RailpackUser {
		t.Errorf("railpack user = %q; images run as %s unless the administrator keeps root", rp.User, buildplan.RailpackUser)
	}
	rootCfg := cfg
	rootCfg.RunAsRoot = true
	if root, err := railpackFor(doc, sources.Manifest{Files: []sources.File{{Path: "api/package.json"}}}, rootCfg); err != nil || root.User != "" {
		t.Errorf("runAsRoot: railpack pin = %+v, %v; want no user", root, err)
	}
	if err := (&providers.Config{RunAsRoot: true}).Validate(providers.Catalog[1]); err == nil {
		t.Error("runAsRoot was accepted for a provider other than Railpack")
	}
	plan := buildplan.Plan{Version: 1, Method: "railpack", Railpack: rp}
	raw, digest := plan.Encode()
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`), digest, string(raw), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	need, poolAllows, err := sc.providerNeeds(ctx, Run{OrgID: "o1", ProjectID: "p1", PlanDigest: digest})
	if err != nil || need["railpack"] != "v0.43.0" {
		t.Fatalf("needs = %v, %v", need, err)
	}
	if !poolAllows("pool-a") || poolAllows("pool-b") {
		t.Error("the provider's pool restriction is not applied at dispatch")
	}
	now := time.Now()
	pool := isolation.PoolState{Profile: isolation.RootlessDedicatedVM, ProfileApproved: true}
	worker := func(providers map[string]string) isolation.WorkerState {
		return isolation.WorkerState{EnrollmentActive: true, ProtocolMajor: 1, VersionCompatible: true, OS: "linux", Architectures: []string{"arm64"},
			IsolationProfiles: []string{"rootless-dedicated-vm"}, FreeSlots: 1, StrictEgress: true, Engines: []string{"buildkit"}, Providers: providers}
	}
	req := isolation.Requirement{Platforms: []string{"linux/arm64"}, Providers: need}
	for _, c := range []struct {
		have map[string]string
		ok   bool
	}{{nil, false}, {map[string]string{"railpack": "v0.42.0"}, false}, {map[string]string{"railpack": "0.43.0"}, true}, {map[string]string{"railpack": "v0.43.0"}, true}} {
		ok, why := isolation.Eligible(pool, worker(c.have), req, now)
		if ok != c.ok || (!ok && !slices.Contains(why, "provider-missing")) {
			t.Errorf("worker with %v: eligible %v %v; want %v", c.have, ok, why, c.ok)
		}
	}
	if ok, _ := isolation.Eligible(pool, worker(nil), isolation.Requirement{Platforms: []string{"linux/arm64"}}, now); !ok {
		t.Error("a build without a provider needs one")
	}

	a := artifacts.CacheScope{OrgID: "o1", TrustDomain: DefaultTrustDomain, ProjectID: "p1"}.ID()
	if b := (artifacts.CacheScope{OrgID: "o1", TrustDomain: DefaultTrustDomain, ProjectID: "p2"}).ID(); a == b || !strings.HasPrefix(a, "cs-") {
		t.Errorf("cache keys for two projects = %s, %s", a, b)
	}

	dockerfile, dd := buildplan.Plan{Version: 1, Method: "dockerfile"}.Encode()
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`), dd, string(dockerfile), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if need, poolAllows, err := sc.providerNeeds(ctx, Run{OrgID: "o1", PlanDigest: dd}); err != nil || need != nil || !poolAllows("pool-b") {
		t.Errorf("a Dockerfile run needs %v, pool-b %v, %v", need, poolAllows("pool-b"), err)
	}
}
