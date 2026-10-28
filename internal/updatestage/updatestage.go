// Package updatestage is how a controller or worker hands a signed release to its host's samuori-self-update, and how
// it reads the outcome back (ADR 0044).
package updatestage

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// Dir is the staging directory the installer creates (group samuori-update).
const Dir = "/var/lib/samuori-update"

// Write puts a release into dir for samuori-self-update; the request goes last, because it starts the helper.
func Write(dir, version string, archive io.Reader, checksums, signature []byte) error {
	_ = os.Remove(filepath.Join(dir, "result"))
	f, err := os.OpenFile(filepath.Join(dir, "release.tar.gz"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, archive); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), checksums, 0o640); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), signature, 0o640); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "request"), []byte(version), 0o640)
}

// Rollback is the request that makes samuori-self-update reinstall the release it kept before the last update.
const Rollback = "rollback"

// WriteRollback asks samuori-self-update to reinstall the previous release.
func WriteRollback(dir string) error {
	_ = os.Remove(filepath.Join(dir, "result"))
	return os.WriteFile(filepath.Join(dir, "request"), []byte(Rollback), 0o640)
}

// Result is what samuori-self-update recorded.
type Result struct {
	Version string `json:"version"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// TakeResult reads and removes the outcome samuori-self-update left in dir.
func TakeResult(dir string) (Result, bool) {
	path := filepath.Join(dir, "result")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Result{}, false
	}
	_ = os.Remove(path)
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return Result{Outcome: "failed", Detail: "the update result is unreadable"}, true
	}
	return r, true
}
