package publication

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/netguard"
)

// httpClient talks to one registry through the address guard (SEC-NET-03).
func httpClient(allowPrivate bool) *http.Client {
	return netguard.Client(allowPrivate, 30*time.Minute, 5)
}

func permitted(a netip.Addr, allowPrivate bool) bool { return netguard.Permitted(a, allowPrivate) }

var (
	hostname   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	repository = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
)

// ValidEndpoint accepts host[:port] only: no scheme, path or credentials.
func ValidEndpoint(e string) error {
	host, port := e, ""
	if h, p, err := net.SplitHostPort(e); err == nil {
		host, port = h, p
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("the endpoint port is invalid")
		}
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !permitted(a, true) {
			return errors.New("the endpoint is a link-local, multicast or unspecified address")
		}
		return nil
	}
	if !hostname.MatchString(host) || len(e) > 253 {
		return errors.New("the endpoint must be host or host:port, without a scheme or path")
	}
	return nil
}
