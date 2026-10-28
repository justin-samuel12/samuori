package admission

import (
	"context"
	"path"
	"strings"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
)

// buildpacksChecks admits a buildpacks build (ADR 0047): the approved builder and run image go through the base-image
// and image-source policies like any other image, and placement needs a verified BuildKit worker that reports the
// CNB platform API Samuori drives. The lifecycle never runs here; it runs on the worker inside the build isolation.
func (c *Checker) buildpacksChecks(ctx context.Context, orgID string, d *buildspec.Document, snap *sources.Snapshot, r *Report, semantic func(string, string, string, ...any), warn func(string, string, ...any)) ([]string, error) {
	if c.methods == nil {
		semantic("buildpacks-unavailable", "/spec/build/method", "this server has no provider registry, so it cannot build with buildpacks")
		return nil, nil
	}
	cfg, err := c.methods.Config(ctx, orgID, "buildpacks")
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	r.providers = map[string]string{"buildpacks": providers.CNBPlatformAPI}
	if cfg.Artifacts["builder"] == "" || cfg.Artifacts["run-image"] == "" {
		semantic("buildpacks-unavailable", "/spec/build/method", "the approved buildpacks provider has no builder or run image; an administrator pins both under Build & conversion providers")
		return nil, nil
	}
	if len(cfg.EgressPresets) == 0 {
		warn("buildpacks-no-egress", "Paketo Buildpacks has no egress presets. Buildpacks download language runtimes and install dependencies, so the build fails unless the organization's internal destinations serve them. An administrator sets presets under Build & conversion providers.")
	}
	ctxDir := "."
	if bp := d.Spec.Build.Buildpacks; bp != nil && bp.Context != "" {
		ctxDir = path.Clean(bp.Context)
	}
	if snap != nil && ctxDir != "." {
		manifest, err := c.sources.Manifest(*snap)
		if err != nil {
			return nil, err
		}
		found := false
		for _, f := range manifest.Files {
			found = found || strings.HasPrefix(f.Path, ctxDir+"/")
		}
		if !found {
			semantic("buildpacks-context-missing", "/spec/build/buildpacks/context", "the upload has no directory %s", ctxDir)
		}
	}
	return []string{cfg.Artifacts["builder"], cfg.Artifacts["run-image"]}, nil
}
