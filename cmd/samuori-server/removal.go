package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/clusterjoin"
	"samuori.invalid/samuori/internal/messaging"
	"samuori.invalid/samuori/internal/updatestage"
	"samuori.invalid/samuori/internal/workers"
)

const (
	forgetRequest = "nats-forget"
	forgetResult  = "nats-forget.result"
)

// natsSystemCommand gives this host's NATS member a system account; the installer runs it as root on every install
// and update and restarts samuori-nats afterwards.
func natsSystemCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cluster nats-system", flag.ContinueOnError)
	configDir := fs.String("config-dir", "/etc/samuori", "the controller's configuration directory")
	natsOwner := fs.String("nats-owner", "nats", "the user the NATS member runs as")
	if err := fs.Parse(args); err != nil {
		return err
	}
	natsDir := filepath.Join(*configDir, "nats")
	if _, err := os.Stat(filepath.Join(natsDir, "nats.conf")); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "no NATS member on this host; nothing to do")
		return nil
	}
	changed, err := clusterjoin.EnsureSystemAccount(natsDir, *natsOwner)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintln(stdout, "NATS system account set up; restart samuori-nats")
	} else {
		fmt.Fprintln(stdout, "NATS system account already set up")
	}
	return nil
}

type forgetOutcome struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// forgetCommand runs as root from samuori-nats-forget.service: it drops the controllers named in the request from this
// host's NATS routes and messaging URLs, reloads NATS, and removes them from the JetStream meta group.
func forgetCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cluster forget", flag.ContinueOnError)
	configDir := fs.String("config-dir", "/etc/samuori", "the controller's configuration directory")
	stage := fs.String("stage-dir", updatestage.Dir, "where the controller leaves the request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req := filepath.Join(*stage, forgetRequest)
	raw, err := os.ReadFile(req)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = os.Remove(req)
	if err != nil {
		return err
	}
	res := forgetOutcome{Outcome: "succeeded"}
	if err := forget(*configDir, raw, stdout); err != nil {
		res = forgetOutcome{Outcome: "failed", Detail: err.Error()}
	}
	out, _ := json.Marshal(res)
	tmp := filepath.Join(*stage, forgetResult+".tmp")
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(*stage, forgetResult))
}

func forget(configDir string, request []byte, stdout io.Writer) error {
	natsDir := filepath.Join(configDir, "nats")
	unlock, err := lockNATSConf(natsDir)
	if err != nil {
		return err
	}
	defer unlock()
	confPath := filepath.Join(natsDir, "nats.conf")
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	own := clusterjoin.OwnName(conf)
	var hosts []string
	sc := bufio.NewScanner(io.LimitReader(bytes.NewReader(request), 64<<10))
	for sc.Scan() {
		h := strings.TrimSpace(sc.Text())
		if h == "" {
			continue
		}
		if !clusterjoin.ValidHost(h) {
			return fmt.Errorf("the request names %q, which is not a host name", h)
		}
		if h != own {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) > 16 {
		return errors.New("the request names more than 16 controllers")
	}
	routesChanged := false
	for _, h := range hosts {
		var ok bool
		if conf, ok = clusterjoin.DropRoute(conf, h); ok {
			routesChanged = true
		}
	}
	if routesChanged {
		if err := replaceKeepingOwner(confPath, conf); err != nil {
			return err
		}
		if out, err := exec.Command("systemctl", "kill", "-s", "HUP", "samuori-nats.service").CombinedOutput(); err != nil {
			return fmt.Errorf("reloading samuori-nats: %v: %s", err, bytes.TrimSpace(out))
		}
	}
	ymlPath := filepath.Join(configDir, "server.yml")
	if yml, err := os.ReadFile(ymlPath); err == nil {
		changed := false
		for _, h := range hosts {
			var ok bool
			if yml, ok = clusterjoin.DropMessagingURL(yml, h); ok {
				changed = true
			}
		}
		if changed {
			if err := replaceKeepingOwner(ymlPath, yml); err != nil {
				return err
			}
		}
	}
	nc, err := clusterjoin.DialSystem(natsDir)
	if err != nil {
		return err
	}
	defer nc.Close()
	for _, h := range hosts {
		var err error
		for range 5 {
			if err = clusterjoin.RemoveServer(context.Background(), nc, h); err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "NATS member %s forgotten\n", h)
	}
	return nil
}

func replaceKeepingOwner(path string, data []byte) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, st.Mode().Perm()); err != nil {
		return err
	}
	if err := chownLike(tmp, st); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// controllerRemovals connects removal to this host's root helpers in the staging directory (samuori-wipe.path and
// samuori-nats-forget.path).
func controllerRemovals(m *cluster.Member, natsOnControllers bool, bus *messaging.Bus) *cluster.Removals {
	r := cluster.NewRemovals(m)
	r.NATSOnControllers = natsOnControllers
	r.StreamPeers = func(ctx context.Context) ([]cluster.StreamPeer, int, error) {
		peers, replicas, err := bus.StreamPeers(ctx)
		out := make([]cluster.StreamPeer, len(peers))
		for i, p := range peers {
			out[i] = cluster.StreamPeer{Name: p.Name, Current: p.Current}
		}
		return out, replicas, err
	}
	r.Wipe = func() error {
		return os.WriteFile(filepath.Join(updatestage.Dir, "wipe"), []byte("removed from the cluster by an administrator\n"), 0o640)
	}
	if natsOnControllers {
		r.Forget = func(hosts []string) error {
			raw, err := askRootHelper(forgetRequest, forgetResult, []byte(strings.Join(hosts, "\n")+"\n"), "samuori-nats-forget.path")
			if err != nil {
				return err
			}
			var res forgetOutcome
			if json.Unmarshal(raw, &res) != nil || res.Outcome != "succeeded" {
				return fmt.Errorf("the NATS member did not forget %s: %s", strings.Join(hosts, ", "), res.Detail)
			}
			return nil
		}
		r.SyncRoutes = routesHelper
	}
	return r
}

// runRemovals runs the parts of controller removal every controller does for itself.
func runRemovals(ctx context.Context, r *cluster.Removals, log *slog.Logger) {
	t := time.NewTicker(cluster.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := r.StageOwnWipe(ctx); err != nil && ctx.Err() == nil {
			log.Error("controller removal: wiping this host", "error", err)
		}
		if err := r.ForgetRemoved(ctx); err != nil && ctx.Err() == nil {
			log.Warn("controller removal: forgetting removed NATS members", "error", err)
		}
		if err := r.SyncNATSRoutes(ctx); err != nil && ctx.Err() == nil {
			log.Warn("NATS routes: this host's member was not brought up to date; retrying", "error", err)
		}
	}
}

func chownLike(path string, st os.FileInfo) error {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return os.Chown(path, int(s.Uid), int(s.Gid))
	}
	return nil
}

// localWorkerRemoval drains or revokes the worker a controller host keeps when that controller is removed
// (ADR 0046, OD-28). The host's wipe deletes the worker's files with the controller's.
func localWorkerRemoval(store *workers.Store, srv *workers.Server) func(context.Context, bool, string) error {
	return func(ctx context.Context, revoke bool, reason string) error {
		local, err := store.EnrolledLocal(ctx)
		if err != nil {
			return err
		}
		action := workers.ActionDrain
		if revoke {
			action = workers.ActionRevoke
		}
		var errs []error
		for id, org := range local {
			_, err := srv.ApplyAction(ctx, authz.Principal{ID: "system:controller-removal", OrgID: org, Kind: authz.System}, id, action, reason, nil, "")
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
}
