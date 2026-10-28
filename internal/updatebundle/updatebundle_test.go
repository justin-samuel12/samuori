package updatebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func archives(t *testing.T, arches ...string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	out := map[string]string{}
	for _, a := range arches {
		p := filepath.Join(dir, a+".tar.gz")
		_ = os.WriteFile(p, []byte("release for "+a), 0o600)
		out[a] = p
	}
	return out
}

func fakeSign(sums []byte) ([]byte, error) { return []byte("sig"), nil }

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	m := Manifest{Version: "0.2.0", WorkerProtocol: Protocol{1, 3}, CreatedAt: time.Unix(0, 0).UTC()}
	if err := Write(&buf, m, archives(t, "arm64", "amd64"), fakeSign); err != nil {
		t.Fatal(err)
	}
	b, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b.Manifest.Version != "0.2.0" || len(b.Archives) != 2 || b.Archives[0].Arch != "amd64" || string(b.Signature) != "sig" {
		t.Fatalf("bundle = %+v", b)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.ManifestRaw, &fields); err != nil {
		t.Fatal(err)
	}
	// Controllers before 0.1.4 refuse unknown manifest fields.
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"architectures", "createdAt", "format", "version", "workerProtocol"}) {
		t.Errorf("manifest fields = %v", keys)
	}
	var got []byte
	if err := Extract(bytes.NewReader(buf.Bytes()), "arm64", "0.2.0", func(r io.Reader) error {
		got, err = io.ReadAll(r)
		return err
	}); err != nil || string(got) != "release for arm64" {
		t.Fatalf("extract = %q, %v", got, err)
	}
}

func rewrite(t *testing.T, bundle []byte, change func(name string, data []byte) (string, []byte, bool)) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(bundle))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		data, _ := io.ReadAll(tr)
		name, data, keep := change(h.Name, data)
		if !keep {
			continue
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	return out.Bytes()
}

func TestReadRejects(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Manifest{Version: "0.2.0", WorkerProtocol: Protocol{1, 3}}, archives(t, "arm64", "amd64"), fakeSign); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	cases := map[string]func(string, []byte) (string, []byte, bool){
		"tampered archive": func(n string, d []byte) (string, []byte, bool) {
			if strings.HasSuffix(n, "arm64.tar.gz") {
				return n, []byte("evil"), true
			}
			return n, d, true
		},
		"missing architecture": func(n string, d []byte) (string, []byte, bool) {
			return n, d, !strings.HasSuffix(n, "amd64.tar.gz")
		},
		"tampered manifest": func(n string, d []byte) (string, []byte, bool) {
			if n == ManifestName {
				return n, bytes.Replace(d, []byte("0.2.0"), []byte("0.3.0"), 1), true
			}
			return n, d, true
		},
		"unexpected entry": func(n string, d []byte) (string, []byte, bool) {
			if n == SigName {
				return "../" + n, d, true
			}
			return n, d, true
		},
		"no signature": func(n string, d []byte) (string, []byte, bool) { return n, d, n != SigName },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var inv *Invalid
			if _, err := Read(bytes.NewReader(rewrite(t, good, change))); !errors.As(err, &inv) {
				t.Fatalf("Read = %v; want refused", err)
			}
		})
	}
	if _, err := Read(strings.NewReader("not a tar")); err == nil {
		t.Error("a file that is not a bundle was read")
	}
}

func TestReadChanges(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ name, body string }{
		{"samuori-linux-amd64/samuori-server", "binary"},
		{"samuori-linux-amd64/ui/CHANGES.tsv", "deadbee\t2026-01-01\tnot this one\n"},
		{"samuori-linux-amd64/CHANGES.tsv", "2b8f3c0\t2026-10-24\tdocs(spikes): developer workflow\nnot a change line\n1a4c601\t2026-10-23\tfix: a\ttab\n"},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	_ = zw.Close()
	got, err := ReadChanges(bytes.NewReader(buf.Bytes()))
	want := []Change{{"2b8f3c0", "2026-10-24", "docs(spikes): developer workflow"}, {"1a4c601", "2026-10-23", "fix: a\ttab"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("changes = %+v, %v", got, err)
	}
	if got, err := ReadChanges(bytes.NewReader(emptyArchive(t))); err != nil || got != nil {
		t.Fatalf("archive without history = %+v, %v", got, err)
	}
}

func emptyArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = tar.NewWriter(zw).Close()
	_ = zw.Close()
	return buf.Bytes()
}
