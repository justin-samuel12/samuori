package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
)

var buildEnd = []apiclient.BuildRunState{"succeeded", "blocked", "failed", "timed_out", "cancelled", "expired", "discarded"}

var validationEnd = []apiclient.ValidationSummaryState{"completed", "cancelled", "timed_out", "errored"}

func (e *env) build(ctx context.Context, args []string) error {
	fs := flags("build", e)
	spec := fs.String("spec", "", "spec ID or name")
	project := fs.String("project", "", "project ID or name, to find the spec by name")
	revision := fs.Int("revision", 0, "spec revision (default: latest)")
	snapshot := fs.String("snapshot", "", "snapshot ID")
	source := fs.String("source", "", "directory or archive to upload first, instead of --snapshot")
	clean := fs.Bool("clean", false, "build without cache")
	wait := fs.Bool("wait", false, "wait for the build and its checks")
	timeout := fs.Duration("timeout", time.Hour, "how long --wait waits")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *spec == "" || (*snapshot == "") == (*source == "") {
		return failf(ExitUsage, "usage: samuori build --spec SPEC (--snapshot ID | --source PATH) [--revision N] [--wait]")
	}
	b, err := e.startBuild(ctx, *spec, *project, *revision, *snapshot, *source, nil, *clean)
	if err != nil {
		return err
	}
	if !*wait {
		return e.result(b, b.Id)
	}
	_, err = e.waitBuild(ctx, b.Id, *timeout)
	return err
}

// startBuild uploads source when given, then starts a build of the spec revision (latest when revision is 0).
func (e *env) startBuild(ctx context.Context, specRef, project string, revision int, snapshot, source string, excludes []string, clean bool) (*apiclient.BuildRun, error) {
	sp, err := e.specRef(ctx, specRef, project)
	if err != nil {
		return nil, err
	}
	if revision == 0 {
		revision = sp.LatestRevision
	}
	if source != "" {
		if snapshot, err = e.uploadPath(ctx, sp.ProjectId, source, excludes); err != nil {
			return nil, err
		}
		e.note("uploaded snapshot %s", snapshot)
	}
	body := apiclient.CreateBuildJSONRequestBody{SpecId: sp.Id, SpecRevision: revision, SnapshotId: snapshot}
	if clean {
		body.CleanBuild = &clean
	}
	r, err := e.api.CreateBuildWithResponse(ctx, &apiclient.CreateBuildParams{IdempotencyKey: newKey("cli-build")}, body)
	if err != nil {
		return nil, err
	}
	if r.JSON201 == nil {
		return nil, problem("starting the build", r.StatusCode(), r.Body)
	}
	e.note("build %s: %s", r.JSON201.Id, r.JSON201.State)
	return r.JSON201, nil
}

func (e *env) result(v any, id string) error {
	if e.json {
		return e.printJSON(v)
	}
	fmt.Fprintln(e.out, id)
	return nil
}

// waitBuild succeeds only when the build succeeded and its checks passed.
func (e *env) waitBuild(ctx context.Context, id string, timeout time.Duration) (*apiclient.BuildRun, error) {
	b, err := e.followBuild(ctx, id, timeout)
	if err != nil {
		return nil, err
	}
	_ = e.result(b, id)
	return b, buildOutcome(b)
}

func buildOutcome(b *apiclient.BuildRun) error {
	if b.State != "succeeded" {
		reason := string(b.State)
		if b.BlockedReason != nil && *b.BlockedReason != "" {
			reason += ": " + *b.BlockedReason
		}
		return failf(ExitFailed, "build %s %s", b.Id, reason)
	}
	verdict := "none"
	if b.Validation.Verdict != nil {
		verdict = string(*b.Validation.Verdict)
	}
	if verdict != "passed" {
		return failf(ExitFailed, "build %s succeeded but its checks ended %s (verdict %s)", b.Id, b.Validation.State, verdict)
	}
	return nil
}

// followBuild polls until the build ends and, when it succeeded, its checks end.
func (e *env) followBuild(ctx context.Context, id string, timeout time.Duration) (*apiclient.BuildRun, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		r, err := e.api.GetBuildWithResponse(ctx, id)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, failf(ExitTimeout, "build %s did not finish within %s", id, timeout)
		}
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, problem("reading the build", r.StatusCode(), r.Body)
		}
		b := r.JSON200
		now := string(b.State)
		if b.Validation != nil {
			now += ", checks " + string(b.Validation.State)
		}
		if now != last {
			e.note("build %s: %s", id, now)
			last = now
		}
		if slices.Contains(buildEnd, b.State) && (b.State != "succeeded" || b.Validation != nil && slices.Contains(validationEnd, b.Validation.State)) {
			return b, nil
		}
		if err := sleep(ctx, e.poll); err != nil {
			return nil, failf(ExitTimeout, "build %s did not finish within %s", id, timeout)
		}
	}
}

func (e *env) builds(ctx context.Context, args []string) error {
	return e.subcommand(args, "list", map[string]func([]string) error{"list": func(args []string) error {
		fs := flags("builds", e)
		project := fs.String("project", "", "project ID or name")
		state := fs.String("state", "", "only builds in this state")
		limit := fs.Int("limit", 50, "how many builds, newest first (at most 200)")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		params := &apiclient.ListBuildsParams{Limit: limit}
		if *project != "" {
			id, err := e.projectID(ctx, *project)
			if err != nil {
				return err
			}
			params.ProjectId = &id
		}
		if *state != "" {
			s := apiclient.BuildRunState(*state)
			params.State = &s
		}
		r, err := e.api.ListBuildsWithResponse(ctx, params)
		if err != nil {
			return err
		}
		if r.JSON200 == nil {
			return problem("listing builds", r.StatusCode(), r.Body)
		}
		var rows [][]string
		for _, b := range r.JSON200.Items {
			checks := "-"
			if b.Validation != nil {
				checks = string(b.Validation.State)
				if b.Validation.Verdict != nil {
					checks = string(*b.Validation.Verdict)
				}
			}
			rows = append(rows, []string{b.Id, b.ProjectId, b.SpecId + "@" + strconv.Itoa(b.SpecRevision), string(b.State), checks, when(b.CreatedAt)})
		}
		return e.table(r.JSON200.Items, []string{"ID", "PROJECT", "SPEC@REVISION", "STATE", "CHECKS", "CREATED"}, rows)
	}}, "builds")
}

func (e *env) status(ctx context.Context, args []string) error {
	pos, err := parse(flags("status", e), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori status BUILD")
	}
	r, err := e.api.GetBuildWithResponse(ctx, pos[0])
	if err != nil {
		return err
	}
	if r.JSON200 == nil {
		return problem("reading the build", r.StatusCode(), r.Body)
	}
	return e.printJSON(r.JSON200)
}

func (e *env) logs(ctx context.Context, args []string) error {
	fs := flags("logs", e)
	follow := fs.Bool("follow", false, "keep printing new output until the build ends")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori logs BUILD [--follow]")
	}
	var printed []byte
	for {
		r, err := e.api.GetBuildLogsWithResponse(ctx, pos[0])
		if err != nil {
			return err
		}
		if r.StatusCode() != http.StatusOK {
			return problem("reading the log", r.StatusCode(), r.Body)
		}
		// The live log is replaced by the stored, masked log when an attempt ends.
		if bytes.HasPrefix(r.Body, printed) {
			_, err = e.out.Write(r.Body[len(printed):])
		} else {
			e.note("--- the attempt ended; its stored log follows ---")
			_, err = e.out.Write(r.Body)
		}
		if err != nil {
			return err
		}
		printed = r.Body
		if !*follow {
			return nil
		}
		b, err := e.api.GetBuildWithResponse(ctx, pos[0])
		if err != nil {
			return err
		}
		if b.JSON200 == nil {
			return problem("reading the build", b.StatusCode(), b.Body)
		}
		if slices.Contains(buildEnd, b.JSON200.State) && r.HTTPResponse.Header.Get("X-Samuori-Log-Live") == "" {
			return nil
		}
		if err := sleep(ctx, e.poll); err != nil {
			return err
		}
	}
}

func (e *env) cancel(ctx context.Context, args []string) error {
	pos, err := parse(flags("cancel", e), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori cancel BUILD")
	}
	r, err := e.api.CancelBuildWithResponse(ctx, pos[0], &apiclient.CancelBuildParams{IdempotencyKey: newKey("cli-cancel")})
	if err != nil {
		return err
	}
	if r.JSON202 == nil {
		return problem("cancelling the build", r.StatusCode(), r.Body)
	}
	return e.result(r.JSON202, string(r.JSON202.State))
}

var publicationEnd = map[apiclient.PublicationState]bool{"published": true, "retired": true, "rejected": true, "expired": true, "blocked": true, "failed": true, "cancelled": true}

func (e *env) publish(ctx context.Context, args []string) error {
	fs := flags("publish", e)
	artifact := fs.String("artifact", "", "artifact ID")
	build := fs.String("build", "", "build ID, instead of --artifact")
	destination := fs.String("destination", "", "destination ID")
	var tags multiFlag
	fs.Var(&tags, "tag", "requested tag (repeatable)")
	wait := fs.Bool("wait", false, "wait until published or refused")
	timeout := fs.Duration("timeout", time.Hour, "how long --wait waits")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *destination == "" || (*artifact == "") == (*build == "") {
		return failf(ExitUsage, "usage: samuori publish (--artifact ID | --build ID) --destination ID [--tag T]... [--wait]")
	}
	if *build != "" {
		id, err := e.artifactOf(ctx, *build)
		if err != nil {
			return err
		}
		*artifact = id
	}
	body := apiclient.RequestPublicationJSONRequestBody{Kind: "publish", ArtifactId: artifact, DestinationId: *destination}
	return e.printPublication(e.requestPublication(ctx, body, tags, *wait, *timeout))
}

func (e *env) printPublication(p *apiclient.Publication, err error) error {
	if p != nil {
		_ = e.result(p, p.Id)
		if err == nil && p.State == "published" && p.DestinationRef != nil && !e.json {
			fmt.Fprintln(e.out, *p.DestinationRef)
		}
	}
	return err
}

func (e *env) promote(ctx context.Context, args []string) error {
	fs := flags("promote", e)
	source := fs.String("publication", "", "published publication to promote")
	destination := fs.String("destination", "", "destination ID")
	var tags multiFlag
	fs.Var(&tags, "tag", "requested tag (repeatable)")
	wait := fs.Bool("wait", false, "wait until published or refused")
	timeout := fs.Duration("timeout", time.Hour, "how long --wait waits")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *destination == "" || *source == "" {
		return failf(ExitUsage, "usage: samuori promote --publication ID --destination ID [--tag T]... [--wait]")
	}
	body := apiclient.RequestPublicationJSONRequestBody{Kind: "promote", SourcePublicationId: source, DestinationId: *destination}
	return e.printPublication(e.requestPublication(ctx, body, tags, *wait, *timeout))
}

func (e *env) artifactOf(ctx context.Context, build string) (string, error) {
	b, err := e.api.GetBuildWithResponse(ctx, build)
	if err != nil {
		return "", err
	}
	if b.JSON200 == nil {
		return "", problem("reading the build", b.StatusCode(), b.Body)
	}
	if b.JSON200.ArtifactId == nil {
		return "", failf(ExitFailed, "build %s has no image (state %s)", build, b.JSON200.State)
	}
	return *b.JSON200.ArtifactId, nil
}

// requestPublication succeeds with wait only when the image is published; it keeps waiting while approval is pending.
// The publication is returned whenever the server answered, also with a failure.
func (e *env) requestPublication(ctx context.Context, body apiclient.RequestPublicationJSONRequestBody, tags []string, wait bool, timeout time.Duration) (*apiclient.Publication, error) {
	if len(tags) > 0 {
		body.Tags = &tags
	}
	r, err := e.api.RequestPublicationWithResponse(ctx, &apiclient.RequestPublicationParams{IdempotencyKey: newKey("cli-publish")}, body)
	if err != nil {
		return nil, err
	}
	if r.JSON202 == nil {
		return nil, problem("requesting the publication", r.StatusCode(), r.Body)
	}
	id := r.JSON202.Id
	if !wait {
		return r.JSON202, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		p, err := e.api.GetPublicationWithResponse(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, failf(ExitTimeout, "publication %s did not finish within %s", id, timeout)
			}
			return nil, err
		}
		if p.JSON200 == nil {
			return nil, problem("reading the publication", p.StatusCode(), p.Body)
		}
		state := p.JSON200.State
		if string(state) != last {
			e.note("publication %s: %s", id, state)
			last = string(state)
		}
		if publicationEnd[state] {
			if state != "published" {
				reason := ""
				if p.JSON200.Reason != nil {
					reason = ": " + *p.JSON200.Reason
				}
				return p.JSON200, failf(ExitFailed, "publication %s %s%s", id, state, reason)
			}
			return p.JSON200, nil
		}
		if err := sleep(ctx, e.poll); err != nil {
			return nil, failf(ExitTimeout, "publication %s did not finish within %s", id, timeout)
		}
	}
}

func (e *env) pipeline(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return failf(ExitUsage, "usage: samuori pipeline run PIPELINE --ref REF [--wait]")
	}
	fs := flags("pipeline run", e)
	ref := fs.String("ref", "", "full ref such as refs/heads/main")
	wait := fs.Bool("wait", false, "wait for the run, then its build and checks")
	timeout := fs.Duration("timeout", time.Hour, "how long --wait waits")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 || *ref == "" {
		return failf(ExitUsage, "usage: samuori pipeline run PIPELINE --ref REF [--wait]")
	}
	if !strings.HasPrefix(*ref, "refs/") {
		*ref = "refs/heads/" + *ref
	}
	id := pos[0]
	r, err := e.api.StartPipelineRunWithResponse(ctx, id, &apiclient.StartPipelineRunParams{IdempotencyKey: newKey("cli-run")}, apiclient.StartPipelineRunJSONRequestBody{Ref: *ref})
	if err != nil {
		return err
	}
	if r.JSON202 == nil {
		return problem("starting the run", r.StatusCode(), r.Body)
	}
	run := r.JSON202
	if !*wait {
		return e.result(run, run.Id)
	}
	deadline := time.Now().Add(*timeout)
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	limit := 50
	for run.State == "queued" || run.State == "fetching" {
		if err := sleep(wctx, e.poll); err != nil {
			return failf(ExitTimeout, "run %s did not start a build within %s", run.Id, *timeout)
		}
		list, err := e.api.ListPipelineRunsWithResponse(wctx, id, &apiclient.ListPipelineRunsParams{Limit: &limit})
		if err != nil {
			return err
		}
		if list.JSON200 == nil {
			return problem("reading the run", list.StatusCode(), list.Body)
		}
		for _, x := range list.JSON200.Items {
			if x.Id == run.Id {
				run = &x
			}
		}
	}
	e.note("run %s: %s", run.Id, run.State)
	if run.State != "started" || run.BuildRunId == nil {
		msg := string(run.State)
		if run.Message != nil {
			msg += ": " + *run.Message
		}
		_ = e.result(run, run.Id)
		return failf(ExitFailed, "run %s %s", run.Id, msg)
	}
	_, err = e.waitBuild(ctx, *run.BuildRunId, time.Until(deadline))
	return err
}

func (e *env) pipelines(ctx context.Context, args []string) error {
	return e.subcommand(args, "list", map[string]func([]string) error{"list": func(args []string) error {
		fs := flags("pipelines", e)
		project := fs.String("project", "", "project ID or name")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		limit := 200
		params := &apiclient.ListPipelinesParams{Limit: &limit}
		if *project != "" {
			id, err := e.projectID(ctx, *project)
			if err != nil {
				return err
			}
			params.ProjectId = &id
		}
		var items []apiclient.Pipeline
		for {
			r, err := e.api.ListPipelinesWithResponse(ctx, params)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return problem("listing pipelines", r.StatusCode(), r.Body)
			}
			items = append(items, r.JSON200.Items...)
			if r.JSON200.NextCursor == nil || *r.JSON200.NextCursor == "" {
				break
			}
			params.Cursor = r.JSON200.NextCursor
		}
		var rows [][]string
		for _, p := range items {
			state := "enabled"
			if !p.Enabled {
				state = "disabled"
			}
			rows = append(rows, []string{p.Id, p.Name, p.RepoUrl, strings.Join(p.Refs, ","), state})
		}
		return e.table(items, []string{"ID", "NAME", "REPOSITORY", "REFS", "STATE"}, rows)
	}}, "pipelines")
}
