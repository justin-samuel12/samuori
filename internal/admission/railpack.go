package admission

import (
	"context"
	"path"
	"strings"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/sources"
)

// railpackChecks admits a railpack build (ADR 0047): the approved builder and runtime images go through the
// base-image policy like any other, and placement needs a verified worker with the approved Railpack installed.
// Railpack itself never runs here; it plans on the worker inside the build isolation.
func (c *Checker) railpackChecks(ctx context.Context, orgID string, d *buildspec.Document, snap *sources.Snapshot, r *Report, semantic func(string, string, string, ...any), warn func(string, string, ...any)) ([]string, error) {
	if c.methods == nil {
		semantic("railpack-unavailable", "/spec/build/method", "this server has no provider registry, so it cannot build with Railpack")
		return nil, nil
	}
	cfg, err := c.methods.Config(ctx, orgID, "railpack")
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	r.providers = map[string]string{"railpack": cfg.Version}
	if len(cfg.EgressPresets) == 0 {
		warn("railpack-no-egress", "Railpack has no egress presets. Planning downloads mise and language runtimes and the build installs dependencies, so the build fails unless the organization's internal destinations serve them. An administrator sets presets under Build & conversion providers.")
	}
	ctxDir := "."
	if rp := d.Spec.Build.Railpack; rp != nil && rp.Context != "" {
		ctxDir = path.Clean(rp.Context)
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
			semantic("railpack-context-missing", "/spec/build/railpack/context", "the upload has no directory %s", ctxDir)
		}
	}
	var images []string
	for _, k := range []string{"builder", "runtime"} {
		if ref := cfg.Artifacts[k]; ref != "" {
			images = append(images, ref)
		}
	}
	return images, nil
}
