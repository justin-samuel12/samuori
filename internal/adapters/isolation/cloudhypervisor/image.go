package cloudhypervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Manifest describes a guest image bundle built by tools/vm-image. Its own digest is the environment image
// reference workers pin (name@sha256:<manifest digest>).
type Manifest struct {
	Arch    string            `json:"arch"`
	Kernel  File              `json:"kernel"`
	RootFS  File              `json:"rootfs"`
	Cmdline string            `json:"cmdline"`
	Tools   map[string]string `json:"tools"`
}

type File struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Image is a verified bundle: the manifest digest matched and every file hashed to its recorded digest.
type Image struct {
	Manifest
	Digest       string
	kernel, root string
}

// LoadImage reads the manifest and hashes its files. Files are opened by paths relative to the manifest.
func LoadImage(manifest string) (*Image, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	img := &Image{Digest: "sha256:" + hex.EncodeToString(sum[:])}
	if err := json.Unmarshal(raw, &img.Manifest); err != nil {
		return nil, fmt.Errorf("guest image manifest: %w", err)
	}
	if img.Arch != runtime.GOARCH {
		return nil, fmt.Errorf("guest image is for %s, this host is %s", img.Arch, runtime.GOARCH)
	}
	dir := filepath.Dir(manifest)
	for _, f := range []struct {
		file File
		dst  *string
	}{{img.Kernel, &img.kernel}, {img.RootFS, &img.root}} {
		if !filepath.IsLocal(f.file.Path) {
			return nil, fmt.Errorf("guest image file %q must be inside the bundle", f.file.Path)
		}
		p := filepath.Join(dir, f.file.Path)
		d, size, err := digestFile(p)
		if err != nil {
			return nil, err
		}
		if d != f.file.Digest || size != f.file.Size {
			return nil, fmt.Errorf("guest image file %s does not match its manifest (%s, %d bytes)", f.file.Path, d, size)
		}
		*f.dst = p
	}
	if strings.ContainsAny(img.Cmdline, "\n\x00") {
		return nil, errors.New("guest image command line is invalid")
	}
	return img, nil
}

// Unchanged is a cheap check before each boot that the verified files were not replaced since loading.
func (img *Image) Unchanged() error {
	for _, f := range []struct {
		path string
		want File
	}{{img.kernel, img.Kernel}, {img.root, img.RootFS}} {
		info, err := os.Stat(f.path)
		if err != nil {
			return err
		}
		if info.Size() != f.want.Size || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("guest image file %s changed or is writable by others", f.path)
		}
	}
	return nil
}

func digestFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}
