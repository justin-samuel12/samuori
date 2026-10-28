package builds

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
)

// TestBuildpacksDispatch: the plan pins the approved builder and run image by digest and names secrets by logical ID
// only; a buildpacks run goes only to a worker that reports the CNB platform API, in a pool the administrator allows
// for the project (SEC-CNB-02, SEC-CNB-04).
func TestBuildpacksDispatch(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := providers.Config{Enabled: true, Version: "v0.21.26", Pools: []string{"pool-a"},
		Artifacts: map[string]string{"builder": "docker.io/paketobuildpacks/ubuntu-noble-builder:0.0.205@sha256:" + strings.Repeat("b", 64), "run-image": "docker.io/paketobuildpacks/ubuntu-noble-run:0.0.150@sha256:" + strings.Repeat("r", 64)}}
	s := &Service{db: d, providers: fakeProviders{cfg}, now: time.Now}
	sc := &Scheduler{s: s}

	doc := &buildspec.Document{Spec: buildspec.Spec{Build: buildspec.Build{Method: "buildpacks", Buildpacks: &buildspec.Buildpacks{Context: "api", Env: map[string]string{"BP_NODE_VERSION": "22.*"},
		Bindings: []buildspec.BuildpacksBinding{{Name: "npmrc", Type: "npmrc", Secret: "npm-token", Key: ".npmrc"}, {Name: "extra", Type: "generic", Secret: "optional"}}}}}}
	secrets := []buildplan.PlanSecret{{ID: "npm-token", Required: true, BindingID: "bind-1", BindingVersion: 3}, {ID: "optional"}}
	if _, err := buildpacksFor(doc, sources.Manifest{Files: []sources.File{{Path: "web/index.html"}}}, cfg, secrets); err == nil {
		t.Error("a buildpacks context missing from the upload was planned")
	}
	bp, err := buildpacksFor(doc, sources.Manifest{Files: []sources.File{{Path: "api/package.json"}}}, cfg, secrets)
	if err != nil || bp.Builder != cfg.Artifacts["builder"] || bp.RunImage != cfg.Artifacts["run-image"] || bp.PlatformAPI != providers.CNBPlatformAPI || bp.TimeoutSeconds != DefaultBuildpacksTimeout {
		t.Fatalf("buildpacks pin = %+v, %v", bp, err)
	}
	if len(bp.Bindings) != 1 || bp.Bindings[0] != (buildplan.PlanBinding{Name: "npmrc", Type: "npmrc", Key: ".npmrc", SecretID: "npm-token"}) {
		t.Errorf("bindings = %+v; an unbound optional secret must not be mounted", bp.Bindings)
	}
	noRun := providers.Config{Enabled: true, Artifacts: map[string]string{"builder": cfg.Artifacts["builder"]}}
	if _, err := buildpacksFor(doc, sources.Manifest{Files: []sources.File{{Path: "api/package.json"}}}, noRun, secrets); err == nil {
		t.Error("a buildpacks build was planned without an approved run image")
	}

	plan := buildplan.Plan{Version: 1, Method: "buildpacks", Buildpacks: bp, Secrets: secrets}
	raw, digest := plan.Encode()
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`), digest, string(raw), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	need, poolAllows, err := sc.providerNeeds(ctx, Run{OrgID: "o1", ProjectID: "p1", PlanDigest: digest})
	if err != nil || need["buildpacks"] != providers.CNBPlatformAPI || len(need) != 1 {
		t.Fatalf("needs = %v, %v", need, err)
	}
	if !poolAllows("pool-a") || poolAllows("pool-b") {
		t.Error("the provider's pool restriction is not applied at dispatch")
	}
	now := time.Now()
	pool := isolation.PoolState{Profile: isolation.RootlessDedicatedVM, ProfileApproved: true}
	worker := func(engines []string, providers map[string]string) isolation.WorkerState {
		return isolation.WorkerState{EnrollmentActive: true, ProtocolMajor: 1, VersionCompatible: true, OS: "linux", Architectures: []string{"arm64"},
			IsolationProfiles: []string{"rootless-dedicated-vm"}, FreeSlots: 1, StrictEgress: true, Engines: engines, Providers: providers}
	}
	req := isolation.Requirement{Platforms: []string{"linux/arm64"}, Providers: need}
	for _, c := range []struct {
		engines []string
		have    map[string]string
		ok      bool
	}{
		{[]string{"buildkit"}, nil, false},
		{[]string{"buildah"}, map[string]string{"railpack": "v0.43.0"}, false},
		{[]string{"buildkit"}, map[string]string{"buildpacks": "0.13"}, false},
		{[]string{"buildkit"}, map[string]string{"buildpacks": providers.CNBPlatformAPI}, true},
	} {
		ok, why := isolation.Eligible(pool, worker(c.engines, c.have), req, now)
		if ok != c.ok || (!ok && !slices.Contains(why, "provider-missing")) {
			t.Errorf("worker %v with %v: eligible %v %v; want %v", c.engines, c.have, ok, why, c.ok)
		}
	}
}
