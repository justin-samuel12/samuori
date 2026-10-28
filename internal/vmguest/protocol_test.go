package vmguest

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/isolation"
)

func TestClassesRoundTrip(t *testing.T) {
	for _, c := range []error{isolation.ErrResourceLimit, isolation.ErrTimeout, isolation.ErrInfrastructure, isolation.ErrUnsupported} {
		if err := Err(ClassOf(&isolation.Failure{Class: c, Detail: "x"}), "x"); !errors.Is(err, c) {
			t.Errorf("%v became %v", c, err)
		}
	}
	if ClassOf(errors.New("compile error")) != ClassBuild || isolation.Requeue(Err(ClassBuild, "compile error")) {
		t.Fatal("application failures must stay application failures")
	}
	if Err("", "") != nil || ClassOf(nil) != "" {
		t.Fatal("success must carry no class")
	}
	if isolation.Requeue(Err("anything-unknown", "x")) {
		t.Fatal("unknown classes must not become requeueable infrastructure failures")
	}
}

func TestTreeRoundTripStaysInside(t *testing.T) {
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "a", "b", "f.txt"), []byte("hello"), 0o644)
	_ = os.Symlink("../b/f.txt", filepath.Join(src, "a", "link"))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := WriteTree(tw, src, SourcePrefix); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	dst := t.TempDir()
	root, _ := os.OpenRoot(dst)
	defer root.Close()
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		h.Name = strings.TrimPrefix(h.Name, SourcePrefix)
		if err := ExtractEntry(root, h, tr); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "b", "f.txt")); string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}

	outside := t.TempDir()
	evil := t.TempDir()
	r2, _ := os.OpenRoot(evil)
	defer r2.Close()
	if err := ExtractEntry(r2, &tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: outside}, nil); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*tar.Header{
		{Name: "../x", Typeflag: tar.TypeReg, Size: 1},
		{Name: "/abs", Typeflag: tar.TypeReg, Size: 1},
		{Name: "esc/planted", Typeflag: tar.TypeReg, Size: 1},
		{Name: "dev", Typeflag: tar.TypeChar},
	} {
		if err := ExtractEntry(r2, h, strings.NewReader("x")); err == nil {
			t.Errorf("%s (%c) was extracted", h.Name, h.Typeflag)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("an entry was written through a symlink outside the tree")
	}
}
