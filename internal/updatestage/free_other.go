//go:build !unix

package updatestage

import "errors"

func FreeBytes(string) (uint64, error) {
	return 0, errors.New("free space is not reported on this platform")
}
