//go:build linux

package toolrun

import (
	"syscall"

	"samuori.invalid/samuori/internal/runtimeprobe"
)

// confine applies no_new_privs, empties the capability bounding set and checks that only loopback is configured,
// which a --network=none step guarantees.
func confine() (string, error) {
	c, err := runtimeprobe.Confine()
	return c.Network, err
}

func credential(uid uint32) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{}}}
}
