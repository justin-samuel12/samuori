// Package gogit fetches one commit over smart HTTP with go-git plumbing (ADR 0033). It uses no worktree, Git config,
// hooks or global protocol registry; every connection goes through the address guard (SEC-NET-03).
package gogit

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"samuori.invalid/samuori/internal/netguard"
	"samuori.invalid/samuori/internal/pipelines"
)

// Fetcher keeps object storage in a temporary directory under Dir while it builds each archive.
type Fetcher struct {
	Dir     string
	Timeout time.Duration
}

func fail(code, format string, args ...any) error {
	return &pipelines.FetchError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// countingTransport fails responses once their bodies exceed the fetch's byte budget.
type countingTransport struct {
	next http.RoundTripper
	left *atomic.Int64
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &countingBody{ReadCloser: resp.Body, left: t.left}
	return resp, nil
}

var errTooLarge = errors.New("the repository download exceeds the size limit")

type countingBody struct {
	io.ReadCloser
	left *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.left.Add(-int64(n)) < 0 {
		return n, errTooLarge
	}
	return n, err
}

func (f Fetcher) Fetch(ctx context.Context, r pipelines.FetchRequest, archive string) (pipelines.Fetched, error) {
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return pipelines.Fetched{}, fail("invalid_url", "the repository URL must be https without credentials")
	}
	if !pipelines.ValidRef(r.Ref) {
		return pipelines.Fetched{}, fail("invalid_ref", "the ref is not a branch or tag")
	}
	timeout := f.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var left atomic.Int64
	left.Store(r.MaxBytes)
	client := netguard.Client(r.AllowPrivateNetwork, timeout, 0)
	client.Transport = countingTransport{next: client.Transport, left: &left}
	ep, err := transport.NewEndpoint(r.URL)
	if err != nil {
		return pipelines.Fetched{}, fail("invalid_url", "the repository URL is not valid")
	}
	var auth transport.AuthMethod
	if r.Token != "" {
		user := r.Username
		if user == "" {
			user = "git"
		}
		auth = &githttp.BasicAuth{Username: user, Password: r.Token}
	}
	sess, err := githttp.NewClient(client).NewUploadPackSession(ep, auth)
	if err != nil {
		return pipelines.Fetched{}, describe(err)
	}
	defer sess.Close()
	adv, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return pipelines.Fetched{}, describe(err)
	}
	refs, err := adv.AllReferences()
	if err != nil {
		return pipelines.Fetched{}, describe(err)
	}
	ref, ok := refs[plumbing.ReferenceName(r.Ref)]
	if !ok || ref.Type() != plumbing.HashReference {
		return pipelines.Fetched{}, fail("ref_not_found", "the repository has no %s", r.Ref)
	}
	want := ref.Hash()
	if peeled, ok := adv.Peeled[r.Ref]; ok {
		want = peeled
	}

	req := packp.NewUploadPackRequestFromCapabilities(adv.Capabilities)
	req.Wants = []plumbing.Hash{want}
	if adv.Capabilities.Supports(capability.Shallow) {
		req.Depth = packp.DepthCommits(1)
		_ = req.Capabilities.Set(capability.Shallow)
	}
	if adv.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		return pipelines.Fetched{}, describe(err)
	}
	defer resp.Close()

	dir, err := os.MkdirTemp(f.Dir, "git-")
	if err != nil {
		return pipelines.Fetched{}, err
	}
	defer os.RemoveAll(dir)
	store := filesystem.NewStorage(osfs.New(dir), cache.NewObjectLRUDefault())
	var pack io.Reader = resp
	switch {
	case req.Capabilities.Supports(capability.Sideband64k):
		pack = sideband.NewDemuxer(sideband.Sideband64k, resp)
	case req.Capabilities.Supports(capability.Sideband):
		pack = sideband.NewDemuxer(sideband.Sideband, resp)
	}
	if err := packfile.UpdateObjectStorage(store, pack); err != nil {
		return pipelines.Fetched{}, describe(err)
	}
	commit, err := object.GetCommit(store, want)
	if err != nil {
		return pipelines.Fetched{}, fail("not_a_commit", "%s does not point to a commit", r.Ref)
	}
	tree, err := commit.Tree()
	if err != nil {
		return pipelines.Fetched{}, describe(err)
	}
	skipped, err := writeTar(store, tree, archive, r.MaxBytes, r.MaxFiles)
	if err != nil {
		return pipelines.Fetched{}, err
	}
	return pipelines.Fetched{Commit: want.String(), Skipped: skipped}, nil
}

// writeTar writes the tree with file modes reduced to executable or not. Symbolic links are written as links so
// snapshot entry rules refuse and count them like an upload's; submodules are skipped and counted.
func writeTar(store storer.EncodedObjectStorer, tree *object.Tree, archive string, maxBytes int64, maxFiles int) (int, error) {
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	var written int64
	files, skipped := 0, 0
	for {
		name, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, describe(err)
		}
		hdr := &tar.Header{Name: name, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)}
		var blob *object.Blob
		switch entry.Mode {
		case filemode.Dir:
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, name+"/", 0o755
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			if blob, err = object.GetBlob(store, entry.Hash); err != nil {
				return 0, describe(err)
			}
			hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeReg, blob.Size, 0o644
			if entry.Mode == filemode.Executable {
				hdr.Mode = 0o755
			}
			if files++; files > maxFiles {
				return 0, fail("too_many_files", "the commit has more than %d files", maxFiles)
			}
		case filemode.Symlink:
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, "target", 0o777
		default:
			skipped++
			continue
		}
		if written += hdr.Size + 1024; written > maxBytes {
			return 0, fail("too_large", "the commit is larger than the %d MiB limit", maxBytes>>20)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return 0, err
		}
		if blob != nil {
			rd, err := blob.Reader()
			if err != nil {
				return 0, describe(err)
			}
			_, err = io.Copy(tw, rd)
			rd.Close()
			if err != nil {
				return 0, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	return skipped, out.Close()
}

// describe turns transport and storage errors into messages that name the problem without URLs or credentials.
func describe(err error) error {
	var fe *pipelines.FetchError
	switch {
	case errors.As(err, &fe):
		return fe
	case errors.Is(err, errTooLarge):
		return fail("too_large", "the repository download exceeds the size limit")
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		return fail("auth_failed", "the repository refused the credentials")
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fail("not_found", "the repository was not found or the credentials cannot read it")
	case errors.Is(err, netguard.ErrBlocked):
		return fail("blocked_address", "the repository address is not allowed")
	case errors.Is(err, context.DeadlineExceeded):
		return fail("timeout", "fetching the repository took too long")
	}
	msg := err.Error()
	if strings.Contains(msg, "redirect") {
		return fail("redirect", "the repository answered with a redirect, which is not followed")
	}
	return fail("fetch_failed", "fetching the repository failed")
}
