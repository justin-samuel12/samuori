// Package imagescan inspects an OCI image archive as untrusted input: it resolves the images in it and walks every
// layer within fixed limits, reporting rule, path, layer and size only, never file contents (ADR 0031).
package imagescan

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type entry struct{ off, size int64 }

// Archive is an indexed OCI image layout tar. Blobs are read in place.
type Archive struct {
	f       io.ReaderAt
	entries map[string]entry
}

var ErrLimit = errors.New("imagescan: limit exceeded")

// Open indexes the archive's entries without reading blob contents.
func Open(f *os.File, maxEntries int) (*Archive, error) {
	a := &Archive{f: f, entries: map[string]entry{}}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return a, nil
		}
		if err != nil {
			return nil, fmt.Errorf("imagescan: reading archive: %w", err)
		}
		if len(a.entries) >= maxEntries {
			return nil, ErrLimit
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		a.entries[strings.TrimPrefix(h.Name, "./")] = entry{off: off, size: h.Size}
	}
}

func blobName(digest string) (string, error) {
	algo, hexpart, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" || len(hexpart) != 64 || strings.Trim(hexpart, "0123456789abcdef") != "" {
		return "", fmt.Errorf("imagescan: unsupported digest %q", digest)
	}
	return "blobs/sha256/" + hexpart, nil
}

func (a *Archive) section(name string) (*io.SectionReader, int64, error) {
	e, ok := a.entries[name]
	if !ok {
		return nil, 0, fmt.Errorf("imagescan: %s is missing", name)
	}
	return io.NewSectionReader(a.f, e.off, e.size), e.size, nil
}

// small reads a JSON document of at most 4 MiB and checks its digest when one is given.
func (a *Archive) small(name, digest string) ([]byte, error) {
	r, size, err := a.section(name)
	if err != nil {
		return nil, err
	}
	if size > 4<<20 {
		return nil, ErrLimit
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if digest != "" {
		sum := sha256.Sum256(raw)
		if "sha256:"+hex.EncodeToString(sum[:]) != digest {
			return nil, fmt.Errorf("imagescan: %s does not match its digest", name)
		}
	}
	return raw, nil
}

type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

type Config struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Config       struct {
		User         string              `json:"User"`
		Env          []string            `json:"Env"`
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		WorkingDir   string              `json:"WorkingDir"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Labels       map[string]string   `json:"Labels"`
	} `json:"config"`
	History []struct {
		CreatedBy  string `json:"created_by"`
		EmptyLayer bool   `json:"empty_layer"`
	} `json:"history"`
}

type Image struct {
	Manifest     string
	Platform     string
	ConfigDigest string
	ConfigRaw    []byte
	Config       Config
	Layers       []Descriptor
	// Annotations are the image manifest's; IndexAnnotations those of the indexes and descriptors that lead to it.
	Annotations      map[string]string
	IndexAnnotations map[string]string
}

const (
	mediaIndex       = "application/vnd.oci.image.index.v1+json"
	mediaManifest    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifest   = "application/vnd.docker.distribution.manifest.v2+json"
	dockerManifestLs = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Images resolves index.json to the image manifests it contains, skipping attestation manifests.
func (a *Archive) Images() ([]Image, error) {
	raw, err := a.small("index.json", "")
	if err != nil {
		return nil, err
	}
	var out []Image
	return out, a.resolve(raw, 0, nil, &out)
}

func (a *Archive) resolve(raw []byte, depth int, parent map[string]string, out *[]Image) error {
	if depth > 2 {
		return ErrLimit
	}
	var idx struct {
		Manifests   []Descriptor      `json:"manifests"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return fmt.Errorf("imagescan: index: %w", err)
	}
	inherited := withAnnotations(parent, idx.Annotations)
	if len(idx.Manifests) > 64 {
		return ErrLimit
	}
	for _, d := range idx.Manifests {
		name, err := blobName(d.Digest)
		if err != nil {
			return err
		}
		body, err := a.small(name, d.Digest)
		if err != nil {
			return err
		}
		switch d.MediaType {
		case mediaIndex, dockerManifestLs:
			if err := a.resolve(body, depth+1, withAnnotations(inherited, d.Annotations), out); err != nil {
				return err
			}
		case mediaManifest, dockerManifest:
			var m struct {
				Config      Descriptor        `json:"config"`
				Layers      []Descriptor      `json:"layers"`
				Annotations map[string]string `json:"annotations"`
			}
			if err := json.Unmarshal(body, &m); err != nil {
				return fmt.Errorf("imagescan: manifest: %w", err)
			}
			if d.Platform != nil && d.Platform.OS == "unknown" {
				continue
			}
			cname, err := blobName(m.Config.Digest)
			if err != nil {
				return err
			}
			craw, err := a.small(cname, m.Config.Digest)
			if err != nil {
				return err
			}
			img := Image{Manifest: d.Digest, ConfigDigest: m.Config.Digest, ConfigRaw: craw, Layers: m.Layers, Annotations: m.Annotations,
				IndexAnnotations: withAnnotations(inherited, d.Annotations)}
			if err := json.Unmarshal(craw, &img.Config); err != nil {
				return fmt.Errorf("imagescan: config: %w", err)
			}
			img.Platform = img.Config.OS + "/" + img.Config.Architecture
			*out = append(*out, img)
		}
	}
	return nil
}

func withAnnotations(base, more map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(more))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// Document reads index.json (empty digest) or a JSON blob of at most 4 MiB, checking the blob against its digest.
func (a *Archive) Document(digest string) ([]byte, error) {
	if digest == "" {
		return a.small("index.json", "")
	}
	name, err := blobName(digest)
	if err != nil {
		return nil, err
	}
	return a.small(name, digest)
}

// Layer opens a layer blob for reading.
func (a *Archive) Layer(d Descriptor) (io.Reader, error) {
	name, err := blobName(d.Digest)
	if err != nil {
		return nil, err
	}
	r, _, err := a.section(name)
	return r, err
}
