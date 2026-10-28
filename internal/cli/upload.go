package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
	"samuori.invalid/samuori/internal/excludes"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func (e *env) upload(ctx context.Context, args []string) error {
	fs := flags("upload", e)
	project := fs.String("project", "", "project ID")
	var extra multiFlag
	fs.Var(&extra, "exclude", "additional exclusion pattern (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *project == "" || len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori upload --project ID [--exclude PATTERN]... PATH")
	}
	id, err := e.uploadPath(ctx, *project, pos[0], extra)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.out, id)
	return nil
}

// uploadPath uploads a directory as a TAR, or an archive as it is, and returns the snapshot ID.
func (e *env) uploadPath(ctx context.Context, project, path string, extra []string) (string, error) {
	if err := excludes.Validate(extra); err != nil {
		return "", failf(ExitUsage, "%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", failf(ExitUsage, "%v", err)
	}
	archive, files := path, 0
	if info.IsDir() {
		tmp, err := os.CreateTemp("", "samuori-upload-*.tar")
		if err != nil {
			return "", err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		var skipped, left int
		if files, skipped, left, err = writeDirTar(path, tmp.Name(), extra); err != nil {
			return "", err
		}
		e.note("packed %d files (%d excluded, %d links or special files skipped)", files, left, skipped)
		archive = tmp.Name()
	} else if files, err = countEntries(path); err != nil {
		return "", failf(ExitUsage, "%s is not a TAR, TAR.GZ or ZIP archive: %v", path, err)
	}
	if files == 0 {
		return "", failf(ExitUsage, "nothing to upload")
	}
	st, err := os.Stat(archive)
	if err != nil {
		return "", err
	}
	body := apiclient.CreateUploadSessionJSONRequestBody{FileCount: files, TotalBytes: st.Size()}
	if len(extra) > 0 {
		body.ExcludePatterns = &extra
	}
	sess, err := e.api.CreateUploadSessionWithResponse(ctx, project, nil, body)
	if err != nil {
		return "", err
	}
	if sess.JSON201 == nil {
		return "", problem("starting the upload", sess.StatusCode(), sess.Body)
	}
	if err := e.sendChunks(ctx, sess.JSON201.Id, archive, sess.JSON201.ChunkBytes, sess.JSON201.MaxChunks); err != nil {
		return "", err
	}
	commit, err := e.api.CommitUploadWithResponse(ctx, sess.JSON201.Id, &apiclient.CommitUploadParams{IdempotencyKey: newKey("cli-commit")})
	if err != nil {
		return "", err
	}
	if commit.JSON202 == nil {
		return "", problem("committing the upload", commit.StatusCode(), commit.Body)
	}
	for {
		op, err := e.api.GetOperationWithResponse(ctx, commit.JSON202.Id)
		if err != nil {
			return "", err
		}
		if op.JSON200 == nil {
			return "", problem("waiting for the snapshot", op.StatusCode(), op.Body)
		}
		switch op.JSON200.Status {
		case apiclient.OperationStatusSucceeded:
			return *op.JSON200.ResourceId, nil
		case apiclient.OperationStatusFailed:
			msg := "the upload was refused"
			if op.JSON200.Error != nil {
				msg = op.JSON200.Error.Message
			}
			return "", failf(ExitFailed, "%s", msg)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return "", err
		}
	}
}

// sendChunks puts each chunk, retrying transient failures; repeating an index replaces it.
func (e *env) sendChunks(ctx context.Context, id, archive string, chunk int64, max int) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, chunk)
	for i := 0; i < max; i++ {
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		var last error
		for attempt := 0; attempt < 4; attempt++ {
			if attempt > 0 {
				if err := sleep(ctx, time.Duration(attempt)*2*time.Second); err != nil {
					return err
				}
			}
			r, err := e.api.PutUploadChunkWithBodyWithResponse(ctx, id, i, nil, "application/octet-stream", bytes.NewReader(buf[:n]))
			if err != nil {
				last = err
				continue
			}
			if r.StatusCode() == http.StatusNoContent || r.StatusCode() == http.StatusOK {
				last = nil
				break
			}
			last = problem(fmt.Sprintf("sending part %d", i+1), r.StatusCode(), r.Body)
			if r.StatusCode() < 500 && r.StatusCode() != http.StatusTooManyRequests {
				return last
			}
		}
		if last != nil {
			return last
		}
	}
	return nil
}

// writeDirTar packs regular files and directories with the server's exclusion rules. Links and special files are
// skipped, as the server would refuse them. Only the executable bit of the mode is kept.
func writeDirTar(root, out string, extra []string) (files, skipped, excluded int, err error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	tw := tar.NewWriter(bw)
	m := excludes.New(extra)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, hit := m.Match(rel); hit {
			excluded++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX, ModTime: time.Unix(0, 0)})
		case !info.Mode().IsRegular():
			skipped++
			return nil
		}
		mode := int64(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: mode, Size: info.Size(), Format: tar.FormatPAX, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if _, err := io.CopyN(tw, in, info.Size()); err != nil {
			return fmt.Errorf("%s changed while it was read: %w", rel, err)
		}
		files++
		return nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, 0, 0, err
	}
	return files, skipped, excluded, bw.Flush()
}

// countEntries counts regular files in a TAR, gzip-compressed TAR or ZIP archive, detected by magic bytes.
func countEntries(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	switch {
	case bytes.Equal(head, []byte("PK\x03\x04")):
		st, _ := f.Stat()
		z, err := zip.NewReader(f, st.Size())
		if err != nil {
			return 0, err
		}
		n := 0
		for _, e := range z.File {
			if e.Mode().IsRegular() {
				n++
			}
		}
		return n, nil
	case head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		return countTar(gz)
	}
	return countTar(f)
}

func countTar(r io.Reader) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		if h.Typeflag == tar.TypeReg {
			n++
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
