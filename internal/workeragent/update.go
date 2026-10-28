package workeragent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"samuori.invalid/samuori/internal/updatestage"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

func (a *Agent) updateDir() string {
	if a.cfg.UpdateDir != "" {
		return a.cfg.UpdateDir
	}
	return updatestage.Dir
}

// update fetches the release a rollout asked for and stages it; the host's samuori-self-update verifies the signature
// and installs it, which restarts this agent (ADR 0044).
func (a *Agent) update(ctx context.Context, client workerv1.WorkerServiceClient, version string) {
	a.log.Info("self-update requested", "version", version)
	fail := func(err error) {
		a.log.Error("self-update", "version", version, "error", err)
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = client.ReportUpdate(rctx, &workerv1.UpdateReport{Version: version, Outcome: "failed", Detail: err.Error()})
	}
	stream, err := client.GetRelease(ctx, &workerv1.GetReleaseRequest{Version: version, Arch: runtime.GOARCH})
	if err != nil {
		fail(err)
		return
	}
	var sums, sig []byte
	for sums == nil || sig == nil {
		c, err := stream.Recv()
		if err != nil {
			fail(err)
			return
		}
		if v := c.GetChecksums(); v != nil {
			sums = v
		}
		if v := c.GetSignature(); v != nil {
			sig = v
		}
	}
	if err := updatestage.Write(a.updateDir(), version, &releaseReader{stream: stream}, sums, sig); err != nil {
		fail(err)
	}
}

// rollback asks the host's samuori-self-update to reinstall the release kept before the last update, after this
// release failed its self-test.
func (a *Agent) rollback(ctx context.Context, client workerv1.WorkerServiceClient) {
	a.log.Warn("rollback requested")
	if err := updatestage.WriteRollback(a.updateDir()); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = client.ReportUpdate(rctx, &workerv1.UpdateReport{Version: updatestage.Rollback, Outcome: "failed", Detail: err.Error()})
	}
}

type releaseReader struct {
	stream interface {
		Recv() (*workerv1.ReleaseChunk, error)
	}
	buf []byte
}

func (r *releaseReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		c, err := r.stream.Recv()
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		r.buf = c.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// reportUpdate tells the controller how the last staged release ended on this host.
func (a *Agent) reportUpdate(ctx context.Context, client workerv1.WorkerServiceClient) {
	res, ok := updatestage.TakeResult(a.updateDir())
	if !ok {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := client.ReportUpdate(rctx, &workerv1.UpdateReport{Version: res.Version, Outcome: res.Outcome, Detail: res.Detail}); err != nil {
		a.log.Warn("self-update result not reported", "error", err)
	}
}

// remove asks the host's samuori-wipe to delete Samuori, then confirms; the controller revokes this worker on the reply.
func (a *Agent) remove(ctx context.Context, client workerv1.WorkerServiceClient) {
	report := &workerv1.RemovalReport{Staged: true}
	if err := os.WriteFile(filepath.Join(a.updateDir(), "wipe"), []byte("removed by an administrator\n"), 0o640); err != nil {
		report = &workerv1.RemovalReport{Detail: err.Error()}
	}
	a.log.Warn("removal requested; the host is removing Samuori", "staged", report.GetStaged())
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := client.ReportRemoval(rctx, report); err != nil {
		a.log.Warn("removal not confirmed to the controller", "error", err)
	}
}
