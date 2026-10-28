// Package vsock carries host–guest traffic for vm-per-job environments: AF_VSOCK inside the guest, and Cloud
// Hypervisor's hybrid vsock (a Unix socket per VM) on the host.
package vsock

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// HostCID is the address a guest uses to reach the host.
const HostCID = 2

// DialGuest opens a stream to a guest port through the VM's hybrid vsock socket.
func DialGuest(ctx context.Context, socket string, port uint32) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	line, err := readLine(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("vsock: guest port %d: %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock: guest port %d refused: %q", port, line)
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// ListenHost accepts guest connections to the host port: Cloud Hypervisor forwards them to "<socket>_<port>".
func ListenHost(socket string, port uint32) (net.Listener, error) {
	return net.Listen("unix", socket+"_"+strconv.FormatUint(uint64(port), 10))
}

// readLine reads the handshake reply a byte at a time so no stream data is consumed with it.
func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 64 {
		if _, err := c.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", fmt.Errorf("handshake reply too long")
}
