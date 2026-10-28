//go:build !linux

package sandbox

import (
	"syscall"
)

func childAttr(bool) *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

const sandboxPlatform = "unsupported"

// restrict applies the limits this platform offers. Process and network denial need Linux; on other systems
// discovery results carry a sandbox-degraded warning and are for development only.
func restrict(l Limits) error {
	return syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: uint64(l.CPUSeconds), Max: uint64(l.CPUSeconds)})
}
