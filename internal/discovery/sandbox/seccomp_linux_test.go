//go:build linux

package sandbox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	platformChildModes["child-escape"] = escapeChild
	platformChildModes["child-x32"] = x32Child
}

type escapeProbe struct {
	name     string
	call     func() (uintptr, unix.Errno)
	postOnly bool
	want     unix.Errno
}

var uringParams [120]byte

func escapeProbes() []escapeProbe {
	sys := func(nr uintptr, a ...uintptr) func() (uintptr, unix.Errno) {
		return func() (uintptr, unix.Errno) {
			a = append(a, 0, 0, 0, 0, 0, 0)
			r, _, e := unix.Syscall6(nr, a[0], a[1], a[2], a[3], a[4], a[5])
			return r, e
		}
	}
	return []escapeProbe{
		{name: "io_uring_setup", call: sys(unix.SYS_IO_URING_SETUP, 8, uintptr(unsafe.Pointer(&uringParams))), want: unix.EPERM},
		{name: "io_uring_enter", call: sys(unix.SYS_IO_URING_ENTER, ^uintptr(0)), want: unix.EPERM},
		{name: "io_uring_register", call: sys(unix.SYS_IO_URING_REGISTER, ^uintptr(0)), want: unix.EPERM},
		{name: "unshare-user", call: sys(unix.SYS_UNSHARE, unix.CLONE_NEWUSER), want: unix.EPERM},
		{name: "setns", call: sys(unix.SYS_SETNS, ^uintptr(0)), want: unix.EPERM},
		{name: "clone3", call: sys(unix.SYS_CLONE3, 0, 0), want: unix.ENOSYS},
		{name: "clone-newuser", call: cloneNewUser, postOnly: true, want: unix.EPERM},
		{name: "bpf", call: sys(unix.SYS_BPF, 0xffff), want: unix.EPERM},
		{name: "keyctl", call: sys(unix.SYS_KEYCTL, 0xffff), want: unix.EPERM},
		{name: "perf_event_open", call: sys(unix.SYS_PERF_EVENT_OPEN), want: unix.EPERM},
		{name: "pidfd_getfd", call: sys(unix.SYS_PIDFD_GETFD, ^uintptr(0)), want: unix.EPERM},
		{name: "name_to_handle_at", call: sys(unix.SYS_NAME_TO_HANDLE_AT, ^uintptr(0)), want: unix.EPERM},
	}
}

// cloneNewUser forks into a new user namespace; a child that escapes the filter exits at once.
func cloneNewUser() (uintptr, unix.Errno) {
	r, _, e := unix.RawSyscall6(unix.SYS_CLONE, unix.CLONE_NEWUSER|uintptr(unix.SIGCHLD), 0, 0, 0, 0, 0)
	if e == 0 && r == 0 {
		unix.RawSyscall(unix.SYS_EXIT_GROUP, 0, 0, 0)
	}
	if e == 0 {
		var ws unix.WaitStatus
		_, _ = unix.Wait4(int(r), &ws, 0, nil)
	}
	return r, e
}

func outcome(r uintptr, e unix.Errno) string {
	if e != 0 {
		return unix.ErrnoName(e)
	}
	return fmt.Sprintf("ok(%d)", r)
}

func escapeChild() int {
	probes := escapeProbes()
	pre := make([]string, len(probes))
	for i, p := range probes {
		if p.postOnly {
			continue
		}
		r, e := p.call()
		if e == 0 && p.name == "io_uring_setup" {
			_ = unix.Close(int(r))
		}
		pre[i] = outcome(r, e)
	}
	if err := restrict(ChildLimits()); err != nil {
		fmt.Fprintln(os.Stderr, "restrict:", err)
		return 3
	}
	var parts []string
	for i, p := range probes {
		parts = append(parts, p.name+"="+pre[i]+"/"+outcome(p.call()))
	}
	fmt.Printf(`{"findings":[],"dockerfiles":[],"warnings":[{"code":%q,"message":"probe"}]}`, strings.Join(parts, ","))
	return 0
}

const x32Socket = x32SyscallBit | 41

func x32Child() int {
	r, _, e := unix.RawSyscall(x32Socket, unix.AF_INET, unix.SOCK_STREAM, 0)
	fmt.Fprintf(os.Stderr, "x32 socket before the filter: %s\n", outcome(r, e))
	if err := restrict(ChildLimits()); err != nil {
		fmt.Fprintln(os.Stderr, "restrict:", err)
		return 3
	}
	r, _, e = unix.RawSyscall(x32Socket, unix.AF_INET, unix.SOCK_STREAM, 0)
	fmt.Printf(`{"findings":[],"dockerfiles":[],"warnings":[{"code":%q,"message":"probe"}]}`, "survived:"+outcome(r, e))
	return 0
}

// seccompData mirrors struct seccomp_data for the classic BPF evaluator below.
func seccompData(arch uint32, nr int64, arg0 uint64) []byte {
	b := make([]byte, 64)
	binary.LittleEndian.PutUint32(b[0:], uint32(nr))
	binary.LittleEndian.PutUint32(b[4:], arch)
	binary.LittleEndian.PutUint64(b[16:], arg0)
	return b
}

func evaluate(t *testing.T, prog []unix.SockFilter, data []byte) uint32 {
	t.Helper()
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			acc = binary.LittleEndian.Uint32(data[in.K:])
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			pc += int(map[bool]uint8{true: in.Jt, false: in.Jf}[acc == in.K])
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			pc += int(map[bool]uint8{true: in.Jt, false: in.Jf}[acc >= in.K])
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			pc += int(map[bool]uint8{true: in.Jt, false: in.Jf}[acc&in.K != 0])
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		default:
			t.Fatalf("unexpected instruction %#x at %d", in.Code, pc)
		}
	}
	t.Fatal("program fell off the end")
	return 0
}

func TestSeccompFilter(t *testing.T) {
	t.Run("SEC-DSC-05", func(t *testing.T) {
		prog, err := filterProgram(runtime.GOARCH)
		if err != nil {
			t.Fatal(err)
		}
		arch := auditArch[runtime.GOARCH]
		const (
			allow  = unix.SECCOMP_RET_ALLOW
			eperm  = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
			enosys = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
			kill   = unix.SECCOMP_RET_KILL_PROCESS
		)
		denied := map[int64]bool{}
		for _, nr := range deniedSyscalls {
			denied[int64(nr)] = true
		}
		for nr := int64(0); nr < 1024; nr++ {
			want := uint32(allow)
			switch {
			case denied[nr]:
				want = eperm
			case nr == unix.SYS_CLONE3:
				want = enosys
			}
			if got := evaluate(t, prog, seccompData(arch, nr, 0)); got != want {
				t.Errorf("syscall %d: %#x, want %#x", nr, got, want)
			}
		}
		const threadFlags = unix.CLONE_VM | unix.CLONE_FS | unix.CLONE_FILES | unix.CLONE_SIGHAND | unix.CLONE_SYSVSEM | unix.CLONE_THREAD
		if got := evaluate(t, prog, seccompData(arch, unix.SYS_CLONE, threadFlags)); got != allow {
			t.Errorf("thread clone: %#x", got)
		}
		for _, f := range []uint64{unix.CLONE_NEWNS, unix.CLONE_NEWCGROUP, unix.CLONE_NEWUTS, unix.CLONE_NEWIPC, unix.CLONE_NEWUSER, unix.CLONE_NEWPID, unix.CLONE_NEWNET} {
			if got := evaluate(t, prog, seccompData(arch, unix.SYS_CLONE, f|uint64(unix.SIGCHLD))); got != eperm {
				t.Errorf("clone with %#x: %#x", f, got)
			}
		}
		for _, other := range []uint32{unix.AUDIT_ARCH_I386, unix.AUDIT_ARCH_ARM, unix.AUDIT_ARCH_X86_64 ^ unix.AUDIT_ARCH_AARCH64 ^ arch} {
			if got := evaluate(t, prog, seccompData(other, unix.SYS_READ, 0)); got != kill {
				t.Errorf("arch %#x: %#x", other, got)
			}
		}
		if runtime.GOARCH == "amd64" {
			for _, nr := range []int64{x32SyscallBit, x32SyscallBit | unix.SYS_READ, x32Socket, x32SyscallBit | 520, 0xffffffff} {
				if got := evaluate(t, prog, seccompData(arch, nr, 0)); got != kill {
					t.Errorf("x32 call %#x: %#x", nr, got)
				}
			}
		}
		if _, err := filterProgram("riscv64"); err == nil {
			t.Error("an unsupported architecture got a filter")
		}
	})
}

func TestSandboxBlocksKernelEscapes(t *testing.T) {
	t.Run("SEC-DSC-05", func(t *testing.T) {
		for _, ns := range []bool{true, false} {
			r := runner("child-escape")
			r.Namespaces = ns
			for _, part := range strings.Split(probe(t, r), ",") {
				name, result, _ := strings.Cut(part, "=")
				pre, post, _ := strings.Cut(result, "/")
				var want string
				for _, p := range escapeProbes() {
					if p.name == name {
						want = unix.ErrnoName(p.want)
					}
				}
				t.Logf("namespaces=%v %s: before %s, after %s", ns, name, pre, post)
				if post != want {
					t.Errorf("namespaces=%v %s after the filter: %s, want %s", ns, name, post, want)
				}
				if pre == post {
					t.Errorf("namespaces=%v %s: the probe does not distinguish the filter (%s before and after)", ns, name, pre)
				}
			}
		}
	})
}

func TestSandboxKillsX32Calls(t *testing.T) {
	t.Run("SEC-DSC-05", func(t *testing.T) {
		if runtime.GOARCH != "amd64" {
			t.Skip("the x32 ABI exists only on amd64; run with make test-vm-amd64")
		}
		_, _, err := runner("child-x32").Run(context.Background(), input)
		t.Logf("result: %v", err)
		if !errors.Is(err, ErrSandbox) || !strings.Contains(err.Error(), "bad system call") {
			t.Fatalf("an x32 socket call was not killed: %v", err)
		}
	})
}
