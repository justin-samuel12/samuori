package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/clusterjoin"
	"samuori.invalid/samuori/internal/updatestage"
)

const (
	routesRequest = "nats-routes"
	routesResult  = "nats-routes.result"
)

var (
	reloadWait = 15 * time.Second
	strayWait  = 10 * time.Second
)

// routesCommand runs as root from samuori-nats-routes.service: it rewrites this host's NATS routes to the plan the
// controller left, turns route verification on or off, reloads NATS and checks that the reload took
// (docs/operators/high-availability.md#nats-routes).
func routesCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cluster routes", flag.ContinueOnError)
	configDir := fs.String("config-dir", "/etc/samuori", "the controller's configuration directory")
	stage := fs.String("stage-dir", updatestage.Dir, "where the controller leaves the request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req := filepath.Join(*stage, routesRequest)
	raw, err := os.ReadFile(req)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = os.Remove(req)
	if err != nil {
		return err
	}
	var plan clusterjoin.RoutePlan
	var res clusterjoin.RouteOutcome
	if len(raw) > 64<<10 || json.Unmarshal(raw, &plan) != nil {
		res = clusterjoin.RouteOutcome{Outcome: "failed", Detail: "the request is not a route plan"}
	} else {
		res = syncRoutes(context.Background(), filepath.Join(*configDir, "nats"), plan, natsHost{})
	}
	fmt.Fprintf(stdout, "NATS routes: %s %s\n", res.Outcome, res.Detail)
	out, _ := json.Marshal(res)
	tmp := filepath.Join(*stage, routesResult+".tmp")
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(*stage, routesResult))
}

// memberHost is the local NATS member as the helper sees it; tests replace it.
type memberHost interface {
	state(ctx context.Context, natsDir string) (clusterjoin.MemberState, error)
	reload() error
	restart() error
}

type natsHost struct{}

func (natsHost) state(ctx context.Context, natsDir string) (clusterjoin.MemberState, error) {
	nc, err := clusterjoin.DialSystem(natsDir)
	if err != nil {
		return clusterjoin.MemberState{}, err
	}
	defer nc.Close()
	return clusterjoin.LocalState(ctx, nc)
}

func (natsHost) reload() error  { return systemctl("kill", "-s", "HUP", "samuori-nats.service") }
func (natsHost) restart() error { return systemctl("restart", "samuori-nats.service") }

func systemctl(args ...string) error {
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// syncRoutes writes the new routes atomically, reloads the member and waits for the reload; a reload that does not
// take puts the previous file back, so the file and the running member never disagree for long.
func syncRoutes(ctx context.Context, natsDir string, plan clusterjoin.RoutePlan, host memberHost) clusterjoin.RouteOutcome {
	fail := func(err error) clusterjoin.RouteOutcome {
		return clusterjoin.RouteOutcome{Outcome: "failed", Detail: err.Error()}
	}
	unlock, err := lockNATSConf(natsDir)
	if err != nil {
		return fail(err)
	}
	defer unlock()
	confPath := filepath.Join(natsDir, "nats.conf")
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return fail(err)
	}
	before, stateErr := host.state(ctx, natsDir)
	var connected []string
	if stateErr == nil {
		connected = before.Peers
	}
	ch, err := clusterjoin.PlanRoutes(clusterjoin.OwnName(conf), clusterjoin.RouteHosts(conf), clusterjoin.RoutesVerified(conf), plan, connected)
	if err != nil {
		return fail(err)
	}
	next, err := clusterjoin.SetRoutes(conf, clusterjoin.RouteURLs(ch.Hosts), ch.Verify)
	if err != nil {
		return fail(err)
	}
	res := clusterjoin.RouteOutcome{Outcome: "succeeded", Hosts: ch.Hosts, Verify: ch.Verify, Waiting: ch.Waiting}
	changed := !bytes.Equal(next, conf)
	if stateErr != nil {
		if changed {
			if err := replaceKeepingOwner(confPath, next); err != nil {
				return fail(err)
			}
			_ = host.reload()
		}
		res.Detail = "the NATS member could not be asked (" + stateErr.Error() + "); the routes were written and apply on its next reload or start"
		return res
	}
	if changed || !slices.Equal(before.Routes, ch.Hosts) {
		if changed {
			if err := replaceKeepingOwner(confPath, next); err != nil {
				return fail(err)
			}
		}
		if err := reloaded(ctx, natsDir, before.ConfigLoaded, host); err != nil {
			if changed {
				_ = replaceKeepingOwner(confPath, conf)
			}
			return fail(fmt.Errorf("%w; the previous routes were kept", err))
		}
	}
	removed := map[string]bool{}
	for _, h := range plan.Removed {
		removed[strings.ToLower(h)] = true
	}
	var after clusterjoin.MemberState
	deadline := time.Now().Add(strayWait)
	for {
		after, err = host.state(ctx, natsDir)
		res.Stray = nil
		if err == nil {
			for _, p := range after.Peers {
				if removed[p] {
					res.Stray = append(res.Stray, p)
				}
			}
		}
		if (err == nil && len(res.Stray) == 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return fail(err)
	}
	for _, h := range ch.Hosts {
		if h != strings.ToLower(clusterjoin.OwnName(conf)) && !slices.Contains(after.Peers, h) {
			res.Missing = append(res.Missing, h)
		}
	}
	if len(res.Stray) > 0 {
		if !plan.Restart {
			res.Outcome, res.Detail = "stray", "routes to removed members survived the reload: "+strings.Join(res.Stray, ", ")
			return res
		}
		if err := host.restart(); err != nil {
			return fail(err)
		}
		res.Outcome, res.Detail = "restarted", "samuori-nats restarted to drop routes to "+strings.Join(res.Stray, ", ")
	}
	return res
}

func reloaded(ctx context.Context, natsDir string, loaded time.Time, host memberHost) error {
	if err := host.reload(); err != nil {
		return err
	}
	deadline := time.Now().Add(reloadWait)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if st, err := host.state(ctx, natsDir); err == nil && st.ConfigLoaded.After(loaded) {
			return nil
		}
	}
	return errors.New("samuori-nats did not reload the new routes; see journalctl -u samuori-nats")
}

// lockNATSConf serializes the root helpers that rewrite nats.conf (cluster forget and cluster routes).
func lockNATSConf(natsDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(natsDir, ".rewrite.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// askRootHelper leaves a request for a path-unit helper and waits for its result.
func askRootHelper(request, result string, payload []byte, unit string) ([]byte, error) {
	_ = os.Remove(filepath.Join(updatestage.Dir, result))
	if err := os.WriteFile(filepath.Join(updatestage.Dir, request), payload, 0o640); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		raw, err := os.ReadFile(filepath.Join(updatestage.Dir, result))
		if err != nil {
			continue
		}
		_ = os.Remove(filepath.Join(updatestage.Dir, result))
		return raw, nil
	}
	return nil, fmt.Errorf("%s did not answer; check that %s is enabled", strings.TrimSuffix(unit, ".path"), unit)
}

func routesHelper(plan clusterjoin.RoutePlan) (clusterjoin.RouteOutcome, error) {
	payload, _ := json.Marshal(plan)
	raw, err := askRootHelper(routesRequest, routesResult, payload, "samuori-nats-routes.path")
	if err != nil {
		return clusterjoin.RouteOutcome{}, err
	}
	var out clusterjoin.RouteOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("samuori-nats-routes answered something that is not a result: %w", err)
	}
	return out, nil
}
