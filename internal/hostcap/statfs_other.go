//go:build !unix

package hostcap

import "errors"

func statfs(string) (dev, total, free uint64, err error) {
	return 0, 0, 0, errors.New("not supported on this platform")
}
