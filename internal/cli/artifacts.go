package cli

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"samuori.invalid/samuori/internal/apiclient"
)

func (e *env) images(ctx context.Context, args []string) error {
	return e.subcommand(args, "list", map[string]func([]string) error{"list": func(args []string) error {
		fs := flags("images", e)
		project := fs.String("project", "", "project ID or name")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		id, err := e.projectID(ctx, *project)
		if err != nil {
			return err
		}
		limit := 200
		r, err := e.api.ListProjectImagesWithResponse(ctx, id, &apiclient.ListProjectImagesParams{Limit: &limit})
		if err != nil {
			return err
		}
		if r.JSON200 == nil {
			return problem("listing images", r.StatusCode(), r.Body)
		}
		var rows [][]string
		for _, img := range r.JSON200.Items {
			checks := "-"
			if img.Validation != nil {
				checks = str(img.Validation.Verdict)
				if checks == "-" {
					checks = string(img.Validation.State)
				}
			}
			var published []string
			for _, p := range img.Publications {
				if p.State == "published" && p.DestinationRef != nil {
					published = append(published, *p.DestinationRef)
				}
			}
			rows = append(rows, []string{img.ArtifactId, img.BuildRunId, str(img.Version), shortDigest(img.Digest), checks, strings.Join(published, " "), when(img.CreatedAt)})
		}
		return e.table(r.JSON200.Items, []string{"ARTIFACT", "BUILD", "VERSION", "DIGEST", "CHECKS", "PUBLISHED", "CREATED"}, rows)
	}}, "images")
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func (e *env) download(ctx context.Context, args []string) error {
	fs := flags("download", e)
	artifact := fs.String("artifact", "", "artifact ID, instead of BUILD")
	out := fs.String("o", "", "file or directory to write (default: the server's file name); - for standard output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if (len(pos) == 1) == (*artifact != "") || len(pos) > 1 {
		return failf(ExitUsage, "usage: samuori download (BUILD | --artifact ID) [-o FILE]")
	}
	if *artifact == "" {
		if *artifact, err = e.artifactOf(ctx, pos[0]); err != nil {
			return err
		}
	}
	resp, err := e.stream.DownloadArtifact(ctx, *artifact)
	if err != nil {
		return err
	}
	path, err := e.save(resp, "downloading the image", *out, *artifact+".tar")
	if err != nil || path == "" {
		return err
	}
	e.note("load it with: docker load -i %s", path)
	return e.result(map[string]string{"path": path, "artifactId": *artifact}, path)
}

func (e *env) deployFiles(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "compose" && args[0] != "helm") {
		return failf(ExitUsage, "usage: samuori deploy-files compose|helm (--build ID | --artifact ID | --project PROJECT) [--publication ID] [-o FILE]")
	}
	kind := args[0]
	fs := flags("deploy-files", e)
	artifact := fs.String("artifact", "", "artifact ID")
	build := fs.String("build", "", "build ID, instead of --artifact")
	project := fs.String("project", "", "project ID or name: one file for all of its images")
	publication := fs.String("publication", "", "publication whose reference the file uses (with --artifact or --build)")
	out := fs.String("o", "", "file or directory to write (default: the server's file name); - for standard output")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	set := 0
	for _, v := range []string{*artifact, *build, *project} {
		if v != "" {
			set++
		}
	}
	if set != 1 || (*project != "" && *publication != "") {
		return failf(ExitUsage, "usage: samuori deploy-files %s (--build ID | --artifact ID | --project PROJECT) [--publication ID] [-o FILE]", kind)
	}
	var err error
	if *build != "" {
		if *artifact, err = e.artifactOf(ctx, *build); err != nil {
			return err
		}
	}
	path, err := e.saveDeployFile(ctx, kind, *artifact, *project, *publication, *out)
	if err != nil || path == "" {
		return err
	}
	return e.result(map[string]string{"path": path}, path)
}

func (e *env) saveDeployFile(ctx context.Context, kind, artifact, project, publication, out string) (string, error) {
	var resp *http.Response
	var err error
	if project != "" {
		id, perr := e.projectID(ctx, project)
		if perr != nil {
			return "", perr
		}
		if kind == "compose" {
			resp, err = e.stream.DownloadProjectDeploymentCompose(ctx, id, nil)
		} else {
			resp, err = e.stream.DownloadProjectDeploymentHelmChart(ctx, id, nil)
		}
	} else {
		var pub *string
		if publication != "" {
			pub = &publication
		}
		if kind == "compose" {
			resp, err = e.stream.DownloadDeploymentCompose(ctx, artifact, &apiclient.DownloadDeploymentComposeParams{PublicationId: pub})
		} else {
			resp, err = e.stream.DownloadDeploymentHelmChart(ctx, artifact, &apiclient.DownloadDeploymentHelmChartParams{PublicationId: pub})
		}
	}
	if err != nil {
		return "", err
	}
	def := "compose.yml"
	if kind == "helm" {
		def = "chart.tgz"
	}
	return e.save(resp, "downloading the "+kind+" file", out, def)
}

// save streams a successful response to out: a file, a directory, - for standard output, or "" for the name the
// server suggests. The file appears only when complete. It returns the path written.
func (e *env) save(resp *http.Response, what, out, def string) (string, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return "", problem(what, resp.StatusCode, body)
	}
	if out == "-" {
		_, err := io.Copy(e.out, resp.Body)
		return "", err
	}
	name := def
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if n := filepath.Base(filepath.Clean("/" + params["filename"])); n != "/" && n != "." {
			name = n
		}
	}
	path := out
	if path == "" {
		path = name
	} else if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, name)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".samuori-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	e.note("wrote %s (%d bytes)", path, n)
	return path, nil
}

func (e *env) requests(ctx context.Context, args []string) error {
	return e.subcommand(args, "list", map[string]func([]string) error{"list": func(args []string) error {
		fs := flags("requests", e)
		project := fs.String("project", "", "project ID or name")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		id, err := e.projectID(ctx, *project)
		if err != nil {
			return err
		}
		type row struct {
			Kind     string `json:"kind"`
			Approval string `json:"approvalId"`
			ID       string `json:"id"`
			State    string `json:"state"`
			Subject  string `json:"subject"`
			Created  string `json:"createdAt"`
		}
		var items []row
		limit := 200
		for cursor := (*string)(nil); ; {
			repo, err := e.api.ListRepositoryRequestsWithResponse(ctx, id, &apiclient.ListRepositoryRequestsParams{Limit: &limit, Cursor: cursor})
			if err != nil {
				return err
			}
			if repo.JSON200 == nil {
				return problem("listing repository requests", repo.StatusCode(), repo.Body)
			}
			for _, r := range repo.JSON200.Items {
				items = append(items, row{"repository", r.ApprovalId, r.Id, string(r.State), r.Repository, when(r.CreatedAt)})
			}
			if cursor = repo.JSON200.NextCursor; cursor == nil || *cursor == "" {
				break
			}
		}
		for cursor := (*string)(nil); ; {
			base, err := e.api.ListBaseImageRequestsWithResponse(ctx, id, &apiclient.ListBaseImageRequestsParams{Limit: &limit, Cursor: cursor})
			if err != nil {
				return err
			}
			if base.JSON200 == nil {
				return problem("listing base-image requests", base.StatusCode(), base.Body)
			}
			for _, r := range base.JSON200.Items {
				items = append(items, row{"base-image", r.ApprovalId, r.Id, string(r.State), r.Repository, when(r.CreatedAt)})
			}
			if cursor = base.JSON200.NextCursor; cursor == nil || *cursor == "" {
				break
			}
		}
		for cursor := (*string)(nil); ; {
			egress, err := e.api.ListEgressAccessRequestsWithResponse(ctx, id, &apiclient.ListEgressAccessRequestsParams{Limit: &limit, Cursor: cursor})
			if err != nil {
				return err
			}
			if egress.JSON200 == nil {
				return problem("listing egress requests", egress.StatusCode(), egress.Body)
			}
			for _, r := range egress.JSON200.Items {
				items = append(items, row{"egress", r.ApprovalId, r.Id, string(r.State), strings.Join(append(append([]string{}, r.Presets...), r.Destinations...), ","), when(r.CreatedAt)})
			}
			if cursor = egress.JSON200.NextCursor; cursor == nil || *cursor == "" {
				break
			}
		}
		var rows [][]string
		for _, r := range items {
			rows = append(rows, []string{r.Approval, r.Kind, r.State, r.Subject, r.Created})
		}
		return e.table(items, []string{"APPROVAL", "KIND", "STATE", "SUBJECT", "CREATED"}, rows)
	}}, "requests")
}

func (e *env) request(ctx context.Context, args []string) error {
	return e.subcommand(args, "", map[string]func([]string) error{
		"repository": func(args []string) error {
			fs := flags("request repository", e)
			project := fs.String("project", "", "project ID or name")
			repository := fs.String("repository", "", "repository to create, such as team/web")
			registry := fs.String("registry", "", "registry ID (optional)")
			why := fs.String("justification", "", "why the project needs it")
			if _, err := parse(fs, args); err != nil {
				return err
			}
			if *repository == "" || *why == "" {
				return failf(ExitUsage, "usage: samuori request repository --project PROJECT --repository NAME --justification TEXT [--registry ID]")
			}
			id, err := e.projectID(ctx, *project)
			if err != nil {
				return err
			}
			body := apiclient.RequestRepositoryJSONRequestBody{Repository: *repository, Justification: *why}
			if *registry != "" {
				body.RegistryId = registry
			}
			r, err := e.api.RequestRepositoryWithResponse(ctx, id, nil, body)
			if err != nil {
				return err
			}
			if r.JSON202 == nil {
				return problem("requesting the repository", r.StatusCode(), r.Body)
			}
			return e.result(r.JSON202, r.JSON202.ApprovalId)
		},
		"base-image": func(args []string) error {
			fs := flags("request base-image", e)
			project := fs.String("project", "", "project ID or name")
			repository := fs.String("repository", "", "image repository, such as registry.example.com/library/node")
			why := fs.String("justification", "", "why the project needs it")
			if _, err := parse(fs, args); err != nil {
				return err
			}
			if *repository == "" || *why == "" {
				return failf(ExitUsage, "usage: samuori request base-image --project PROJECT --repository REPO --justification TEXT")
			}
			id, err := e.projectID(ctx, *project)
			if err != nil {
				return err
			}
			r, err := e.api.RequestBaseImageWithResponse(ctx, id, nil, apiclient.RequestBaseImageJSONRequestBody{Repository: *repository, Justification: *why})
			if err != nil {
				return err
			}
			if r.JSON202 == nil {
				return problem("requesting the base image", r.StatusCode(), r.Body)
			}
			return e.result(r.JSON202, r.JSON202.ApprovalId)
		},
		"egress": func(args []string) error {
			fs := flags("request egress", e)
			project := fs.String("project", "", "project ID or name")
			why := fs.String("justification", "", "why builds need it")
			method := fs.String("method", "", "build method the access is for (default: the server's choice)")
			var presets, destinations, internal multiFlag
			fs.Var(&presets, "preset", "egress preset (repeatable)")
			fs.Var(&destinations, "destination", "public destination host[:port] (repeatable)")
			fs.Var(&internal, "internal", "internal destination (repeatable)")
			var ev apiclient.EgressEvidenceRef
			discovery := fs.String("discovery", "", "evidence: discovery ID")
			build := fs.String("build", "", "evidence: build whose proxy denied the destinations")
			spec := fs.String("spec", "", "evidence: spec ID of an admission check")
			revision := fs.Int("revision", 0, "evidence: spec revision")
			snapshot := fs.String("snapshot", "", "evidence: snapshot ID of an admission check")
			code := fs.String("code", "", "evidence: readiness item code")
			path := fs.String("path", "", "evidence: readiness item path")
			line := fs.Int("line", 0, "evidence: readiness item line")
			if _, err := parse(fs, args); err != nil {
				return err
			}
			if *why == "" || len(presets)+len(destinations)+len(internal) == 0 || *discovery == "" && *build == "" && *spec == "" {
				return failf(ExitUsage, "usage: samuori request egress --project PROJECT --justification TEXT (--preset P | --destination D)... (--discovery ID | --build ID | --spec S --revision N --snapshot ID) [--code C --path P --line N]")
			}
			id, err := e.projectID(ctx, *project)
			if err != nil {
				return err
			}
			for _, f := range []struct {
				v   string
				dst **string
			}{{*discovery, &ev.DiscoveryId}, {*build, &ev.BuildId}, {*spec, &ev.SpecId}, {*snapshot, &ev.SnapshotId}, {*code, &ev.Code}, {*path, &ev.Path}} {
				if f.v != "" {
					v := f.v
					*f.dst = &v
				}
			}
			if *revision != 0 {
				ev.SpecRevision = revision
			}
			if *line != 0 {
				ev.Line = line
			}
			body := apiclient.RequestEgressAccessJSONRequestBody{Justification: *why, Presets: append([]string{}, presets...), Destinations: append([]string{}, destinations...), Evidence: ev}
			if len(internal) > 0 {
				in := []string(internal)
				body.InternalDestinations = &in
			}
			if *method != "" {
				m := apiclient.EgressAccessRequestCreateMethod(*method)
				body.Method = &m
			}
			r, err := e.api.RequestEgressAccessWithResponse(ctx, id, nil, body)
			if err != nil {
				return err
			}
			if r.JSON202 == nil {
				return problem("requesting egress access", r.StatusCode(), r.Body)
			}
			return e.result(r.JSON202, r.JSON202.ApprovalId)
		},
	}, "request")
}

func (e *env) withdraw(ctx context.Context, args []string) error {
	fs := flags("withdraw", e)
	reason := fs.String("reason", "", "reason recorded in the audit trail")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori withdraw APPROVAL [--reason TEXT]")
	}
	body := apiclient.WithdrawApprovalJSONRequestBody{}
	if *reason != "" {
		body.Reason = reason
	}
	r, err := e.api.WithdrawApprovalWithResponse(ctx, pos[0], nil, body)
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("withdrawing the request", r.StatusCode(), r.Body)
	}
	return e.result(r.JSON200, string(r.JSON200.State))
}
