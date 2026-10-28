package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"samuori.invalid/samuori/internal/buildplan"
)

type fixedResolver map[string]netip.Addr

func (r fixedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, errors.New("no such host")
}

// TestDenied: the structured accessor lists each denied destination once with its count and reason, leaves out
// allowed ones, and reports decisions beyond the per-attempt limit.
func TestDenied(t *testing.T) {
	srv := New(Options{
		Resolver: fixedResolver{"ok.example": netip.MustParseAddr("93.184.216.34"), "rebind.example": netip.MustParseAddr("127.0.0.1")},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			c, _ := net.Pipe()
			return c, nil
		},
	})
	sess, err := srv.Open([]buildplan.EgressRule{{Host: "ok.example", Port: 443}, {Host: "rebind.example", Port: 443}, {Host: "gone.example", Port: 443}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	connect := func(hostport string) {
		r := httptest.NewRequest(http.MethodConnect, "http://"+hostport, nil)
		r.Host = hostport
		r.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+sess.token)))
		srv.ServeHTTP(httptest.NewRecorder(), r)
	}
	for _, d := range []string{"github.com:443", "github.com:443", "registry.npmjs.org:443", "ok.example:443", "rebind.example:443", "gone.example:443"} {
		connect(d)
	}
	denied, unrecorded := sess.Denied()
	want := []Denial{
		{Host: "github.com", Port: 443, Count: 2, Reason: ReasonNotAllowed},
		{Host: "gone.example", Port: 443, Count: 1, Reason: ReasonUnresolved},
		{Host: "rebind.example", Port: 443, Count: 1, Reason: ReasonBlockedAddress + " (127.0.0.1)"},
		{Host: "registry.npmjs.org", Port: 443, Count: 1, Reason: ReasonNotAllowed},
	}
	if unrecorded != 0 || len(denied) != len(want) {
		t.Fatalf("denied = %+v, %d unrecorded", denied, unrecorded)
	}
	for i := range want {
		if denied[i] != want[i] {
			t.Errorf("denial %d = %+v; want %+v", i, denied[i], want[i])
		}
	}
	for range maxDecisions {
		connect("github.com:443")
	}
	if denied, unrecorded := sess.Denied(); unrecorded != 6 || denied[0].Count != maxDecisions-4 {
		t.Errorf("after the limit: %+v, %d unrecorded", denied, unrecorded)
	}
}
