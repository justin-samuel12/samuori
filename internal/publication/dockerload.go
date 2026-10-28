package publication

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"samuori.invalid/samuori/internal/imagescan"
)

const dockerManifestName = "manifest.json"

var (
	repoSeparators = regexp.MustCompile(`[._-]*[^a-z0-9._-]+[._-]*|[._-]{2,}`)
	tagUnsafe      = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

// dockerRepoTag turns names into a reference docker load can apply, such as payments/orders-api:2026-10-27.1.
func dockerRepoTag(project, app, tag string) string {
	var parts []string
	for _, n := range []string{project, app} {
		if c := strings.Trim(repoSeparators.ReplaceAllString(strings.ToLower(n), "-"), "._-"); c != "" {
			parts = append(parts, c)
		}
	}
	tag = strings.TrimLeft(tagUnsafe.ReplaceAllString(tag, "-"), ".-")
	if len(parts) == 0 || tag == "" {
		return ""
	}
	return strings.Join(parts, "/") + ":" + tag[:min(len(tag), 128)]
}

// DockerLoadable is an OCI image archive that is written out with Docker's manifest.json added, so it loads with
// docker load on the classic image store as well as on stores that read OCI archives. Blobs are not changed.
type DockerLoadable struct {
	src      *os.File
	manifest []byte
}

// NewDockerLoadable reads the archive's images before anything is written, so an unreadable archive is reported
// while the caller can still answer with an error.
func NewDockerLoadable(src *os.File, repoTag string) (*DockerLoadable, error) {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	a, err := imagescan.Open(src, 1<<16)
	if err != nil {
		return nil, err
	}
	images, err := a.Images()
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, errors.New("the archive holds no image")
	}
	type entry struct {
		Config   string
		RepoTags []string `json:",omitempty"`
		Layers   []string
	}
	var manifest []entry
	for _, img := range images {
		e := entry{Config: blobPath(img.ConfigDigest)}
		for _, l := range img.Layers {
			e.Layers = append(e.Layers, blobPath(l.Digest))
		}
		// Every entry of one archive would claim the same tag, so only a single-platform image is tagged.
		if len(images) == 1 && repoTag != "" {
			e.RepoTags = []string{repoTag}
		}
		manifest = append(manifest, e)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return &DockerLoadable{src: src, manifest: raw}, nil
}

func (d *DockerLoadable) Write(w io.Writer) error {
	if _, err := d.src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	raw := d.manifest
	tr, tw := tar.NewReader(d.src), tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if strings.TrimPrefix(h.Name, "./") == dockerManifestName {
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: dockerManifestName, Mode: 0o644, Size: int64(len(raw)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(raw); err != nil {
		return err
	}
	return tw.Close()
}

func blobPath(digest string) string {
	alg, hex, _ := strings.Cut(digest, ":")
	return fmt.Sprintf("blobs/%s/%s", alg, hex)
}
