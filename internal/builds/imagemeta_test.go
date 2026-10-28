package builds

import (
	"context"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/workers"
)

// TestImageMetadata: the plan carries the automatic OCI values, spec values win for the same key, revision appears
// only for a real commit, and each method gets only what it applies.
func TestImageMetadata(t *testing.T) {
	created := time.Date(2026, 10, 26, 9, 30, 0, 0, time.FixedZone("x", 3600))
	commit := "0123456789abcdef0123456789abcdef01234567"
	doc := &buildspec.Document{Metadata: buildspec.Metadata{Name: "api"}, Spec: buildspec.Spec{Outputs: &buildspec.Outputs{Image: &buildspec.ImageOutput{
		Labels:      map[string]string{"org.opencontainers.image.title": "Payments API", "com.example.team": "payments"},
		Annotations: map[string]string{"org.opencontainers.image.source": "https://example.com/repo"},
	}}}}

	img := imageMetadata(doc, "dockerfile", created, commit)
	wantLabels := map[string]string{
		ociCreated: "2026-10-26T08:30:00Z", ociRevision: commit, ociTitle: "Payments API", "com.example.team": "payments",
	}
	if !maps.Equal(img.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", img.Labels, wantLabels)
	}
	wantAnn := map[string]string{
		ociCreated: "2026-10-26T08:30:00Z", ociRevision: commit, ociTitle: "api", "org.opencontainers.image.source": "https://example.com/repo",
	}
	if !maps.Equal(img.Annotations, wantAnn) {
		t.Errorf("annotations = %v, want %v", img.Annotations, wantAnn)
	}

	for _, rev := range []string{"", "main", "v1.2.3", "0123456789ABCDEF0123456789ABCDEF01234567", commit[:12]} {
		if got := imageMetadata(doc, "recipe", created, rev); got.Labels[ociRevision] != "" || got.Annotations[ociRevision] != "" {
			t.Errorf("revision %q was recorded", rev)
		}
	}

	if rp := imageMetadata(doc, "railpack", created, ""); rp.Labels != nil || rp.Annotations["org.opencontainers.image.source"] == "" {
		t.Errorf("railpack: %+v, want annotations only", rp)
	}
	if bp := imageMetadata(doc, "buildpacks", created, commit); bp != nil {
		t.Errorf("buildpacks: %+v, want nothing", bp)
	}
}

// TestImageMetadataInPlanDigest: the plan digest covers labels and annotations, so a worker builds exactly what was
// admitted.
func TestImageMetadataInPlanDigest(t *testing.T) {
	created := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	doc := &buildspec.Document{Metadata: buildspec.Metadata{Name: "api"}}
	p := Plan{Version: 1, Method: "dockerfile", Platforms: []string{"linux/amd64"}}
	_, bare := p.Encode()
	p.Image = imageMetadata(doc, p.Method, created, "")
	_, auto := p.Encode()
	doc.Spec.Outputs = &buildspec.Outputs{Image: &buildspec.ImageOutput{Labels: map[string]string{"com.example.team": "a"}}}
	p.Image = imageMetadata(doc, p.Method, created, "")
	_, withA := p.Encode()
	doc.Spec.Outputs.Image.Labels["com.example.team"] = "b"
	p.Image = imageMetadata(doc, p.Method, created, "")
	_, withB := p.Encode()
	if bare == auto || auto == withA || withA == withB {
		t.Errorf("digests do not follow image metadata: %s %s %s %s", bare, auto, withA, withB)
	}
}

// TestImageMetadataNeedsProtocol16: a plan with image metadata goes only to workers that negotiated protocol 1.6,
// since older workers would build without it.
func TestImageMetadataNeedsProtocol16(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`), now); err != nil {
		t.Fatal(err)
	}
	sc := &Scheduler{s: &Service{db: d, now: time.Now}}
	raw, digest := Plan{Version: 1, Method: "dockerfile", Image: &buildplan.PlanImage{Labels: map[string]string{ociCreated: "2026-10-26T00:00:00Z"}}}.Encode()
	if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, 'o1', ?, ?)`), digest, string(raw), now); err != nil {
		t.Fatal(err)
	}
	if got, err := sc.planMinor(ctx, Run{OrgID: "o1", PlanDigest: digest}); err != nil || got != workers.ImageMetadataMinor {
		t.Fatalf("planMinor = %d, %v", got, err)
	}
	for _, w := range []struct {
		id    string
		minor int
	}{{"old", 5}, {"new", 6}} {
		if _, err := d.SQL.ExecContext(ctx, d.Rebind(`INSERT INTO workers (id, org_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, created_at)
			VALUES (?, 'o1', ?, 'enrolled', 'active', ?, ?, '{}', 'v', ?, ?)`), w.id, w.id, w.id, now.Add(time.Hour), w.minor, now); err != nil {
			t.Fatal(err)
		}
	}
	if sc.negotiated(ctx, "old", workers.ImageMetadataMinor) || !sc.negotiated(ctx, "new", workers.ImageMetadataMinor) {
		t.Error("image metadata plans must go only to workers on protocol 1.6 or later")
	}
}
