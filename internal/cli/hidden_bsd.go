//go:build darwin || freebsd || netbsd || openbsd

package cli

import "golang.org/x/sys/unix"

const ioctlGetTermios, ioctlSetTermios = unix.TIOCGETA, unix.TIOCSETA
