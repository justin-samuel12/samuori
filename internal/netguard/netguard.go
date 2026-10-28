// Package netguard dials only addresses an administrator may target (SEC-NET-03): link-local addresses, including
// cloud metadata services, are never dialled, and loopback and private ranges only when explicitly allowed.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var ErrBlocked = errors.New("the address is not allowed")

// Permitted reports whether a may be dialled.
func Permitted(a netip.Addr, allowPrivate bool) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsMulticast(), a.IsUnspecified(), a.IsInterfaceLocalMulticast():
		return false
	case a.IsLoopback(), a.IsPrivate(), netip.MustParsePrefix("100.64.0.0/10").Contains(a):
		return allowPrivate
	}
	return a.IsGlobalUnicast()
}

// Control checks every resolved address just before connecting, so DNS answers cannot bypass the rules.
func Control(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !Permitted(a, allowPrivate) {
			return fmt.Errorf("%w: %s", ErrBlocked, host)
		}
		return nil
	}
}

// Client returns an HTTP client whose every connection, including redirects, passes the guard. Proxy environment
// variables are ignored.
func Client(allowPrivate bool, timeout time.Duration, maxRedirects int) *http.Client {
	d := &net.Dialer{Timeout: 15 * time.Second, Control: Control(allowPrivate)}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second,
			ResponseHeaderTimeout: time.Minute, MaxIdleConnsPerHost: 4},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}
