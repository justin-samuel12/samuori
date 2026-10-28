//go:build linux

package vsock

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type addr struct{ cid, port uint32 }

func (a addr) Network() string { return "vsock" }
func (a addr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

type conn struct {
	*os.File
	local, remote addr
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

func (c *conn) CloseWrite() error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

func newConn(fd int, local, remote addr) (net.Conn, error) {
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &conn{File: os.NewFile(uintptr(fd), "vsock:"+remote.String()), local: local, remote: remote}, nil
}

// Dial connects from the guest to a vsock address.
func Dial(cid, port uint32, timeout time.Duration) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock: dial %d:%d: %w", cid, port, err)
	}
	return newConn(fd, addr{}, addr{cid, port})
}

type listener struct {
	f    *os.File
	port uint32
}

// Listen accepts vsock connections on port from any CID; in a guest that is the host.
func Listen(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &listener{f: os.NewFile(uintptr(fd), "vsock-listener"), port: port}, nil
}

func (l *listener) Accept() (net.Conn, error) {
	raw, err := l.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var sa unix.Sockaddr
	var aerr error
	if err := raw.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC)
		return !errors.Is(aerr, unix.EAGAIN)
	}); err != nil {
		return nil, err
	}
	if aerr != nil {
		return nil, aerr
	}
	remote := addr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = addr{vm.CID, vm.Port}
	}
	return newConn(nfd, addr{unix.VMADDR_CID_ANY, l.port}, remote)
}

func (l *listener) Close() error   { return l.f.Close() }
func (l *listener) Addr() net.Addr { return addr{unix.VMADDR_CID_ANY, l.port} }
