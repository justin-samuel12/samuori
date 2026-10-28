// Package updatebundle reads and writes the single signed file `make update` produces: a tar holding a manifest,
// one release archive per architecture, SHA256SUMS over all of them and its SSH signature (ADR 0045,
// docs/operators/upgrades.md#self-update). Verifying the signature is the caller's job.
package updatebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	Format       = 1
	Extension    = ".samuoriupdate"
	ManifestName = "manifest.json"
	SumsName     = "SHA256SUMS"
	SigName      = "SHA256SUMS.sig"
	maxSmall     = 1 << 20
	MaxArchive   = 1 << 30
)

var (
	versionRE  = regexp.MustCompile(`^[A-Za-z0-9.+_-]{1,64}$`)
	archiveRE  = regexp.MustCompile(`^samuori-([A-Za-z0-9.+_-]{1,64})-linux-(amd64|arm64)\.tar\.gz$`)
	knownArchs = []string{"amd64", "arm64"}
)

type Protocol struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
}

type Manifest struct {
	Format         int       `json:"format"`
	Version        string    `json:"version"`
	WorkerProtocol Protocol  `json:"workerProtocol"`
	Architectures  []string  `json:"architectures"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Change struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Summary string `json:"summary"`
}

// ChangesName is the change history inside each release archive, samuori-linux-<arch>/CHANGES.tsv. It lives in the
// archive, covered by SHA256SUMS, because controllers before 0.1.4 refuse unknown manifest fields and bundle entries.
const ChangesName = "CHANGES.tsv"

// ReadChanges finds the change history in a release archive (.tar.gz); an archive without one has none.
func ReadChanges(archive io.Reader) ([]Change, error) {
	zr, err := gzip.NewReader(archive)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		dir, base := path.Split(strings.TrimPrefix(h.Name, "./"))
		if base == ChangesName && strings.Count(dir, "/") == 1 && h.Typeflag == tar.TypeReg {
			return ParseChanges(tr)
		}
	}
}

// ParseChanges reads `git log --format=%h%x09%cs%x09%s` output.
func ParseChanges(r io.Reader) ([]Change, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSmall/2+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSmall/2 {
		return nil, invalid("the change history is larger than %d bytes", maxSmall/2)
	}
	var out []Change
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		out = append(out, Change{Commit: f[0], Date: f[1], Summary: strings.TrimSpace(f[2])})
	}
	return out, nil
}

// Invalid is a malformed bundle; the message is safe to show.
type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

func invalid(format string, a ...any) error { return &Invalid{fmt.Sprintf(format, a...)} }

func ArchiveName(version, arch string) string {
	return "samuori-" + version + "-linux-" + arch + ".tar.gz"
}

// ParseArchiveName returns the version and architecture of a release archive name.
func ParseArchiveName(name string) (version, arch string, ok bool) {
	m := archiveRE.FindStringSubmatch(name)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

type Archive struct {
	Arch, Name, Digest string
	Size               int64
}

// Bundle is a bundle whose structure and checksums are verified; its signature is not.
type Bundle struct {
	Manifest    Manifest
	ManifestRaw []byte
	Sums        []byte
	Signature   []byte
	Archives    []Archive
}

func (b *Bundle) Archive(arch string) (Archive, bool) {
	for _, a := range b.Archives {
		if a.Arch == arch {
			return a, true
		}
	}
	return Archive{}, false
}

// Sum returns the digest SHA256SUMS lists for name.
func Sum(sums []byte, name string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// Write builds a bundle from release archives (architecture to path). sign returns the SSH signature over the
// checksums.
func Write(w io.Writer, m Manifest, archives map[string]string, sign func(sums []byte) ([]byte, error)) error {
	m.Format = Format
	m.Architectures = m.Architectures[:0]
	for arch := range archives {
		m.Architectures = append(m.Architectures, arch)
	}
	slices.Sort(m.Architectures)
	if err := m.check(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	var sums bytes.Buffer
	digest := sha256.Sum256(raw)
	fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(digest[:]), ManifestName)
	for _, arch := range m.Architectures {
		d, err := fileDigest(archives[arch])
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", d, ArchiveName(m.Version, arch))
	}
	sig, err := sign(sums.Bytes())
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, f := range []struct {
		name string
		data []byte
	}{{ManifestName, raw}, {SumsName, sums.Bytes()}, {SigName, sig}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data)), ModTime: m.CreatedAt, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	for _, arch := range m.Architectures {
		if err := addFile(tw, archives[arch], ArchiveName(m.Version, arch), m.CreatedAt); err != nil {
			return err
		}
	}
	return tw.Close()
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func addFile(tw *tar.Writer, path, name string, mod time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: fi.Size(), ModTime: mod, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func (m Manifest) check() error {
	switch {
	case m.Format != Format:
		return invalid("the bundle format %d is not supported; this controller reads format %d", m.Format, Format)
	case !versionRE.MatchString(m.Version) || m.Version == "rollback":
		return invalid("the manifest names no valid version")
	case m.WorkerProtocol.Major == 0:
		return invalid("the manifest names no worker protocol")
	case len(m.Architectures) == 0:
		return invalid("the bundle holds no architecture")
	}
	seen := map[string]bool{}
	for _, a := range m.Architectures {
		if !slices.Contains(knownArchs, a) || seen[a] {
			return invalid("the manifest lists an unknown or repeated architecture %q", a)
		}
		seen[a] = true
	}
	return nil
}

// Read checks a bundle: only the expected regular files, a valid manifest, an archive for each listed architecture
// and every file matching SHA256SUMS. The reader is read through once.
func Read(r io.Reader) (*Bundle, error) {
	b := &Bundle{}
	tr := tar.NewReader(r)
	seen := map[string]bool{}
	digests := map[string]string{}
	sizes := map[string]int64{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalid("the file is not an update bundle from make update")
		}
		name := h.Name
		if h.Typeflag != tar.TypeReg || strings.ContainsAny(name, "/\\") || seen[name] {
			return nil, invalid("the bundle holds an unexpected entry %q", clip(name))
		}
		seen[name] = true
		switch name {
		case ManifestName, SumsName, SigName:
			data, err := io.ReadAll(io.LimitReader(tr, maxSmall+1))
			if err != nil {
				return nil, invalid("the bundle could not be read")
			}
			if len(data) > maxSmall {
				return nil, invalid("%s is too large", name)
			}
			switch name {
			case ManifestName:
				b.ManifestRaw = data
			case SumsName:
				b.Sums = data
			default:
				b.Signature = data
			}
		default:
			if _, _, ok := ParseArchiveName(name); !ok {
				return nil, invalid("the bundle holds an unexpected entry %q", clip(name))
			}
			if h.Size > MaxArchive {
				return nil, invalid("%s is larger than 1 GiB", name)
			}
			hash := sha256.New()
			n, err := io.Copy(hash, tr)
			if err != nil {
				return nil, invalid("the bundle could not be read")
			}
			digests[name] = hex.EncodeToString(hash.Sum(nil))
			sizes[name] = n
		}
	}
	if b.ManifestRaw == nil || b.Sums == nil || b.Signature == nil {
		return nil, invalid("the bundle lacks %s, %s or %s; build it with make update", ManifestName, SumsName, SigName)
	}
	digest := sha256.Sum256(b.ManifestRaw)
	if Sum(b.Sums, ManifestName) != hex.EncodeToString(digest[:]) {
		return nil, invalid("the manifest does not match its signed checksum")
	}
	dec := json.NewDecoder(bytes.NewReader(b.ManifestRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b.Manifest); err != nil {
		return nil, invalid("the manifest is unreadable")
	}
	if err := b.Manifest.check(); err != nil {
		return nil, err
	}
	for _, arch := range b.Manifest.Architectures {
		name := ArchiveName(b.Manifest.Version, arch)
		got, ok := digests[name]
		if !ok {
			return nil, invalid("the manifest lists %s but the bundle holds no %s", arch, name)
		}
		if want := Sum(b.Sums, name); want == "" || want != got {
			return nil, invalid("%s does not match its signed checksum", name)
		}
		b.Archives = append(b.Archives, Archive{Arch: arch, Name: name, Digest: got, Size: sizes[name]})
		delete(digests, name)
	}
	for name := range digests {
		return nil, invalid("%s is not listed in the manifest", name)
	}
	return b, nil
}

// Extract streams one architecture's archive from the bundle to fn.
func Extract(r io.Reader, arch string, version string, fn func(io.Reader) error) error {
	want := ArchiveName(version, arch)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return invalid("the bundle holds no %s", want)
		}
		if err != nil {
			return err
		}
		if h.Name == want && h.Typeflag == tar.TypeReg {
			return fn(tr)
		}
	}
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
