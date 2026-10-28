package workeragent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

type sourceClient struct {
	workerv1.WorkerServiceClient
	chunks []*workerv1.SourceChunk
}

func (c *sourceClient) GetSource(context.Context, *workerv1.GetSourceRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[workerv1.SourceChunk], error) {
	return &fakeInput{chunks: c.chunks}, nil
}

// A Dockerfile that copies source and switches to a non-root USER must be able to read it (found in the
// controller-to-worker run, docs/spikes/e2e-providers.md): the build context keeps checkout modes, and only the
// attempt directory is private.
func TestFetchSourceGivesTheBuildContextCheckoutModes(t *testing.T) {
	server, run := []byte("console.log(1)\n"), []byte("#!/bin/sh\n")
	manifest, _ := json.Marshal(map[string]any{"version": 1, "files": []map[string]any{
		{"path": "src/server.js", "size": len(server), "digest": sum(server)},
		{"path": "run.sh", "size": len(run), "digest": sum(run), "executable": true},
	}, "directories": []string{"src"}})
	client := &sourceClient{chunks: []*workerv1.SourceChunk{
		{Kind: &workerv1.SourceChunk_ManifestJson{ManifestJson: manifest}},
		{Kind: &workerv1.SourceChunk_File{File: &workerv1.SourceFileStart{Path: "src/server.js", Digest: sum(server), Size: uint64(len(server))}}},
		{Kind: &workerv1.SourceChunk_Data{Data: server}},
		{Kind: &workerv1.SourceChunk_File{File: &workerv1.SourceFileStart{Path: "run.sh", Digest: sum(run), Size: uint64(len(run)), Executable: true}}},
		{Kind: &workerv1.SourceChunk_Data{Data: run}},
	}}
	work, err := os.MkdirTemp(t.TempDir(), "attempt-")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(work, "source")
	if err := fetchSource(context.Background(), client, &workerv1.LeaseRef{}, sum(manifest), dir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{"src/server.js": 0o644, "run.sh": 0o755, "src": 0o755} {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v, want %v", path, got, want)
		}
	}
	if info, _ := os.Stat(work); info.Mode().Perm() != 0o700 {
		t.Errorf("attempt directory mode %v, want 0700", info.Mode().Perm())
	}
}
