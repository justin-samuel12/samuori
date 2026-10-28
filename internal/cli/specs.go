package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
)

// discovery returns the snapshot's newest discovery, starting one when there is none or rerun is set. Discovery is
// static and never runs the uploaded code.
func (e *env) discovery(ctx context.Context, snapshot string, rerun bool, timeout time.Duration) (*apiclient.Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := ""
	if !rerun {
		list, err := e.api.ListSnapshotDiscoveriesWithResponse(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		if list.JSON200 == nil {
			return nil, problem("reading the discoveries", list.StatusCode(), list.Body)
		}
		if len(list.JSON200.Items) > 0 && list.JSON200.Items[0].Status != apiclient.DiscoveryStatusFailed {
			id = list.JSON200.Items[0].Id
		}
	}
	if id == "" {
		op, err := e.api.StartDiscoveryWithResponse(ctx, snapshot, nil)
		if err != nil {
			return nil, err
		}
		if op.JSON202 == nil {
			return nil, problem("starting discovery", op.StatusCode(), op.Body)
		}
		if id, err = e.awaitOperation(ctx, op.JSON202.Id, "discovery"); err != nil {
			return nil, err
		}
	}
	for {
		d, err := e.api.GetDiscoveryWithResponse(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, failf(ExitTimeout, "discovery %s did not finish within %s", id, timeout)
			}
			return nil, err
		}
		if d.JSON200 == nil {
			return nil, problem("reading the discovery", d.StatusCode(), d.Body)
		}
		if d.JSON200.Status != apiclient.DiscoveryStatusRunning {
			return d.JSON200, nil
		}
		if err := sleep(ctx, time.Second); err != nil {
			return nil, failf(ExitTimeout, "discovery %s did not finish within %s", id, timeout)
		}
	}
}

// awaitOperation waits for an asynchronous operation and returns the ID of what it produced.
func (e *env) awaitOperation(ctx context.Context, id, what string) (string, error) {
	for {
		op, err := e.api.GetOperationWithResponse(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return "", failf(ExitTimeout, "%s did not finish in time", what)
			}
			return "", err
		}
		if op.JSON200 == nil {
			return "", problem("waiting for the "+what, op.StatusCode(), op.Body)
		}
		switch op.JSON200.Status {
		case apiclient.OperationStatusSucceeded:
			if op.JSON200.ResourceId == nil {
				return "", fmt.Errorf("the %s finished without a result", what)
			}
			return *op.JSON200.ResourceId, nil
		case apiclient.OperationStatusFailed:
			msg := "the " + what + " was refused"
			if op.JSON200.Error != nil {
				msg = op.JSON200.Error.Message
			}
			return "", failf(ExitFailed, "%s", msg)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return "", failf(ExitTimeout, "%s did not finish in time", what)
		}
	}
}

func (e *env) analyze(ctx context.Context, args []string) error {
	fs := flags("analyze", e)
	rerun := fs.Bool("rerun", false, "run discovery again instead of showing the newest result")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for discovery")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori analyze SNAPSHOT [--rerun]")
	}
	d, err := e.discovery(ctx, pos[0], *rerun, *timeout)
	if err != nil {
		return err
	}
	if e.json {
		if err := e.printJSON(d); err != nil {
			return err
		}
	} else {
		e.printDiscovery(d)
	}
	if d.Status == apiclient.DiscoveryStatusFailed {
		msg := "discovery failed"
		if d.Error != nil {
			msg += ": " + d.Error.Message
		}
		return failf(ExitFailed, "%s", msg)
	}
	return nil
}

func (e *env) printDiscovery(d *apiclient.Discovery) {
	fmt.Fprintf(e.out, "Discovery %s: %s (static analysis; the uploaded code was not run)\n", d.Id, d.Status)
	var readiness []apiclient.ReadinessItem
	if d.Findings != nil && len(*d.Findings) > 0 {
		fmt.Fprintln(e.out, "\nApplications:")
		var rows [][]string
		for _, f := range *d.Findings {
			rows = append(rows, []string{f.SourceSubpath, str(f.Language), str(f.Framework), str(f.PackageManager), string(f.Confidence)})
			if f.Readiness != nil {
				readiness = append(readiness, *f.Readiness...)
			}
		}
		_ = e.table(nil, []string{"PATH", "LANGUAGE", "FRAMEWORK", "PACKAGES", "CONFIDENCE"}, rows)
	}
	if d.Dockerfiles != nil && len(*d.Dockerfiles) > 0 {
		fmt.Fprintln(e.out, "\nDockerfiles:")
		var rows [][]string
		for _, df := range *d.Dockerfiles {
			rows = append(rows, []string{df.Path, strings.Join(df.Stages, ","), list(df.BaseImages), list(df.ExposedPorts)})
			if df.Readiness != nil {
				readiness = append(readiness, *df.Readiness...)
			}
		}
		_ = e.table(nil, []string{"PATH", "STAGES", "BASE IMAGES", "PORTS"}, rows)
	}
	if len(readiness) > 0 {
		fmt.Fprintln(e.out, "\nBefore building:")
		for _, r := range readiness {
			e.readinessLine(r)
		}
	}
	if d.Warnings != nil {
		for _, w := range *d.Warnings {
			fmt.Fprintf(e.out, "warning %s: %s\n", w.Code, w.Message)
		}
	}
}

func (e *env) readinessLine(r apiclient.ReadinessItem) {
	where := ""
	if r.Path != nil {
		where = " (" + *r.Path
		if r.Line != nil {
			where += ":" + strconv.Itoa(*r.Line)
		}
		where += ")"
	}
	fmt.Fprintf(e.out, "  [%s] %s%s: %s\n", r.Severity, r.Code, where, r.Message)
	if r.Fix != "" {
		fmt.Fprintf(e.out, "      fix: %s\n", r.Fix)
	}
}

func list(p *[]string) string {
	if p == nil || len(*p) == 0 {
		return "-"
	}
	return strings.Join(*p, ",")
}

func (e *env) spec(ctx context.Context, args []string) error {
	return e.subcommand(args, "", map[string]func([]string) error{
		"init":     func(a []string) error { return e.specInit(ctx, a) },
		"list":     func(a []string) error { return e.specList(ctx, a) },
		"show":     func(a []string) error { return e.specShow(ctx, a) },
		"validate": func(a []string) error { return e.specValidate(ctx, a) },
		"check":    func(a []string) error { return e.specCheck(ctx, a) },
		"apply":    func(a []string) error { return e.specApply(ctx, a) },
	}, "spec")
}

var nameChars = regexp.MustCompile(`[^a-z0-9-]+`)

var specNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func (e *env) specInit(ctx context.Context, args []string) error {
	fs := flags("spec init", e)
	snapshot := fs.String("snapshot", "", "start from the server's suggestions for this snapshot's discovery")
	method := fs.String("method", "", "recipe, dockerfile, railpack or buildpacks (default: the server's recommendation, else ./Dockerfile or railpack)")
	out := fs.String("o", "samuori.spec.yml", "file to write; - for standard output")
	force := fs.Bool("force", false, "replace an existing file")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return failf(ExitUsage, "usage: samuori spec init [NAME] [--snapshot SNAPSHOT] [--method M] [-o FILE]")
	}
	if *method != "" && !slices.Contains([]string{"recipe", "dockerfile", "railpack", "buildpacks"}, *method) {
		return failf(ExitUsage, "--method is recipe, dockerfile, railpack or buildpacks")
	}
	name := ""
	if len(pos) == 1 {
		if name = pos[0]; !specNamePattern.MatchString(name) {
			return failf(ExitUsage, "give a spec NAME of lowercase letters, digits and -")
		}
	}
	doc := ""
	if *snapshot != "" {
		s, err := e.suggestion(ctx, *snapshot, name, *method)
		if err != nil {
			return err
		}
		if s != nil {
			doc, *method = s.CanonicalYaml, string(s.Method)
		}
	}
	if doc == "" {
		if *method == "recipe" {
			return failf(ExitFailed, "no recipe spec could be suggested; upload the source and pass --snapshot, or use the web wizard")
		}
		if name == "" {
			if wd, err := os.Getwd(); err == nil {
				name = strings.Trim(nameChars.ReplaceAllString(strings.ToLower(filepath.Base(wd)), "-"), "-")
			}
			if !specNamePattern.MatchString(name) {
				return failf(ExitUsage, "give a spec NAME of lowercase letters, digits and -")
			}
		}
		if _, err := os.Stat("Dockerfile"); err == nil && *method == "" && *snapshot == "" {
			*method = "dockerfile"
		}
		if *method == "" {
			*method = "railpack"
		}
		doc = fmt.Sprintf("apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nmetadata:\n  name: %s\nspec:\n  build:\n    method: %s\n    %s:\n      context: \".\"\n", name, *method, *method)
		if *method == "dockerfile" {
			doc += "      path: \"Dockerfile\"\n"
		}
	}
	if *out == "-" {
		_, err := fmt.Fprint(e.out, doc)
		return err
	}
	mode := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		mode = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(*out, mode, 0o644)
	if err != nil {
		return failf(ExitUsage, "%v (add --force to replace it)", err)
	}
	if _, err := f.WriteString(doc); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	e.note("wrote %s (%s method); check it with samuori spec validate %s, then save it with samuori spec apply %s --project PROJECT", *out, *method, *out, *out)
	return nil
}

// suggestion returns the server's recommended spec for the snapshot, or its first one for method, after explaining it
// on standard error. It returns nil when the server suggests nothing that fits.
func (e *env) suggestion(ctx context.Context, snapshot, name, method string) (*apiclient.SpecSuggestion, error) {
	if _, err := e.discovery(ctx, snapshot, false, 5*time.Minute); err != nil {
		return nil, err
	}
	params := &apiclient.ListSpecSuggestionsParams{}
	if name != "" {
		params.Name = &name
	}
	r, err := e.api.ListSpecSuggestionsWithResponse(ctx, snapshot, params)
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, problem("reading the spec suggestions", r.StatusCode(), r.Body)
	}
	for _, w := range r.JSON200.Warnings {
		e.note("note %s: %s", w.Code, w.Message)
	}
	var pick *apiclient.SpecSuggestion
	var others []string
	for i, s := range r.JSON200.Items {
		if pick == nil && (method == "" || string(s.Method) == method) {
			pick = &r.JSON200.Items[i]
			continue
		}
		others = append(others, s.Title)
	}
	if pick == nil {
		if method != "" {
			e.note("the server has no %s suggestion for this snapshot (the project may not be allowed to use it); writing a plain starter", method)
		}
		return nil, nil
	}
	e.note("suggested: %s", pick.Title)
	for _, why := range pick.Reasons {
		e.note("  %s", why)
	}
	for _, w := range pick.Warnings {
		e.note("  warning %s: %s", w.Code, w.Message)
	}
	if !pick.Valid {
		for _, d := range append(pick.SchemaErrors, pick.SemanticErrors...) {
			e.note("  to fix %s: %s", str(d.Field), str(d.Reason))
		}
	}
	if len(others) > 0 {
		e.note("other suggestions (choose with --method): %s", strings.Join(others, "; "))
	}
	return pick, nil
}

func (e *env) specList(ctx context.Context, args []string) error {
	fs := flags("spec list", e)
	project := fs.String("project", "", "project ID or name")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	id, err := e.projectID(ctx, *project)
	if err != nil {
		return err
	}
	limit := 200
	r, err := e.api.ListSpecsWithResponse(ctx, id, &apiclient.ListSpecsParams{Limit: &limit})
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("listing specs", r.StatusCode(), r.Body)
	}
	var rows [][]string
	for _, s := range r.JSON200.Items {
		created := "-"
		if s.CreatedAt != nil {
			created = when(*s.CreatedAt)
		}
		rows = append(rows, []string{s.Id, s.Name, strconv.Itoa(s.LatestRevision), created})
	}
	return e.table(r.JSON200.Items, []string{"ID", "NAME", "REVISION", "CREATED"}, rows)
}

func (e *env) specShow(ctx context.Context, args []string) error {
	fs := flags("spec show", e)
	project := fs.String("project", "", "project ID or name, to find the spec by name")
	revision := fs.Int("revision", 0, "revision (default: latest)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori spec show SPEC [--project PROJECT] [--revision N]")
	}
	s, err := e.specRef(ctx, pos[0], *project)
	if err != nil {
		return err
	}
	if *revision == 0 {
		*revision = s.LatestRevision
	}
	r, err := e.api.GetSpecRevisionWithResponse(ctx, s.Id, *revision)
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("reading the revision", r.StatusCode(), r.Body)
	}
	if e.json {
		return e.printJSON(r.JSON200)
	}
	e.note("spec %s (%s) revision %d", s.Name, s.Id, *revision)
	var doc any
	if err := json.Unmarshal(r.Body, &struct {
		Document *any `json:"document"`
	}{&doc}); err != nil {
		return err
	}
	return e.printJSON(doc)
}

// specInput reads a spec file: JSON (a document, or {"document": …} as the API takes it) or YAML.
func specInput(path string) (apiclient.ValidateSpecDocumentJSONRequestBody, error) {
	var in apiclient.ValidateSpecDocumentJSONRequestBody
	raw, err := os.ReadFile(path)
	if err != nil {
		return in, failf(ExitUsage, "%v", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		text := string(raw)
		in.Yaml = &text
		return in, nil
	}
	var wrapped struct {
		Document json.RawMessage `json:"document"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Document) > 0 {
		raw = wrapped.Document
	}
	var doc apiclient.BuildSpecDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return in, failf(ExitUsage, "%s is not a spec document: %v", path, err)
	}
	in.Document = &doc
	return in, nil
}

// validated validates a spec file on the server and prints its errors; it fails with ExitFailed when invalid.
func (e *env) validated(ctx context.Context, path string) (*apiclient.SpecValidation, error) {
	in, err := specInput(path)
	if err != nil {
		return nil, err
	}
	r, err := e.api.ValidateSpecDocumentWithResponse(ctx, nil, in)
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil {
		return nil, problem("validating the spec", r.StatusCode(), r.Body)
	}
	v := r.JSON200
	if v.Valid {
		return v, nil
	}
	if e.json {
		_ = e.printJSON(v)
	}
	for _, d := range append(v.SchemaErrors, v.SemanticErrors...) {
		e.note("  %s: %s", str(d.Field), str(d.Reason))
	}
	return nil, failf(ExitFailed, "%s is not a valid spec", path)
}

func (e *env) specValidate(ctx context.Context, args []string) error {
	pos, err := parse(flags("spec validate", e), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori spec validate FILE")
	}
	v, err := e.validated(ctx, pos[0])
	if err != nil {
		return err
	}
	return e.result(v, "valid "+str(v.Digest))
}

func (e *env) specCheck(ctx context.Context, args []string) error {
	fs := flags("spec check", e)
	project := fs.String("project", "", "project ID or name, to find the spec by name")
	revision := fs.Int("revision", 0, "revision (default: latest)")
	snapshot := fs.String("snapshot", "", "snapshot ID")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *snapshot == "" {
		return failf(ExitUsage, "usage: samuori spec check SPEC --snapshot SNAPSHOT [--project PROJECT] [--revision N]")
	}
	s, err := e.specRef(ctx, pos[0], *project)
	if err != nil {
		return err
	}
	if *revision == 0 {
		*revision = s.LatestRevision
	}
	r, err := e.api.CheckAdmissionWithResponse(ctx, s.Id, *revision, nil, apiclient.CheckAdmissionJSONRequestBody{SnapshotId: *snapshot})
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("checking admission", r.StatusCode(), r.Body)
	}
	a := r.JSON200
	if e.json {
		_ = e.printJSON(a)
	} else {
		verdict := "admissible"
		if !a.Admissible {
			verdict = "not admissible"
		}
		fmt.Fprintf(e.out, "%s revision %d with snapshot %s: %s\n", s.Name, *revision, *snapshot, verdict)
		for _, list := range []*[]apiclient.ProblemDetail{a.SchemaErrors, a.SemanticErrors} {
			if list != nil {
				for _, d := range *list {
					fmt.Fprintf(e.out, "  error %s: %s\n", str(d.Field), str(d.Reason))
				}
			}
		}
		if a.PolicyDecisions != nil {
			for _, p := range *a.PolicyDecisions {
				fmt.Fprintf(e.out, "  policy %s v%d: %s %s\n", p.PolicyId, p.PolicyVersion, p.Outcome, str(p.Reason))
			}
		}
		for _, sr := range a.SecretReadiness {
			fmt.Fprintf(e.out, "  secret %s: %s %s\n", sr.Id, sr.Status, str(sr.Reason))
		}
		if a.Readiness != nil {
			for _, item := range *a.Readiness {
				e.readinessLine(item)
			}
		}
		for _, list := range []*[]apiclient.Warning{a.PlatformIssues, a.Warnings} {
			if list != nil {
				for _, w := range *list {
					fmt.Fprintf(e.out, "  warning %s: %s\n", w.Code, w.Message)
				}
			}
		}
	}
	if !a.Admissible {
		return failf(ExitFailed, "%s revision %d is not admissible with snapshot %s", s.Name, *revision, *snapshot)
	}
	return nil
}

func (e *env) specApply(ctx context.Context, args []string) error {
	fs := flags("spec apply", e)
	project := fs.String("project", "", "project ID or name")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *project == "" {
		return failf(ExitUsage, "usage: samuori spec apply FILE --project PROJECT")
	}
	v, err := e.validated(ctx, pos[0])
	if err != nil {
		return err
	}
	pid, err := e.projectID(ctx, *project)
	if err != nil {
		return err
	}
	if v.Document == nil {
		return fmt.Errorf("the server returned no canonical document")
	}
	meta, _ := (*v.Document)["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	body, err := json.Marshal(map[string]any{"document": *v.Document})
	if err != nil {
		return err
	}
	existing, err := e.api.ListSpecsWithResponse(ctx, pid, &apiclient.ListSpecsParams{Name: &name})
	if err != nil {
		return err
	}
	if existing.JSON200 == nil {
		return problem("finding the spec", existing.StatusCode(), existing.Body)
	}
	if len(existing.JSON200.Items) == 0 {
		r, err := e.api.CreateSpecWithBodyWithResponse(ctx, pid, nil, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		if r.JSON201 == nil {
			return problem("creating the spec", r.StatusCode(), r.Body)
		}
		e.note("created spec %s revision 1", name)
		return e.result(r.JSON201, r.JSON201.SpecId)
	}
	s := existing.JSON200.Items[0]
	latest, err := e.api.GetSpecRevisionWithResponse(ctx, s.Id, s.LatestRevision)
	if err != nil {
		return err
	}
	if latest.JSON200 == nil {
		return problem("reading the latest revision", latest.StatusCode(), latest.Body)
	}
	if v.Digest != nil && latest.JSON200.Digest == *v.Digest {
		e.note("spec %s is unchanged at revision %d", name, s.LatestRevision)
		return e.result(latest.JSON200, s.Id)
	}
	r, err := e.api.CreateSpecRevisionWithBodyWithResponse(ctx, s.Id, &apiclient.CreateSpecRevisionParams{IfMatch: fmt.Sprintf("%q", strconv.Itoa(s.LatestRevision))}, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if r.JSON201 == nil {
		return problem("adding a revision", r.StatusCode(), r.Body)
	}
	e.note("spec %s revision %d", name, r.JSON201.Revision)
	return e.result(r.JSON201, s.Id)
}
