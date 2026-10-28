// Package toolrun runs one approved provider tool inside a conversion step with no network (ADR 0048). It starts as
// the step's first process, confines itself as the runtime probe does, runs the tool as an unprivileged user with an
// empty environment and a time limit, and always writes a result so a failing tool never looks like a broken
// runner (docs/architecture/kubernetes-conversion.md#execution).
package toolrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/convertjob"
)

// ToolUID runs the tool; it owns only the work and output directories.
const ToolUID = 65534

type Spec struct {
	Args           []string          `json:"args"`
	Mkdirs         []string          `json:"mkdirs"`
	Files          map[string]string `json:"files,omitempty"`
	Keep           []string          `json:"keep,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
}

type Result struct {
	ExitCode   int    `json:"exitCode"`
	TimedOut   bool   `json:"timedOut"`
	Error      string `json:"error,omitempty"`
	Network    string `json:"network,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// SpecFile is the run specification's name in the step's read-only tool directory.
const SpecFile = "spec.json"

// Main is the entry point for "tool-run <spec JSON>" or "tool-run @<spec file>" inside the step.
func Main(arg string) int {
	var spec Spec
	res := Result{ExitCode: -1}
	out := convertjob.OutputRoot
	raw := []byte(arg)
	if name, ok := strings.CutPrefix(arg, "@"); ok {
		raw, _ = readCapped(name, 256<<10)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		res.Error = "invalid tool specification"
	} else if c, err := confine(); err != nil {
		res.Error = "the conversion sandbox is not as required: " + err.Error()
	} else {
		res = execute(spec, out, "/work", true)
		res.Network = c
	}
	if err := writeMeta(out, res, nil, nil, false); err != nil {
		fmt.Fprintln(os.Stderr, "tool-run:", err)
		return 1
	}
	return 0
}

type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// execute runs the tool with an empty environment in work/run, with HOME and TMPDIR at work/tmp beside it (a
// temporary directory below the working directory breaks Move2Kube's path handling). As root in the step it hands
// those and the output directories to ToolUID, writes the job's files to work/files owned by root and readable
// only, and drops to that user.
func execute(spec Spec, out, work string, drop bool) (res Result) {
	res = Result{ExitCode: -1}
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()
	if len(spec.Args) == 0 {
		res.Error = "no tool to run"
		return res
	}
	if err := convertjob.ValidFiles(spec.Files); err != nil {
		res.Error = err.Error()
		return res
	}
	tmp, cwd, files := filepath.Join(work, "tmp"), filepath.Join(work, "run"), filepath.Join(work, "files")
	if err := os.MkdirAll(files, 0o755); err != nil {
		res.Error = "preparing directories: " + err.Error()
		return res
	}
	for name, c := range spec.Files {
		if err := os.WriteFile(filepath.Join(files, name), []byte(c), 0o444); err != nil {
			res.Error = "writing configuration: " + err.Error()
			return res
		}
	}
	dirs := []string{out, filepath.Join(out, convertjob.MetaDir), tmp, cwd}
	for _, d := range spec.Mkdirs {
		if !convertjob.LocalPath(d) {
			res.Error = "invalid output directory"
			return res
		}
		dirs = append(dirs, filepath.Join(out, filepath.FromSlash(d)))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			res.Error = "preparing directories: " + err.Error()
			return res
		}
		if drop {
			if err := os.Chown(d, ToolUID, ToolUID); err != nil {
				res.Error = "preparing directories: " + err.Error()
				return res
			}
		}
	}
	timeout := time.Duration(min(max(spec.TimeoutSeconds, 1), convertjob.MaxTimeout)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stdout, stderr := &capped{max: convertjob.MaxLogBytes}, &capped{max: convertjob.MaxLogBytes}
	cmd := exec.CommandContext(ctx, spec.Args[0], spec.Args[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, stdout, stderr
	cmd.Env = []string{"HOME=" + tmp, "TMPDIR=" + tmp, "PATH=/nonexistent"}
	if drop {
		cmd.SysProcAttr = credential(ToolUID)
	}
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		res.TimedOut, res.Error = true, fmt.Sprintf("the tool did not finish within %s", timeout)
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	case err != nil:
		res.Error = "the tool did not start: " + err.Error()
	default:
		res.ExitCode = 0
	}
	res.DurationMs = time.Since(start).Milliseconds()
	if perr := prune(out, spec.Keep); perr != nil && res.Error == "" {
		res.Error = "removing unrequested output: " + perr.Error()
	}
	if werr := writeMeta(out, res, stdout.buf.Bytes(), stderr.buf.Bytes(), true); werr != nil && res.Error == "" {
		res.Error = werr.Error()
	}
	return res
}

// prune removes everything below out that is not a kept path, an ancestor of one, or the runner's MetaDir, without
// following symbolic links. With no kept paths it keeps everything.
func prune(out string, keep []string) error {
	if len(keep) == 0 {
		return nil
	}
	kept := func(rel string) (whole, ancestor bool) {
		for _, k := range keep {
			if rel == k || strings.HasPrefix(rel, k+"/") {
				return true, false
			}
			if strings.HasPrefix(k, rel+"/") {
				ancestor = true
			}
		}
		return false, ancestor
	}
	return filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == convertjob.MetaDir {
			return filepath.SkipDir
		}
		whole, ancestor := kept(rel)
		switch {
		case whole && d.IsDir():
			return filepath.SkipDir
		case whole || (ancestor && d.IsDir()):
			return nil
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func writeMeta(out string, res Result, stdout, stderr []byte, logs bool) error {
	dir := filepath.Join(out, convertjob.MetaDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, _ := json.Marshal(res)
	files := map[string][]byte{"result.json": raw}
	if logs {
		files["stdout"], files["stderr"] = stdout, stderr
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReadMeta reads the runner's result and logs from an exported output directory.
func ReadMeta(out string) (Result, []byte, []byte, error) {
	dir := filepath.Join(out, convertjob.MetaDir)
	read := func(name string) []byte {
		b, _ := readCapped(filepath.Join(dir, name), convertjob.MaxLogBytes)
		return b
	}
	raw, err := readCapped(filepath.Join(dir, "result.json"), 64<<10)
	if err != nil {
		return Result{}, nil, nil, errors.New("the conversion step produced no result")
	}
	var r Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Result{}, nil, nil, errors.New("the conversion step result is malformed")
	}
	return r, read("stdout"), read("stderr"), nil
}

func readCapped(p string, n int64) ([]byte, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > n {
		return nil, errors.New("not a regular file within the size limit")
	}
	return os.ReadFile(p)
}
