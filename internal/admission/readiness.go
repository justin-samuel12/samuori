package admission

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/egress/proxy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/sources"
)

// Build readiness (ADR 0050): admission sends the build context to the discovery sandbox, which predicts network
// needs statically, and compares them with the egress the build method gets. The items are advice; admission's
// blocking rules do not change.

const (
	maxContextFiles   = 50_000
	maxContextContent = 16 << 20
)

// contextInput lists the files under dir and includes the content of the manifests and lockfiles discovery reads.
func (c *Checker) contextInput(m sources.Manifest, dir string) (discovery.Input, error) {
	in := discovery.Input{Context: dir}
	budget := int64(maxContextContent)
	for _, f := range m.Files {
		if dir != "." && !strings.HasPrefix(f.Path, dir+"/") {
			continue
		}
		if len(in.Files) == maxContextFiles {
			break
		}
		file := discovery.InputFile{Path: f.Path, Size: f.Size}
		if f.Size <= discovery.MaxContent(f.Path) && f.Size <= budget {
			data, err := c.sources.ReadFile(f)
			if err != nil {
				return discovery.Input{}, err
			}
			file.Content = data
			if file.Content == nil {
				file.Content = []byte{}
			}
			budget -= f.Size
		}
		in.Files = append(in.Files, file)
	}
	return in, nil
}

// methodReadiness predicts, for recipe, Railpack and Buildpacks builds, the needs of the application at the build
// context; a failure only adds a warning.
func (c *Checker) methodReadiness(ctx context.Context, d *buildspec.Document, snap *sources.Snapshot, r *Report) []discovery.NetworkNeed {
	if snap == nil || c.analyzer == nil {
		return nil
	}
	dir := "."
	switch b := d.Spec.Build; {
	case b.Method == providers.MethodRailpack && b.Railpack != nil && b.Railpack.Context != "":
		dir = cleanDir(b.Railpack.Context)
	case b.Method == providers.MethodBuildpacks && b.Buildpacks != nil && b.Buildpacks.Context != "":
		dir = cleanDir(b.Buildpacks.Context)
	}
	m, err := c.sources.Manifest(*snap)
	if err == nil {
		var in discovery.Input
		if in, err = c.contextInput(m, dir); err == nil {
			var res discovery.Result
			if res, err = c.analyzer.Analyse(ctx, in); err == nil {
				var needs []discovery.NetworkNeed
				for _, f := range res.Findings {
					if f.SourceSubpath == dir {
						needs = append(needs, f.NetworkNeeds...)
						r.Readiness = append(r.Readiness, f.Readiness...)
					}
				}
				return needs
			}
		}
	}
	r.Warnings = append(r.Warnings, discovery.Warning{Code: "readiness-unavailable", Message: "Samuori could not predict this build's network needs; a build that is denied network access lists the hosts it tried to reach."})
	return nil
}

// DiscoveryReadiness adds to a discovery result, for display, the egress findings under the organization's current
// policy and the project's allowances: Dockerfiles as Dockerfile builds, applications against everything the policy
// allows any build.
func DiscoveryReadiness(res *discovery.Result, org egress.Policy, project []egress.Allowance) error {
	cat, err := egress.Builtin()
	if err != nil {
		return err
	}
	for i := range res.Dockerfiles {
		df := &res.Dockerfiles[i]
		p := org.ForProject("dockerfile", project)
		df.Readiness = append(df.Readiness, predictEgress(cat, df.NetworkNeeds, "dockerfile", p.DockerfilePresets, p, nil)...)
		discovery.SortReadiness(df.Readiness)
	}
	for i := range res.Findings {
		f := &res.Findings[i]
		p := org.ForProject("", project)
		f.Readiness = append(f.Readiness, predictEgress(cat, f.NetworkNeeds, "", forEveryMethod(cat, p.AllowedPresets), p, nil)...)
		discovery.SortReadiness(f.Readiness)
	}
	return nil
}

// forEveryMethod keeps the presets not limited to some build methods: a prediction for any build cannot count on
// mise, which only Railpack builds get.
func forEveryMethod(cat *egress.Catalog, ids []string) []string {
	var out []string
	for _, id := range ids {
		if pr, ok := cat.Get(id); ok && len(pr.Methods) == 0 {
			out = append(out, id)
		}
	}
	return out
}

func cleanDir(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "."
	}
	return p
}

var methodLabel = map[string]string{"": "any build", "dockerfile": "Dockerfile builds", "recipe": "this recipe", providers.MethodRailpack: "Railpack builds", providers.MethodBuildpacks: "Buildpacks builds"}

// predictEgress compares predicted needs with the egress a build of method gets: the plan allowlist resolved from
// presets plus the policy's destinations. Needs whose preset admission already refuses are marked blocking.
func predictEgress(cat *egress.Catalog, needs []discovery.NetworkNeed, method string, presets []string, p egress.Policy, refused []string) []discovery.Readiness {
	plan, _ := cat.Resolve(presets, p)
	allowed := map[string]bool{}
	for _, rule := range plan.Allow {
		allowed[net.JoinHostPort(rule.Host, strconv.Itoa(rule.Port))] = true
	}
	// For any build, hosts a need lists outside a preset reach every build only through policy destinations and
	// project allowances; allowed presets join a build only when its recipe or method asks for them.
	direct := allowed
	if method == "" {
		bare, _ := cat.Resolve(nil, p)
		direct = map[string]bool{}
		for _, rule := range bare.Allow {
			direct[net.JoinHostPort(rule.Host, strconv.Itoa(rule.Port))] = true
		}
	}
	planned := map[string]bool{}
	for _, id := range plan.Presets {
		planned[strings.SplitN(id, "@", 2)[0]] = true
	}
	label := methodLabel[method]
	var out []discovery.Readiness
	for _, n := range needs {
		dests := n.Destinations
		if n.Preset != "" {
			if planned[n.Preset] {
				continue
			}
			pr, ok := cat.Get(n.Preset)
			if !ok {
				continue
			}
			dests = pr.Destinations
		}
		open := allowed
		if n.Preset == "" {
			open = direct
		}
		var missing []string
		for _, d := range dests {
			if _, err := egress.ParseDestination(d); err == nil && !open[d] {
				missing = append(missing, d)
			}
		}
		if len(missing) == 0 {
			continue
		}
		if n.Preset != "" {
			missing = missing[:1]
		}
		hint := hints(cat, missing, method, p)
		where := ""
		if n.Line > 0 {
			where = fmt.Sprintf(" (%s line %d)", n.Path, n.Line)
		}
		it := discovery.Readiness{Code: "egress-" + n.Kind, Severity: n.Severity, Confidence: n.Confidence, Path: n.Path, Line: n.Line, Fixer: discovery.Administrator, Fix: hint,
			Requestable: requestable(n.Preset, missing)}
		switch {
		case n.Preset != "" && !slices.Contains(p.AllowedPresets, n.Preset):
			it.Message = fmt.Sprintf("`%s`%s needs the %s preset (%s), which the egress policy does not allow.", n.Cause, where, n.Preset, strings.Join(dests, ", "))
			it.Blocking = slices.Contains(refused, n.Preset)
		case n.Preset != "":
			it.Message = fmt.Sprintf("`%s`%s needs the %s preset (%s), which %s may not use.", n.Cause, where, n.Preset, strings.Join(dests, ", "), label)
		case n.Kind == "downloader":
			it.Message = fmt.Sprintf("%s. %s %s not allowed for %s.", n.Cause, list(missing), isAre(missing), label)
		default:
			it.Message = fmt.Sprintf("`%s`%s reaches %s, which %s not allowed for %s.", n.Cause, where, strings.Join(missing, ", "), isAre(missing), label)
		}
		switch {
		case n.Fix != "" && n.Fixer == discovery.Developer:
			it.Fixer, it.Fix, it.Alternative = discovery.Developer, n.Fix, "Or: "+hint
		case n.Fix != "":
			it.Alternative = n.Fix
		case n.Kind == "url":
			it.Alternative = "Or add the file to the upload and COPY it instead of downloading it during the build."
		}
		out = append(out, it)
	}
	return out
}

// hints is Advise's administrator advice for each missing destination, one sentence per preset.
func hints(cat *egress.Catalog, missing []string, method string, p egress.Policy) string {
	var out, unlisted, order []string
	group := map[string][]string{}
	hint := map[string]string{}
	request := false
	for _, d := range missing {
		r, _ := egress.ParseDestination(d)
		adv := cat.Advise(egress.Denial{Host: r.Host, Port: r.Port, Reason: proxy.ReasonNotAllowed}, method, p)
		adv.Hint = strings.TrimSuffix(adv.Hint, egress.RequestHint)
		request = request || adv.Requestable
		switch {
		case adv.Allowable && len(adv.Presets) == 0:
			if pr, ok := cat.ForOtherMethods(d, method); ok {
				key := "other:" + pr.ID
				if _, seen := group[key]; !seen {
					order = append(order, key)
					hint[key] = fmt.Sprintf("in the %s preset, which is for %s; an organization administrator can add ", pr.ID, pr.MethodsLabel())
				}
				group[key] = append(group[key], d)
				continue
			}
			unlisted = append(unlisted, d)
			continue
		case !adv.Allowable:
			out = append(out, adv.Hint)
			continue
		case method == "":
			key := "other:" + adv.Presets[0]
			if _, seen := group[key]; !seen {
				order = append(order, key)
				hint[key] = fmt.Sprintf("in the %s preset, which reaches only builds that need it; an organization administrator can set %s to All builds, or add ", adv.Presets[0], adv.Presets[0])
			}
			group[key] = append(group[key], d)
			continue
		}
		key := adv.Presets[0]
		if _, seen := group[key]; !seen {
			order = append(order, key)
			hint[key] = strings.TrimPrefix(adv.Hint, d+" is ")
		}
		group[key] = append(group[key], d)
	}
	for _, key := range order {
		ds := group[key]
		h := list(ds) + " " + isAre(ds) + " " + hint[key]
		if strings.HasPrefix(key, "other:") {
			h += addAs(ds)
		}
		out = append(out, h)
	}
	if len(unlisted) > 0 {
		out = append(out, list(unlisted)+" "+isAre(unlisted)+" in no egress preset; an organization administrator can add "+addAs(unlisted))
	}
	h := strings.Join(out, " ")
	if request {
		h += egress.RequestHint
	}
	return h
}

// requestable is what a project member may ask to open for an egress need: its preset, or the missing destinations,
// public or internal (ADR 0052).
func requestable(preset string, missing []string) *discovery.EgressEntries {
	e := &discovery.EgressEntries{Presets: []string{}, Destinations: []string{}}
	if preset != "" {
		e.Presets = append(e.Presets, preset)
		return e
	}
	for _, d := range missing {
		if _, err := egress.RequestableDestination(d); err == nil {
			e.Destinations = append(e.Destinations, d)
		} else if _, err := egress.RequestableInternal(d); err == nil {
			e.Destinations = append(e.Destinations, d)
			e.Internal = append(e.Internal, d)
		}
	}
	if len(e.Destinations) == 0 {
		return nil
	}
	return e
}

// network says how hosts in no preset can be added: public, internal, or "public or internal" for a mix.
func network(ds []string) string {
	pub, internal := false, false
	for _, d := range ds {
		if _, err := egress.RequestableDestination(d); err == nil {
			pub = true
		} else {
			internal = true
		}
	}
	switch {
	case pub && internal:
		return "public or internal"
	case internal:
		return egress.NetworkInternal
	}
	return egress.NetworkPublic
}

func addAs(ds []string) string {
	if len(ds) == 1 {
		return "it as a " + network(ds) + " destination in the egress policy."
	}
	return "them as " + network(ds) + " destinations in the egress policy."
}

func list(ds []string) string {
	switch len(ds) {
	case 1:
		return ds[0]
	case 2:
		return ds[0] + " and " + ds[1]
	}
	return strings.Join(ds[:len(ds)-1], ", ") + " and " + ds[len(ds)-1]
}

func isAre(ds []string) string {
	if len(ds) == 1 {
		return "is"
	}
	return "are"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// specReadiness repeats, as readiness items, what the spec itself predicts: a port the Dockerfile does not expose
// and secrets that are not ready. Secret items repeat admission's own refusals and are marked blocking.
func specReadiness(d *buildspec.Document, a *discovery.DockerfileAnalysis, r *Report) {
	if app := d.Spec.Application; a != nil && app != nil && app.Port > 0 && len(a.ExposedPorts) > 0 {
		port := strconv.Itoa(app.Port)
		if !slices.ContainsFunc(a.ExposedPorts, func(p string) bool { return p == port || strings.HasPrefix(p, port+"/") }) {
			r.Readiness = append(r.Readiness, discovery.Readiness{Code: "port-mismatch", Severity: discovery.MayFail, Fixer: discovery.Developer, Confidence: discovery.Likely, Path: a.Path,
				Message: fmt.Sprintf("The spec says the application listens on port %d, but %s exposes %s. Checks that probe port %d will fail if the application listens elsewhere.", app.Port, a.Path, strings.Join(a.ExposedPorts, ", "), app.Port),
				Fix:     "Set the application port to the one the application listens on, or change EXPOSE to match."})
		}
	}
	for _, s := range r.Secrets {
		if s.Required && s.Status != "ready" {
			r.Readiness = append(r.Readiness, discovery.Readiness{Code: "secret-not-ready", Severity: discovery.WillFail, Fixer: discovery.Administrator, Confidence: discovery.Certain, Blocking: true,
				Message: fmt.Sprintf("The build needs the secret %q, which is not ready for this project.", s.ID), Fix: s.Reason})
		}
	}
	for _, i := range r.SemanticErrors {
		if i.Rule == "secret-undeclared" || i.Rule == "secret-target-mismatch" {
			r.Readiness = append(r.Readiness, discovery.Readiness{Code: i.Rule, Severity: discovery.WillFail, Fixer: discovery.Developer, Confidence: discovery.Certain, Blocking: true,
				Message: upperFirst(i.Message) + ".", Fix: "Declare the secret in the spec's secrets with the same ID and mount target as the Dockerfile."})
		}
	}
}
