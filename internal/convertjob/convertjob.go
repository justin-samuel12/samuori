// Package convertjob is the contract between the controller and a worker for one Kubernetes conversion run
// (ADR 0048, docs/architecture/kubernetes-conversion.md). The job is bound to the lease by its digest; it names the
// approved tool by checksum and the argv to run, never a shell command, and carries no credentials.
package convertjob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Paths inside the conversion sandbox: the input is mounted read-only at InputRoot, outputs go to OutputRoot, and
// the job's configuration files are written for the tool to read at FilesRoot.
const (
	InputRoot  = "/in"
	OutputRoot = "/out"
	FilesRoot  = "/work/files"
	// MetaDir under OutputRoot holds the runner's result, stdout and stderr; it is not an output file.
	MetaDir = ".samuori"
)

// Limits on what a worker reports back; anything larger fails the run.
const (
	MaxOutputFiles = 500
	MaxFileBytes   = 1 << 20
	MaxOutputBytes = 3 << 20
	MaxLogBytes    = 256 << 10
	MaxTimeout     = 7200
	DefaultTimeout = 300
	MaxJobFiles    = 4
	MaxJobFileSize = 32 << 10
)

// ProvidersDir is where workers look for approved provider tools: <dir>/<name>/<name> and <dir>/<name>/VERSION.
const ProvidersDir = "/usr/local/lib/samuori/providers"

type Job struct {
	Version         int               `json:"version"`
	ConversionID    string            `json:"conversionId"`
	RunID           string            `json:"runId"`
	Provider        string            `json:"provider"`
	ProviderVersion string            `json:"providerVersion"`
	ToolSHA256      map[string]string `json:"toolSha256"`
	Args            []string          `json:"args"`
	Mkdirs          []string          `json:"mkdirs"`
	// Files are configuration files the runner writes to FilesRoot before the tool starts; never credentials.
	Files map[string]string `json:"files,omitempty"`
	// Keep, when set, lists the output paths kept after the tool exits; the runner removes everything else under
	// OutputRoot inside the step, so it never leaves the sandbox.
	Keep           []string `json:"keep,omitempty"`
	InputDigest    string   `json:"inputDigest"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	fileRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	sha256Re = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ValidName reports whether s can name a provider directory.
func ValidName(s string) bool { return nameRe.MatchString(s) }

func (j Job) Canonical() []byte {
	b, _ := json.Marshal(j)
	return b
}

func (j Job) Digest() string { return Digest(j.Canonical()) }

func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (j Job) Validate() error {
	switch {
	case j.Version != 1:
		return fmt.Errorf("unsupported job version %d", j.Version)
	case !nameRe.MatchString(j.Provider):
		return errors.New("invalid provider name")
	case j.ProviderVersion == "":
		return errors.New("the job names no approved provider version")
	case !sha256Re.MatchString(j.InputDigest):
		return errors.New("the job has no input digest")
	case j.TimeoutSeconds <= 0 || j.TimeoutSeconds > MaxTimeout:
		return errors.New("the job time limit is out of range")
	case len(j.Args) == 0 || len(j.Args) > 64:
		return errors.New("the job has no arguments or too many")
	}
	for arch, sum := range j.ToolSHA256 {
		if !sha256Re.MatchString(sum) || (arch != "amd64" && arch != "arm64") {
			return fmt.Errorf("invalid tool checksum for %s", arch)
		}
	}
	for _, a := range j.Args {
		if strings.ContainsRune(a, 0) || len(a) > 4096 {
			return errors.New("invalid argument")
		}
	}
	for _, d := range j.Mkdirs {
		if !LocalPath(d) {
			return fmt.Errorf("output directory %q is not a relative path", d)
		}
	}
	if err := ValidFiles(j.Files); err != nil {
		return err
	}
	for _, k := range j.Keep {
		if !LocalPath(k) || k == MetaDir || strings.HasPrefix(k, MetaDir+"/") {
			return fmt.Errorf("kept output %q is not a relative path", k)
		}
	}
	return nil
}

// ValidFiles bounds a job's configuration files: a few small files with plain names.
func ValidFiles(files map[string]string) error {
	if len(files) > MaxJobFiles {
		return errors.New("the job has too many configuration files")
	}
	total := 0
	for name, c := range files {
		if !fileRe.MatchString(name) {
			return fmt.Errorf("invalid configuration file name %q", name)
		}
		if total += len(c); total > MaxJobFileSize || strings.ContainsRune(c, 0) {
			return errors.New("the job's configuration files are too large")
		}
	}
	return nil
}

// LocalPath reports whether p is a clean, relative slash path that stays below its root.
func LocalPath(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") &&
		!strings.ContainsAny(p, "\\\x00") && len(p) <= 512
}
