package builds

import (
	"context"
	"errors"
	"maps"
	"path"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
)

// DefaultBuildpacksTimeout bounds the lifecycle step when the administrator sets no limit.
const DefaultBuildpacksTimeout = 1800

// buildpacksPlan pins the approved buildpacks provider into p and resolves the build's egress from the provider's
// presets, which the organization's egress policy must allow (ADR 0047).
func (s *Service) buildpacksPlan(ctx context.Context, p *Plan, doc *buildspec.Document, manifest sources.Manifest, orgID string, eff policy.Effective) error {
	if s.providers == nil {
		return errors.New("this server has no provider registry")
	}
	c, err := s.providers.Config(ctx, orgID, "buildpacks")
	if err != nil {
		return err
	}
	if !c.Enabled {
		return errors.New("Paketo Buildpacks is not enabled; an administrator enables it under Build & conversion providers")
	}
	bp, err := buildpacksFor(doc, manifest, c, p.Secrets)
	if err != nil {
		return err
	}
	p.Buildpacks = bp
	cat, err := egress.Builtin()
	if err != nil {
		return err
	}
	var refused []string
	if p.Egress, refused = cat.Resolve(c.EgressPresets, eff.Egress); len(refused) > 0 {
		return errors.New("the egress policy does not allow " + strings.Join(refused, ", "))
	}
	return nil
}

// buildpacksFor keeps only bindings whose secret is bound for the project; an unbound optional secret is never
// mounted.
func buildpacksFor(doc *buildspec.Document, manifest sources.Manifest, c providers.Config, secrets []buildplan.PlanSecret) (*buildplan.PlanBuildpacks, error) {
	bp := &buildplan.PlanBuildpacks{Version: c.Version, PlatformAPI: providers.CNBPlatformAPI, Builder: c.Artifacts["builder"], RunImage: c.Artifacts["run-image"],
		Context: ".", TimeoutSeconds: c.TimeoutSeconds}
	if bp.Builder == "" || bp.RunImage == "" {
		return nil, errors.New("the approved buildpacks provider has no builder or run image")
	}
	if bp.TimeoutSeconds == 0 {
		bp.TimeoutSeconds = DefaultBuildpacksTimeout
	}
	bound := map[string]bool{}
	for _, s := range secrets {
		bound[s.ID] = s.BindingID != ""
	}
	if spec := doc.Spec.Build.Buildpacks; spec != nil {
		if spec.Context != "" {
			bp.Context = path.Clean(spec.Context)
		}
		if len(spec.Env) > 0 {
			bp.Env = maps.Clone(spec.Env)
		}
		for _, b := range spec.Bindings {
			if bound[b.Secret] {
				bp.Bindings = append(bp.Bindings, buildplan.PlanBinding{Name: b.Name, Type: b.Type, Key: b.BindingKey(), SecretID: b.Secret})
			}
		}
	}
	if !contextInUpload(manifest, bp.Context) {
		return nil, errors.New("the buildpacks context " + bp.Context + " is not in the upload")
	}
	return bp, nil
}
