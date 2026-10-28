package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/discovery"
)

// The test binary doubles as the subprocess: the first argument selects the child behaviour.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "child-") {
		os.Exit(child(os.Args[1], os.Args[2:]))
	}
	os.Exit(m.Run())
}

// platformChildModes run their own probes around restrict.
var platformChildModes = map[string]func() int{}

func child(mode string, args []string) int {
	if f, ok := platformChildModes[mode]; ok {
		return f()
	}
	if mode == "child-discover" {
		if err := ChildMain(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	l := DefaultLimits()
	fmt.Sscan(os.Getenv("SAMUORI_DISCOVERY_CPU"), &l.CPUSeconds)
	fmt.Sscan(os.Getenv("SAMUORI_DISCOVERY_AS"), &l.AddressSpaceBytes)
	if err := restrict(l); err != nil {
		fmt.Fprintln(os.Stderr, "restrict:", err)
		return 3
	}
	report := func(s string) int {
		fmt.Printf(`{"findings":[],"dockerfiles":[],"warnings":[{"code":%q,"message":"probe"}]}`, s)
		return 0
	}
	switch mode {
	case "child-net":
		var results []string
		for _, target := range append(args, "1.1.1.1:53") {
			c, err := net.DialTimeout("tcp", target, time.Second)
			if err == nil {
				c.Close()
				results = append(results, "connected:"+target)
			} else {
				results = append(results, "blocked")
			}
		}
		return report(strings.Join(results, ","))
	case "child-exec":
		if err := exec.Command("/bin/true").Run(); err != nil {
			return report("blocked")
		}
		return report("spawned")
	case "child-hang":
		for {
		}
	case "child-memory":
		var keep [][]byte
		for i := 0; i < 64; i++ {
			b := make([]byte, 128<<20)
			for j := range b {
				b[j] = 1
			}
			keep = append(keep, b)
		}
		return report(fmt.Sprintf("allocated-%d", len(keep)))
	case "child-badout":
		fmt.Print(`{"findings":[{"detectorId":"x","detectorVersion":"1","sourceSubpath":"../../etc","confidence":"high","evidence":[]}],"dockerfiles":[],"warnings":[]}`)
		return 0
	case "child-unknown":
		fmt.Print(`{"findings":[],"dockerfiles":[],"warnings":[],"exec":"rm -rf /"}`)
		return 0
	case "child-flood":
		fmt.Print(strings.Repeat("x", 10<<20))
		return 0
	}
	return 2
}

func runner(mode string, args ...string) Runner {
	l := DefaultLimits()
	l.Timeout = 5 * time.Second
	return Runner{Command: append([]string{os.Args[0], mode}, args...), Limits: l, Namespaces: true}
}

var input = discovery.Input{Files: []discovery.InputFile{{Path: "package.json", Size: 2, Content: []byte(`{"name":"x","scripts":{"start":"node ."},"dependencies":{"express":"5"}}`)}}}

func probe(t *testing.T, r Runner) string {
	t.Helper()
	res, _, err := r.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return res.Warnings[0].Code
}

func linuxOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process and network denial are implemented with Linux seccomp and namespaces; run in the Linux dev VM")
	}
}

func TestDiscoveryRunsOutOfProcess(t *testing.T) {
	res, sb, err := runner("child-discover").Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Framework != "Express" {
		t.Fatalf("findings = %+v", res.Findings)
	}
	t.Logf("sandbox: %+v", sb)
	if runtime.GOOS == "linux" && !sb.Seccomp {
		t.Fatal("seccomp not reported on Linux")
	}
	if runtime.GOOS != "linux" && res.Warnings[len(res.Warnings)-1].Code != "sandbox-degraded" {
		t.Fatal("degraded sandbox not reported")
	}
}

func TestNoNetwork(t *testing.T) {
	t.Run("SEC-DSC-01", func(t *testing.T) {
		linuxOnly(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		for _, ns := range []bool{true, false} {
			r := runner("child-net", ln.Addr().String())
			r.Namespaces = ns
			if got := probe(t, r); got != "blocked,blocked" {
				t.Fatalf("namespaces=%v: %s", ns, got)
			}
		}
	})
}

func TestResourceLimits(t *testing.T) {
	t.Run("SEC-DSC-02", func(t *testing.T) {
		r := runner("child-hang")
		r.Limits.Timeout, r.Limits.CPUSeconds = 3*time.Second, 1
		start := time.Now()
		if _, _, err := r.Run(context.Background(), input); !errors.Is(err, ErrSandbox) || time.Since(start) > 6*time.Second {
			t.Fatalf("hung child: %v after %s", err, time.Since(start))
		}
		if _, _, err := runner("child-flood").Run(context.Background(), input); !errors.Is(err, ErrSandbox) {
			t.Fatalf("output flood: %v", err)
		}
		big := discovery.Input{Files: []discovery.InputFile{{Path: "a", Content: make([]byte, 65<<20)}}}
		if _, _, err := runner("child-discover").Run(context.Background(), big); err == nil {
			t.Fatal("oversized input accepted")
		}
		for _, mode := range []string{"child-badout", "child-unknown"} {
			if _, _, err := runner(mode).Run(context.Background(), input); !errors.Is(err, ErrSandbox) {
				t.Errorf("%s accepted: %v", mode, err)
			}
		}
		if runtime.GOOS == "linux" {
			r := runner("child-memory")
			r.Limits.AddressSpaceBytes = 1 << 30
			if _, _, err := r.Run(context.Background(), input); !errors.Is(err, ErrSandbox) {
				t.Fatalf("8 GiB allocation under a 1 GiB limit: %v", err)
			}
		}
	})
}

func TestNoProcessSpawning(t *testing.T) {
	t.Run("SEC-DSC-03", func(t *testing.T) {
		linuxOnly(t)
		if got := probe(t, runner("child-exec")); got != "blocked" {
			t.Fatalf("exec from discovery: %s", got)
		}
	})
}
