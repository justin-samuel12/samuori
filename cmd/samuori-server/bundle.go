package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"samuori.invalid/samuori/internal/updatebundle"
	"samuori.invalid/samuori/internal/version"
	"samuori.invalid/samuori/internal/workers"
)

// updateBundle writes the signed update bundle make update produces (docs/developers/release.md):
// samuori-server update-bundle -key KEY -out FILE ARCHIVE...
func updateBundle(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("update-bundle", flag.ContinueOnError)
	key := fs.String("key", "", "SSH signing key (make release-key)")
	dest := fs.String("out", "", "bundle to write, samuori-<version>"+updatebundle.Extension)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" || *dest == "" || fs.NArg() == 0 {
		return errors.New("usage: samuori-server update-bundle -key KEY -out FILE samuori-<version>-linux-<arch>.tar.gz...")
	}
	archives := map[string]string{}
	for _, p := range fs.Args() {
		v, arch, ok := updatebundle.ParseArchiveName(filepath.Base(p))
		if !ok || v != version.Version {
			return fmt.Errorf("%s is not a release archive of %s", p, version.Version)
		}
		archives[arch] = p
	}
	m := updatebundle.Manifest{Version: version.Version, WorkerProtocol: updatebundle.Protocol{Major: workers.ProtocolMajor, Minor: workers.ProtocolMinor}, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	tmp := *dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := updatebundle.Write(f, m, archives, func(sums []byte) ([]byte, error) { return sshSign(*key, sums) }); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, *dest); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (%d architectures)\n", *dest, len(archives))
	return nil
}

func sshSign(key string, data []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "samuori-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, updatebundle.SumsName)
	if err := os.WriteFile(f, data, 0o600); err != nil {
		return nil, err
	}
	if b, err := exec.Command("ssh-keygen", "-Y", "sign", "-q", "-n", "samuori-release", "-f", key, f).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("signing: %v: %s", err, b)
	}
	return os.ReadFile(f + ".sig")
}
