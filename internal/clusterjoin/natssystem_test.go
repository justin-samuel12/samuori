package clusterjoin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestSystemAccountUpgradeIdempotent(t *testing.T) {
	t.Run("SEC-HA-08", func(t *testing.T) {
		me, _ := user.Current()
		dir := t.TempDir()
		ca, err := Create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		sc, err := ca.Issue(NATSServer, "ctrl-1.example")
		if err != nil {
			t.Fatal(err)
		}
		for f, data := range map[string][]byte{"ca.crt": ca.CertPEM(), "server.crt": sc.Cert, "server.key": sc.Key} {
			if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := Bundle{Host: "ctrl-1.example", NATSURLs: []string{NATSURL("ctrl-1.example")}}.natsConfig(dir)
		old = string(bytes.ReplaceAll([]byte(old), []byte(systemInclude+"\n"), nil))
		if err := os.WriteFile(filepath.Join(dir, "nats.conf"), []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		changed, err := EnsureSystemAccount(dir, me.Username)
		if err != nil || !changed {
			t.Fatalf("upgrading a nats.conf without a system account: changed %v, %v", changed, err)
		}
		snapshot := map[string][]byte{}
		for _, f := range []string{"nats.conf", systemConfFile, systemPassword} {
			snapshot[f], _ = os.ReadFile(filepath.Join(dir, f))
		}
		if st, _ := os.Stat(filepath.Join(dir, systemPassword)); st.Mode().Perm() != 0o600 {
			t.Errorf("the system account password is mode %o", st.Mode().Perm())
		}
		if bytes.Contains(snapshot[systemConfFile], snapshot[systemPassword]) || bytes.Contains(snapshot["nats.conf"], snapshot[systemPassword]) {
			t.Error("the NATS configuration holds the cleartext system password")
		}
		changed, err = EnsureSystemAccount(dir, me.Username)
		if err != nil || changed {
			t.Fatalf("a second upgrade changed something: %v, %v", changed, err)
		}
		for f, want := range snapshot {
			if got, _ := os.ReadFile(filepath.Join(dir, f)); !bytes.Equal(got, want) {
				t.Errorf("%s changed on a second upgrade", f)
			}
		}
		if n := bytes.Count(snapshot["nats.conf"], []byte(systemInclude)); n != 1 {
			t.Errorf("the include appears %d times", n)
		}
		if _, err := server.ProcessConfigFile(filepath.Join(dir, "nats.conf")); err != nil {
			t.Fatalf("the upgraded nats.conf does not parse: %v", err)
		}
	})
}

// TestNATSMemberRemoval is the spike in docs/spikes/nats-member-removal.md: three clustered JetStream members run
// the generated configuration, one leaves, and the system account removes it from the meta group.
func TestNATSMemberRemoval(t *testing.T) {
	t.Run("SEC-HA-09", func(t *testing.T) {
		me, _ := user.Current()
		ca, err := Create(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		names := []string{"n1", "n2", "n3"}
		cport := map[string]int{}
		port := map[string]int{}
		for _, n := range names {
			cport[n], port[n] = freePort(t), freePort(t)
		}
		var routes []string
		for _, n := range names {
			routes = append(routes, fmt.Sprintf("nats-route://127.0.0.1:%d", cport[n]))
		}
		srv := map[string]*server.Server{}
		options := map[string]*server.Options{}
		dirs := map[string]string{}
		for _, n := range names {
			dir := t.TempDir()
			dirs[n] = dir
			sc, err := ca.Issue(NATSServer, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			for f, data := range map[string][]byte{"ca.crt": ca.CertPEM(), "server.crt": sc.Cert, "server.key": sc.Key} {
				if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			conf := Bundle{Host: n, NATSURLs: []string{NATSURL(n)}}.natsConfig(dir)
			if err := os.WriteFile(filepath.Join(dir, "nats.conf"), []byte(conf), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureSystemAccount(dir, me.Username); err != nil {
				t.Fatal(err)
			}
			opts, err := server.ProcessConfigFile(filepath.Join(dir, "nats.conf"))
			if err != nil {
				t.Fatalf("the generated nats.conf does not parse: %v", err)
			}
			if opts.SystemAccount != "SYS" || opts.NoAuthUser != defaultClientUser {
				t.Fatalf("system account %q, default user %q", opts.SystemAccount, opts.NoAuthUser)
			}
			opts.Host, opts.Port, opts.ClientAdvertise = "127.0.0.1", port[n], ""
			opts.Cluster.Host, opts.Cluster.Port, opts.Cluster.Advertise = "127.0.0.1", cport[n], ""
			opts.Routes = server.RoutesFromStr(fmt.Sprint(routes[0], ",", routes[1], ",", routes[2]))
			opts.StoreDir = filepath.Join(dir, "js")
			opts.NoSigs, opts.NoLog = true, true
			options[n] = opts
			s, err := server.NewServer(opts)
			if err != nil {
				t.Fatal(err)
			}
			go s.Start()
			srv[n] = s
			t.Cleanup(s.Shutdown)
		}
		for _, n := range names {
			if !srv[n].ReadyForConnections(10 * time.Second) {
				t.Fatalf("%s did not start", n)
			}
		}
		cc, err := ca.Issue(NATSClient, "controller")
		if err != nil {
			t.Fatal(err)
		}
		cdir := t.TempDir()
		_ = os.WriteFile(filepath.Join(cdir, "c.crt"), cc.Cert, 0o600)
		_ = os.WriteFile(filepath.Join(cdir, "c.key"), cc.Key, 0o600)
		url := func(n string) string { return fmt.Sprintf("tls://127.0.0.1:%d", port[n]) }
		controller, err := nats.Connect(url("n1"), nats.RootCAs(filepath.Join(dirs["n1"], "ca.crt")), nats.ClientCert(filepath.Join(cdir, "c.crt"), filepath.Join(cdir, "c.key")))
		if err != nil {
			t.Fatalf("a controller with only its certificate cannot connect after the upgrade: %v", err)
		}
		defer controller.Close()
		js, _ := jetstream.New(controller)
		ctx := context.Background()
		var stream jetstream.Stream
		deadline := time.Now().Add(30 * time.Second)
		for {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			stream, err = js.CreateStream(cctx, jetstream.StreamConfig{Name: "Samuori", Subjects: []string{"samuori.>"}, Replicas: 3})
			cancel()
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("an R3 stream in the global account: %v", err)
		}

		pw, _ := os.ReadFile(filepath.Join(dirs["n1"], systemPassword))
		if nc, err := dialSystem(url("n1"), []byte("wrong"), filepath.Join(dirs["n1"], "ca.crt"), filepath.Join(dirs["n1"], "server.crt"), filepath.Join(dirs["n1"], "server.key")); err == nil {
			nc.Close()
			t.Fatal("a wrong system password was accepted")
		}
		if nc, err := nats.Connect(url("n1"), nats.UserInfo(SystemUser, string(pw)), nats.RootCAs(filepath.Join(dirs["n1"], "ca.crt"))); err == nil {
			nc.Close()
			t.Fatal("the system account was reachable without a client certificate")
		}
		sys, err := dialSystem(url("n1"), bytes.TrimSpace(pw), filepath.Join(dirs["n1"], "ca.crt"), filepath.Join(dirs["n1"], "server.crt"), filepath.Join(dirs["n1"], "server.key"))
		if err != nil {
			t.Fatalf("system account: %v", err)
		}
		defer sys.Close()

		if info, err := srv["n1"].Jsz(&server.JSzOptions{}); err != nil || info.Meta == nil || info.Meta.Size != 3 {
			t.Fatalf("meta group before the removal: %+v, %v", info, err)
		}
		srv["n3"].Shutdown()
		srv["n3"].WaitForShutdown()
		deadline = time.Now().Add(30 * time.Second)
		for {
			err = RemoveServer(ctx, sys, "n3")
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			t.Fatalf("removing the departed member: %v", err)
		}
		for _, n := range []string{"n1", "n2"} {
			var size int
			deadline = time.Now().Add(15 * time.Second)
			for {
				if info, err := srv[n].Jsz(&server.JSzOptions{}); err == nil && info.Meta != nil {
					size = info.Meta.Size
				}
				if size == 2 || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if size != 2 {
				t.Fatalf("%s counts %d members in the meta group after the removal; want 2", n, size)
			}
		}
		if err := RemoveServer(ctx, sys, "n3"); err != nil {
			t.Errorf("removing it again is not idempotent: %v", err)
		}
		if _, err := js.UpdateStream(ctx, jetstream.StreamConfig{Name: "Samuori", Subjects: []string{"samuori.>"}, Replicas: 2}); err != nil {
			t.Fatalf("lowering the stream to the remaining members: %v", err)
		}
		if _, err := js.Publish(ctx, "samuori.test", []byte("x")); err != nil {
			t.Fatalf("JetStream after the removal: %v", err)
		}
		_ = stream

		conf, _ := os.ReadFile(filepath.Join(dirs["n1"], "nats.conf"))
		conf = routesLine.ReplaceAll(conf, []byte("${1}"+fmt.Sprint("nats-route://n1:6222, nats-route://n3:6222")+"${3}"))
		dropped, ok := DropRoute(conf, "n3")
		if !ok || bytes.Contains(dropped, []byte("n3:6222")) || !bytes.Contains(dropped, []byte("nats-route://n1:6222")) {
			t.Fatalf("routes after dropping n3:\n%s", dropped)
		}
		if _, again := DropRoute(dropped, "n3"); again {
			t.Error("dropping the route twice changed the file again")
		}
		opts := options["n1"].Clone()
		opts.Routes = server.RoutesFromStr(routes[0] + "," + routes[1])
		if err := srv["n1"].ReloadOptions(opts); err != nil {
			t.Fatalf("reloading routes without the departed member: %v", err)
		}
	})
}
