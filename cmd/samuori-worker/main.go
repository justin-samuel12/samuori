// Command samuori-worker runs builds for a controller. It dials out to the controller and needs no database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"samuori.invalid/samuori/internal/version"
	"strconv"
	"strings"
	"syscall"

	_ "samuori.invalid/samuori/internal/adapters/isolation/cloudhypervisor"
	_ "samuori.invalid/samuori/internal/adapters/isolation/rootlesshost"
	"samuori.invalid/samuori/internal/convertjob"
	"samuori.invalid/samuori/internal/egress/firewall"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/runtimeprobe"
	"samuori.invalid/samuori/internal/telemetry"
	"samuori.invalid/samuori/internal/toolrun"
	"samuori.invalid/samuori/internal/vmguest"
	"samuori.invalid/samuori/internal/workeragent"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "egress-rules" {
		os.Exit(egressRules(os.Args[2:]))
	}
	if len(os.Args) == 2 && os.Args[1] == "guest-agent" {
		if err := vmguest.Main(telemetry.NewLogger(os.Stderr, "info", "json")); err != nil {
			fmt.Fprintln(os.Stderr, "samuori-worker guest-agent:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "join-code" {
		os.Exit(joinCode(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) == 3 && os.Args[1] == "runtime-probe" {
		os.Exit(runtimeprobe.Main(os.Args[2]))
	}
	if len(os.Args) == 3 && os.Args[1] == "tool-run" {
		os.Exit(toolrun.Main(os.Args[2]))
	}
	var cfg workeragent.Config
	flag.StringVar(&cfg.Controller, "controller", "127.0.0.1:8444", "controller worker endpoint (host:port), or comma-separated endpoints of clustered controllers")
	flag.StringVar(&cfg.ServerName, "server-name", "localhost", "name in the controller certificate")
	flag.StringVar(&cfg.StateDir, "state-dir", "/var/lib/samuori-worker", "directory for the worker key and certificate")
	flag.StringVar(&cfg.CAPin, "ca-pin", "", "controller CA pin (sha256:...), required for enrollment")
	pinFile := flag.String("ca-pin-file", "", "file holding the controller CA pin, instead of --ca-pin")
	flag.StringVar(&cfg.TokenFile, "enrollment-token-file", "", "file holding a one-time enrollment token (mode 0600); deleted after use")
	flag.StringVar(&cfg.Name, "name", "", "worker name shown to operators")
	flag.StringVar(&cfg.EnvironmentImage, "environment-image", "", "digest-pinned build environment image used by the isolation provider")
	flag.StringVar(&cfg.ProvidersDir, "providers-dir", convertjob.ProvidersDir, "directory of optional conversion tools as <name>/<name> with <name>/VERSION; a tool runs only at its approved version and checksum")
	provider := flag.String("isolation-provider", "none", "build isolation provider: none, kvm-cloud-hypervisor (vm-per-job on a KVM host), rootless-dedicated-vm (a dedicated Linux VM; untrusted builds once the pool is verified) or rootless-host (trusted workloads only)")
	providerDir := flag.String("provider-dir", "/var/lib/samuori-worker/environments", "directory for build environments")
	flags := map[string]*string{}
	for _, s := range []struct{ key, def, usage string }{
		{"egress", "strict", "build egress: strict (proxy, build user and firewall) or unrestricted (trusted development hosts only)"},
		{"egress-listen", "", "address for the egress proxy, reachable from builds and allowed by the worker firewall (host:port)"},
		{"build-user", "", "dedicated user that runs build daemons; the firewall confines its traffic"},
		{"build-group", "", "group shared by the worker and the build user"},
		{"daemon-dir", "", "directory for build daemon state, writable by the build group"},
		{"mirrors", "", "registry mirrors as upstream=host:port, comma-separated, for example docker.io=10.0.0.5:5000"},
		{"insecure-mirrors", "false", "reach mirrors over plain HTTP (development only)"},
		{"probe-binary", "", "static runtime-test probe; defaults to this samuori-worker binary"},
		{"web-launcher", "", "static samuori-web copied into images that serve a frontend; defaults to samuori-web next to this binary"},
		{"cgroup-limits", "", "systemd: run each daemon in a systemd user scope with its memory and process limits (required by rootless-dedicated-vm)"},
		{"dedicated-host", "false", "confirm this VM runs nothing but this worker (required by rootless-dedicated-vm)"},
		{"max-processes", "", "process limit for each build daemon and its steps (default 2048)"},
		{"recycle-after-builds", "", "stop accepting builds after this many, so the VM can be replaced (rootless-dedicated-vm, default 100)"},
		{"recycle-after-hours", "", "stop accepting builds after this many hours (rootless-dedicated-vm, default 24)"},
		{"engine", "", "build engine: buildkit (default) or buildah, which builds without secrets only"},
		{"slots", "", "builds this worker runs at once (default 1)"},
		{"vmm", "", "kvm-cloud-hypervisor: path of the cloud-hypervisor binary"},
		{"vm-image", "", "kvm-cloud-hypervisor: guest image manifest built by tools/vm-image"},
		{"warm-vms", "", "kvm-cloud-hypervisor: VMs booted ahead for builds with the default limits (each still serves one build)"},
		{"vm-vcpus", "", "kvm-cloud-hypervisor: default vCPUs per VM (2)"},
		{"vm-memory-gib", "", "kvm-cloud-hypervisor: default build memory per VM in GiB (4)"},
		{"vm-disk-gib", "", "kvm-cloud-hypervisor: default disk per VM in GiB, the build's disk quota (20)"},
		{"vm-max-vcpus", "", "kvm-cloud-hypervisor: most vCPUs a build may request (4)"},
		{"vm-max-memory-gib", "", "kvm-cloud-hypervisor: most memory a build may request in GiB (8)"},
		{"vm-max-disk-gib", "", "kvm-cloud-hypervisor: most disk a build may request in GiB (40)"},
	} {
		flags[s.key] = flag.String(s.key, s.def, s.usage)
	}
	showVersion := flag.Bool("version", false, "print the version and exit")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	tracingEndpoint := flag.String("tracing-endpoint", "", "OTLP/HTTP traces URL; attempts then continue the controller's traces")
	tracingRatio := flag.Float64("tracing-sample-ratio", 1, "fraction of new traces sampled")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		fmt.Println(version.Credit)
		return
	}
	settings := map[string]string{"dir": *providerDir}
	for k, v := range flags {
		if *v != "" {
			settings[k] = *v
		}
	}
	if self, err := os.Executable(); err == nil {
		if settings["probe-binary"] == "" {
			settings["probe-binary"] = self
		}
		if settings["web-launcher"] == "" {
			settings["web-launcher"] = filepath.Join(filepath.Dir(self), "samuori-web")
		}
	}
	if *pinFile != "" && cfg.CAPin == "" {
		if raw, err := os.ReadFile(*pinFile); err == nil {
			cfg.CAPin = strings.TrimSpace(string(raw))
		}
	}
	cfg.Version = version.Version
	log := telemetry.NewLogger(os.Stderr, *logLevel, "json")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{Endpoint: *tracingEndpoint, SampleRatio: *tracingRatio, Service: "samuori-worker", Version: version.Version})
	if err != nil {
		fmt.Fprintln(os.Stderr, "samuori-worker:", err)
		os.Exit(2)
	}
	defer func() { _ = shutdown(context.Background()) }()
	p, err := isolation.Open(*provider, settings)
	if err != nil {
		fmt.Fprintln(os.Stderr, "samuori-worker:", err)
		os.Exit(2)
	}
	if r, ok := p.(interface{ ImageRef() string }); ok && cfg.EnvironmentImage == "" {
		cfg.EnvironmentImage = r.ImageRef()
	}
	a := workeragent.New(cfg, log)
	a.UseProvider(p)
	h := a.Host()
	log.Info("host isolation facts", "os", h.OS, "arch", h.Arch, "kvm", h.KVM, "nestedVirtualization", h.NestedVirt, "candidateProfiles", h.CandidateProfiles, "provider", *provider)
	for _, n := range h.Notes {
		log.Info("isolation note", "note", n)
	}
	if p == nil {
		log.Warn("no isolation provider: this worker cannot build uploaded source")
	}
	if expired := a.Enrolled() && a.Expired(); !a.Enrolled() || expired {
		if expired {
			log.Warn("the worker certificate has expired; re-enrolling with the enrollment token")
		}
		if cfg.TokenFile == "" {
			fmt.Fprintln(os.Stderr, "samuori-worker: not enrolled; pass --enrollment-token-file and --ca-pin")
			os.Exit(2)
		}
		id, err := a.Enroll(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "samuori-worker: enrollment failed:", err)
			os.Exit(1)
		}
		log.Info("enrolled", "worker", id)
	}
	if err := a.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "samuori-worker:", err)
		// Exit status 3 tells systemd not to restart: only a new enrollment can help (RestartPreventExitStatus).
		if errors.Is(err, workeragent.ErrIdentityExpired) || errors.Is(err, workeragent.ErrIdentityRevoked) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

// egressRules prints the nftables ruleset that confines the build user to the egress proxy and mirrors. An operator
// applies it as root, for example: samuori-worker egress-rules --build-user samuori-build --allow 10.0.0.5:3128 | nft -f -
func egressRules(args []string) int {
	fs := flag.NewFlagSet("egress-rules", flag.ContinueOnError)
	buildUser := fs.String("build-user", "", "dedicated build user")
	allow := fs.String("allow", "", "comma-separated proxy and mirror addresses (ip:port)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	u, err := user.Lookup(*buildUser)
	if err != nil {
		fmt.Fprintln(os.Stderr, "samuori-worker egress-rules:", err)
		return 2
	}
	uid, _ := strconv.Atoi(u.Uid)
	var addrs []netip.AddrPort
	for _, a := range strings.Split(*allow, ",") {
		ap, err := netip.ParseAddrPort(strings.TrimSpace(a))
		if err != nil {
			fmt.Fprintln(os.Stderr, "samuori-worker egress-rules: --allow needs ip:port values:", err)
			return 2
		}
		addrs = append(addrs, ap)
	}
	rs, err := firewall.Ruleset(firewall.Config{BuildUID: uid, Allow: addrs})
	if err != nil {
		fmt.Fprintln(os.Stderr, "samuori-worker egress-rules:", err)
		return 2
	}
	fmt.Print(rs)
	return 0
}
