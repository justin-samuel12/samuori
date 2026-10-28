package imagescan

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// ociArchive writes a one-image OCI archive with the given config labels and manifest annotations.
func ociArchive(t *testing.T, labels, annotations map[string]string, env ...string) *Archive {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	put := func(name string, b []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	blob := func(b []byte) Descriptor {
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		put("blobs/sha256/"+h, b)
		return Descriptor{Digest: "sha256:" + h, Size: int64(len(b))}
	}
	layer := blob(make([]byte, 1024))
	layer.MediaType = "application/vnd.oci.image.layer.v1.tar"
	cfg, _ := json.Marshal(map[string]any{"os": "linux", "architecture": "amd64", "config": map[string]any{"Labels": labels, "Env": env}})
	config := blob(cfg)
	config.MediaType = "application/vnd.oci.image.config.v1+json"
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaManifest, "config": config, "layers": []Descriptor{layer}, "annotations": annotations})
	m := blob(man)
	m.MediaType = mediaManifest
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []Descriptor{m}})
	put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	put("index.json", idx)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	a, err := Open(f, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestMetadataLabelsAndAnnotations: the metadata class flags labels and annotations whose key looks secret or whose
// value is in a known credential format, and never reports the value.
func TestMetadataLabelsAndAnnotations(t *testing.T) {
	a := ociArchive(t,
		map[string]string{"org.opencontainers.image.title": "api", "com.example.api-token": "abc123", "com.example.note": "AKIAABCDEFGHIJKLMNOP"},
		map[string]string{"org.opencontainers.image.created": "2026-10-26T00:00:00Z", "com.example.db-password": "hunter2", "com.example.empty-secret": ""})
	rep := Scan(a, Options{Metadata: true, Limits: DefaultLimits()})
	if !rep.Complete {
		t.Fatalf("incomplete: %s", rep.Reason)
	}
	var got []string
	for _, f := range rep.Findings {
		if f.Class != ClassMetadata {
			t.Errorf("unexpected finding %+v", f)
			continue
		}
		got = append(got, f.Rule+" "+f.Path)
	}
	want := []string{
		"metadata-label config.Labels com.example.api-token",
		"metadata-label config.Labels com.example.note",
		"metadata-annotation manifest.annotations com.example.db-password",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("findings = %v, want %v", got, want)
	}
	if off := Scan(a, Options{Limits: DefaultLimits()}); len(off.Findings) != 0 {
		t.Errorf("metadata findings without the metadata class: %v", off.Findings)
	}
}

// TestMetadataEnvPublicKeyFingerprints: OpenPGP fingerprints under a GPG or PGP key name, as official base images
// set them, are public; anything else under a key-like name is still flagged.
func TestMetadataEnvPublicKeyFingerprints(t *testing.T) {
	a := ociArchive(t, nil, nil,
		"PATH=/usr/local/bin:/usr/bin",
		"GPG_KEY=A035C8C19219BA821ECEA86B64E628F8D684696D",
		"NODE_GPG_KEYS=4ED778F539E3634C779C87C6D7062848A1AB005C 141F07595B7B3FFE74309A937405533BE57C7D57",
		"GPG_KEY_PASSPHRASE=A035C8C19219BA821ECEA86B64E628F8D684696D",
		"SIGNING_GPG_KEY=-----BEGIN PGP PRIVATE KEY BLOCK-----",
		"API_KEY=A035C8C19219BA821ECEA86B64E628F8D684696D",
		"PGP_KEYS=",
	)
	rep := Scan(a, Options{Metadata: true, Limits: DefaultLimits()})
	var got []string
	for _, f := range rep.Findings {
		got = append(got, f.Path)
	}
	want := []string{"config.Env[3] GPG_KEY_PASSPHRASE", "config.Env[4] SIGNING_GPG_KEY", "config.Env[5] API_KEY"}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %v, want %v", got, want)
	}
}
