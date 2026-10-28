//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// childAttr isolates the discovery subprocess in fresh user, network, mount, IPC and UTS namespaces when the
// kernel allows unprivileged user namespaces.
func childAttr(namespaces bool) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if namespaces {
		attr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 65534, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 65534, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	return attr
}

const sandboxPlatform = "linux"

// deniedSyscalls fail with EPERM: no programs can be started, no sockets created (directly or through io_uring),
// no namespaces entered and no kernel subsystems that widen the attack surface reached
// (docs/architecture/discovery.md#sandbox-implementation).
var deniedSyscalls = []uintptr{
	unix.SYS_EXECVE, unix.SYS_EXECVEAT,
	unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT,
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD, unix.SYS_PROCESS_MADVISE,
	unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE, unix.SYS_MOUNT_SETATTR,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_REBOOT, unix.SYS_ACCT, unix.SYS_QUOTACTL, unix.SYS_PERSONALITY,
}

// namespaceCloneFlags are refused on clone(2); clone3 is refused with ENOSYS because its flags are in memory the
// filter cannot read. CLONE_NEWTIME is absent because clone(2) cannot carry it.
const namespaceCloneFlags = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// x32SyscallBit marks x32-ABI system calls, which share AUDIT_ARCH_X86_64 and would otherwise bypass every
// number comparison.
const x32SyscallBit = 0x40000000

var auditArch = map[string]uint32{"amd64": unix.AUDIT_ARCH_X86_64, "arm64": unix.AUDIT_ARCH_AARCH64}

type bpfInsn struct {
	f      unix.SockFilter
	jt, jf string
	label  string
}

// filterProgram builds the seccomp program for goarch. Any other architecture, including the 32-bit compat ABIs,
// and every x32 call kill the process.
func filterProgram(goarch string) ([]unix.SockFilter, error) {
	arch, ok := auditArch[goarch]
	if !ok {
		return nil, fmt.Errorf("seccomp: unsupported architecture %s", goarch)
	}
	const (
		ldw  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		jset = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
	)
	p := []bpfInsn{
		{f: unix.SockFilter{Code: ldw, K: 4}},
		{f: unix.SockFilter{Code: jeq, K: arch}, jf: "kill"},
		{f: unix.SockFilter{Code: ldw, K: 0}},
	}
	if goarch == "amd64" {
		p = append(p, bpfInsn{f: unix.SockFilter{Code: jge, K: x32SyscallBit}, jt: "kill"})
	}
	for _, nr := range deniedSyscalls {
		p = append(p, bpfInsn{f: unix.SockFilter{Code: jeq, K: uint32(nr)}, jt: "eperm"})
	}
	p = append(p,
		bpfInsn{f: unix.SockFilter{Code: jeq, K: unix.SYS_CLONE3}, jt: "enosys"},
		bpfInsn{f: unix.SockFilter{Code: jeq, K: unix.SYS_CLONE}, jf: "allow"},
		bpfInsn{f: unix.SockFilter{Code: ldw, K: 16}},
		bpfInsn{f: unix.SockFilter{Code: jset, K: namespaceCloneFlags}, jt: "eperm"},
		bpfInsn{f: unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW}, label: "allow"},
		bpfInsn{f: unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)}, label: "eperm"},
		bpfInsn{f: unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)}, label: "enosys"},
		bpfInsn{f: unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS}, label: "kill"},
	)
	at := map[string]int{}
	for i, in := range p {
		if in.label != "" {
			at[in.label] = i
		}
	}
	out := make([]unix.SockFilter, len(p))
	for i, in := range p {
		out[i] = in.f
		for _, j := range []struct {
			label string
			dst   *uint8
		}{{in.jt, &out[i].Jt}, {in.jf, &out[i].Jf}} {
			if j.label == "" {
				continue
			}
			off := at[j.label] - i - 1
			if off < 0 || off > 255 {
				return nil, fmt.Errorf("seccomp: jump to %s out of range", j.label)
			}
			*j.dst = uint8(off)
		}
	}
	return out, nil
}

// restrict is called by the child before it reads any untrusted input.
func restrict(l Limits) error {
	for _, r := range []struct {
		res int
		val uint64
	}{
		{unix.RLIMIT_CPU, uint64(l.CPUSeconds)},
		{unix.RLIMIT_AS, uint64(l.AddressSpaceBytes)},
		{unix.RLIMIT_FSIZE, 0},
		{unix.RLIMIT_NOFILE, 64},
		{unix.RLIMIT_CORE, 0},
	} {
		if err := unix.Setrlimit(r.res, &unix.Rlimit{Cur: r.val, Max: r.val}); err != nil {
			return fmt.Errorf("rlimit %d: %w", r.res, err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	prog, err := filterProgram(runtime.GOARCH)
	if err != nil {
		return err
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}
