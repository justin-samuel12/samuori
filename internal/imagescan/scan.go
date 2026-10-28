package imagescan

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"samuori.invalid/samuori/internal/secretscan"
)

type Limits struct {
	MaxLayers       int
	MaxEntries      int
	MaxUncompressed int64
	MaxFileScan     int64
	MaxFindings     int
}

func DefaultLimits() Limits {
	return Limits{MaxLayers: 256, MaxEntries: 1_000_000, MaxUncompressed: 32 << 30, MaxFileScan: 1 << 20, MaxFindings: 500}
}

// Finding locates a match. It never carries matched content.
type Finding struct {
	Rule     string `json:"rule"`
	Class    string `json:"class"`
	Path     string `json:"path"`
	Layer    string `json:"layer,omitempty"`
	Size     int64  `json:"size"`
	SecretID string `json:"secretId,omitempty"`
}

type Report struct {
	Images    []string  `json:"images"`
	Findings  []Finding `json:"findings"`
	Truncated int       `json:"truncated"`
	Complete  bool      `json:"complete"`
	Reason    string    `json:"reason,omitempty"`
	Layers    int       `json:"layers"`
	Entries   int       `json:"entries"`
	Bytes     int64     `json:"bytes"`
}

type Options struct {
	Rules []Rule
	// Values are delivered secret values by logical ID, for the exact-bytes canary.
	Values map[string][]byte
	// Metadata checks image config environment and history for secret-like values.
	Metadata bool
	Limits   Limits
}

type scanner struct {
	o    Options
	rep  Report
	seen map[string]bool
}

// Scan inspects every image in the archive. An archive the scanner cannot fully inspect within its limits yields an
// incomplete report, which callers must treat as a failed check.
func Scan(a *Archive, o Options) Report {
	s := &scanner{o: o, seen: map[string]bool{}, rep: Report{Images: []string{}, Findings: []Finding{}}}
	imgs, err := a.Images()
	if err != nil {
		s.rep.Reason = err.Error()
		return s.rep
	}
	if len(imgs) == 0 {
		s.rep.Reason = "the archive contains no image"
		return s.rep
	}
	done := map[string]bool{}
	for _, img := range imgs {
		s.rep.Images = append(s.rep.Images, img.Platform)
		s.canary("image config", img.ConfigDigest, img.ConfigRaw)
		if o.Metadata {
			s.metadata(img)
		}
		for _, l := range img.Layers {
			if done[l.Digest] {
				continue
			}
			done[l.Digest] = true
			if s.rep.Layers++; s.rep.Layers > o.Limits.MaxLayers {
				s.rep.Reason = "the image has more layers than the scanner allows"
				return s.rep
			}
			if err := s.layer(a, l); err != nil {
				s.rep.Reason = fmt.Sprintf("layer %s: %v", short(l.Digest), err)
				return s.rep
			}
		}
	}
	sort.Slice(s.rep.Findings, func(i, j int) bool {
		a, b := s.rep.Findings[i], s.rep.Findings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Rule < b.Rule
	})
	s.rep.Complete = true
	return s.rep
}

func short(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func (s *scanner) add(f Finding) {
	key := f.Rule + "\x00" + f.Path + "\x00" + f.Layer + "\x00" + f.SecretID
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	if len(s.rep.Findings) >= s.o.Limits.MaxFindings {
		s.rep.Truncated++
		return
	}
	s.rep.Findings = append(s.rep.Findings, f)
}

func (s *scanner) canary(where, layer string, b []byte) {
	for id, v := range s.o.Values {
		if len(v) > 0 && bytes.Contains(b, v) {
			s.add(Finding{Rule: RuleBuildSecret, Class: ClassBuildSecret, Path: where, Layer: layer, Size: int64(len(b)), SecretID: id})
		}
	}
}

var (
	secretName   = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|private[_-]?key|credential|access[_-]?key|_key$)`)
	historyAssig = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=(\S+)`)
	// Official images verify downloads with OpenPGP keys named by fingerprint (python's GPG_KEY); a fingerprint
	// identifies a public key and is not a secret.
	pgpKeyName     = regexp.MustCompile(`(?i)(^|_)(gpg|pgp)_keys?$`)
	pgpFingerprint = regexp.MustCompile(`^([0-9A-Fa-f]{40}|[0-9A-Fa-f]{64})$`)
)

func publicKeyFingerprints(name, value string) bool {
	if !pgpKeyName.MatchString(name) {
		return false
	}
	fields := strings.Fields(value)
	for _, f := range fields {
		if !pgpFingerprint.MatchString(f) {
			return false
		}
	}
	return len(fields) > 0
}

// SecretLikeName reports whether a variable, label or annotation name suggests its value is a credential.
func SecretLikeName(name string) bool { return secretName.MatchString(name) }

// SecretLikeEntry flags a non-empty value under a secret-looking key, or a value in a known credential format.
func SecretLikeEntry(key, value string) bool {
	if publicKeyFingerprints(key, value) {
		return false
	}
	return value != "" && (secretName.MatchString(key) || len(secretscan.Match([]byte(value))) > 0)
}

// metadata flags secret-looking environment values, build arguments recorded in history, labels and annotations;
// values are not reported.
func (s *scanner) metadata(img Image) {
	for _, m := range []struct {
		rule, where string
		kv          map[string]string
	}{
		{RuleMetadataLabel, "config.Labels", img.Config.Config.Labels},
		{RuleMetadataAnnot, "manifest.annotations", img.Annotations},
		{RuleMetadataAnnot, "index.annotations", img.IndexAnnotations},
	} {
		for k, v := range m.kv {
			if SecretLikeEntry(k, v) {
				s.add(Finding{Rule: m.rule, Class: ClassMetadata, Path: m.where + " " + k, Layer: img.ConfigDigest})
			}
		}
	}
	for i, e := range img.Config.Config.Env {
		name, value, _ := strings.Cut(e, "=")
		if value != "" && secretName.MatchString(name) && !publicKeyFingerprints(name, value) {
			s.add(Finding{Rule: RuleMetadataEnv, Class: ClassMetadata, Path: fmt.Sprintf("config.Env[%d] %s", i, name), Layer: img.ConfigDigest})
		}
	}
	for i, h := range img.Config.History {
		cmd := h.CreatedBy
		if !strings.HasPrefix(cmd, "RUN |") && !strings.Contains(cmd, "ARG ") {
			continue
		}
		for _, m := range historyAssig.FindAllStringSubmatch(cmd, 20) {
			if secretName.MatchString(m[1]) {
				s.add(Finding{Rule: RuleMetadataHistory, Class: ClassMetadata, Path: fmt.Sprintf("history[%d] %s", i, m[1]), Layer: img.ConfigDigest})
			}
		}
	}
}

type counted struct {
	r io.Reader
	n int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *scanner) layer(a *Archive, d Descriptor) error {
	raw, err := a.Layer(d)
	if err != nil {
		return err
	}
	h := sha256.New()
	tee := io.TeeReader(raw, h)
	var plain io.Reader
	switch {
	case strings.HasSuffix(d.MediaType, "tar+gzip") || strings.HasSuffix(d.MediaType, "tar.gzip"):
		zr, err := gzip.NewReader(tee)
		if err != nil {
			return err
		}
		plain = zr
	case strings.HasSuffix(d.MediaType, ".tar") || strings.HasSuffix(d.MediaType, "layer.v1.tar"):
		plain = tee
	default:
		return fmt.Errorf("unsupported layer media type %s", d.MediaType)
	}
	budget := s.o.Limits.MaxUncompressed - s.rep.Bytes
	cr := &counted{r: io.LimitReader(plain, budget+1)}
	tooBig := errors.New("the image is larger than the scanner allows")
	tr := tar.NewReader(cr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil {
			if s.rep.Entries++; s.rep.Entries > s.o.Limits.MaxEntries {
				return errors.New("too many files")
			}
			name := path.Clean("/" + hdr.Name)[1:]
			if hdr.Typeflag != tar.TypeReg {
				s.pathRules(name, d.Digest, 0, nil)
				continue
			}
			err = s.file(tr, name, d.Digest, hdr.Size)
		}
		if cr.n > budget {
			return tooBig
		}
		if err != nil {
			return err
		}
	}
	if cr.n > budget {
		return tooBig
	}
	s.rep.Bytes += cr.n
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != d.Digest {
		return errors.New("the layer does not match its digest")
	}
	return nil
}

// file applies path and content rules to one regular file, streaming it once. Content rules see the first
// MaxFileScan bytes; the canary sees the whole file.
func (s *scanner) file(r io.Reader, name, layer string, size int64) error {
	head := &bytes.Buffer{}
	br := bufio.NewReaderSize(r, 64<<10)
	overlap := 0
	for _, v := range s.o.Values {
		overlap = max(overlap, len(v)-1)
	}
	var tail []byte
	found := map[string]bool{}
	buf := make([]byte, 64<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if int64(head.Len()) < s.o.Limits.MaxFileScan {
				head.Write(chunk[:min(int64(n), s.o.Limits.MaxFileScan-int64(head.Len()))])
			}
			if len(s.o.Values) > 0 {
				window := append(tail, chunk...)
				for id, v := range s.o.Values {
					if !found[id] && len(v) > 0 && bytes.Contains(window, v) {
						found[id] = true
					}
				}
				if overlap > 0 {
					tail = append(tail[:0], window[max(0, len(window)-overlap):]...)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	for id := range found {
		s.add(Finding{Rule: RuleBuildSecret, Class: ClassBuildSecret, Path: name, Layer: layer, Size: size, SecretID: id})
	}
	s.pathRules(name, layer, size, head.Bytes())
	return nil
}

func (s *scanner) pathRules(name, layer string, size int64, head []byte) {
	for _, r := range s.o.Rules {
		if r.matches(name, head) {
			s.add(Finding{Rule: r.ID, Class: r.Class, Path: name, Layer: layer, Size: size})
		}
	}
}
