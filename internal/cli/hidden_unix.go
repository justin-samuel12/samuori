//go:build linux || darwin || freebsd || netbsd || openbsd

package cli

import (
	"bufio"
	"os"

	"golang.org/x/sys/unix"
)

// readHidden reads one line from a terminal with echo turned off.
func readHidden(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }()
	return bufio.NewReader(f).ReadString('\n')
}
