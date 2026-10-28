package config

import (
	"fmt"
	"os"
	"strings"
)

// ReadPrivateFile reads a credential file, refusing files readable by group or others.
func ReadPrivateFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must not be accessible by group or others (chmod 600)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}
