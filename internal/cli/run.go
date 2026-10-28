package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
)

var buildEnd = []apiclient.BuildRunState{"succeeded", "blocked", "failed", "timed_out", "cancelled", "expired", "discarded"}

var validationEnd = []apiclient.ValidationSummaryState{"completed", "cancelled", "timed_out", "errored"}

func (e *env) build(ctx context.Context, args []string) error {
	fs := flags("build", e)
	spec := fs.String("spec", "", "spec ID")
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
		return failf(ExitUsage, "usage: samuori build --spec ID (--snapshot ID | --source PATH) [--revision N] [--wait]")
	}
	if *revision == 0 || *source != "" {
		sp, err := e.api.GetSpecWithResponse(ctx, *spec)
		if err != nil {
			return err
		}
		if sp.JSON200 == nil {
			return problem("reading the spec", sp.StatusCode(), sp.Body)
		}
		if *revision == 0 {
			*revision = sp.JSON200.LatestRevision
		}
		if *source != "" {
			id, err := e.uploadPath(ctx, sp.JSON200.ProjectId, *source, nil)
			if err != nil {
				return err
			}
			e.note("uploaded snapshot %s", id)
			*snapshot = id
		}
	}
	body := apiclient.CreateBuildJSONRequestBody{SpecId: *spec, SpecRevision: *revision, SnapshotId: *snapshot}
	if *clean {
		body.CleanBuild = clean
	}
	r, err := e.api.CreateBuildWithResponse(ctx, &apiclient.CreateBuildParams{IdempotencyKey: newKey("cli-build")}, body)
	if err != nil {
		return err
	}
	if r.JSON201 == nil {
		return problem("starting the build", r.StatusCode(), r.Body)
	}
	if !*wait {
		return e.result(r.JSON201, r.JSON201.Id)
	}
	e.note("build %s: %s", r.JSON201.Id, r.JSON201.State)
	return e.waitBuild(ctx, r.JSON201.Id, *timeout)
}

func (e *env) result(v any, id string) error {
	if e.json {
		return e.printJSON(v)
	}
	fmt.Fprintln(e.out, id)
	return nil
}

// waitBuild succeeds only when the build succeeded and its checks passed.
func (e *env) waitBuild(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		r, err := e.api.GetBuildWithResponse(ctx, id)
		if errors.Is(err, context.DeadlineExceeded) {
			return failf(ExitTimeout, "build %s did not finish within %s", id, timeout)
		}
		if err != nil {
			return err
		}
		if r.JSON200 == nil {
			return problem("reading the build", r.StatusCode(), r.Body)
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
		if slices.Contains(buildEnd, b.State) {
			if b.State != "succeeded" {
				reason := string(b.State)
				if b.BlockedReason != nil && *b.BlockedReason != "" {
					reason += ": " + *b.BlockedReason
				}
				_ = e.result(b, id)
				return failf(ExitFailed, "build %s %s", id, reason)
			}
			if b.Validation != nil && slices.Contains(validationEnd, b.Validation.State) {
				verdict := "none"
				if b.Validation.Verdict != nil {
					verdict = string(*b.Validation.Verdict)
				}
				_ = e.result(b, id)
				if verdict != "passed" {
					return failf(ExitFailed, "build %s succeeded but its checks ended %s (verdict %s)", id, b.Validation.State, verdict)
				}
				return nil
			}
		}
		if err := sleep(ctx, e.poll); err != nil {
			return failf(ExitTimeout, "build %s did not finish within %s", id, timeout)
		}
	}
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
		b, err := e.api.GetBuildWithResponse(ctx, *build)
		if err != nil {
			return err
		}
		if b.JSON200 == nil {
			return problem("reading the build", b.StatusCode(), b.Body)
		}
		if b.JSON200.ArtifactId == nil {
			return failf(ExitFailed, "build %s has no image (state %s)", *build, b.JSON200.State)
		}
		*artifact = *b.JSON200.ArtifactId
	}
	body := apiclient.RequestPublicationJSONRequestBody{Kind: "publish", ArtifactId: artifact, DestinationId: *destination}
	if len(tags) > 0 {
		t := []string(tags)
		body.Tags = &t
	}
	r, err := e.api.RequestPublicationWithResponse(ctx, &apiclient.RequestPublicationParams{IdempotencyKey: newKey("cli-publish")}, body)
	if err != nil {
		return err
	}
	if r.JSON202 == nil {
		return problem("requesting the publication", r.StatusCode(), r.Body)
	}
	id := r.JSON202.Id
	if !*wait {
		return e.result(r.JSON202, id)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	last := ""
	for {
		p, err := e.api.GetPublicationWithResponse(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return failf(ExitTimeout, "publication %s did not finish within %s", id, *timeout)
			}
			return err
		}
		if p.JSON200 == nil {
			return problem("reading the publication", p.StatusCode(), p.Body)
		}
		state := p.JSON200.State
		if string(state) != last {
			e.note("publication %s: %s", id, state)
			last = string(state)
		}
		if publicationEnd[state] {
			_ = e.result(p.JSON200, id)
			if state != "published" {
				reason := ""
				if p.JSON200.Reason != nil {
					reason = ": " + *p.JSON200.Reason
				}
				return failf(ExitFailed, "publication %s %s%s", id, state, reason)
			}
			if p.JSON200.DestinationRef != nil && !e.json {
				fmt.Fprintln(e.out, *p.JSON200.DestinationRef)
			}
			return nil
		}
		if err := sleep(ctx, e.poll); err != nil {
			return failf(ExitTimeout, "publication %s did not finish within %s", id, *timeout)
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
	return e.waitBuild(ctx, *run.BuildRunId, time.Until(deadline))
}
