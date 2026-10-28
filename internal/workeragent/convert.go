package workeragent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/toolrun"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

func (a *Agent) providersDir() string {
	if a.cfg.ProvidersDir != "" {
		return a.cfg.ProvidersDir
	}
	return convertjob.ProvidersDir
}

var versionFile = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// InstalledProviders lists the tools under dir as <name>/<name> with a <name>/VERSION file. Installing a tool does
// not approve it: the controller sends a job only for the approved version and checksum, checked again before the
// tool runs.
func InstalledProviders(dir string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !convertjob.ValidName(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name, "VERSION"))
		v := strings.TrimSpace(string(raw))
		if err != nil || !versionFile.MatchString(v) {
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, name, name)); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	errToolMismatch = errors.New("the installed tool does not match the approved checksum")
	errToolVersion  = errors.New("the installed tool is not the approved version")
)

// stageTool copies the provider's tool into dst/tool while hashing the copy, so the bytes that run are the bytes
// that were checked against the approved SHA-256.
func stageTool(dir string, job convertjob.Job, arch, dst string) (string, error) {
	want := job.ToolSHA256[arch]
	if want == "" {
		return "", fmt.Errorf("no approved checksum for %s", arch)
	}
	raw, err := os.ReadFile(filepath.Join(dir, job.Provider, "VERSION"))
	if err != nil || strings.TrimSpace(string(raw)) != job.ProviderVersion {
		return "", errToolVersion
	}
	in, err := os.Open(filepath.Join(dir, job.Provider, job.Provider))
	if err != nil {
		return "", err
	}
	defer in.Close()
	if info, err := in.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("the installed tool is not a regular file")
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", err
	}
	out, err := os.OpenFile(filepath.Join(dst, "tool"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != want {
		_ = os.Remove(filepath.Join(dst, "tool"))
		return got, errToolMismatch
	}
	return got, nil
}

func (a *Agent) convertAssigned(ctx context.Context, client workerv1.WorkerServiceClient, _ func(*workerv1.WorkerMessage) error, as *workerv1.ConversionAssignment) {
	id := as.GetLease().GetAttemptId()
	refuse := func(detail string) {
		go reportConversion(a, client, &workerv1.ConversionReport{Lease: as.GetLease(), State: workerv1.ConversionState_CONVERSION_STATE_FAILED, FailureClass: "infrastructure", Detail: detail,
			Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED})
	}
	if a.manager == nil {
		refuse("this worker has no isolation provider")
		return
	}
	a.mu.Lock()
	if _, dup := a.leases[id]; dup {
		a.mu.Unlock()
		return
	}
	if len(a.leases) >= a.slots || a.manager.Quarantined() != "" || a.selfTesting {
		a.mu.Unlock()
		refuse("every daemon slot is busy")
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.leases[id] = running{lease: as.GetLease(), cancel: cancel, conversion: true}
	a.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			a.mu.Lock()
			delete(a.leases, id)
			a.mu.Unlock()
		}()
		a.convert(runCtx, client, as)
	}()
}

func reportConversion(a *Agent, client workerv1.WorkerServiceClient, r *workerv1.ConversionReport) {
	deadline := time.Now().Add(reportWindow)
	for {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ack, err := client.ReportConversion(rctx, r)
		cancel()
		if err == nil {
			if !ack.GetAccepted() {
				a.log.Warn("conversion report not accepted", "attempt", r.GetLease().GetAttemptId(), "rejection", ack.GetRejection())
			}
			return
		}
		switch status.Code(err) {
		case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
			if time.Now().Before(deadline) {
				time.Sleep(time.Second)
				continue
			}
		}
		a.log.Warn("conversion report failed", "attempt", r.GetLease().GetAttemptId(), "error", err)
		return
	}
}

// convert runs one conversion: the job must match the digest bound to the lease, the tool must match its approved
// checksum, and the input must match the job's digest before the isolation provider runs anything. The environment
// gets no secrets, no mirror tokens and no network.
func (a *Agent) convert(ctx context.Context, client workerv1.WorkerServiceClient, as *workerv1.ConversionAssignment) {
	lease := as.GetLease()
	fail := func(class, detail string, cleanup workerv1.CleanupOutcome) {
		reportConversion(a, client, &workerv1.ConversionReport{Lease: lease, State: workerv1.ConversionState_CONVERSION_STATE_FAILED, FailureClass: class, Detail: detail, Cleanup: cleanup})
	}
	destroyed := workerv1.CleanupOutcome_CLEANUP_OUTCOME_DESTROYED
	resp, err := client.GetConversionJob(ctx, &workerv1.GetConversionJobRequest{Lease: lease})
	if err != nil {
		a.log.Warn("conversion job refused", "attempt", lease.GetAttemptId(), "error", err)
		return
	}
	var job convertjob.Job
	if resp.GetJobDigest() != as.GetJobDigest() || convertjob.Digest(resp.GetJobJson()) != as.GetJobDigest() || json.Unmarshal(resp.GetJobJson(), &job) != nil {
		fail("infrastructure", "the job does not match the digest bound to the lease", destroyed)
		return
	}
	if err := job.Validate(); err != nil {
		fail("infrastructure", "invalid job: "+err.Error(), destroyed)
		return
	}
	reportConversion(a, client, &workerv1.ConversionReport{Lease: lease, State: workerv1.ConversionState_CONVERSION_STATE_RUNNING, Cleanup: workerv1.CleanupOutcome_CLEANUP_OUTCOME_PENDING})
	work, err := os.MkdirTemp(a.cfg.StateDir, "conversion-")
	if err != nil {
		fail("infrastructure", "could not create a work directory", destroyed)
		return
	}
	defer os.RemoveAll(work)
	toolDir, inputDir, outputDir := filepath.Join(work, "tool"), filepath.Join(work, "input"), filepath.Join(work, "output")
	digest, err := stageTool(a.providersDir(), job, runtime.GOARCH, toolDir)
	switch {
	case errors.Is(err, errToolMismatch):
		fail("policy", fmt.Sprintf("%s: %s is %s, the approved checksum is %s; the tool was not run", errToolMismatch, job.Provider, digest, job.ToolSHA256[runtime.GOARCH]), destroyed)
		return
	case err != nil:
		fail("policy", job.Provider+" "+job.ProviderVersion+" cannot run here: "+err.Error(), destroyed)
		return
	}
	stream, err := client.GetConversionInput(ctx, &workerv1.GetConversionInputRequest{Lease: lease})
	if err == nil {
		err = receiveFiles(stream, job.InputDigest, inputDir)
	}
	if err == nil {
		err = contextModes(inputDir)
	}
	if err != nil {
		fail("infrastructure", "input fetch failed: "+err.Error(), destroyed)
		return
	}
	run, _ := json.Marshal(toolrun.Spec{Args: append([]string{"/.samuori-tool/tool"}, job.Args...), Mkdirs: job.Mkdirs, Files: job.Files, Keep: job.Keep, TimeoutSeconds: job.TimeoutSeconds})
	lim := as.GetLimits()
	spec := isolation.Spec{AttemptID: lease.GetAttemptId(), Profile: isolation.Profile(as.GetIsolationProfile()), BaseImage: a.cfg.EnvironmentImage,
		Limits: isolation.Limits{VCPUs: lim.GetVcpus(), MemoryBytes: lim.GetMemoryBytes(), DiskBytes: lim.GetDiskBytes(), WallClock: lim.GetWallClock().AsDuration()}}
	_, cleanup, err := a.manager.Convert(ctx, spec, isolation.ConvertSpec{ToolDir: toolDir, InputDir: inputDir, OutputDir: outputDir, Run: run})
	outcome := destroyed
	if cleanup.Outcome == isolation.CleanupQuarantined {
		outcome = workerv1.CleanupOutcome_CLEANUP_OUTCOME_FAILED_QUARANTINED
	}
	switch {
	case errors.Is(err, isolation.ErrTimeout):
		fail("timeout", err.Error(), outcome)
		return
	case err != nil && isolation.Requeue(err):
		fail("infrastructure", err.Error(), outcome)
		return
	case err != nil:
		fail("application", err.Error(), outcome)
		return
	}
	res, stdout, stderr, err := toolrun.ReadMeta(outputDir)
	if err != nil {
		fail("infrastructure", err.Error(), outcome)
		return
	}
	files, err := collectOutput(outputDir)
	if err != nil {
		fail("application", err.Error(), outcome)
		return
	}
	if res.Error != "" && !res.TimedOut {
		fail("infrastructure", "the tool runner reported: "+res.Error, outcome)
		return
	}
	reportConversion(a, client, &workerv1.ConversionReport{Lease: lease, State: workerv1.ConversionState_CONVERSION_STATE_COMPLETED, Files: files, ToolStdout: stdout, ToolStderr: stderr,
		ExitCode: int32(res.ExitCode), TimedOut: res.TimedOut, Cleanup: outcome, ToolDigest: digest})
}

// contextModes gives verified source the modes of an ordinary checkout (0644, 0755 for directories and executables),
// so a build context or conversion input is readable by the image or tool user; the 0700 attempt directory keeps it
// private on the host.
func contextModes(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if d.IsDir() {
			mode = 0o755
		} else if info, err := d.Info(); err == nil && info.Mode().Perm()&0o100 != 0 {
			mode = 0o755
		}
		return os.Chmod(p, mode)
	})
}

// collectOutput returns the regular files the step exported, by path relative to its /out, within the report limits;
// symbolic links and other special files are refused.
func collectOutput(dir string) ([]*workerv1.ConversionFile, error) {
	var out []*workerv1.ConversionFile
	total := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == convertjob.MetaDir && d.IsDir() {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("the tool wrote %s, which is not a regular file", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if len(out) >= convertjob.MaxOutputFiles || info.Size() > convertjob.MaxFileBytes || total+int(info.Size()) > convertjob.MaxOutputBytes {
			return errors.New("the tool's output exceeds the conversion limits")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += len(b)
		out = append(out, &workerv1.ConversionFile{Path: rel, Content: b})
		return nil
	})
	return out, err
}
