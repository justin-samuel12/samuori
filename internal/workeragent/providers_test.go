package workeragent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/isolation"
)

type buildpacksProvider struct{ stubProvider }

func (buildpacksProvider) Capabilities(context.Context) (isolation.Capabilities, error) {
	return isolation.Capabilities{Profiles: []isolation.Profile{isolation.RootlessDedicatedVM}, FreeSlots: 1, Engines: []string{"buildkit"},
		Providers: map[string]string{"buildpacks": buildplan.CNBPlatformAPI}}, nil
}

// The buildpacks capability the isolation provider reports reaches the worker protocol, which dispatch reads
// (SEC-CNB-04).
func TestBuildpacksCapabilityReported(t *testing.T) {
	a := New(Config{StateDir: t.TempDir(), ProvidersDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.UseProvider(buildpacksProvider{})
	if got := a.Capabilities(context.Background()).GetProviders()["buildpacks"]; got != buildplan.CNBPlatformAPI {
		t.Errorf("protocol capabilities report buildpacks %q, want %q", got, buildplan.CNBPlatformAPI)
	}
}

// A worker reports build providers from its isolation provider and conversion tools only when it can convert.
func TestReportedProviders(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"kompose", "railpack"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, name, "VERSION"), []byte("v1.0.0\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, name, name), []byte("x"), 0o755)
	}
	got := reportedProviders(map[string]string{"railpack": "v0.43.0"}, true, dir)
	if got["railpack"] != "v0.43.0" || got["kompose"] != "v1.0.0" || len(got) != 2 {
		t.Errorf("convert-ready worker reports %v", got)
	}
	if got := reportedProviders(nil, true, dir); got["railpack"] != "" {
		t.Errorf("an installed Railpack the isolation provider cannot run is reported: %v", got)
	}
	if got := reportedProviders(map[string]string{"railpack": "v0.43.0"}, false, dir); got["kompose"] != "" || got["railpack"] == "" {
		t.Errorf("a worker that cannot convert reports %v", got)
	}
}
