package publication

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type blobRef struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// ociImage writes a one-image OCI archive with a gzip layer holding /hello, plus extra entries.
func ociImage(t *testing.T, extra map[string][]byte) *os.File {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	put := func(name string, b []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	blob := func(media string, b []byte) blobRef {
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		put("blobs/sha256/"+h, b)
		return blobRef{media, "sha256:" + h, int64(len(b))}
	}
	var layerTar bytes.Buffer
	lw := tar.NewWriter(&layerTar)
	_ = lw.WriteHeader(&tar.Header{Name: "hello", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = lw.Write([]byte("hi"))
	_ = lw.Close()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(layerTar.Bytes())
	_ = zw.Close()
	diff := sha256.Sum256(layerTar.Bytes())
	layer := blob("application/vnd.oci.image.layer.v1.tar+gzip", gz.Bytes())
	cfg, _ := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "config": map[string]any{"Cmd": []string{"/hello"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + hex.EncodeToString(diff[:])}}})
	config := blob("application/vnd.oci.image.config.v1+json", cfg)
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": config, "layers": []blobRef{layer}})
	m := blob("application/vnd.oci.image.manifest.v1+json", man)
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []blobRef{m}})
	put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	put("index.json", idx)
	for name, b := range extra {
		put(name, b)
	}
	_ = tw.Close()
	path := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func entries(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

// TestDockerLoadable: the download keeps every OCI entry and adds Docker's manifest.json naming the config and
// layers, tagged for a single image, replacing any manifest.json already in the archive; an unreadable archive is
// refused before anything is written. With SAMUORI_TEST_DOCKER=1 the result is loaded with docker load.
func TestDockerLoadable(t *testing.T) {
	src := ociImage(t, map[string][]byte{"manifest.json": []byte(`[{"Config":"forged"}]`)})
	original := map[string][]byte{}
	raw, _ := io.ReadAll(src)
	for k, v := range entries(t, raw) {
		original[k] = v
	}
	l, err := NewDockerLoadable(src, "test/bassel-audit:2026-10-27.1")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := l.Write(&out); err != nil {
		t.Fatal(err)
	}
	got := entries(t, out.Bytes())
	for name, b := range original {
		if name != "manifest.json" && !bytes.Equal(got[name], b) {
			t.Errorf("entry %s changed", name)
		}
	}
	var manifest []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(got["manifest.json"], &manifest); err != nil || len(manifest) != 1 {
		t.Fatalf("manifest.json = %s (%v)", got["manifest.json"], err)
	}
	e := manifest[0]
	if !slices.Equal(e.RepoTags, []string{"test/bassel-audit:2026-10-27.1"}) || got[e.Config] == nil || len(e.Layers) != 1 || got[e.Layers[0]] == nil ||
		!strings.HasPrefix(e.Config, "blobs/sha256/") {
		t.Fatalf("manifest entry = %+v", e)
	}

	garbage, _ := os.CreateTemp(t.TempDir(), "x")
	_, _ = garbage.WriteString("not an archive")
	if _, err := NewDockerLoadable(garbage, ""); err == nil {
		t.Error("an unreadable archive was accepted")
	}

	if os.Getenv("SAMUORI_TEST_DOCKER") != "1" {
		return
	}
	path := filepath.Join(t.TempDir(), "load.tar")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := exec.Command("docker", "load", "-i", path).CombinedOutput()
	if err != nil || !strings.Contains(string(res), "test/bassel-audit:2026-10-27.1") {
		t.Fatalf("docker load: %v\n%s", err, res)
	}
	_ = exec.Command("docker", "rmi", "test/bassel-audit:2026-10-27.1").Run()
}

func TestDockerRepoTag(t *testing.T) {
	for _, c := range []struct{ project, app, tag, want string }{
		{"test", "bassel-audit", "2026-10-27.1", "test/bassel-audit:2026-10-27.1"},
		{"Payments API", "Orders__API", "samuori-0123456789ab", "payments-api/orders-api:samuori-0123456789ab"},
		{"", "", "x", ""},
		{"p", "", "-.bad tag", "p:bad-tag"},
	} {
		if got := dockerRepoTag(c.project, c.app, c.tag); got != c.want {
			t.Errorf("dockerRepoTag(%q, %q, %q) = %q, want %q", c.project, c.app, c.tag, got, c.want)
		}
	}
}
