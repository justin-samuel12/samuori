package builds

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/testsupport/dbtest"
)

// TestPinnedPlansNeedBuildKit: a plan with base images pinned at admission goes only to BuildKit workers, which
// build the pinned image in place of the tag (ADR 0049).
func TestPinnedPlansNeedBuildKit(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.SQLite(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: d, now: time.Now}
	sc := &Scheduler{s: s}
	store := func(p buildplan.Plan) string {
		raw, digest := p.Encode()
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`), digest, string(raw), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	pin := buildplan.BaseImagePin{Ref: "node:18-alpine", Image: "docker.io/library/node@sha256:" + strings.Repeat("d", 64)}
	pinned := store(buildplan.Plan{Version: 1, Method: "dockerfile", Dockerfile: &buildplan.PlanDockerfile{Path: "Dockerfile", BaseImagePins: []buildplan.BaseImagePin{pin}}})
	plain := store(buildplan.Plan{Version: 1, Method: "dockerfile", Dockerfile: &buildplan.PlanDockerfile{Path: "Dockerfile"}})

	run := Run{OrgID: "o1", PlanDigest: pinned}
	if got, err := s.BaseImagePins(ctx, run); err != nil || len(got) != 1 || got[0] != pin {
		t.Fatalf("pins = %v, %v", got, err)
	}
	needs, err := sc.pinsBaseImages(ctx, run)
	if err != nil || !needs {
		t.Fatalf("pinned plan needs = %v, %v", needs, err)
	}
	if needs, _ := sc.pinsBaseImages(ctx, Run{OrgID: "o1", PlanDigest: plain}); needs {
		t.Error("a plan without pins asked for BuildKit")
	}

	pool := isolation.PoolState{Profile: isolation.RootlessDedicatedVM, ProfileApproved: true}
	worker := func(engine string) isolation.WorkerState {
		return isolation.WorkerState{EnrollmentActive: true, ProtocolMajor: 1, VersionCompatible: true, OS: "linux", Architectures: []string{"amd64"},
			IsolationProfiles: []string{"rootless-dedicated-vm"}, FreeSlots: 1, StrictEgress: true, Engines: []string{engine}}
	}
	req := isolation.Requirement{Platforms: []string{"linux/amd64"}, BaseImagePins: true}
	if ok, why := isolation.Eligible(pool, worker("buildah"), req, time.Now()); ok || !slices.Contains(why, "engine-cannot-pin-base-images") {
		t.Errorf("a Buildah worker took a pinned plan: %v %v", ok, why)
	}
	if ok, why := isolation.Eligible(pool, worker("buildkit"), req, time.Now()); !ok {
		t.Errorf("a BuildKit worker refused a pinned plan: %v", why)
	}

	now := time.Now().UTC()
	for _, w := range []struct {
		id    string
		minor int
	}{{"old", 4}, {"new", 5}} {
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO workers (id, org_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at)
			VALUES (?, 'o1', ?, 'enrolled', 'active', ?, ?, '{}', 'v', ?, ?)`), w.id, w.id, w.id, now.Add(time.Hour), w.minor, now); err != nil {
			t.Fatal(err)
		}
	}
	if sc.buildsPins(ctx, "old") || !sc.buildsPins(ctx, "new") || sc.buildsPins(ctx, "missing") {
		t.Error("pinned plans must go only to workers on protocol 1.5 or later")
	}
}
