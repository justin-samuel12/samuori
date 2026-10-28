package specs

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/providers"
)

// RecipeChoice is what suggestions need to know about a recipe in the organization's catalogue.
type RecipeChoice struct {
	ID, Version                 string
	Languages, ApplicationTypes []string
	Approved                    bool
}

type SuggestInput struct {
	Project string
	// Name, when set, replaces every suggestion's metadata.name.
	Name    string
	Result  discovery.Result
	Recipes []RecipeChoice
	// Default is the organization's default build method; Allowed reports whether the project may use a method.
	Default string
	Allowed func(method string) bool
}

type Suggestion struct {
	Method        string
	Title         string
	SourceSubpath string
	Recommended   bool
	Reasons       []string
	Warnings      []discovery.Warning
	Document      map[string]any
}

// MaxSuggestions bounds the list for uploads with many applications.
const MaxSuggestions = 20

// Suggest turns a discovery into starting BuildSpecs, the way the web wizard does
// (docs/architecture/buildspec.md#suggested-specs). It reads only the discovery result; nothing in the upload runs.
func Suggest(in SuggestInput) (items []Suggestion, notes []discovery.Warning) {
	var apps []discovery.Finding
	for _, f := range in.Result.Findings {
		if f.ApplicationKind != "" {
			apps = append(apps, f)
		}
	}
	var recipeItems []Suggestion
	if in.Allowed(providers.MethodRecipe) {
		for _, f := range apps {
			doc := draftForApp(appName(&f, in.Project), &f, in.Recipes)
			if get(doc, "spec", "build", "recipe", "id") == nil {
				notes = append(notes, discovery.Warning{Code: "no-recipe", Path: f.SourceSubpath,
					Message: fmt.Sprintf("No approved recipe builds a %s %s; use another method or ask an administrator for a recipe.", orUnknown(f.Language), f.ApplicationKind)})
				continue
			}
			recipeItems = append(recipeItems, recipeSuggestion(doc, &f, nil))
		}
		for _, c := range frontendBackendPairs(apps) {
			doc := draftForApp(appName(&c.be, in.Project), &c.be, in.Recipes)
			if get(doc, "spec", "build", "recipe", "id") == nil {
				continue
			}
			recipeItems = append(recipeItems, recipeSuggestion(withFrontend(doc, &c.fe), &c.be, &c.fe))
		}
	}
	var dockerItems []Suggestion
	if in.Allowed(providers.MethodDockerfile) {
		for _, df := range in.Result.Dockerfiles {
			dockerItems = append(dockerItems, dockerfileSuggestion(in, df))
		}
	}
	var primary *discovery.Finding
	if len(apps) > 0 {
		primary = &apps[0]
	} else if len(in.Result.Findings) > 0 {
		primary = &in.Result.Findings[0]
	}
	var planned []Suggestion
	for _, m := range []string{providers.MethodRailpack, providers.MethodBuildpacks} {
		if in.Allowed(m) {
			planned = append(planned, plannedSuggestion(in.Project, primary, in.Recipes, m))
		}
	}

	all := slices.Concat(recipeItems, dockerItems, planned)
	if pick := recommend(in, recipeItems, dockerItems, planned); pick >= 0 {
		first := all[pick]
		first.Recommended = true
		all = append([]Suggestion{first}, slices.Delete(all, pick, pick+1)...)
	}
	if len(all) > MaxSuggestions {
		notes = append(notes, discovery.Warning{Code: "suggestions-truncated", Message: fmt.Sprintf("Only the first %d suggestions are listed.", MaxSuggestions)})
		all = all[:MaxSuggestions]
	}
	for i := range all {
		if in.Name != "" {
			all[i].Document["metadata"].(map[string]any)["name"] = in.Name
		}
		assumePort(&all[i])
	}
	return all, notes
}

// recommend mirrors the wizard's "Choose how to build" recommendation: the organization's default when it fits, then
// the first Dockerfile, then a recipe, then Railpack. It returns an index into the concatenated suggestions.
func recommend(in SuggestInput, recipe, docker, planned []Suggestion) int {
	recipeFits := len(recipe) > 0
	byMethod := func(m string) int {
		for i, s := range planned {
			if s.Method == m {
				return len(recipe) + len(docker) + i
			}
		}
		return -1
	}
	switch in.Default {
	case providers.MethodDockerfile:
		if len(docker) > 0 {
			return len(recipe)
		}
	case providers.MethodRailpack, providers.MethodBuildpacks:
		if i := byMethod(in.Default); i >= 0 {
			return i
		}
	case providers.MethodRecipe:
		if recipeFits {
			return 0
		}
	}
	switch {
	case len(docker) > 0:
		return len(recipe)
	case recipeFits:
		return 0
	}
	if i := byMethod(providers.MethodRailpack); i >= 0 {
		return i
	}
	return -1
}

func recipeSuggestion(doc map[string]any, be, fe *discovery.Finding) Suggestion {
	id, version := get(doc, "spec", "build", "recipe", "id"), get(doc, "spec", "build", "recipe", "version")
	s := Suggestion{Method: providers.MethodRecipe, SourceSubpath: be.SourceSubpath, Document: doc, Warnings: findingWarnings(be),
		Title: fmt.Sprintf("Recipe %v %v for %s", id, version, be.SourceSubpath)}
	s.Reasons = append(s.Reasons, fmt.Sprintf("Discovery found a %s %s in %s (%s confidence).", orUnknown(be.Language), be.ApplicationKind, be.SourceSubpath, be.Confidence),
		fmt.Sprintf("Recipe %v %v is approved for %v %v applications.", id, version, get(doc, "spec", "application", "runtime", "language"), be.ApplicationKind))
	if len(be.SuggestedCommands) > 0 {
		s.Reasons = append(s.Reasons, "Install, build, start and test commands come from the project's manifest.")
	}
	if fe != nil {
		s.Title = fmt.Sprintf("Recipe %v %v for %s with the frontend in %s", id, version, be.SourceSubpath, fe.SourceSubpath)
		s.Reasons = append(s.Reasons, fmt.Sprintf("The static site in %s is served from the same image; /api goes to the application on port 8000 (ADR 0055).", fe.SourceSubpath))
		s.Warnings = append(s.Warnings, findingWarnings(fe)...)
	}
	return s
}

func dockerfileSuggestion(in SuggestInput, df discovery.DockerfileAnalysis) Suggestion {
	dir := path.Dir(df.Path)
	var app *discovery.Finding
	for i, f := range in.Result.Findings {
		if f.ApplicationKind != "" && f.SourceSubpath == dir {
			app = &in.Result.Findings[i]
			break
		}
	}
	doc := withDockerfile(fillFrom(draftForApp(slug(in.Project), nil, nil), app, df), df)
	s := Suggestion{Method: providers.MethodDockerfile, SourceSubpath: dir, Document: doc, Title: "Dockerfile " + df.Path,
		Reasons: []string{"The upload has a Dockerfile; Samuori builds it exactly as written."}}
	if len(df.Stages) > 0 {
		s.Reasons = append(s.Reasons, fmt.Sprintf("The build stops at the last stage, %s.", df.Stages[len(df.Stages)-1]))
	}
	if app != nil {
		s.Reasons = append(s.Reasons, fmt.Sprintf("The application type and runtime come from the %s %s found in %s.", orUnknown(app.Language), app.ApplicationKind, dir))
	}
	if get(doc, "spec", "application", "port") != nil {
		s.Reasons = append(s.Reasons, "The port is the one the Dockerfile exposes.")
	}
	s.Warnings = append(append(s.Warnings, df.Ambiguities...), df.RefusedFeatures...)
	for _, r := range df.Readiness {
		s.Warnings = append(s.Warnings, discovery.Warning{Code: r.Code, Message: r.Message, Path: r.Path})
	}
	return s
}

// plannedSuggestion is the wizard's switch to Railpack or Buildpacks from the primary application's draft. Unlike the
// wizard, it builds the application's folder rather than the upload's root.
func plannedSuggestion(project string, f *discovery.Finding, recipes []RecipeChoice, method string) Suggestion {
	d := draftForApp(appName(f, project), f, recipes)
	cmd, start := get(d, "spec", "build", "recipe", "build"), get(d, "spec", "build", "recipe", "start")
	build := d["spec"].(map[string]any)["build"].(map[string]any)
	delete(build, "recipe")
	build["method"] = method
	opts := map[string]any{}
	dir := "."
	if f != nil && f.SourceSubpath != "" {
		dir = f.SourceSubpath
	}
	if dir != "." {
		opts["context"] = dir
	}
	s := Suggestion{Method: method, SourceSubpath: dir, Document: d}
	if method == providers.MethodRailpack {
		if c, ok := cmd.(string); ok && c != "" {
			opts["buildCommand"] = c
		}
		if c, ok := start.(string); ok && c != "" {
			opts["startCommand"] = c
		}
		s.Title = "Railpack for " + dir
		s.Reasons = []string{"Railpack plans the build on the worker from the files in " + dir + "; nothing runs during discovery."}
	} else {
		s.Title = "Buildpacks for " + dir
		s.Reasons = []string{"The organization's approved buildpacks builder detects and builds " + dir + " on the worker."}
	}
	if len(opts) > 0 {
		build[method] = opts
	}
	if f != nil {
		s.Warnings = findingWarnings(f)
	}
	return s
}

func findingWarnings(f *discovery.Finding) []discovery.Warning {
	out := append([]discovery.Warning{}, f.Warnings...)
	for _, r := range f.Readiness {
		out = append(out, discovery.Warning{Code: r.Code, Message: r.Message, Path: r.Path})
	}
	return out
}

// assumedPorts are conventional listening ports, used only when the http-startup check needs a port and discovery
// found none; the suggestion says so.
var assumedPorts = map[string]int{"nodejs": 3000, "python": 8000, "dotnet": 8080}

func assumePort(s *Suggestion) {
	if get(s.Document, "spec", "validation", "profile") != "http-startup" || get(s.Document, "spec", "application", "port") != nil {
		return
	}
	lang, _ := get(s.Document, "spec", "application", "runtime", "language").(string)
	port, ok := assumedPorts[lang]
	if !ok {
		port = 8080
	}
	app, _ := s.Document["spec"].(map[string]any)["application"].(map[string]any)
	if app == nil {
		app = map[string]any{}
		s.Document["spec"].(map[string]any)["application"] = app
	}
	app["port"] = port
	s.Warnings = append(s.Warnings, discovery.Warning{Code: "port-assumed",
		Message: fmt.Sprintf("Discovery cannot tell which port the application listens on; %d is assumed. Set spec.application.port to the real one.", port)})
}

func orUnknown(s string) string {
	if s == "" {
		return "unrecognised"
	}
	return s
}

// The functions below mirror ui/src/spec/model.ts (fromFinding, pickRecipe, appName, withFrontend,
// frontendBackendPairs, defaultProfile, slug) and the wizard's withDockerfile and fillFrom. Keep them in step;
// TestSuggestMatchesWizard and model.test.ts check both against testdata/suggestions.json.

var languageOf = map[string]string{"javascript": "nodejs", "typescript": "nodejs", "csharp": "dotnet", "fsharp": "dotnet", "visualbasic": "dotnet"}

func draftForApp(name string, f *discovery.Finding, recipes []RecipeChoice) map[string]any {
	app, runtime, recipe := map[string]any{}, map[string]any{}, map[string]any{}
	if f != nil {
		put(app, "type", f.ApplicationKind)
		lang := f.Language
		if m, ok := languageOf[lang]; ok {
			lang = m
		}
		put(runtime, "language", lang)
		put(runtime, "framework", f.Framework)
		for _, k := range []string{"node", "python", "dotnet", "powershell"} {
			if v, ok := f.RuntimeConstraints[k]; ok {
				put(runtime, "versionConstraint", v)
				break
			}
		}
		for _, k := range []string{"install", "build", "start", "test"} {
			put(recipe, k, f.SuggestedCommands[k])
		}
		if f.SourceSubpath != "" && f.SourceSubpath != "." {
			recipe["path"] = f.SourceSubpath
		}
	}
	if len(runtime) > 0 {
		app["runtime"] = runtime
	}
	if r := pickRecipe(recipes, runtime["language"], app["type"]); r != nil {
		recipe["id"], recipe["version"] = r.ID, r.Version
	}
	build := map[string]any{"method": providers.MethodRecipe}
	if len(recipe) > 0 {
		build["recipe"] = recipe
	}
	kind, _ := app["type"].(string)
	spec := map[string]any{"build": build, "validation": map[string]any{"profile": defaultProfile(kind)}}
	if len(app) > 0 {
		spec["application"] = app
	}
	return map[string]any{"apiVersion": buildspec.APIVersion, "kind": "ImageBuild", "metadata": map[string]any{"name": name}, "spec": spec}
}

func pickRecipe(recipes []RecipeChoice, lang, kind any) *RecipeChoice {
	l, _ := lang.(string)
	k, _ := kind.(string)
	for i, r := range recipes {
		if r.Approved && slices.Contains(r.Languages, l) && (k == "" || slices.Contains(r.ApplicationTypes, k)) {
			return &recipes[i]
		}
	}
	return nil
}

func appName(f *discovery.Finding, project string) string {
	if f != nil && f.SourceSubpath != "." && f.SourceSubpath != "" {
		return slug(path.Base(f.SourceSubpath))
	}
	if project == "" {
		project = "app"
	}
	return slug(project)
}

func defaultProfile(kind string) string {
	if p, ok := map[string]string{"http-service": "http-startup", "static-site": "static-serve", "worker": "process-start", "cli-task": "cli-smoke"}[kind]; ok {
		return p
	}
	return "none-requested"
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

func slug(s string) string {
	s = strings.Trim(nonName.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 63 {
		s = s[:63]
	}
	if s == "" {
		return "app"
	}
	return s
}

func withFrontend(doc map[string]any, fe *discovery.Finding) map[string]any {
	front := map[string]any{"path": fe.SourceSubpath, "apiPrefixes": []any{"/api"}, "backendPort": 8000}
	put(front, "install", fe.SuggestedCommands["install"])
	put(front, "build", fe.SuggestedCommands["build"])
	spec := doc["spec"].(map[string]any)
	build := spec["build"].(map[string]any)
	recipe, _ := build["recipe"].(map[string]any)
	if recipe == nil {
		recipe = map[string]any{}
		build["recipe"] = recipe
	}
	recipe["frontend"] = front
	app, _ := spec["application"].(map[string]any)
	if app == nil {
		app = map[string]any{}
		spec["application"] = app
	}
	app["type"] = "http-service"
	if _, ok := app["port"]; !ok {
		app["port"] = 8080
	}
	spec["validation"] = map[string]any{"profile": "http-startup"}
	return doc
}

type frontendBackend struct{ fe, be discovery.Finding }

func frontendBackendPairs(apps []discovery.Finding) []frontendBackend {
	var out []frontendBackend
	for _, fe := range apps {
		if fe.ApplicationKind != "static-site" || fe.SourceSubpath == "." || !slices.Contains([]string{"javascript", "typescript", "nodejs"}, fe.Language) {
			continue
		}
		for _, be := range apps {
			if be.ApplicationKind == "http-service" && be.SourceSubpath != "." && be.SourceSubpath != fe.SourceSubpath {
				out = append(out, frontendBackend{fe, be})
			}
		}
	}
	return out
}

// fillFrom copies the application found in a Dockerfile's folder, and the port when the Dockerfile exposes exactly one.
func fillFrom(doc map[string]any, f *discovery.Finding, df discovery.DockerfileAnalysis) map[string]any {
	spec := doc["spec"].(map[string]any)
	if f != nil {
		found := draftForApp("", f, nil)["spec"].(map[string]any)
		if app, ok := found["application"]; ok {
			spec["application"] = app
		}
	}
	kind, _ := get(doc, "spec", "application", "type").(string)
	if kind != "" {
		spec["validation"] = map[string]any{"profile": defaultProfile(kind)}
	}
	if len(df.ExposedPorts) == 1 && kind == "http-service" {
		var port int
		if _, err := fmt.Sscanf(df.ExposedPorts[0], "%d", &port); err == nil && fmt.Sprint(port) == df.ExposedPorts[0] {
			spec["application"].(map[string]any)["port"] = port
		}
	}
	return doc
}

func withDockerfile(doc map[string]any, df discovery.DockerfileAnalysis) map[string]any {
	d := map[string]any{"context": path.Dir(df.Path), "path": path.Base(df.Path)}
	if n := len(df.Stages); n > 0 {
		d["target"] = df.Stages[n-1]
	}
	doc["spec"].(map[string]any)["build"] = map[string]any{"method": providers.MethodDockerfile, "dockerfile": d}
	return doc
}

func put(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func get(doc map[string]any, keys ...string) any {
	var cur any = doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}
