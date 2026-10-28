// Package vmguest is the vm-per-job guest agent and the protocol the host provider speaks to it over vsock
// (docs/architecture/vm-per-job.md). The guest has no network device: every connection out is a vsock relay to
// the host, which serves only the build's egress proxy session, the registry mirrors and the attempt's callbacks.
package vmguest

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/isolation"
)

// Vsock ports. The agent listens on AgentPort; the host listens on the others.
const (
	AgentPort      = 1024
	CallbackPort   = 1025
	ProxyPort      = 1026
	MirrorPortBase = 1100
	MaxMirrors     = 32
)

// RelayIP is the guest-only address its relays listen on; build steps reach the proxy and mirrors through it.
const RelayIP = "10.88.0.1"

const (
	ProxyRelayPort      = 3128
	MirrorRelayPortBase = 5000
)

// Mirror is one registry mirror relayed from the guest to the host.
type Mirror struct {
	Upstream string `json:"upstream"`
	Index    int    `json:"index"`
}

func (m Mirror) Relay() string    { return fmt.Sprintf("%s:%d", RelayIP, MirrorRelayPortBase+m.Index) }
func (m Mirror) HostPort() uint32 { return uint32(MirrorPortBase + m.Index) }

// RunRequest is request.json in the run stream. Secret values never travel in it: the guest asks the host for
// each one through the callback port while the build runs.
type RunRequest struct {
	AttemptID       string           `json:"attemptId"`
	SecretBearing   bool             `json:"secretBearing"`
	TrustDomain     string           `json:"trustDomain"`
	Limits          isolation.Limits `json:"limits"`
	ProxyURL        string           `json:"proxyUrl"`
	Mirrors         []Mirror         `json:"mirrors"`
	InsecureMirrors bool             `json:"insecureMirrors"`
	Tasks           int              `json:"tasks"`
	Cache           bool             `json:"cache"`
}

// RunResult is result.json in the reply stream. The host treats every field as untrusted.
type RunResult struct {
	Class          string            `json:"class"`
	Detail         string            `json:"detail"`
	Logs           string            `json:"logs"`
	ArtifactDigest string            `json:"artifactDigest"`
	CacheDigest    string            `json:"cacheDigest"`
	Checks         []isolation.Check `json:"checks"`
}

// Failure classes carried in RunResult.Class; an empty class is success and "build" an application failure.
const (
	ClassBuild          = "build"
	ClassResourceLimit  = "resource-limit"
	ClassTimeout        = "timeout"
	ClassInfrastructure = "infrastructure"
	ClassUnsupported    = "unsupported"
)

var classes = map[string]error{ClassResourceLimit: isolation.ErrResourceLimit, ClassTimeout: isolation.ErrTimeout,
	ClassInfrastructure: isolation.ErrInfrastructure, ClassUnsupported: isolation.ErrUnsupported}

// ClassOf names an isolation failure class for the wire.
func ClassOf(err error) string {
	if err == nil {
		return ""
	}
	for name, c := range classes {
		if errors.Is(err, c) {
			return name
		}
	}
	return ClassBuild
}

// Err rebuilds the failure a guest reported; detail is guest text and must already be bounded and masked.
func Err(class, detail string) error {
	if class == "" {
		return nil
	}
	if c, ok := classes[class]; ok {
		return &isolation.Failure{Class: c, Detail: detail}
	}
	return errors.New(detail)
}

// Stream entry names.
const (
	EntryRequest  = "request.json"
	EntryPlan     = "plan.json"
	EntryResult   = "result.json"
	EntryImage    = "image.tar"
	EntryCache    = "cache.tar"
	SourcePrefix  = "source/"
	CachePrefix   = "cache/"
	MaxResultSize = 8 << 20
)

// WriteFile adds one regular file entry.
func WriteFile(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	_, err := io.CopyN(tw, r, size)
	return err
}

// WriteBytes adds an in-memory file entry.
func WriteBytes(tw *tar.Writer, name string, b []byte) error {
	return WriteFile(tw, name, int64(len(b)), strings.NewReader(string(b)))
}

// WriteTree adds dir's regular files, directories and symlinks under prefix.
func WriteTree(tw *tar.Writer, dir, prefix string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := prefix + filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: int64(info.Mode().Perm()), Typeflag: tar.TypeDir, ModTime: info.ModTime()})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Mode: 0o777, Typeflag: tar.TypeSymlink, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg, ModTime: info.ModTime()}); err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			return err
		}
		return nil
	})
}

// ExtractEntry writes one tree entry below root, which never lets a path or link escape it.
func ExtractEntry(root *os.Root, h *tar.Header, r io.Reader) error {
	name := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
	if !filepath.IsLocal(name) {
		return fmt.Errorf("entry %q leaves the tree", h.Name)
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		return root.MkdirAll(name, os.FileMode(h.Mode).Perm()|0o700)
	case tar.TypeSymlink:
		return root.Symlink(h.Linkname, name)
	case tar.TypeReg:
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode).Perm()|0o600)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(f, r, h.Size); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	return fmt.Errorf("entry %q has unsupported type %c", h.Name, h.Typeflag)
}
