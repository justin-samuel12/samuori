package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/clusterjoin"
)

const testNATSConf = `server_name: "a.example"
listen: 0.0.0.0:4222
tls { cert_file: "s.crt", key_file: "s.key", ca_file: "ca.crt", verify: true }
cluster {
  name: samuori
  listen: 0.0.0.0:6222
  advertise: "a.example:6222"
  routes: [nats-route://a.example:6222, nats-route://b.example:6222]
  tls { cert_file: "s.crt", key_file: "s.key", ca_file: "ca.crt", verify: true }
}
`

type fakeMember struct {
	loaded         time.Time
	routes, peers  []string
	applies        bool
	down           bool
	reloads        int
	restarts       int
	peersAfterLoad []string
}

func (f *fakeMember) state(context.Context, string) (clusterjoin.MemberState, error) {
	if f.down {
		return clusterjoin.MemberState{}, errors.New("connection refused")
	}
	return clusterjoin.MemberState{ConfigLoaded: f.loaded, Routes: f.routes, Peers: f.peers}, nil
}

func (f *fakeMember) reload() error {
	f.reloads++
	if f.applies {
		f.loaded = f.loaded.Add(time.Second)
		f.routes = nil
		if f.peersAfterLoad != nil {
			f.peers = f.peersAfterLoad
		}
	}
	return nil
}

func (f *fakeMember) restart() error {
	f.restarts++
	return nil
}

func TestSyncRoutesHelper(t *testing.T) {
	t.Run("SEC-HA-10", func(t *testing.T) {
		ctx := context.Background()
		reloadWait, strayWait = time.Second, time.Second
		dir := t.TempDir()
		conf := filepath.Join(dir, "nats.conf")
		if err := os.WriteFile(conf, []byte(testNATSConf), 0o644); err != nil {
			t.Fatal(err)
		}
		plan := clusterjoin.RoutePlan{Members: []string{"a.example", "b.example", "c.example"}, Verify: true}

		stuck := &fakeMember{peers: []string{"b.example"}, routes: []string{"a.example", "b.example"}}
		if res := syncRoutes(ctx, dir, plan, stuck); res.Outcome != "failed" || stuck.reloads != 1 {
			t.Fatalf("a reload that did not take: %+v", res)
		}
		if got, _ := os.ReadFile(conf); string(got) != testNATSConf {
			t.Fatalf("the file was left at routes the member did not load:\n%s", got)
		}

		m := &fakeMember{peers: []string{"b.example"}, routes: []string{"a.example", "b.example"}, applies: true}
		res := syncRoutes(ctx, dir, plan, m)
		got, _ := os.ReadFile(conf)
		if res.Outcome != "succeeded" || !res.Verify || m.reloads != 1 || !clusterjoin.RoutesVerified(got) ||
			!slices.Equal(clusterjoin.RouteHosts(got), []string{"a.example", "b.example", "c.example"}) || !slices.Equal(res.Missing, []string{"c.example"}) {
			t.Fatalf("filling the routes: %+v\n%s", res, got)
		}
		m.routes = res.Hosts
		if res := syncRoutes(ctx, dir, plan, m); res.Outcome != "succeeded" || m.reloads != 1 {
			t.Fatalf("the same plan again reloaded the member: %+v, %d reloads", res, m.reloads)
		}

		m.peers = []string{"b.example", "c.example"}
		m.peersAfterLoad = []string{"b.example", "c.example"}
		removal := clusterjoin.RoutePlan{Members: []string{"a.example", "b.example"}, Removed: []string{"c.example"}, Verify: true}
		res = syncRoutes(ctx, dir, removal, m)
		if res.Outcome != "stray" || !slices.Equal(res.Stray, []string{"c.example"}) || m.restarts != 0 {
			t.Fatalf("a surviving route to a removed member: %+v", res)
		}
		removal.Restart = true
		if res := syncRoutes(ctx, dir, removal, m); res.Outcome != "restarted" || m.restarts != 1 {
			t.Fatalf("restart allowed: %+v", res)
		}

		if res := syncRoutes(ctx, dir, clusterjoin.RoutePlan{Removed: []string{"b.example"}, Verify: true}, m); res.Outcome != "failed" {
			t.Fatalf("a plan that cuts the member off was applied: %+v", res)
		}

		down := &fakeMember{down: true}
		if res := syncRoutes(ctx, dir, plan, down); res.Outcome != "succeeded" || !res.Verify || res.Detail == "" {
			t.Fatalf("with the member stopped: %+v", res)
		}
		if got, _ := os.ReadFile(conf); !slices.Contains(clusterjoin.RouteHosts(got), "c.example") || !clusterjoin.RoutesVerified(got) {
			t.Fatalf("with the member stopped, the routes were not written or verification changed:\n%s", got)
		}
	})
}
