package workeragent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/toolrun"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// convertProvider records what the agent hands the isolation provider and writes what a conversion step exports.
type convertProvider struct {
	stubProvider
	mu        sync.Mutex
	specs     []isolation.Spec
	runs      []isolation.ConvertSpec
	tool      []byte
	runSpec   toolrun.Spec
	inputMode os.FileMode
}

func (p *convertProvider) Capabilities(context.Context) (isolation.Capabilities, error) {
	return isolation.Capabilities{Profiles: []isolation.Profile{isolation.RootlessDedicatedVM}, FreeSlots: 1}, nil
}
func (p *convertProvider) ConvertReady() error { return nil }
func (p *convertProvider) Prepare(_ context.Context, s isolation.Spec) (isolation.Env, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.specs = append(p.specs, s)
	return isolation.Env{ID: s.AttemptID, AttemptID: s.AttemptID, Profile: s.Profile}, nil
}
func (p *convertProvider) Destroy(context.Context, isolation.Env) error { return nil }
func (p *convertProvider) Convert(_ context.Context, _ isolation.Env, s isolation.ConvertSpec) (isolation.ConvertResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = append(p.runs, s)
	p.tool, _ = os.ReadFile(filepath.Join(s.ToolDir, "tool"))
	if info, err := os.Stat(filepath.Join(s.InputDir, "compose.yml")); err == nil {
		p.inputMode = info.Mode().Perm()
	}
	_ = json.Unmarshal(s.Run, &p.runSpec)
	out := filepath.Join(s.OutputDir, "manifests")
	_ = os.MkdirAll(filepath.Join(s.OutputDir, convertjob.MetaDir), 0o755)
	_ = os.MkdirAll(out, 0o755)
	_ = os.WriteFile(filepath.Join(out, "api-deployment.yaml"), []byte("kind: Deployment\n"), 0o644)
	_ = os.WriteFile(filepath.Join(s.OutputDir, convertjob.MetaDir, "result.json"), []byte(`{"exitCode":0,"timedOut":false,"durationMs":5}`), 0o644)
	_ = os.WriteFile(filepath.Join(s.OutputDir, convertjob.MetaDir, "stderr"), []byte("WARN something\n"), 0o644)
	return isolation.ConvertResult{}, nil
}

type fakeInput struct {
	grpc.ClientStream
	chunks []*workerv1.SourceChunk
}

func (f *fakeInput) Recv() (*workerv1.SourceChunk, error) {
	if len(f.chunks) == 0 {
		return nil, io.EOF
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return c, nil
}

type convertClient struct {
	workerv1.WorkerServiceClient
	job     []byte
	input   []*workerv1.SourceChunk
	mu      sync.Mutex
	reports []*workerv1.ConversionReport
}

func (c *convertClient) GetConversionJob(context.Context, *workerv1.GetConversionJobRequest, ...grpc.CallOption) (*workerv1.GetConversionJobResponse, error) {
	return &workerv1.GetConversionJobResponse{JobDigest: convertjob.Digest(c.job), JobJson: c.job}, nil
}
func (c *convertClient) GetConversionInput(context.Context, *workerv1.GetConversionInputRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[workerv1.SourceChunk], error) {
	return &fakeInput{chunks: append([]*workerv1.SourceChunk(nil), c.input...)}, nil
}
func (c *convertClient) ReportConversion(_ context.Context, r *workerv1.ConversionReport, _ ...grpc.CallOption) (*workerv1.ConversionReportAck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, r)
	return &workerv1.ConversionReportAck{Accepted: true}, nil
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func conversionFixture(t *testing.T, approved string) (*Agent, *convertProvider, *convertClient, *workerv1.ConversionAssignment, []byte) {
	t.Helper()
	dir := t.TempDir()
	tool := []byte("#!/bin/false\nnot really kompose\n")
	if err := os.MkdirAll(filepath.Join(dir, "providers", "kompose"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "providers", "kompose", "kompose"), tool, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "providers", "kompose", "VERSION"), []byte("v1.38.0\n"), 0o644)
	if approved == "" {
		approved = sum(tool)
	}
	compose := []byte("services:\n  api:\n    image: example.invalid/api\n")
	manifest, _ := json.Marshal(map[string]any{"version": 1, "files": []map[string]any{{"path": "compose.yml", "size": len(compose), "digest": sum(compose)}}, "directories": []string{}})
	job := convertjob.Job{Version: 1, ConversionID: "c1", RunID: "r1", Provider: "kompose", ProviderVersion: "v1.38.0", ToolSHA256: map[string]string{runtime.GOARCH: approved},
		Args: []string{"convert", "-f", "/in/compose.yml", "-o", "/out/manifests"}, Mkdirs: []string{"manifests"}, Files: map[string]string{"cfg.yml": "a: b\n"}, Keep: []string{"manifests"},
		InputDigest: sum(manifest), TimeoutSeconds: 60}
	client := &convertClient{job: job.Canonical(), input: []*workerv1.SourceChunk{
		{Kind: &workerv1.SourceChunk_ManifestJson{ManifestJson: manifest}},
		{Kind: &workerv1.SourceChunk_File{File: &workerv1.SourceFileStart{Path: "compose.yml", Digest: sum(compose), Size: uint64(len(compose))}}},
		{Kind: &workerv1.SourceChunk_Data{Data: compose}},
	}}
	p := &convertProvider{}
	a := New(Config{StateDir: dir, ProvidersDir: filepath.Join(dir, "providers")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.UseProvider(p)
	as := &workerv1.ConversionAssignment{Lease: &workerv1.LeaseRef{AttemptId: "a1", FencingGeneration: 1}, ConversionId: "c1", JobDigest: job.Digest(),
		IsolationProfile: string(isolation.RootlessDedicatedVM), Limits: &workerv1.ResourceLimits{Vcpus: 1, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, WallClock: durationpb.New(time.Minute)}}
	return a, p, client, as, tool
}

func last(c *convertClient) *workerv1.ConversionReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reports) == 0 {
		return nil
	}
	return c.reports[len(c.reports)-1]
}

// TestConversionRunsOnlyTheApprovedTool (SEC-K8S-01, SEC-K8S-02): the agent runs the tool only when its bytes match
// the approved checksum, and the environment receives no secret release, mirror token or cache.
func TestConversionRunsOnlyTheApprovedTool(t *testing.T) {
	t.Run("approved tool, no credentials", func(t *testing.T) {
		a, p, client, as, tool := conversionFixture(t, "")
		if got := a.Capabilities(context.Background()).GetProviders()["kompose"]; got != "v1.38.0" {
			t.Errorf("advertised kompose = %q", got)
		}
		a.convert(context.Background(), client, as)
		rep := last(client)
		if rep.GetState() != workerv1.ConversionState_CONVERSION_STATE_COMPLETED || rep.GetToolDigest() != sum(tool) {
			t.Fatalf("report = %v %q %q", rep.GetState(), rep.GetFailureClass(), rep.GetDetail())
		}
		if len(rep.GetFiles()) != 1 || rep.GetFiles()[0].GetPath() != "manifests/api-deployment.yaml" || !strings.Contains(string(rep.GetToolStderr()), "WARN") {
			t.Errorf("files %v, stderr %q", rep.GetFiles(), rep.GetToolStderr())
		}
		if len(p.specs) != 1 {
			t.Fatalf("prepared %d environments", len(p.specs))
		}
		s := p.specs[0]
		if s.ReleaseSecret != nil || s.MirrorToken != nil || s.SecretBearing || s.CacheDir != "" {
			t.Error("the conversion environment can obtain secrets, mirror tokens or a cache")
		}
		if string(p.tool) != string(tool) || p.runSpec.Args[0] != "/.samuori-tool/tool" || p.runSpec.Args[1] != "convert" {
			t.Errorf("ran %q with %v", p.tool, p.runSpec.Args)
		}
		if p.runSpec.Files["cfg.yml"] != "a: b\n" || len(p.runSpec.Keep) != 1 {
			t.Errorf("the job's configuration or kept outputs did not reach the step: %+v", p.runSpec)
		}
		if p.inputMode != 0o644 {
			t.Errorf("input mode %v: the unprivileged tool user could not read it", p.inputMode)
		}
	})
	t.Run("checksum mismatch refused", func(t *testing.T) {
		a, p, client, as, _ := conversionFixture(t, "sha256:"+strings.Repeat("0", 64))
		a.convert(context.Background(), client, as)
		rep := last(client)
		if rep.GetState() != workerv1.ConversionState_CONVERSION_STATE_FAILED || rep.GetFailureClass() != "policy" || !strings.Contains(rep.GetDetail(), "approved checksum") {
			t.Fatalf("report = %v %q %q", rep.GetState(), rep.GetFailureClass(), rep.GetDetail())
		}
		if len(p.specs) != 0 || len(p.runs) != 0 {
			t.Error("an environment was prepared for a tool that failed its checksum")
		}
	})
	t.Run("job bound to the lease digest", func(t *testing.T) {
		a, p, client, as, _ := conversionFixture(t, "")
		as.JobDigest = "sha256:" + strings.Repeat("1", 64)
		a.convert(context.Background(), client, as)
		if rep := last(client); rep.GetState() != workerv1.ConversionState_CONVERSION_STATE_FAILED || len(p.runs) != 0 {
			t.Fatalf("a job that does not match its lease ran: %v", rep)
		}
	})
}
