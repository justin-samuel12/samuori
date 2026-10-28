//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package cli

import (
	"errors"
	"os"
)

func readHidden(*os.File) (string, error) {
	return "", errors.New("pipe the token on standard input")
}
