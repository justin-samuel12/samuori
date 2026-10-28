//go:build unix

package hostcap

import (
	"os"
	"syscall"
)

func statfs(dir string) (dev, total, free uint64, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0, 0, 0, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		dev = uint64(st.Dev)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, 0, 0, err
	}
	bs := uint64(fs.Bsize)
	return dev, uint64(fs.Blocks) * bs, uint64(fs.Bavail) * bs, nil
}
