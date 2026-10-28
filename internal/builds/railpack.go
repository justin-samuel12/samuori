package builds

import (
	"context"
	"errors"
	"path"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
)

// DefaultRailpackTimeout bounds railpack prepare when the administrator sets no limit.
const DefaultRailpackTimeout = 600

// railpackPlan pins the approved Railpack provider into p and resolves the build's egress from the provider's
// presets, which the organization's egress policy must allow (ADR 0047).
func (s *Service) railpackPlan(ctx context.Context, p *Plan, doc *buildspec.Document, manifest sources.Manifest, orgID string, eff policy.Effective) error {
	if s.providers == nil {
		return errors.New("this server has no provider registry")
	}
	c, err := s.providers.Config(ctx, orgID, "railpack")
	if err != nil {
		return err
	}
	if !c.Enabled {
		return errors.New("Railpack is not enabled; an administrator enables it under Build & conversion providers")
	}
	rp, err := railpackFor(doc, manifest, c)
	if err != nil {
		return err
	}
	for _, sec := range p.Secrets {
		if rp.SecretEnv == nil {
			rp.SecretEnv = map[string]string{}
		}
		rp.SecretEnv[buildspec.RailpackSecretEnv(sec.ID)] = sec.ID
	}
	p.Railpack = rp
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

func railpackFor(doc *buildspec.Document, manifest sources.Manifest, c providers.Config) (*buildplan.PlanRailpack, error) {
	rp := &buildplan.PlanRailpack{Version: c.Version, Frontend: c.Artifacts["frontend"], Context: ".", TimeoutSeconds: c.TimeoutSeconds,
		Binaries: map[string]string{}, Images: []string{}}
	if rp.TimeoutSeconds == 0 {
		rp.TimeoutSeconds = DefaultRailpackTimeout
	}
	if !c.RunAsRoot {
		rp.User = buildplan.RailpackUser
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if d := c.Artifacts["binary-"+arch]; d != "" {
			rp.Binaries[arch] = d
		}
	}
	for _, k := range []string{"builder", "runtime"} {
		if ref := c.Artifacts[k]; ref != "" {
			rp.Images = append(rp.Images, ref)
		}
	}
	if spec := doc.Spec.Build.Railpack; spec != nil {
		if spec.Context != "" {
			rp.Context = path.Clean(spec.Context)
		}
		rp.BuildCommand, rp.StartCommand = spec.BuildCommand, spec.StartCommand
	}
	if !contextInUpload(manifest, rp.Context) {
		return nil, errors.New("the Railpack context " + rp.Context + " is not in the upload")
	}
	return rp, nil
}

func contextInUpload(m sources.Manifest, dir string) bool {
	if dir == "." {
		return len(m.Files) > 0
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, dir+"/") {
			return true
		}
	}
	return false
}
