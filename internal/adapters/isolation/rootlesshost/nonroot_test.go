package rootlesshost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/imagescan"
)

type tarEntry struct {
	name string
	dir  bool
	mode int64
}

func layerTar(t *testing.T, entries []tarEntry) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: tar.TypeReg}
		if e.dir {
			h.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gz(t *testing.T, raw []byte) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(raw)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type layout struct {
	t     *testing.T
	blobs map[string][]byte
}

func (l *layout) add(v any) (string, int) {
	raw, ok := v.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			l.t.Fatal(err)
		}
	}
	d := digestOf(raw)
	l.blobs[d] = raw
	return d, len(raw)
}

func desc(media, digest string, size int, platform map[string]string) map[string]any {
	d := map[string]any{"mediaType": media, "digest": digest, "size": size}
	if platform != nil {
		d["platform"] = platform
	}
	return d
}

// image adds an image whose layers are given uncompressed; the first is stored gzip-compressed, the rest plain.
func (l *layout) image(user, workdir, arch string, layers ...[]byte) map[string]any {
	var diffIDs []string
	var descs []any
	for i, raw := range layers {
		diffIDs = append(diffIDs, digestOf(raw))
		if i == 0 {
			d, n := l.add(gz(l.t, raw))
			descs = append(descs, desc(mediaLayerGzip, d, n, nil))
		} else {
			d, n := l.add(raw)
			descs = append(descs, desc("application/vnd.oci.image.layer.v1.tar", d, n, nil))
		}
	}
	cfg := map[string]any{"architecture": arch, "os": "linux",
		"config":  map[string]any{"User": user, "WorkingDir": workdir, "Cmd": []string{"node", "server.js"}},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": diffIDs},
		"history": []any{map[string]any{"created_by": "base"}}}
	cd, cn := l.add(cfg)
	md, mn := l.add(map[string]any{"schemaVersion": 2, "mediaType": mediaOCIManifest, "config": desc("application/vnd.oci.image.config.v1+json", cd, cn, nil), "layers": descs})
	return desc(mediaOCIManifest, md, mn, map[string]string{"os": "linux", "architecture": arch})
}

func (l *layout) write(index map[string]any) string {
	p := filepath.Join(l.t.TempDir(), "image.tar")
	f, err := os.Create(p)
	if err != nil {
		l.t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	idx, _ := json.Marshal(index)
	tarBytes(tw, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	tarBytes(tw, "index.json", idx)
	for d, raw := range l.blobs {
		tarBytes(tw, "blobs/sha256/"+strings.TrimPrefix(d, "sha256:"), raw)
	}
	if err := tw.Close(); err != nil {
		l.t.Fatal(err)
	}
	f.Close()
	return p
}

func archiveNames(t *testing.T, p string) map[string]bool {
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]bool{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = true
	}
}

func TestNonRootImage(t *testing.T) {
	l := &layout{t: t, blobs: map[string][]byte{}}
	base := layerTar(t, []tarEntry{{name: "app/", dir: true, mode: 0o755}, {name: "app/src/", dir: true, mode: 0o755}, {name: "app/src/lib/", dir: true, mode: 0o755},
		{name: "app/gone/", dir: true, mode: 0o755}, {name: "app/locked/", dir: true, mode: 0o500}, {name: "app/server.js", mode: 0o644},
		{name: "etc/", dir: true, mode: 0o755}, {name: "etc/passwd", mode: 0o644}})
	upper := layerTar(t, []tarEntry{{name: "app/.wh.gone"}, {name: "app/src/.wh..wh..opq"}, {name: "app/src/index.js", mode: 0o644},
		{name: "app/tmp/", dir: true, mode: 0o1777}, {name: "./app/node_modules/", dir: true, mode: 0o755}})
	rootImg := l.image("", "/app", "amd64", base, upper)
	userImg := l.image("1000:1000", "/app", "arm64", base)
	nd, nn := l.add(map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex, "manifests": []any{rootImg, userImg}})
	archive := l.write(map[string]any{"schemaVersion": 2, "manifests": []any{desc(mediaOCIIndex, nd, nn, nil)}})
	before := archiveNames(t, archive)

	if err := NonRootImage(archive, "10001:10001"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := imagescan.Open(f, 1000)
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := a.Images()
	if err != nil || len(imgs) != 2 {
		t.Fatalf("images = %+v, %v", imgs, err)
	}
	for _, img := range imgs {
		switch img.Platform {
		case "linux/arm64":
			if img.Manifest != userImg["digest"] || img.Config.Config.User != "1000:1000" || len(img.Layers) != 1 {
				t.Errorf("an image with a non-root user changed: %+v", img)
			}
		case "linux/amd64":
			if img.Config.Config.User != "10001:10001" || len(img.Layers) != 3 || img.Manifest == rootImg["digest"] {
				t.Fatalf("root image = %+v", img)
			}
			var cfg struct {
				RootFS struct {
					DiffIDs []string `json:"diff_ids"`
				} `json:"rootfs"`
				History []map[string]any `json:"history"`
				Config  map[string]any   `json:"config"`
			}
			json.Unmarshal(img.ConfigRaw, &cfg)
			if cfg.Config["WorkingDir"] != "/app" || len(cfg.History) != 2 || !strings.Contains(cfg.History[1]["created_by"].(string), "10001:10001") {
				t.Errorf("config = %s", img.ConfigRaw)
			}
			last := img.Layers[2]
			r, err := a.Layer(last)
			if err != nil {
				t.Fatal(err)
			}
			zr, err := gzip.NewReader(r)
			if err != nil {
				t.Fatal(err)
			}
			plain, _ := io.ReadAll(zr)
			if len(cfg.RootFS.DiffIDs) != 3 || cfg.RootFS.DiffIDs[2] != digestOf(plain) {
				t.Errorf("diff IDs %v do not end with the new layer's %s", cfg.RootFS.DiffIDs, digestOf(plain))
			}
			got := map[string]int64{}
			tr := tar.NewReader(bytes.NewReader(plain))
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if h.Typeflag != tar.TypeDir || h.Uid != 10001 || h.Gid != 10001 {
					t.Errorf("entry %s: type %c, owner %d:%d; want directories owned by 10001:10001", h.Name, h.Typeflag, h.Uid, h.Gid)
				}
				got[h.Name] = h.Mode
			}
			want := map[string]int64{"app/": 0o755, "app/src/": 0o755, "app/locked/": 0o700, "app/tmp/": 0o1777, "app/node_modules/": 0o755}
			if len(got) != len(want) {
				t.Errorf("layer directories = %v; want %v (whiteouts and opaque directories applied, files left out)", got, want)
			}
			for n, m := range want {
				if got[n] != m {
					t.Errorf("%s: mode %o; want %o", n, got[n], m)
				}
			}
		default:
			t.Errorf("unexpected platform %s", img.Platform)
		}
	}
	after := archiveNames(t, archive)
	if after["blobs/sha256/"+strings.TrimPrefix(rootImg["digest"].(string), "sha256:")] || after["blobs/sha256/"+strings.TrimPrefix(nd, "sha256:")] {
		t.Error("the replaced manifest and index are still in the archive")
	}
	if !after["blobs/sha256/"+strings.TrimPrefix(userImg["digest"].(string), "sha256:")] || len(after) != len(before)+1 {
		t.Errorf("archive entries: %d before, %d after; want the unchanged blobs kept and one new layer", len(before), len(after))
	}

	l2 := &layout{t: t, blobs: map[string][]byte{}}
	only := l2.image("app", "/srv", "amd64", base)
	unchanged := l2.write(map[string]any{"schemaVersion": 2, "manifests": []any{only}})
	raw, _ := os.ReadFile(unchanged)
	if err := NonRootImage(unchanged, "10001:10001"); err != nil {
		t.Fatal(err)
	}
	if raw2, _ := os.ReadFile(unchanged); !bytes.Equal(raw, raw2) {
		t.Error("an archive whose images already run as non-root was rewritten")
	}
}

func TestNonRootUserSetting(t *testing.T) {
	for _, u := range []string{"", "root", "0:0", "10001", "10001:0", "app:app", "-1:5"} {
		if _, _, err := parseUser(u); err == nil {
			t.Errorf("parseUser(%q) accepted", u)
		}
	}
	for u, root := range map[string]bool{"": true, "0": true, "root": true, "0:1000": true, "root:root": true, "1000": false, "app": false, "10001:10001": false} {
		if rootUser(u) != root {
			t.Errorf("rootUser(%q) = %v", u, !root)
		}
	}
}
