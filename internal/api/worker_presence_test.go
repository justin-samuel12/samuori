package api

import (
	"context"
	"testing"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/workers"
)

type presence map[string]bool

func (p presence) Connected(id string) bool { return p[id] }
func (presence) RequestSelfTest(context.Context, authz.Principal, string, string) ([]workers.SelfTest, error) {
	return nil, workers.ErrNoSelfTestWorker
}
func (presence) RequestWorkerSelfTest(context.Context, authz.Principal, string, string) (workers.SelfTest, error) {
	return workers.SelfTest{}, workers.ErrNoSelfTestWorker
}

// TestWorkerConnectedApartFromEligibility: an eligible worker without a live stream is reported as not connected,
// so the portal never counts it as ready to build.
func TestWorkerConnectedApartFromEligibility(t *testing.T) {
	online := presence{}
	l := newLiveWith(t, false, smallChunks(), func(d *Deps) { d.Channel = online })
	l.setup(t)
	admin := l.adminMember(t)
	id := enrollConversionWorker(t, l)

	get := func() body {
		t.Helper()
		st, _, b := l.json(t, admin, "GET", "/workers/"+id, "", nil)
		if st != 200 {
			t.Fatalf("get worker = %d %v", st, b)
		}
		return b
	}
	w := get()
	if w["connected"] != false {
		t.Errorf("connected = %v without a stream, want false", w["connected"])
	}
	if e := w["eligibility"].(map[string]any); e["untrustedBuilds"] != true {
		t.Fatalf("the fixture worker should be eligible: %v", e)
	}
	online[id] = true
	if w := get(); w["connected"] != true {
		t.Errorf("connected = %v with a stream, want true", w["connected"])
	}
}
