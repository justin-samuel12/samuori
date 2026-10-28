package sources

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"

	"samuori.invalid/samuori/internal/excludes"
	"samuori.invalid/samuori/internal/storage"
)

// Limits bound what one upload may expand to (TM-13). Nested archives are stored as files, never extracted.
type Limits struct {
	MaxArchiveBytes      int64
	MaxFiles             int
	MaxFileBytes         int64
	MaxUncompressedBytes int64
	MaxRatio             int64
	MaxDepth             int
	MaxWarnings          int
}

func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:      1 << 30,
		MaxFiles:             50_000,
		MaxFileBytes:         512 << 20,
		MaxUncompressedBytes: 4 << 30,
		MaxRatio:             100,
		MaxDepth:             48,
		MaxWarnings:          200,
	}
}

// Rejection is a validation failure the uploader can act on; its message is safe to show.
type Rejection struct{ Code, Message string }

func (r *Rejection) Error() string { return r.Message }

func reject(code, format string, args ...any) error {
	return &Rejection{Code: code, Message: fmt.Sprintf(format, args...)}
}

// File is one regular file in a snapshot manifest. Only the executable bit of the mode is kept so the digest
// depends on content alone.
type File struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	Executable bool   `json:"executable,omitempty"`
}

// Manifest is the canonical snapshot content; its SHA-256 over Canonical() is the snapshot digest.
type Manifest struct {
	Version     int      `json:"version"`
	Files       []File   `json:"files"`
	Directories []string `json:"directories"`
}

func (m Manifest) Canonical() []byte {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	sort.Strings(m.Directories)
	b, _ := json.Marshal(m)
	return b
}

func (m Manifest) Digest() string {
	sum := sha256.Sum256(m.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

type Result struct {
	Manifest   Manifest
	TotalBytes int64
	Rejected   int
	Excluded   int
	Warnings   []Warning
}

type entryKind int

const (
	kindFile entryKind = iota
	kindDir
	kindLink
	kindSpecial
)

type entry struct {
	name       string
	kind       entryKind
	size       int64
	executable bool
	open       func() (io.Reader, error)
}

type extractor struct {
	lim     Limits
	ex      excluder
	blobs   *storage.Blobs
	tree    *tree
	res     Result
	written int64
}

// Extract validates every entry of a TAR, gzip-compressed TAR or ZIP archive and stores accepted files in blobs.
func Extract(archive string, lim Limits, excludes []string, blobs *storage.Blobs) (Result, error) {
	f, err := os.Open(archive)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	if info.Size() > lim.MaxArchiveBytes {
		return Result{}, reject("too_large", "the upload is larger than the %d MiB limit", lim.MaxArchiveBytes>>20)
	}
	x := &extractor{lim: lim, ex: newExcluder(excludes), blobs: blobs, tree: newTree()}
	x.res.Manifest.Version = 1

	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		err = x.gzipTar(f, info.Size())
	case bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")):
		err = x.zip(f, info.Size())
	case len(head) == 512 && bytes.Equal(head[257:262], []byte("ustar")):
		err = x.tar(f)
	default:
		err = reject("unsupported_format", "the upload is not a TAR, TAR.GZ or ZIP archive")
	}
	if err != nil {
		return Result{}, err
	}
	if len(x.res.Manifest.Files) == 0 {
		return Result{}, reject("empty", "the upload contains no files after exclusions")
	}
	if x.res.Manifest.Files == nil {
		x.res.Manifest.Files = []File{}
	}
	if x.res.Manifest.Directories == nil {
		x.res.Manifest.Directories = []string{}
	}
	return x.res, nil
}

func (x *extractor) gzipTar(f *os.File, compressed int64) error {
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return reject("corrupt_archive", "the compressed archive is damaged")
	}
	defer zr.Close()
	ratio := &ratioReader{r: zr, limit: max(compressed, 1<<20) * x.lim.MaxRatio}
	return x.tar(ratio)
}

func (x *extractor) tar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, errRatio) {
			return bombError()
		}
		if err != nil {
			return reject("corrupt_archive", "the archive is damaged or truncated")
		}
		e := entry{name: h.Name, size: h.Size, executable: h.Mode&0o111 != 0, open: func() (io.Reader, error) { return tr, nil }}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			e.kind = kindFile
		case tar.TypeDir:
			e.kind = kindDir
		case tar.TypeSymlink, tar.TypeLink:
			e.kind = kindLink
		case tar.TypeXGlobalHeader:
			continue
		default:
			e.kind = kindSpecial
		}
		if err := x.add(e); err != nil {
			return err
		}
	}
}

func (x *extractor) zip(f *os.File, size int64) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return reject("corrupt_archive", "the ZIP archive is damaged")
	}
	if len(zr.File) > x.lim.MaxFiles*2 {
		return reject("too_many_files", "the upload has more than %d files", x.lim.MaxFiles)
	}
	for _, zf := range zr.File {
		mode := zf.Mode()
		e := entry{name: zf.Name, size: int64(zf.UncompressedSize64), executable: mode&0o111 != 0}
		switch {
		case mode.IsDir():
			e.kind = kindDir
		case mode&fs.ModeSymlink != 0:
			e.kind = kindLink
		case mode.IsRegular():
			e.kind = kindFile
		default:
			e.kind = kindSpecial
		}
		if zf.Flags&0x1 != 0 {
			return reject("encrypted_archive", "encrypted ZIP entries are not supported")
		}
		limit := max(int64(zf.CompressedSize64), 1<<20) * x.lim.MaxRatio
		e.open = func() (io.Reader, error) {
			rc, err := zf.Open()
			if err != nil {
				return nil, reject("corrupt_archive", "the ZIP archive is damaged")
			}
			return &ratioReader{r: rc, limit: limit}, nil
		}
		if err := x.add(e); err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) warn(w Warning) {
	switch {
	case len(x.res.Warnings) < x.lim.MaxWarnings:
		x.res.Warnings = append(x.res.Warnings, w)
	case len(x.res.Warnings) == x.lim.MaxWarnings:
		x.res.Warnings = append(x.res.Warnings, Warning{WarnTruncated, "More warnings were found than can be listed.", ""})
	}
}

func (x *extractor) add(e entry) error {
	p, err := normalizePath(e.name, x.lim.MaxDepth)
	if err != nil {
		x.res.Rejected++
		x.warn(Warning{WarnPathRejected, "Refused an entry with an unsafe or unsupported path.", safeName(e.name)})
		return nil
	}
	switch e.kind {
	case kindLink:
		x.res.Rejected++
		x.warn(Warning{WarnLinkRejected, "Refused a symbolic or hard link; snapshots contain only regular files.", p})
		return nil
	case kindSpecial:
		x.res.Rejected++
		x.warn(Warning{WarnSpecialRejected, "Refused a device, pipe, socket or other special file.", p})
		return nil
	}
	if pat, ok := x.ex.Match(p); ok {
		x.res.Excluded++
		if excludes.IsSecretFile(pat) && e.kind == kindFile {
			x.warn(Warning{WarnSecretExcluded, "Left out a file that usually holds credentials.", p})
		}
		return nil
	}
	if err := x.tree.add(p, e.kind == kindDir); err != nil {
		var c *collision
		errors.As(err, &c)
		return reject("path_collision", "%q and %q differ only by letter case or Unicode form; rename one of them", c.a, c.b)
	}
	if e.kind == kindDir {
		x.res.Manifest.Directories = append(x.res.Manifest.Directories, p)
		return nil
	}
	if len(x.res.Manifest.Files) >= x.lim.MaxFiles {
		return reject("too_many_files", "the upload has more than %d files", x.lim.MaxFiles)
	}
	if e.size > x.lim.MaxFileBytes {
		return reject("file_too_large", "%q is larger than the %d MiB per-file limit", p, x.lim.MaxFileBytes>>20)
	}
	if x.written+e.size > x.lim.MaxUncompressedBytes {
		return bombError()
	}
	r, err := e.open()
	if err != nil {
		return err
	}
	head := &headBuffer{max: contentScanBytes}
	digest, n, err := x.blobs.PutClass(storage.ClassSource, io.TeeReader(r, head), x.lim.MaxFileBytes)
	switch {
	case errors.Is(err, errRatio) || errors.Is(err, storage.ErrTooLarge):
		return bombError()
	case err != nil && isArchiveError(err):
		return reject("corrupt_archive", "the archive is damaged or truncated")
	case err != nil:
		return err
	}
	if n != e.size {
		return reject("corrupt_archive", "%q is shorter or longer than the archive declares", p)
	}
	x.written += n
	x.res.TotalBytes += n
	x.res.Manifest.Files = append(x.res.Manifest.Files, File{Path: p, Size: n, Digest: digest, Executable: e.executable})
	for _, w := range scanContent(p, head.buf.Bytes()) {
		x.warn(w)
	}
	return nil
}

func isArchiveError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrChecksum) || errors.Is(err, zip.ErrChecksum) || errors.Is(err, tar.ErrHeader)
}

func bombError() error {
	return reject("expansion_limit", "the archive expands far beyond its size or the total size limit (possible archive bomb)")
}

func safeName(s string) string {
	b := []rune{}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		b = append(b, r)
		if len(b) >= 200 {
			break
		}
	}
	return string(b)
}

var errRatio = errors.New("decompression ratio exceeded")

type ratioReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (r *ratioReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.limit {
		return n, errRatio
	}
	return n, err
}

type headBuffer struct {
	buf bytes.Buffer
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
