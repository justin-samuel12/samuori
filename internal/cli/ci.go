package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
)

type ciSummary struct {
	SnapshotID    string   `json:"snapshotId,omitempty"`
	BuildID       string   `json:"buildId,omitempty"`
	BuildState    string   `json:"buildState,omitempty"`
	ArtifactID    string   `json:"artifactId,omitempty"`
	PublicationID string   `json:"publicationId,omitempty"`
	Reference     string   `json:"reference,omitempty"`
	Files         []string `json:"deploymentFiles,omitempty"`
}

// ci uploads, builds and waits for the checks, then optionally publishes and saves deployment files. It succeeds only
// when every requested step did.
func (e *env) ci(ctx context.Context, args []string) error {
	fs := flags("ci", e)
	spec := fs.String("spec", "", "spec ID or name")
	project := fs.String("project", "", "project ID or name, to find the spec by name")
	revision := fs.Int("revision", 0, "spec revision (default: latest)")
	var excludes, tags multiFlag
	fs.Var(&excludes, "exclude", "additional exclusion pattern (repeatable)")
	clean := fs.Bool("clean", false, "build without cache")
	destination := fs.String("publish", "", "destination ID to publish the image to")
	fs.Var(&tags, "tag", "requested tag with --publish (repeatable)")
	deployDir := fs.String("deploy-files", "", "directory for a starting compose.yml and Helm chart")
	timeout := fs.Duration("timeout", time.Hour, "how long to wait for everything")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *spec == "" || len(pos) > 1 || (len(tags) > 0 && *destination == "") {
		return failf(ExitUsage, "usage: samuori ci --spec SPEC [--project PROJECT] [--publish DESTINATION [--tag T]...] [--deploy-files DIR] [PATH]")
	}
	source := "."
	if len(pos) == 1 {
		source = pos[0]
	}
	deadline := time.Now().Add(*timeout)
	var sum ciSummary
	err = func() error {
		b, err := e.startBuild(ctx, *spec, *project, *revision, "", source, excludes, *clean)
		if err != nil {
			return err
		}
		sum.SnapshotID, sum.BuildID = b.SnapshotId, b.Id
		if b, err = e.followBuild(ctx, b.Id, time.Until(deadline)); err != nil {
			return err
		}
		sum.BuildState = string(b.State)
		if err := buildOutcome(b); err != nil {
			return err
		}
		sum.ArtifactID = *b.ArtifactId
		if *destination != "" {
			body := apiclient.RequestPublicationJSONRequestBody{Kind: "publish", ArtifactId: b.ArtifactId, DestinationId: *destination}
			p, err := e.requestPublication(ctx, body, tags, true, time.Until(deadline))
			if p != nil {
				sum.PublicationID = p.Id
				if p.DestinationRef != nil {
					sum.Reference = *p.DestinationRef
				}
			}
			if err != nil {
				return err
			}
		}
		if *deployDir != "" {
			if err := os.MkdirAll(*deployDir, 0o755); err != nil {
				return err
			}
			for _, kind := range []string{"compose", "helm"} {
				path, err := e.saveDeployFile(ctx, kind, sum.ArtifactID, "", sum.PublicationID, *deployDir)
				if err != nil {
					return err
				}
				sum.Files = append(sum.Files, path)
			}
		}
		return nil
	}()
	if e.json {
		_ = e.printJSON(sum)
	} else {
		for _, kv := range [][2]string{{"snapshot", sum.SnapshotID}, {"build", sum.BuildID}, {"image", sum.ArtifactID}, {"publication", sum.PublicationID}, {"reference", sum.Reference}} {
			if kv[1] != "" {
				fmt.Fprintf(e.out, "%-12s %s\n", kv[0], kv[1])
			}
		}
		for _, f := range sum.Files {
			fmt.Fprintf(e.out, "%-12s %s\n", "file", f)
		}
	}
	return err
}
