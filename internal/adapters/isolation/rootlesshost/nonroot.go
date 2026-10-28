package rootlesshost

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
	"sort"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/imagescan"
)

const (
	mediaOCIIndex    = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaLayerGzip   = "application/vnd.oci.image.layer.v1.tar+gzip"
	maxOwnedDirs     = 200_000
)

// rootUser reports whether an image configuration's User runs as root.
func rootUser(u string) bool {
	name, _, _ := strings.Cut(u, ":")
	return name == "" || name == "0" || name == "root"
}

func parseUser(u string) (int, int, error) {
	name, group, ok := strings.Cut(u, ":")
	uid, err1 := strconv.Atoi(name)
	gid, err2 := strconv.Atoi(group)
	if !ok || err1 != nil || err2 != nil || uid <= 0 || gid <= 0 {
		return 0, 0, fmt.Errorf("the non-root user %q must be numeric uid:gid", u)
	}
	return uid, gid, nil
}

// NonRootImage adds a final layer to every image in the archive whose configuration runs as root: the working
// directory and the directories below it become owned by user (numeric uid:gid), and the configuration's User is
// set to it. Files keep their owner, so the application can create files but not change the ones it shipped with
// (ADR 0047, SEC-RPK-08). Images that already name a non-root user are left as they are.
func NonRootImage(archive, user string) error {
	uid, gid, err := parseUser(user)
	if err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	a, err := imagescan.Open(f, imagescan.DefaultLimits().MaxEntries)
	if err != nil {
		return err
	}
	rw := &nonRootRewrite{a: a, user: user, uid: uid, gid: gid, blobs: map[string][]byte{}, replaced: map[string]bool{}}
	index, err := a.Document("")
	if err != nil {
		return err
	}
	newIndex, err := rw.index(index, 0)
	if err != nil || len(rw.replaced) == 0 {
		return err
	}
	tmp := archive + ".nonroot"
	if err := rw.write(f, tmp, newIndex); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, archive)
}

type nonRootRewrite struct {
	a        *imagescan.Archive
	user     string
	uid, gid int
	blobs    map[string][]byte
	replaced map[string]bool
}

func (rw *nonRootRewrite) put(raw []byte) (string, int64) {
	sum := sha256.Sum256(raw)
	d := "sha256:" + hex.EncodeToString(sum[:])
	rw.blobs[d] = raw
	return d, int64(len(raw))
}

func str(m map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

func setJSON(m map[string]json.RawMessage, k string, v any) {
	raw, _ := json.Marshal(v)
	m[k] = raw
}

// index rewrites an image index (or index.json) and returns it; depth bounds nested indexes.
func (rw *nonRootRewrite) index(raw []byte, depth int) ([]byte, error) {
	if depth > 2 {
		return nil, errors.New("the image index is nested too deeply")
	}
	var idx map[string]json.RawMessage
	var descs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("image index: %w", err)
	}
	if err := json.Unmarshal(idx["manifests"], &descs); err != nil || len(descs) == 0 || len(descs) > 64 {
		return nil, errors.New("the image index lists no usable manifests")
	}
	for _, d := range descs {
		digest := str(d, "digest")
		body, err := rw.a.Document(digest)
		if err != nil {
			return nil, err
		}
		var out []byte
		switch str(d, "mediaType") {
		case mediaOCIIndex:
			out, err = rw.index(body, depth+1)
		case mediaOCIManifest:
			var p struct{ OS string }
			_ = json.Unmarshal(d["platform"], &p)
			if p.OS == "unknown" {
				return nil, errors.New("the image has attestation manifests, which the non-root layer would invalidate")
			}
			out, err = rw.manifest(body)
		default:
			return nil, fmt.Errorf("unsupported manifest media type %q", str(d, "mediaType"))
		}
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(out, body) {
			rw.replaced[digest] = true
			nd, size := rw.put(out)
			setJSON(d, "digest", nd)
			setJSON(d, "size", size)
		}
	}
	setJSON(idx, "manifests", descs)
	return json.Marshal(idx)
}

func (rw *nonRootRewrite) manifest(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	var cfgDesc map[string]json.RawMessage
	var layers []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if json.Unmarshal(m["config"], &cfgDesc) != nil || json.Unmarshal(m["layers"], &layers) != nil {
		return nil, errors.New("the manifest has no config or layers")
	}
	cfgDigest := str(cfgDesc, "digest")
	cfgRaw, err := rw.a.Document(cfgDigest)
	if err != nil {
		return nil, err
	}
	var cfg, inner, rootfs map[string]json.RawMessage
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	if c, ok := cfg["config"]; ok && json.Unmarshal(c, &inner) != nil {
		return nil, errors.New("the image config is malformed")
	}
	if inner == nil {
		inner = map[string]json.RawMessage{}
	}
	if !rootUser(str(inner, "User")) {
		return raw, nil
	}
	if json.Unmarshal(cfg["rootfs"], &rootfs) != nil {
		return nil, errors.New("the image config has no rootfs")
	}
	var diffIDs []string
	var history []map[string]json.RawMessage
	_ = json.Unmarshal(rootfs["diff_ids"], &diffIDs)
	_ = json.Unmarshal(cfg["history"], &history)
	entry := map[string]json.RawMessage{}
	setJSON(entry, "created_by", "samuori: USER "+rw.user+" (ADR 0047)")
	workdir := path.Clean("/" + str(inner, "WorkingDir"))
	if workdir != "/" {
		dirs, err := rw.directories(layers, workdir)
		if err != nil {
			return nil, err
		}
		blob, diffID, err := rw.ownedLayer(workdir, dirs)
		if err != nil {
			return nil, err
		}
		d, size := rw.put(blob)
		layer := map[string]json.RawMessage{}
		setJSON(layer, "mediaType", mediaLayerGzip)
		setJSON(layer, "digest", d)
		setJSON(layer, "size", size)
		layers = append(layers, layer)
		diffIDs = append(diffIDs, diffID)
		setJSON(entry, "comment", "directories under "+workdir+" owned by "+rw.user)
	} else {
		setJSON(entry, "empty_layer", true)
	}
	setJSON(inner, "User", rw.user)
	setJSON(rootfs, "diff_ids", diffIDs)
	setJSON(cfg, "config", inner)
	setJSON(cfg, "rootfs", rootfs)
	setJSON(cfg, "history", append(history, entry))
	newCfg, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	rw.replaced[cfgDigest] = true
	nd, size := rw.put(newCfg)
	setJSON(cfgDesc, "digest", nd)
	setJSON(cfgDesc, "size", size)
	setJSON(m, "config", cfgDesc)
	setJSON(m, "layers", layers)
	return json.Marshal(m)
}

// directories lists the directories at or below workdir in the image's merged filesystem with their modes,
// applying whiteouts layer by layer. Symbolic links are not followed.
func (rw *nonRootRewrite) directories(layers []map[string]json.RawMessage, workdir string) (map[string]int64, error) {
	state := map[string]int64{}
	under := func(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }
	remove := func(dir string, self bool) {
		for p := range state {
			if (self && p == dir) || strings.HasPrefix(p, dir+"/") {
				delete(state, p)
			}
		}
	}
	for _, l := range layers {
		d := imagescan.Descriptor{MediaType: str(l, "mediaType"), Digest: str(l, "digest")}
		r, err := rw.a.Layer(d)
		if err != nil {
			return nil, err
		}
		var plain io.Reader
		switch {
		case strings.HasSuffix(d.MediaType, "tar+gzip") || strings.HasSuffix(d.MediaType, "tar.gzip"):
			zr, err := gzip.NewReader(r)
			if err != nil {
				return nil, err
			}
			plain = zr
		case strings.HasSuffix(d.MediaType, ".tar") || strings.HasSuffix(d.MediaType, "layer.v1.tar"):
			plain = r
		default:
			return nil, fmt.Errorf("unsupported layer media type %s", d.MediaType)
		}
		var opaque, gone []string
		dirs := map[string]int64{}
		tr := tar.NewReader(plain)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading layer %s: %w", d.Digest, err)
			}
			p := path.Clean("/" + h.Name)
			base := path.Base(p)
			switch {
			case base == ".wh..wh..opq":
				opaque = append(opaque, path.Dir(p))
			case strings.HasPrefix(base, ".wh."):
				gone = append(gone, path.Join(path.Dir(p), strings.TrimPrefix(base, ".wh.")))
			case h.Typeflag == tar.TypeDir:
				if under(p, workdir) {
					dirs[p] = h.Mode
				}
			default:
				gone = append(gone, p)
			}
		}
		for _, o := range opaque {
			remove(o, false)
		}
		for _, g := range gone {
			remove(g, true)
		}
		for p, mode := range dirs {
			state[p] = mode
		}
		if len(state) > maxOwnedDirs {
			return nil, fmt.Errorf("more than %d directories under %s", maxOwnedDirs, workdir)
		}
	}
	return state, nil
}

// ownedLayer writes a gzip layer holding only directory entries, owned by the non-root user. It returns the
// compressed blob and its diff ID.
func (rw *nonRootRewrite) ownedLayer(workdir string, dirs map[string]int64) ([]byte, string, error) {
	if _, ok := dirs[workdir]; !ok {
		dirs[workdir] = 0o755
	}
	paths := make([]string, 0, len(dirs))
	for p := range dirs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var plain, compressed bytes.Buffer
	tw := tar.NewWriter(&plain)
	for _, p := range paths {
		h := &tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(p, "/") + "/", Mode: dirs[p]&0o7777 | 0o700, Uid: rw.uid, Gid: rw.gid,
			ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(plain.Bytes())
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(plain.Bytes()); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return compressed.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}

// write copies the archive with the new index.json and blobs, leaving out the documents they replace.
func (rw *nonRootRewrite) write(src *os.File, dst string, index []byte) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "index.json" || (strings.HasPrefix(name, "blobs/sha256/") && rw.replaced["sha256:"+strings.TrimPrefix(name, "blobs/sha256/")]) {
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	digests := make([]string, 0, len(rw.blobs))
	for d := range rw.blobs {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	for _, d := range digests {
		if err := tarBytes(tw, "blobs/sha256/"+strings.TrimPrefix(d, "sha256:"), rw.blobs[d]); err != nil {
			return err
		}
	}
	if err := tarBytes(tw, "index.json", index); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return out.Close()
}
