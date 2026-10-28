package rootlesshost

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/isolation"
)

const (
	// cnbAppImage is the name the lifecycle exports the image under inside the build; it never leaves the step.
	cnbAppImage   = "samuori.invalid/app:build"
	cnbAppLayout  = "/layout-repo/samuori.invalid/app/build"
	cnbLayoutDir  = "/layout-repo"
	cnbMetaFile   = "buildpacks-metadata.json"
	cnbSBOMFile   = "buildpacks-sbom.tar"
	maxCNBRecord  = 4 << 20
	maxCNBSBOM    = 32 << 20
	maxCNBEntries = 4096
)

var (
	cnbImageRe   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+(:[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}$`)
	cnbIdentRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	cnbKeyRe     = regexp.MustCompile(`^(\.?[A-Za-z0-9_][A-Za-z0-9_.-]{0,126})$`)
	cnbEnvRe     = regexp.MustCompile(`^(BP|BPE|BPL)_[A-Z0-9_]{1,120}$`)
	cnbAPIRe     = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	cnbSBOMNames = regexp.MustCompile(`^(launch|build)(/[A-Za-z0-9_.-]+){1,4}$`)
)

// BuildpacksDockerfile renders the internal Dockerfile that runs the CNB lifecycle (ADR 0047): the approved builder
// runs creator as its CNB user with the source, the platform directory and the run image as an OCI layout, and the
// final stage holds only the exported layout, report and SBOM. Every value is validated before it is written; the
// non-secret env values and binding types live in the platform context, never in this text.
func BuildpacksDockerfile(bp *buildplan.PlanBuildpacks) ([]byte, error) {
	if err := checkBuildpacks(bp); err != nil {
		return nil, err
	}
	runRef, runPath, err := runLayout(bp.RunImage)
	if err != nil {
		return nil, err
	}
	owner := "--chown=${CNB_USER_ID}:${CNB_GROUP_ID}"
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s AS samuori-build\n", bp.Builder)
	// Export to an OCI layout is an experimental lifecycle feature; warn mode allows it and says so in the log.
	fmt.Fprintf(&b, "ENV CNB_PLATFORM_API=%s CNB_EXPERIMENTAL_MODE=warn\n", bp.PlatformAPI)
	fmt.Fprintf(&b, "COPY --from=samuoriplatform %s / /platform/\n", owner)
	fmt.Fprintf(&b, "COPY --from=samuorirun %s / %s/\n", owner, runPath)
	fmt.Fprintf(&b, "COPY %s . /workspace/\n", owner)
	b.WriteString("USER ${CNB_USER_ID}:${CNB_GROUP_ID}\n")
	b.WriteString(`RUN ["/bin/sh", "-c", "[ \"$(id -u)\" -ne 0 ] || { echo 'the builder must run the lifecycle as a non-root CNB user' >&2; exit 1; }"]` + "\n")
	b.WriteString("RUN")
	for _, bd := range bp.Bindings {
		fmt.Fprintf(&b, " --mount=type=secret,id=%s,target=/platform/bindings/%s/%s,uid=${CNB_USER_ID},gid=${CNB_GROUP_ID},mode=0400,required=true", bd.SecretID, bd.Name, bd.Key)
	}
	argv, _ := json.Marshal([]string{"/cnb/lifecycle/creator", "-app=/workspace", "-layers=/layers", "-platform=/platform", "-layout", "-layout-dir=" + cnbLayoutDir,
		"-run-image=" + runRef, "-report=/layers/report.toml", "-log-level=info", cnbAppImage})
	fmt.Fprintf(&b, " %s\n", argv)
	b.WriteString(`RUN ["mkdir", "-p", "/layers/sbom"]` + "\n")
	b.WriteString("FROM scratch\n")
	fmt.Fprintf(&b, "COPY --from=samuori-build %s/ /layout/\n", cnbAppLayout)
	b.WriteString("COPY --from=samuori-build /layers/report.toml /report.toml\n")
	b.WriteString("COPY --from=samuori-build /layers/sbom/ /sbom/\n")
	return []byte(b.String()), nil
}

func checkBuildpacks(bp *buildplan.PlanBuildpacks) error {
	switch {
	case bp == nil:
		return errors.New("the plan names the buildpacks method without a buildpacks pin")
	case !cnbImageRe.MatchString(bp.Builder) || !cnbImageRe.MatchString(bp.RunImage):
		return errors.New("the builder and run image must be image references pinned by digest")
	case !cnbAPIRe.MatchString(bp.PlatformAPI):
		return fmt.Errorf("%q is not a CNB platform API version", bp.PlatformAPI)
	case !filepath.IsLocal(filepath.FromSlash(bp.Context)) && bp.Context != ".":
		return errors.New("the buildpacks context must stay inside the source")
	}
	for k, v := range bp.Env {
		if !cnbEnvRe.MatchString(k) || strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("buildpacks variable %q is not allowed", k)
		}
	}
	for _, bd := range bp.Bindings {
		if !cnbIdentRe.MatchString(bd.Name) || !cnbIdentRe.MatchString(bd.Type) || !cnbIdentRe.MatchString(bd.SecretID) || !cnbKeyRe.MatchString(bd.Key) || bd.Key == "type" || bd.Key == "provider" {
			return fmt.Errorf("binding %q is not valid", bd.Name)
		}
	}
	return nil
}

// runLayout returns the run image reference the lifecycle is given, without its tag, and the layout path the
// lifecycle reads it from: <layout-dir>/<registry>/<repository>/sha256/<hex> (buildpacks RFC 0119).
func runLayout(ref string) (string, string, error) {
	base, digest, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(base, ":"); i > strings.LastIndex(base, "/") {
		base = base[:i]
	}
	d, err := name.NewDigest(base+"@"+digest, name.StrictValidation)
	if err != nil {
		return "", "", err
	}
	algo, hexpart, _ := strings.Cut(d.DigestStr(), ":")
	return d.String(), path.Join(cnbLayoutDir, d.Context().RegistryStr(), d.Context().RepositoryStr(), algo, hexpart), nil
}

// buildpacksRun is one attempt's buildpacks build; everything lives under dir, which Destroy removes.
type buildpacksRun struct {
	p    *Provider
	e    *env
	plan buildplan.Plan
	bp   *buildplan.PlanBuildpacks
	dir  string
}

// buildpacks pulls the run image into an OCI layout with a first solve in this attempt's daemon, writes the platform
// directory and the lifecycle Dockerfile, and points req at them. The caller runs the lifecycle solve.
func (p *Provider) buildpacks(ctx context.Context, e *env, plan buildplan.Plan, req *buildkit.Request, outDir string) (*buildpacksRun, string, error) {
	bp := plan.Buildpacks
	if p.cfg.Engine != engine.BuildKit {
		return nil, "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: "buildpacks builds need the BuildKit engine"}
	}
	df, err := BuildpacksDockerfile(bp)
	if err != nil {
		return nil, "", &isolation.Failure{Class: isolation.ErrUnsupported, Detail: err.Error()}
	}
	r := &buildpacksRun{p: p, e: e, plan: plan, bp: bp, dir: filepath.Join(outDir, "cnb")}
	for _, d := range []string{"", "pull", "run", "df", "out", "platform/env", "platform/bindings"} {
		if err := os.MkdirAll(filepath.Join(r.dir, d), 0o700); err != nil {
			return nil, "", infra(err)
		}
	}
	for k, v := range bp.Env {
		if err := os.WriteFile(filepath.Join(r.dir, "platform", "env", k), []byte(v), 0o600); err != nil {
			return nil, "", infra(err)
		}
	}
	for _, bd := range bp.Bindings {
		bdir := filepath.Join(r.dir, "platform", "bindings", bd.Name)
		if err := os.MkdirAll(bdir, 0o700); err != nil {
			return nil, "", infra(err)
		}
		if err := os.WriteFile(filepath.Join(bdir, "type"), []byte(bd.Type), 0o600); err != nil {
			return nil, "", infra(err)
		}
	}
	pullLog, err := r.pullRunImage(ctx, req)
	if err != nil {
		return nil, pullLog, err
	}
	if err := os.WriteFile(filepath.Join(r.dir, "df", "Dockerfile"), df, 0o600); err != nil {
		return nil, pullLog, infra(err)
	}
	req.ContextDir = filepath.Join(e.spec.SourceDir, filepath.FromSlash(bp.Context))
	req.DockerfileDir, req.Filename, req.Target = filepath.Join(r.dir, "df"), "Dockerfile", ""
	req.NamedContexts = map[string]string{"samuoriplatform": filepath.Join(r.dir, "platform"), "samuorirun": filepath.Join(r.dir, "run")}
	req.OutputDir = filepath.Join(r.dir, "out")
	req.Platforms = plan.Platforms
	req.BuildArgs = nil
	// The lifecycle's layers and both base images would fill a BuildKit cache that only an identical rebuild reuses;
	// buildpacks builds keep no cache yet (docs/architecture/build-cache.md).
	req.CacheImportDir, req.CacheExportDir = "", ""
	return r, pullLog, nil
}

// pullRunImage exports the approved run image as an OCI layout through this attempt's daemon, so it is pulled by
// digest through the same mirrors and limits as every other image.
func (r *buildpacksRun) pullRunImage(ctx context.Context, req *buildkit.Request) (string, error) {
	dir := filepath.Join(r.dir, "pull")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+r.bp.RunImage+"\n"), 0o600); err != nil {
		return "", infra(err)
	}
	archive := filepath.Join(r.dir, "run.tar")
	pull := buildkit.Request{ContextDir: dir, DockerfileDir: dir, Filename: "Dockerfile", Platforms: r.plan.Platforms, OutputTar: archive,
		MirrorHosts: req.MirrorHosts, MirrorToken: req.MirrorToken}
	res, err := r.e.runner.Build(ctx, pull)
	log := "--- run image ---\n" + res.Log
	if err != nil {
		return log, fmt.Errorf("%w: the run image %s could not be pulled", engine.ErrBuild, r.bp.RunImage)
	}
	if err := unpackLayout(archive, filepath.Join(r.dir, "run")); err != nil {
		return log, infra(err)
	}
	if err := os.Remove(archive); err != nil {
		return log, infra(err)
	}
	return log, nil
}

// finish turns the exported layout into the OCI archive every method produces and returns the lineage records.
func (r *buildpacksRun) finish(archive string) ([]isolation.Record, error) {
	out := filepath.Join(r.dir, "out")
	if err := PackLayout(filepath.Join(out, "layout"), archive); err != nil {
		return nil, fmt.Errorf("%w: the lifecycle's OCI layout is not usable: %v", engine.ErrBuild, err)
	}
	meta, err := BuildpacksMetadata(archive, r.bp, out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", engine.ErrBuild, err)
	}
	rec, err := writeRecord(filepath.Join(r.dir, cnbMetaFile), meta, "buildpacks-metadata")
	if err != nil {
		return nil, err
	}
	records := []isolation.Record{rec}
	sbom := filepath.Join(r.dir, cnbSBOMFile)
	n, err := packSBOM(filepath.Join(out, "sbom"), sbom)
	if err != nil {
		return nil, infra(err)
	}
	if n > 0 {
		digest, err := fileDigest(sbom)
		if err != nil {
			return nil, infra(err)
		}
		records = append(records, isolation.Record{Kind: "buildpacks-sbom", File: sbom, Digest: digest})
	}
	return records, nil
}

var blobNameRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// PackLayout writes the OCI layout in dir as an image archive. It accepts only layout files, one image manifest,
// and blobs whose content matches their name, so the archive is exactly what the lifecycle exported.
func PackLayout(dir, archive string) error {
	index, err := readSmall(filepath.Join(dir, "index.json"))
	if err != nil {
		return err
	}
	var idx struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	if len(idx.Manifests) != 1 || idx.Manifests[0].MediaType != "application/vnd.oci.image.manifest.v1+json" {
		return errors.New("the layout must hold exactly one OCI image manifest")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		return err
	}
	blobs := map[string]bool{}
	for _, de := range entries {
		if !de.Type().IsRegular() || !blobNameRe.MatchString(de.Name()) {
			return fmt.Errorf("unexpected entry blobs/sha256/%s", de.Name())
		}
		blobs[de.Name()] = true
	}
	man, err := readSmall(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(idx.Manifests[0].Digest, "sha256:")))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var m struct {
		Config struct{ Digest string }   `json:"config"`
		Layers []struct{ Digest string } `json:"layers"`
	}
	if err := json.Unmarshal(man, &m); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	for _, d := range append([]string{idx.Manifests[0].Digest, m.Config.Digest}, func() []string {
		var l []string
		for _, x := range m.Layers {
			l = append(l, x.Digest)
		}
		return l
	}()...) {
		if !blobs[strings.TrimPrefix(d, "sha256:")] {
			return fmt.Errorf("the layout has no blob %s", d)
		}
	}
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	err = func() error {
		if err := tarBytes(tw, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
			return err
		}
		if err := tarBytes(tw, "index.json", index); err != nil {
			return err
		}
		names := slices.Sorted(func(yield func(string) bool) {
			for n := range blobs {
				if !yield(n) {
					return
				}
			}
		})
		for _, n := range names {
			if err := tarBlob(tw, filepath.Join(dir, "blobs", "sha256", n), n); err != nil {
				return err
			}
		}
		return tw.Close()
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func readSmall(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCNBRecord+1))
	if err == nil && len(raw) > maxCNBRecord {
		err = errors.New(filepath.Base(p) + " is too large")
	}
	return raw, err
}

func tarBytes(tw *tar.Writer, name string, raw []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(raw)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(raw)
	return err
}

// tarBlob copies one blob into the archive and fails if its content does not match its name.
func tarBlob(tw *tar.Writer, p, hexName string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/sha256/" + hexName, Mode: 0o644, Size: st.Size(), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tw, h), f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != hexName {
		return fmt.Errorf("blob %s does not match its digest", hexName)
	}
	return nil
}

// BuildpacksMetadata is the lineage record of a buildpacks build: the pins it ran, the image's buildpacks labels
// (buildpacks, processes, launcher and layer metadata) and the lifecycle report.
func BuildpacksMetadata(archive string, bp *buildplan.PlanBuildpacks, out string) ([]byte, error) {
	cfg, manifest, err := layoutImageConfig(archive)
	if err != nil {
		return nil, err
	}
	var c struct {
		Config struct {
			User       string            `json:"User"`
			Entrypoint []string          `json:"Entrypoint"`
			WorkingDir string            `json:"WorkingDir"`
			Labels     map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, err
	}
	label := func(k string) json.RawMessage {
		if v := c.Config.Labels[k]; json.Valid([]byte(v)) {
			return json.RawMessage(v)
		}
		return nil
	}
	build := label("io.buildpacks.build.metadata")
	if build == nil {
		return nil, errors.New("the image has no io.buildpacks.build.metadata label; the lifecycle did not export it")
	}
	report, _ := readSmall(filepath.Join(out, "report.toml"))
	rec := map[string]any{
		"builder": bp.Builder, "runImage": bp.RunImage, "platformApi": bp.PlatformAPI, "lifecycleVersion": bp.Version,
		"image":             map[string]any{"manifest": manifest, "user": c.Config.User, "entrypoint": c.Config.Entrypoint, "workingDir": c.Config.WorkingDir},
		"buildMetadata":     build,
		"lifecycleMetadata": label("io.buildpacks.lifecycle.metadata"),
		"projectMetadata":   label("io.buildpacks.project.metadata"),
		"report":            string(report),
	}
	raw, err := json.Marshal(rec)
	if err == nil && len(raw) > maxCNBRecord {
		err = errors.New("the buildpacks metadata exceeds 4 MiB")
	}
	return raw, err
}

func layoutImageConfig(archive string) ([]byte, string, error) {
	files, err := readArchive(archive, func(n string) bool { return n == "index.json" })
	if err != nil {
		return nil, "", err
	}
	var idx struct {
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &idx); err != nil || len(idx.Manifests) != 1 {
		return nil, "", errors.New("the archive has no single manifest")
	}
	md := idx.Manifests[0].Digest
	files, err = readArchive(archive, func(n string) bool { return n == "blobs/sha256/"+strings.TrimPrefix(md, "sha256:") })
	if err != nil {
		return nil, "", err
	}
	var m struct {
		Config struct{ Digest string } `json:"config"`
	}
	if err := json.Unmarshal(files["blobs/sha256/"+strings.TrimPrefix(md, "sha256:")], &m); err != nil {
		return nil, "", err
	}
	cname := "blobs/sha256/" + strings.TrimPrefix(m.Config.Digest, "sha256:")
	files, err = readArchive(archive, func(n string) bool { return n == cname })
	if err != nil {
		return nil, "", err
	}
	if files[cname] == nil {
		return nil, "", errors.New("the archive has no image configuration")
	}
	return files[cname], md, nil
}

func readArchive(archive string, want func(string) bool) (map[string][]byte, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || !want(h.Name) {
			continue
		}
		if h.Size > maxCNBRecord {
			return nil, errors.New(h.Name + " is too large")
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[h.Name] = raw
	}
}

// packSBOM archives the lifecycle's SBOM files (launch and build, per buildpack) as one lineage record. Only regular
// files with plain names are kept; the total is bounded.
func packSBOM(dir, archive string) (int, error) {
	var names []string
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !cnbSBOMNames.MatchString(rel) {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if total += st.Size(); total > maxCNBSBOM || len(names) >= maxCNBEntries {
			return errors.New("the buildpacks SBOM exceeds its limit")
		}
		names = append(names, rel)
		return nil
	})
	if err != nil || len(names) == 0 {
		return 0, err
	}
	slices.Sort(names)
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(f)
	for _, n := range names {
		raw, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(n)))
		if rerr == nil {
			rerr = tarBytes(tw, n, raw)
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return len(names), err
}
