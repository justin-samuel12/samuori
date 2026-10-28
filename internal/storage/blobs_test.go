package storage

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPutIsContentAddressedAndIdempotent(t *testing.T) {
	b, err := NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d1, n, err := b.Put(strings.NewReader("hello"), 10)
	if err != nil || n != 5 || d1 != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("put = %s %d %v", d1, n, err)
	}
	d2, _, err := b.Put(strings.NewReader("hello"), 10)
	if err != nil || d2 != d1 {
		t.Fatalf("second put = %s %v", d2, err)
	}
	got, err := b.ReadAll(d1, 10)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q %v", got, err)
	}
	if _, _, err := b.Put(strings.NewReader("too long"), 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize put = %v", err)
	}
	if _, err := b.Open("sha256:" + strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := b.Open("../../etc/passwd"); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestTamperedBlobDetected(t *testing.T) {
	t.Run("SEC-UPL-06", func(t *testing.T) {
		b, _ := NewBlobs(t.TempDir())
		d, _, _ := b.Put(strings.NewReader("original content"), 100)
		p, _ := b.path(d)
		_ = os.Chmod(p, 0o640)
		if err := os.WriteFile(p, []byte("altered content!"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := b.ReadAll(d, 100); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("tampered read = %v", err)
		}
		rc, _ := b.Open(d)
		defer rc.Close()
		if _, err := io.Copy(io.Discard, rc); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("streamed tampered read = %v", err)
		}
	})
}

// TestRepeatedPutRestartsGrace stores content that an old, unreferenced blob already holds. The repeat may not be
// recorded yet, so garbage collection must see it as new.
func TestRepeatedPutRestartsGrace(t *testing.T) {
	b, err := NewBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, _, _ := b.Put(strings.NewReader("again"), 10)
	p, _ := b.path(d)
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(p, old, old)
	if _, _, err := b.Put(strings.NewReader("again"), 10); err != nil {
		t.Fatal(err)
	}
	if mod, ok := b.ModTime(d); !ok || time.Since(mod) > time.Minute {
		t.Fatalf("the repeated put kept the old time %v", mod)
	}
}
