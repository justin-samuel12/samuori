package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/adapters/git/gogit"
	ldapadapter "samuori.invalid/samuori/internal/adapters/identity/ldap"
	oidcadapter "samuori.invalid/samuori/internal/adapters/identity/oidc"
	"samuori.invalid/samuori/internal/adapters/kms"
	"samuori.invalid/samuori/internal/adapters/notifysend"
	ecradapter "samuori.invalid/samuori/internal/adapters/registry/ecr"
	"samuori.invalid/samuori/internal/adapters/registry/tags"
	"samuori.invalid/samuori/internal/adapters/secretstore/awssm"
	"samuori.invalid/samuori/internal/adapters/secretstore/openbao"
	ocistore "samuori.invalid/samuori/internal/adapters/storage/oci"
	s3store "samuori.invalid/samuori/internal/adapters/storage/s3"
	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/api"
	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/backup"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/capacity"
	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/clusterjoin"
	"samuori.invalid/samuori/internal/config"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/dbcredentials"
	"samuori.invalid/samuori/internal/discovery/jobs"
	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/federation"
	"samuori.invalid/samuori/internal/health"
	"samuori.invalid/samuori/internal/identity"
	"samuori.invalid/samuori/internal/imagesources"
	"samuori.invalid/samuori/internal/kubeconvert"
	"samuori.invalid/samuori/internal/messaging"
	"samuori.invalid/samuori/internal/notify"
	"samuori.invalid/samuori/internal/observability"
	"samuori.invalid/samuori/internal/operations"
	"samuori.invalid/samuori/internal/pipelines"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/providers"
	"samuori.invalid/samuori/internal/publication"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/selfupdate"
	"samuori.invalid/samuori/internal/server"
	"samuori.invalid/samuori/internal/signing"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/telemetry"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/topology"
	"samuori.invalid/samuori/internal/validation"
	"samuori.invalid/samuori/internal/version"
	"samuori.invalid/samuori/internal/workers"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(version.Version)
		fmt.Println(version.Credit)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "scan-image" {
		if err := validation.ScanChildMain(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "scan-image:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "default-hostname" {
		fmt.Println((&setupHost{}).DefaultHostname(context.Background()))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "update-bundle" {
		if err := updateBundle(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "update-bundle:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "new-cluster-key" {
		if err := newClusterKey(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "new-cluster-key:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "backup" || os.Args[1] == "verify-backup" || os.Args[1] == "restore") {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		var err error
		switch os.Args[1] {
		case "backup":
			err = backupCommand(ctx, os.Args[2:], os.Stdout)
		case "verify-backup":
			err = verifyBackupCommand(ctx, os.Args[2:], os.Stdout)
		default:
			err = restoreCommand(ctx, os.Args[2:], os.Stdout)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, os.Args[1]+":", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "set-database-password" {
		if err := setDatabasePassword(context.Background(), os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "set-database-password:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "preflight" {
		if err := preflight(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "preflight:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		if err := setupCommand(ctx, os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "samuori-setup:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "cluster" {
		if err := clusterCommand(os.Args[2], os.Args[3:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "samuori-server cluster "+os.Args[2]+":", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "topology" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := topologyCommand(ctx, os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "samuori-server topology:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "check-config" {
		cfg, origins, err := config.Load(os.Args[2:], os.Environ(), config.OSReadFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "check-config:", err)
			os.Exit(1)
		}
		fmt.Printf("configuration is valid: role %s, cluster %t, %d settings from file, %d from the environment\n", cfg.Role, cfg.Cluster, count(origins, config.SourceFile), count(origins, config.SourceEnv))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "prepare-secrets" {
		if err := prepareSecrets(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "prepare-secrets:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate-database" {
		if err := migrateDatabase(context.Background(), os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "migrate-database:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		if err := sandbox.ChildMain(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "discover:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "samuori-server:", err)
		os.Exit(1)
	}
}

// loadConfig applies a database password set on the Database page; configured is the DSN as written
// (docs/operators/database.md).
func loadConfig(args []string) (cfg config.Config, origins map[string]config.Source, configured string, err error) {
	if cfg, origins, err = config.Load(args, os.Environ(), config.OSReadFile); err != nil {
		return cfg, nil, "", err
	}
	configured = cfg.Database
	if cfg.Database, err = db.EffectiveDSN(cfg.Database, cfg.DataDir); err != nil {
		return cfg, nil, "", err
	}
	if cfg.MigrationDB == configured {
		cfg.MigrationDB = cfg.Database
	}
	return cfg, origins, configured, nil
}

func run(ctx context.Context, args []string) error {
	cfg, origins, configuredDSN, err := loadConfig(args)
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(os.Stderr, cfg.LogLevel, cfg.LogFormat)
	log.Info("configuration loaded", "role", cfg.Role, "dataDir", cfg.DataDir, "sources", origins)
	stopTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{Endpoint: cfg.TracingEndpoint, SampleRatio: cfg.TracingSampleRatio})
	if err != nil {
		return err
	}
	defer func() { _ = stopTracing(context.WithoutCancel(ctx)) }()
	if cfg.TracingEndpoint != "" {
		log.Info("tracing enabled", "endpoint", cfg.TracingEndpoint, "sampleRatio", cfg.TracingSampleRatio)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	up, err := migrate(ctx, cfg, log)
	if err != nil {
		return err
	}
	store, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	log.Info("database ready", "dialect", store.Dialect)
	member, err := cluster.JoinAs(ctx, store, version.Version, controllerIdentity(cfg), log)
	if err != nil {
		return err
	}
	go member.Run(ctx)
	log.Info("controller registered", "controller", member.ID, "cluster", cfg.Cluster, "version", version.Version)

	checks := health.NewRegistry(2 * time.Second)
	checks.Register(health.Check{Name: "clock", Critical: true, Run: member.CheckClock})
	if cfg.Cluster {
		checks.Register(health.Check{Name: "cluster-membership", Critical: true, Run: member.CheckFresh})
	}
	checks.Register(health.Check{Name: "controller-versions", Run: func(ctx context.Context) error {
		list, err := member.Controllers(ctx)
		if err != nil {
			return err
		}
		for _, c := range list {
			if c.Live && !version.ControllersCompatible(version.Version, c.Version) {
				return fmt.Errorf("controller %s runs %s, outside the supported upgrade window of %s", c.Hostname, c.Version, version.Version)
			}
		}
		return nil
	}})
	checks.Register(health.Check{Name: "data-dir", Critical: true, Run: func(context.Context) error {
		return writable(cfg.DataDir)
	}})
	checks.Register(health.Check{Name: "database", Critical: true, Run: store.Ping})

	blobs, err := storage.NewBlobs(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		return fmt.Errorf("artifact storage: %w", err)
	}
	checks.Register(health.Check{Name: "artifact-storage", Critical: true, Run: func(context.Context) error {
		return writable(filepath.Join(blobs.Root(), "tmp"))
	}})
	remotes, err := openRemotes(ctx, cfg)
	if err != nil {
		return err
	}
	for name, r := range remotes {
		checks.Register(health.Check{Name: map[string]string{"s3": "object-storage", "oci": "registry-storage"}[name], Critical: true, Run: r.Check})
	}
	for _, r := range remotes {
		if err := r.Check(ctx); err != nil {
			return fmt.Errorf("%s: %w", r.Describe(), err)
		}
	}
	for _, c := range storage.Classes {
		backend := cfg.StorageRoute(string(c))
		blobs.Route(c, remotes[backend])
		log.Info("storage route", "class", c, "backend", backend)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	tenants := tenancy.NewStore(store)
	authPolicy := auth.DefaultPolicy()
	if cfg.DevWeakPasswords {
		authPolicy.DevWeakPasswords = true
		log.Warn("local development: local account passwords are exempt from the password policy (dev.weakPasswords)")
	}
	authn := auth.NewService(store, tenants, authPolicy)
	if err := bootstrap(ctx, cfg, authn, log); err != nil {
		return err
	}
	topologyPlans := topology.NewStore(store)
	if cfg.ConfigFile != "" {
		var org string
		if err := store.SQL.QueryRowContext(ctx, `SELECT id FROM organizations ORDER BY created_at LIMIT 1`).Scan(&org); err == nil {
			if imported, err := topologyPlans.ImportFile(ctx, org, filepath.Join(filepath.Dir(cfg.ConfigFile), topology.PlanFile)); err != nil {
				log.Warn("topology plan from samuori-setup not imported", "error", err)
			} else if imported {
				log.Info("topology plan imported from samuori-setup")
			}
		}
	}
	ops := operations.NewStore(store)
	uploads, err := sources.NewService(store, blobs, ops, tenants, log, filepath.Join(cfg.DataDir, "uploads"), sources.DefaultConfig())
	if err != nil {
		return err
	}
	uploads.UseOwnership(member)
	if cfg.Cluster {
		uploads.ShareChunks()
	}
	if err := uploads.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconciling uploads: %w", err)
	}
	go uploads.RunJanitor(ctx, 10*time.Minute)
	defer uploads.Wait()

	self, err := os.Executable()
	if err != nil {
		return err
	}
	runner := sandbox.Runner{Command: []string{self, "discover"}, Limits: sandbox.DefaultLimits(), Namespaces: true}
	catalog, err := recipes.LoadBuiltin()
	if err != nil {
		return fmt.Errorf("recipe catalog: %w", err)
	}
	policies := policy.NewStore(store, catalog)
	if err := policies.EnsureDefaults(ctx); err != nil {
		return fmt.Errorf("default policies: %w", err)
	}
	discoveries := jobs.NewService(store, uploads, ops, runner, policies, log)
	discoveries.UseOwnership(member)
	if err := discoveries.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconciling discoveries: %w", err)
	}
	go discoveries.RunReconciler(ctx, 30*time.Second)
	defer discoveries.Wait()
	recipeStatus := recipes.NewStatuses(store, catalog)
	recipeStatus.UseImagePolicy(policies.BaseImageRefusal)
	specSvc := specs.NewService(store, tenants, policies)

	var secretSvc *secrets.Service
	if cfg.Cluster {
		secretSvc, err = secrets.NewServiceWithKey(store, cfg.ClusterKeyFile)
	} else {
		secretSvc, err = secrets.NewService(store, filepath.Join(cfg.DataDir, "keys"))
	}
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	dbCreds, err := dbcredentials.New(store, member, cfg.DataDir, configuredDSN, secretSvc.Derive("database-password"), log)
	if err != nil {
		return fmt.Errorf("database password: %w", err)
	}
	go dbCreds.Run(ctx)
	pkiDir := filepath.Join(cfg.DataDir, "pki")
	var sharedPKI *cluster.SharedDir
	if cfg.Cluster {
		if sharedPKI, err = cluster.NewSharedDir(store, "pki", pkiDir, member.ID, secretSvc.Derive("cluster-material")); err != nil {
			return err
		}
		if _, err := sharedPKI.Pull(ctx); err != nil {
			return fmt.Errorf("shared controller material: %w", err)
		}
	}
	ca, err := pki.Open(pkiDir)
	if err != nil {
		return fmt.Errorf("internal CA: %w", err)
	}
	var join api.ClusterJoin
	if cfg.Cluster {
		if join, err = clusterJoin(ctx, cfg, store, member.ID, secretSvc.Derive("cluster-material")); err != nil {
			return err
		}
	}
	workerStore := workers.NewStore(store, ca)
	workerSrv := workers.NewServer(workerStore, log)
	bao := openbao.New()
	secretSvc.UseExternal(secrets.ProviderOpenBao, bao)
	secretSvc.UseExternal(secrets.ProviderVault, bao)
	secretSvc.UseExternal(secrets.ProviderAWS, awssm.New())
	checker := admission.NewChecker(specSvc, uploads, recipeStatus, policies, discoveries, workerStore)
	checker.UseBindings(secretSvc)
	providerStore := providers.NewStore(store)
	checker.UseMethodPolicy(providerStore)
	imageSources := imagesources.NewStore(store, secretSvc)
	imageSources.Shared = cfg.SharedMirrors
	checker.UseImageSources(imageSources)
	checker.UseTagResolver(tags.New())
	buildSvc := builds.NewService(store, tenants, specSvc, uploads, recipeStatus, policies, checker)
	buildSvc.UseSecrets(secretSvc)
	buildSvc.UseProviders(providerStore)
	buildSvc.UsePlacement(workerStore)
	fedStore := federation.NewStore(store)
	buildSvc.UsePeerRoutes(fedStore)
	var bus *messaging.Bus
	if m := cfg.Messaging; len(m.URLs) > 0 {
		bus, err = messaging.Connect(ctx, messaging.External{URLs: m.URLs, CredentialsFile: m.CredentialsFile, CAFile: m.CAFile, CertFile: m.CertFile, KeyFile: m.KeyFile,
			Replicas: m.Replicas, Name: "samuori-controller-" + member.ID}, log)
	} else {
		bus, err = messaging.Start(ctx, filepath.Join(cfg.DataDir, "nats"), log)
	}
	if err != nil {
		return err
	}
	defer bus.Close()
	checks.Register(health.Check{Name: "removal", Critical: true, Run: member.Draining})
	checks.Register(health.Check{Name: "messaging", Critical: true, Run: func(ctx context.Context) error {
		err := bus.Healthy(ctx)
		if err != nil && cfg.Cluster && member.NeverJoined(ctx) {
			return health.Degraded(fmt.Errorf("waiting for a second controller to join: %w", err))
		}
		return err
	}})
	if bus.External() {
		go bus.GrowReplicas(ctx, 30*time.Second)
		checks.Register(health.Check{Name: "messaging-replicas", Run: func(ctx context.Context) error {
			have, want, err := bus.Replicas(ctx)
			if err == nil && have < want {
				err = fmt.Errorf("the stream has %d of %d replicas; it grows as NATS members join", have, want)
			}
			return err
		}})
	}
	relay := messaging.NewRelay(store, bus)
	buildSvc.OnCommit(relay.Notify)
	art, err := artifacts.NewStore(store, blobs, pkiDir)
	if err != nil {
		return fmt.Errorf("artifact store: %w", err)
	}
	if sharedPKI != nil {
		if err := sharedPKI.Push(ctx); err != nil {
			return fmt.Errorf("sharing controller material (if another controller started at the same moment, start this one again): %w", err)
		}
		ca.OnSave(func() error { return sharedPKI.Push(context.WithoutCancel(ctx)) })
		go followPKI(ctx, sharedPKI, ca, workerSrv, log)
	}
	art.CacheLimits = artifacts.CacheLimits{TTL: cfg.BuildCacheTTL, MaxScopeBytes: cfg.BuildCacheMaxScopeBytes}
	scheduler := builds.NewScheduler(buildSvc, workerStore, workerSrv, art, log)
	var sbom validation.SBOMGenerator
	if s := validation.NewSyft(); s != nil {
		sbom = s
	} else {
		log.Warn("syft is not installed; SBOM checks will be skipped")
	}
	scanner := validation.SandboxScanner{Runner: sandbox.Runner{Command: []string{self, "scan-image"}, Limits: validation.ScanLimits(), Namespaces: true}}
	validations := validation.NewService(store, blobs, tenants, policies, scanner, sbom, log)
	validations.UseOwnership(member)
	vulns, err := validation.NewVulnerabilityScanner(cfg.VulnerabilityScanner)
	if err != nil {
		return err
	}
	if vulns != nil {
		validations.UseVulnerabilityScanner(vulns)
		log.Info("vulnerability scanner enabled", "scanner", cfg.VulnerabilityScanner)
	}
	validations.OnCommit(relay.Notify)
	scheduler.UseValidations(validations)
	scheduler.UseMirrorTokens(imageSources)
	approvalSvc := approvals.NewService(store, tenants)
	publications := publication.NewService(store, blobs, tenants, secretSvc, validations, publication.OrasPusher{}, log)
	publications.UseOwnership(member)
	publications.UseDownloadPolicy(policies.DeveloperDownloads)
	publications.OnCommit(relay.Notify)
	publications.UseApprovals(approvalSvc)
	approvalSvc.UseBaseImagePolicy(policies)
	approvalSvc.UseEgressPolicy(policies)
	checker.UseEgressAllowances(approvalSvc)
	buildSvc.UseEgressAllowances(approvalSvc)
	publications.UseECR(ecradapter.New())
	signingKeys := signing.NewStore(store, secretSvc)
	signingKeys.UseRemote(&kms.Remote{Secrets: secretSvc, Bao: bao})
	publications.UseSigning(signingKeys)
	validations.OnComplete(func(ctx context.Context, tx *sql.Tx, r validation.Run) error {
		if r.BuildRunID == "" {
			return nil
		}
		if err := buildSvc.RecordEvent(ctx, tx, r.BuildRunID, "validation", map[string]string{"validationId": r.ID, "state": r.State, "verdict": r.Verdict}); err != nil {
			return err
		}
		return publications.AutoPublish(ctx, tx, r)
	})
	if err := publications.ReleaseStale(ctx); err != nil {
		return fmt.Errorf("releasing publication locks: %w", err)
	}
	go member.Singleton(ctx, "artifact-gc", 6*time.Hour, func(ctx context.Context, _ int64) error {
		res, err := art.Collect(ctx, artifacts.DefaultGCPolicy())
		if err == nil {
			log.Info("artifact garbage collection", "deleted", res.Deleted, "referenced", res.Referenced, "keptForGrace", res.Kept, "droppedArtifacts", res.DroppedArtifacts)
		}
		return err
	})
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := blobs.EvictCache(cfg.CacheEvictAfter); err != nil {
					log.Error("cache eviction", "error", err)
				} else if n > 0 {
					log.Info("cache eviction", "removed", n)
				}
			}
		}
	}()
	workerSrv.UseAttempts(scheduler)
	kube := kubeconvert.NewService(store, tenants, uploads, blobs, providerStore, ops)
	kube.OnCommit(relay.Notify)
	kubeScheduler := kubeconvert.NewScheduler(kube, workerStore, workerSrv, log)
	workerSrv.UseConversions(kubeScheduler)
	updates := selfupdate.New(store, blobs, member, workerSrv, runtime.GOARCH, log)
	workerSrv.UseReleases(updates)
	go member.Singleton(ctx, "worker-removals", 5*time.Second, func(ctx context.Context, _ int64) error { return workerSrv.ProcessRemovals(ctx) })
	clusterCA, _ := os.ReadFile(filepath.Join(cfg.DataDir, "cluster-pki", "ca.crt"))
	removals := controllerRemovals(member, clusterjoin.MembersOnControllers(cfg, clusterCA), bus)
	removals.LocalWorker = localWorkerRemoval(workerStore, workerSrv)
	go member.Singleton(ctx, "controller-removals", 5*time.Second, removals.Process)
	go runRemovals(ctx, removals, log)
	go updates.Run(ctx)
	hosts := capacity.New(store, member.ID, cfg.DataDir, log)
	go hosts.Run(ctx)
	if cfg.Cluster {
		if err := workerSrv.UseCluster(ctx, member, bus, secretSvc.Derive("controller-signals")); err != nil {
			return fmt.Errorf("worker routing: %w", err)
		}
	}
	member.OnLeadership(func(ctx context.Context) {
		if err := buildSvc.Rewake(ctx); err != nil {
			log.Error("re-enqueuing builds", "error", err)
		}
	})
	go relay.Run(ctx)
	go workerSrv.RunSelfTestRenewal(ctx, 30*time.Minute)
	go member.Singleton(ctx, "ca-retirement", time.Hour, func(context.Context, int64) error { return workerSrv.RetireOnce() })
	go member.Singleton(ctx, "controller-pruning", 24*time.Hour, func(ctx context.Context, _ int64) error { return member.Prune(ctx) })
	workerSrv.OnRevoke(func(ctx context.Context, w workers.Worker, compromisedSince *time.Time) error {
		if err := buildSvc.FenceWorker(ctx, w.ID, w.RevokeReason); err != nil {
			return err
		}
		if compromisedSince == nil {
			return nil
		}
		n, err := art.QuarantineWorker(ctx, w.OrgID, w.ID, *compromisedSince, w.RevokeReason)
		log.Warn("worker revoked as compromised; its images are quarantined", "worker", w.ID, "images", n)
		return err
	})
	if cfg.LocalWorkerDir != "" {
		lw := &workers.LocalWorker{Store: workerStore, Dir: cfg.LocalWorkerDir, Profile: cfg.LocalWorkerProfile, Log: log}
		if err := lw.CheckDir(); err != nil {
			return err
		}
		go lw.Run(ctx, 15*time.Second)
	}
	for _, c := range []struct{ durable, subject string }{{"dispatch", builds.SubjectQueued}, {"deliver", builds.SubjectAssign}, {"cancel", builds.SubjectCancel}} {
		go func() {
			if err := bus.Consume(ctx, c.durable, c.subject, scheduler.Deliver); err != nil {
				log.Error("consumer stopped", "consumer", c.durable, "error", err)
			}
		}()
	}
	for _, c := range []struct{ durable, subject string }{{"conversion-dispatch", kubeconvert.SubjectQueued}, {"conversion-deliver", kubeconvert.SubjectAssign}, {"conversion-cancel", kubeconvert.SubjectCancel}} {
		go func() {
			if err := bus.Consume(ctx, c.durable, c.subject, kubeScheduler.Deliver); err != nil {
				log.Error("consumer stopped", "consumer", c.durable, "error", err)
			}
		}()
	}
	go func() {
		if err := bus.Consume(ctx, "validate", validation.SubjectPending, validations.Deliver); err != nil {
			log.Error("consumer stopped", "consumer", "validate", "error", err)
		}
	}()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			if err := validations.Reconcile(ctx); err != nil && ctx.Err() == nil {
				log.Error("validation reconciliation", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	go func() {
		if err := bus.Consume(ctx, "publish", publication.SubjectRequested, publications.Deliver); err != nil {
			log.Error("consumer stopped", "consumer", "publish", "error", err)
		}
	}()
	go publications.RunReconciler(ctx, 15*time.Second, func(err error) { log.Error("publication reconciliation", "error", err) })
	go buildSvc.RunReconciler(ctx, 5*time.Second, func(err error) { log.Error("build reconciliation", "error", err) })
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := scheduler.Dispatch(ctx); err != nil {
					log.Error("dispatch", "error", err)
				}
				if _, err := kubeScheduler.Dispatch(ctx); err != nil {
					log.Error("conversion dispatch", "error", err)
				}
			}
		}
	}()
	origin := &federation.Origin{Store: fedStore, Builds: buildSvc, Scheduler: scheduler, Sources: uploads, CA: ca, Log: log, AllowPrivatePeers: cfg.FederationPrivatePeers}
	go member.Singleton(ctx, "federation", 5*time.Second, func(ctx context.Context, _ int64) error { return origin.Step(ctx) })
	if cfg.FederationListen != "" {
		if err := serveFederation(ctx, cfg, store, &federation.Executor{Store: fedStore, Sources: uploads, Specs: specSvc, Builds: buildSvc, Validations: validations, Blobs: blobs, CA: ca, Log: log}, log); err != nil {
			return err
		}
	}
	if cfg.WorkerListen != "" {
		wln, err := net.Listen("tcp", cfg.WorkerListen)
		if err != nil {
			return fmt.Errorf("worker endpoint: %w", err)
		}
		go func() {
			if err := workerSrv.Serve(ctx, wln, cfg.WorkerHostnames); err != nil {
				log.Error("worker endpoint stopped", "error", err)
			}
		}()
	}

	idStore := identity.NewStore(store, secretSvc)
	redirect := ""
	if cfg.ExternalURL != "" {
		redirect = cfg.ExternalURL + "/api/v1/auth/oidc/callback"
	}
	signIn := identity.NewService(idStore, oidcadapter.New(), ldapadapter.New(), redirect)
	authn.UseDirectory(signIn.LDAPLogin)

	notifications := notify.NewService(store, secretSvc)
	notifications.UseSender("webhook", notifysend.Webhook{})
	notifications.UseSender("slack", notifysend.Slack{})
	notifications.UseSender("teams", notifysend.Teams{})
	if cfg.SMTPHost != "" {
		password := ""
		if cfg.SMTPPasswordFile != "" {
			if password, err = config.ReadPrivateFile(cfg.SMTPPasswordFile); err != nil {
				return fmt.Errorf("SMTP password: %w", err)
			}
		}
		notifications.UseSender("email", notifysend.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, From: cfg.SMTPFrom, Username: cfg.SMTPUsername, Password: strings.TrimSpace(password)})
		notifications.EmailReady = true
	}
	dispatcher := notify.NewDispatcher(store, cfg.ExternalURL)
	go member.Singleton(ctx, "notification-dispatch", 5*time.Second, func(ctx context.Context, _ int64) error {
		_, err := dispatcher.Dispatch(ctx)
		return err
	})
	go notifications.RunDelivery(ctx, 10*time.Second, func(err error) { log.Error("notification delivery", "error", err) })

	pipelineSvc := pipelines.NewService(store, secretSvc, uploads, buildSvc, specSvc, tenants, gogit.Fetcher{Dir: uploads.Staging()})
	pipelineSvc.UseOwnership(member)
	go pipelineSvc.RunLoop(ctx, 5*time.Second, func(err error) { log.Error("pipeline run", "error", err) })

	retention := &audit.Retention{DB: store, Dir: cfg.AuditExportDir}
	go member.Singleton(ctx, "audit-retention", time.Hour, func(ctx context.Context, _ int64) error { return retention.Run(ctx) })
	backups, err := backupService(cfg, store)
	if err != nil {
		return err
	}
	if err := recordUpgrade(ctx, store, backups, up); err != nil {
		return fmt.Errorf("recording the upgrade: %w", err)
	}
	if err := backups.CheckTools(ctx); err != nil {
		log.Error("backups cannot run", "error", err)
	}
	checks.Register(health.Check{Name: "backup-tools", Run: backups.CheckTools})
	if backups.Enabled() && cfg.Backup.Interval > 0 {
		go member.Singleton(ctx, "backup", cfg.Backup.Interval, func(ctx context.Context, _ int64) error {
			rec, err := backups.Run(ctx, nil, "")
			if err == nil {
				log.Info("backup written", "file", rec.File, "bytes", rec.Size, "rows", rec.Rows, "blobs", rec.Blobs)
			}
			return err
		})
	}
	diagnostics := observability.Source{DB: store, Health: checks, Connected: workerSrv.Connected}
	metrics := telemetry.NewMetrics(diagnostics.Gauges)
	if cfg.MetricsListen != "" {
		if err := serveMetrics(ctx, cfg, metrics, log); err != nil {
			return err
		}
	}

	handler, err := api.Handler(api.New(api.Deps{Log: log, Health: checks, Tenancy: tenants, Auth: authn, DataDir: cfg.DataDir, SecureCookies: cfg.TLSEnabled(), Sources: uploads, Operations: ops, Discovery: discoveries,
		Specs: specSvc, Admission: checker, Recipes: recipeStatus, Policies: policies,
		Workers: workerStore, DB: store, Builds: buildSvc, Secrets: secretSvc, Validations: validations, Publications: publications, Approvals: approvalSvc, Signing: signingKeys, Artifacts: art, Bus: bus, Channel: workerSrv,
		Identity: signIn, IdentityStore: idStore, ImageSources: imageSources, Notify: notifications, Pipelines: pipelineSvc, Topology: topologyPlans, Providers: providerStore, Kube: kube, TopologyInfo: api.TopologyInfo{Storage: weakestStorage(cfg), ExternalURL: cfg.ExternalURL},
		Retention: retention, Diagnostics: diagnostics, ConfirmKey: secretSvc.Derive("policy-confirmation"), Backups: backups, Cluster: member, Removals: removals, ClusterJoin: join, WorkerJoin: api.WorkerJoin{Listen: cfg.WorkerListen, Hostnames: cfg.WorkerHostnames}, Database: dbCreds, SelfUpdate: updates, Capacity: hosts, Federation: fedStore,
		FederationInfo: api.FederationInfo{CAPin: func() string { return ca.Issuing().Pin() }, Listening: cfg.FederationListen != "", Hostnames: cfg.FederationHostnames}, Telemetry: api.TelemetryStatus{MetricsEnabled: cfg.MetricsListen != "", TracingEnabled: cfg.TracingEndpoint != ""}}), authn)
	if err != nil {
		return err
	}
	srv := server.New(cfg, log, handler)
	srv.UseMetrics(metrics)
	return srv.Serve(ctx, ln)
}

// serveMetrics exposes Prometheus metrics on their own listener, behind a bearer token when one is configured.
func serveMetrics(ctx context.Context, cfg config.Config, m *telemetry.Metrics, log *slog.Logger) error {
	token := ""
	if cfg.MetricsTokenFile != "" {
		t, err := config.ReadPrivateFile(cfg.MetricsTokenFile)
		if err != nil {
			return fmt.Errorf("metrics token: %w", err)
		}
		if token = strings.TrimSpace(t); len(token) < 16 {
			return errors.New("metrics token: use at least 16 characters")
		}
	}
	ln, err := net.Listen("tcp", cfg.MetricsListen)
	if err != nil {
		return fmt.Errorf("metrics listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler(token))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped", "error", err)
		}
	}()
	log.Info("metrics enabled", "addr", ln.Addr().String(), "token", token != "")
	return nil
}

// weakestStorage names the least resilient backend any storage class uses.
func weakestStorage(cfg config.Config) topology.Storage {
	worst := topology.S3
	for _, c := range storage.Classes {
		switch cfg.StorageRoute(string(c)) {
		case "local":
			return topology.Local
		case "oci":
			worst = topology.OCI
		}
	}
	return worst
}

func bootstrap(ctx context.Context, cfg config.Config, a *auth.Service, log *slog.Logger) error {
	done, err := a.SetupCompleted(ctx)
	if err != nil || done {
		return err
	}
	if cfg.BootstrapAdmin != "" {
		pw, err := config.ReadPrivateFile(cfg.BootstrapPWFile)
		if err != nil {
			return fmt.Errorf("bootstrap password: %w", err)
		}
		in := auth.BootstrapInput{OrganizationName: cfg.BootstrapOrg, Username: cfg.BootstrapAdmin, Email: cfg.BootstrapEmail, Password: pw}
		if err := a.Bootstrap(ctx, in, cfg.DataDir, "", false); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		log.Info("setup completed from configuration", "admin", cfg.BootstrapAdmin)
		return nil
	}
	ensure := a.EnsureBootstrapToken
	if cfg.Cluster {
		ensure = a.EnsureSharedBootstrapToken
	}
	path, err := ensure(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	if path == "" {
		log.Warn("setup required: another controller holds the one-time bootstrap token in its data directory")
		return nil
	}
	log.Warn("setup required: open the web UI and use the one-time bootstrap token stored in this file", "path", path)
	return nil
}

type upgrade struct {
	from, to   int64
	ran        int
	backupPath string
	manifest   backup.Manifest
}

// migrate brings the schema to this release's version. When an existing installation is upgraded and backups are
// configured, it first writes a backup, so a failed upgrade can be undone (D28).
func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) (upgrade, error) {
	m, err := db.Open(ctx, cfg.MigrationDB)
	if err != nil {
		return upgrade{}, err
	}
	defer m.Close()
	from, known, err := m.Schema(ctx)
	if err != nil {
		return upgrade{}, err
	}
	u := upgrade{from: from, to: known}
	if from > 0 && from < known && cfg.Backup.Dir != "" {
		svc, err := backupService(cfg, m)
		if err != nil {
			return upgrade{}, err
		}
		u.backupPath = filepath.Join(cfg.Backup.Dir, fmt.Sprintf("samuori-before-schema-%d-%s.age", known, time.Now().UTC().Format("20060102T150405Z")))
		if u.manifest, err = svc.WriteFile(ctx, u.backupPath); err != nil {
			return upgrade{}, fmt.Errorf("backup before upgrading the schema from %d to %d: %w", from, known, err)
		}
		log.Info("backup written before the schema upgrade", "file", u.backupPath, "from", from, "to", known)
	}
	if u.ran, err = m.Migrate(ctx); err != nil {
		return upgrade{}, err
	}
	return u, nil
}

// recordUpgrade audits a schema upgrade this controller applied and lists the backup taken before it.
func recordUpgrade(ctx context.Context, store *db.DB, backups *backup.Service, u upgrade) error {
	if u.ran == 0 || u.from == 0 {
		return nil
	}
	reason := fmt.Sprintf("schema %d to %d by Samuori %s", u.from, u.to, version.Version)
	if u.backupPath != "" {
		if _, err := backups.Record(ctx, u.backupPath, u.manifest, "upgrade", "before "+reason); err != nil {
			return err
		}
	}
	return store.InTx(ctx, func(tx *sql.Tx) error {
		return audit.Append(ctx, store, tx, audit.Event{Actor: authz.Principal{Kind: authz.System}, Action: "upgrade.migrate", ResourceType: "schema", ResourceID: strconv.FormatInt(u.to, 10), Outcome: "succeeded", Reason: reason})
	})
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".health-*")
	if err != nil {
		return err
	}
	name := f.Name()
	return errors.Join(f.Close(), os.Remove(filepath.Clean(name)))
}

// migrateDatabase copies a stopped controller's SQLite database into a new PostgreSQL database and verifies the copy
// (D07): samuori-server migrate-database --from sqlite:///var/lib/samuori/samuori.db --to postgres://owner@host/samuori. The source
// is brought to the current schema first and otherwise only read; any difference rolls the target back to empty.
func migrateDatabase(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("migrate-database", flag.ContinueOnError)
	from := fs.String("from", "", "source SQLite DSN (sqlite:///path/samuori.db)")
	to := fs.String("to", "", "target PostgreSQL DSN, as a role that owns the schema")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !strings.HasPrefix(*from, "sqlite://") || !(strings.HasPrefix(*to, "postgres://") || strings.HasPrefix(*to, "postgresql://")) {
		return errors.New("usage: samuori-server migrate-database --from sqlite://PATH --to postgres://DSN")
	}
	src, err := db.Open(ctx, *from)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Migrate(ctx); err != nil {
		return err
	}
	var running int
	if err := src.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM controllers WHERE stopped_at IS NULL AND heartbeat_at > ?`, time.Now().UTC().Add(-cluster.DeadAfter)).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return errors.New("a controller is still running on the source database; stop it first")
	}
	dst, err := db.Open(ctx, *to)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := dst.Migrate(ctx); err != nil {
		return err
	}
	start := time.Now()
	tables, err := db.Transfer(ctx, src, dst, func(table string, rows int64) { fmt.Fprintf(out, "copied %-32s %8d rows\n", table, rows) })
	if err != nil {
		return err
	}
	var total int64
	for _, t := range tables {
		total += t.Rows
		fmt.Fprintf(out, "verified %-30s %8d rows  sha256:%s\n", t.Name, t.Rows, t.Checksum)
	}
	fmt.Fprintf(out, "%d tables, %d rows copied and verified in %s. Point database.dsn at PostgreSQL before starting the controller; keep the SQLite file until the new database is backed up.\n", len(tables), total, time.Since(start).Round(time.Millisecond))
	return nil
}

// serveFederation accepts federation peers on their own listener. The listener presents this installation's
// organization, so it starts only once first-run setup has created it.
func serveFederation(ctx context.Context, cfg config.Config, store *db.DB, e *federation.Executor, log *slog.Logger) error {
	if err := store.SQL.QueryRowContext(ctx, `SELECT id FROM organizations ORDER BY created_at LIMIT 1`).Scan(&e.OrgID); err != nil {
		log.Warn("the federation listener starts after first-run setup; restart the controller once setup is complete")
		return nil
	}
	ln, err := net.Listen("tcp", cfg.FederationListen)
	if err != nil {
		return fmt.Errorf("federation listener: %w", err)
	}
	go func() {
		if err := e.Serve(ctx, ln, cfg.FederationHostnames); err != nil {
			log.Error("federation listener stopped", "error", err)
		}
	}()
	log.Info("federation listener", "addr", ln.Addr().String(), "organization", e.OrgID)
	return nil
}

// backupService configures backups from settings; it is disabled without backup.dir.
func backupService(cfg config.Config, store *db.DB) (*backup.Service, error) {
	if cfg.Backup.Dir == "" {
		return backup.NewService(store, "", 0, backup.Options{}), nil
	}
	recipients, err := backup.ReadRecipients(cfg.Backup.RecipientsFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Backup.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	keyFile := filepath.Join(cfg.DataDir, "keys", "secrets.key")
	if cfg.Cluster {
		keyFile = cfg.ClusterKeyFile
	}
	skipBlobs := true
	for _, c := range config.StorageClasses {
		if cfg.StorageRoute(c) == "local" {
			skipBlobs = false
		}
	}
	svc := backup.NewService(store, cfg.Backup.Dir, cfg.Backup.Keep, backup.Options{DSN: cfg.Database, DataDir: cfg.DataDir, KeyFile: keyFile, SkipBlobs: skipBlobs,
		PGDump: cfg.Backup.PGDump, PGRestore: cfg.Backup.PGRestore, Version: version.Version, Recipients: recipients, Temp: cfg.Backup.Dir})
	svc.Interval = cfg.Backup.Interval
	return svc, nil
}

// backupCommand writes one backup with the controller's settings: samuori-server backup [settings flags].
func backupCommand(ctx context.Context, args []string, out io.Writer) error {
	cfg, _, _, err := loadConfig(args)
	if err != nil {
		return err
	}
	store, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	svc, err := backupService(cfg, store)
	if err != nil {
		return err
	}
	rec, err := svc.Run(ctx, &authz.Principal{Kind: authz.System}, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\n%d bytes, sha256:%s, %d tables, %d rows, %d blobs, keys included: %t\n", rec.File, rec.Size, rec.SHA256, rec.Tables, rec.Rows, rec.Blobs, rec.IncludesKeys)
	return nil
}

// verifyBackupCommand decrypts an archive with an age identity and checks every entry and table:
// samuori-server verify-backup --identity-file KEY ARCHIVE.
func verifyBackupCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("verify-backup", flag.ContinueOnError)
	identity := fs.String("identity-file", "", "age identity file (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *identity == "" || fs.NArg() != 1 {
		return errors.New("usage: samuori-server verify-backup --identity-file KEY ARCHIVE")
	}
	ids, err := backup.ReadIdentities(*identity)
	if err != nil {
		return err
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := backup.Verify(ctx, f, ids, "")
	if err != nil {
		return err
	}
	var rows int64
	for _, t := range m.Tables {
		rows += t.Rows
	}
	scope := "every file and table matches its checksum"
	if m.Dialect == db.Postgres {
		scope = "every file matches its checksum; tables are checked when restored"
	}
	fmt.Fprintf(out, "backup of %s (%s, schema %d, Samuori %s): %d tables, %d rows, %d blobs, keys included: %t; %s\n", m.CreatedAt.Format(time.RFC3339), m.Dialect, m.SchemaVersion, m.Version, len(m.Tables), rows, m.Blobs, m.IncludesKeys, scope)
	return nil
}

// restoreCommand fills an empty data directory and database from an archive:
// samuori-server restore --identity-file KEY --data-dir DIR [--database DSN] [--key-file PATH] ARCHIVE.
func restoreCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	identity := fs.String("identity-file", "", "age identity file (mode 0600)")
	dataDir := fs.String("data-dir", "", "empty data directory to restore into")
	database := fs.String("database", "", "empty target database (default: SQLite in the data directory)")
	keyFile := fs.String("key-file", "", "where to write the secret store key (default: keys/secrets.key in the data directory)")
	pgRestore := fs.String("pg-restore", "", "pg_restore binary (default: pg_restore on PATH)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *identity == "" || *dataDir == "" || fs.NArg() != 1 {
		return errors.New("usage: samuori-server restore --identity-file KEY --data-dir DIR [--database DSN] ARCHIVE")
	}
	if *database == "" {
		*database = "sqlite://" + filepath.Join(*dataDir, "samuori.db")
	}
	ids, err := backup.ReadIdentities(*identity)
	if err != nil {
		return err
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	start := time.Now()
	res, err := backup.Restore(ctx, f, backup.RestoreOptions{Identities: ids, DataDir: *dataDir, DSN: *database, KeyFile: *keyFile, PGRestore: *pgRestore})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "restored the backup of %s into %s in %s: %d tables verified, %d blobs, %d migrations applied\n", res.Manifest.CreatedAt.Format(time.RFC3339), *dataDir, time.Since(start).Round(time.Millisecond), len(res.Manifest.Tables), res.Manifest.Blobs, res.Migrated)
	return nil
}

func count(origins map[string]config.Source, src config.Source) int {
	n := 0
	for _, s := range origins {
		if s == src {
			n++
		}
	}
	return n
}

// prepareSecrets copies files mounted from a secret store, which may be group-readable, into a private directory
// as files only this user can read, so the key-file checks hold in containers: samuori-server prepare-secrets SRC... DST.
// Each SRC is a directory; its regular files land in DST/<base of SRC>/.
func prepareSecrets(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: samuori-server prepare-secrets SRC... DST")
	}
	dst := args[len(args)-1]
	for _, src := range args[:len(args)-1] {
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		dir := filepath.Join(dst, filepath.Base(src))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for _, e := range entries {
			info, err := os.Stat(filepath.Join(src, e.Name()))
			if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), raw, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// newClusterKey writes a new 32-byte cluster key to a file that must not exist yet: samuori-server new-cluster-key PATH.
// A standalone controller joining a cluster uses its data-dir/keys/secrets.key instead, so its secrets stay readable.
func newClusterKey(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: samuori-server new-cluster-key PATH")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	f, err := os.OpenFile(args[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// healthcheck asks a running controller for its health, for container health checks without a shell or curl:
// samuori-server healthcheck [http://127.0.0.1:8080].
func healthcheck(args []string) int {
	base := "http://127.0.0.1:8080"
	if len(args) > 0 {
		base = args[0]
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(base, "/") + "/api/v1/system/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	var h struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || resp.StatusCode != http.StatusOK || h.Status != "ok" {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP %d, status %q\n", resp.StatusCode, h.Status)
		return 1
	}
	return 0
}

// followPKI picks up CA changes made on other controllers and passes a changed trust bundle to this controller's
// workers.
func followPKI(ctx context.Context, shared *cluster.SharedDir, ca *pki.Authority, w *workers.Server, log *slog.Logger) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changed, err := shared.Pull(ctx)
		if err != nil {
			log.Error("shared controller material", "error", err)
			continue
		}
		if !changed {
			continue
		}
		bundle, err := ca.Reload()
		if err != nil {
			log.Error("reloading the internal CA", "error", err)
			continue
		}
		log.Info("internal CA changed on another controller", "revision", shared.Revision())
		if bundle {
			w.PushBundle()
		}
	}
}

// clusterJoin shares the cluster CA through the database and, when this controller holds it, lets more controllers
// join with tokens (docs/operators/high-availability.md#join). Clusters set up by hand without cluster init get
// tokens refused.
func clusterJoin(ctx context.Context, cfg config.Config, store *db.DB, memberID string, key []byte) (api.ClusterJoin, error) {
	dir := filepath.Join(cfg.DataDir, "cluster-pki")
	shared, err := cluster.NewSharedDir(store, "cluster-pki", dir, memberID, key)
	if err != nil {
		return api.ClusterJoin{}, err
	}
	if _, err := shared.Pull(ctx); err != nil {
		return api.ClusterJoin{}, fmt.Errorf("shared cluster CA: %w", err)
	}
	authority, err := clusterjoin.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return api.ClusterJoin{Tokens: clusterjoin.NewStore(store)}, nil
	}
	if err != nil {
		return api.ClusterJoin{}, fmt.Errorf("cluster CA: %w", err)
	}
	if err := shared.Push(ctx); err != nil && !errors.Is(err, cluster.ErrStale) {
		return api.ClusterJoin{}, fmt.Errorf("sharing the cluster CA: %w", err)
	}
	if cfg.TLSCertFile == "" {
		return api.ClusterJoin{Tokens: clusterjoin.NewStore(store)}, nil
	}
	certPEM, err := os.ReadFile(cfg.TLSCertFile)
	if err != nil {
		return api.ClusterJoin{}, err
	}
	pin, err := clusterjoin.Pin(certPEM)
	if err != nil {
		return api.ClusterJoin{}, err
	}
	host := apiHost(certPEM)
	_, port, _ := net.SplitHostPort(cfg.Listen)
	return api.ClusterJoin{Tokens: clusterjoin.NewStore(store), Pin: pin, URL: "https://" + net.JoinHostPort(host, port),
		Bundle: func(joiner string) (clusterjoin.Bundle, error) {
			base, err := clusterjoin.FromConfig(cfg, authority)
			if err != nil {
				return clusterjoin.Bundle{}, err
			}
			return base.ForHost(authority, joiner)
		}}, nil
}

// controllerIdentity is this controller's certificate name and pin, by which a removed host is recognised.
func controllerIdentity(cfg config.Config) cluster.Identity {
	id := cluster.Identity{LocalWorker: cfg.Cluster && cfg.LocalWorkerDir != ""}
	if cfg.TLSCertFile == "" {
		return id
	}
	certPEM, err := os.ReadFile(cfg.TLSCertFile)
	if err != nil {
		return id
	}
	id.Host = apiHost(certPEM)
	id.CertPin, _ = clusterjoin.Pin(certPEM)
	return id
}

// apiHost is the name in this controller's API certificate that other hosts use.
func apiHost(certPEM []byte) string {
	b, _ := pem.Decode(certPEM)
	if b != nil {
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			for _, n := range c.DNSNames {
				if n != "localhost" {
					return n
				}
			}
			if len(c.IPAddresses) > 0 {
				return c.IPAddresses[0].String()
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

// openRemotes connects the configured shared blob backends: s3 and oci.
func openRemotes(ctx context.Context, cfg config.Config) (map[string]storage.Remote, error) {
	remotes := map[string]storage.Remote{}
	if cfg.S3Bucket != "" {
		var cred []byte
		if cfg.S3CredentialsFile != "" {
			v, err := config.ReadPrivateFile(cfg.S3CredentialsFile)
			if err != nil {
				return nil, fmt.Errorf("object storage credentials: %w", err)
			}
			cred = []byte(strings.TrimSpace(v))
		}
		remote, err := s3store.New(ctx, s3store.Config{Bucket: cfg.S3Bucket, Prefix: cfg.S3Prefix, Region: cfg.S3Region, Endpoint: cfg.S3Endpoint, PathStyle: cfg.S3PathStyle,
			AuthMethod: cfg.S3AuthMethod, RoleARN: cfg.S3RoleARN, ExternalID: cfg.S3ExternalID, Credential: cred})
		if err != nil {
			return nil, fmt.Errorf("object storage: %w", err)
		}
		remotes["s3"] = remote
	}
	if cfg.OCIRepository != "" {
		oc := ocistore.Config{Repository: cfg.OCIRepository, PlainHTTP: cfg.OCIPlainHTTP, AllowPrivateNetwork: cfg.OCIAllowPrivate}
		if cfg.OCICredentialsFile != "" {
			v, err := config.ReadPrivateFile(cfg.OCICredentialsFile)
			if err != nil {
				return nil, fmt.Errorf("registry storage credentials: %w", err)
			}
			user, pass, ok := strings.Cut(strings.TrimSpace(v), ":")
			if !ok {
				return nil, errors.New("registry storage credentials: the file must hold USERNAME:PASSWORD")
			}
			oc.Username, oc.Password = user, pass
		}
		remote, err := ocistore.New(oc)
		if err != nil {
			return nil, fmt.Errorf("registry storage: %w", err)
		}
		remotes["oci"] = remote
	}
	return remotes, nil
}

// setDatabasePassword stores, for this controller only, a PostgreSQL password already set at the database, after
// signing in with it. It recovers a controller that missed a change made on the Database page
// (docs/operators/database.md#recover-a-controller). The password is read from standard input.
func setDatabasePassword(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--env-file" {
			continue
		}
		if !inline && i+1 < len(args) {
			value = args[i+1]
			args = append(args[:i:i], args[i+2:]...)
		} else {
			args = append(args[:i:i], args[i+1:]...)
		}
		if err := loadEnvFile(value); err != nil {
			return err
		}
		break
	}
	cfg, _, configured, err := loadConfig(args)
	if err != nil {
		return err
	}
	if dialect, _, err := db.ParseDSN(configured); err != nil || dialect != db.Postgres {
		return errors.New("this controller does not use PostgreSQL")
	}
	raw, err := io.ReadAll(io.LimitReader(in, 1025))
	if err != nil {
		return err
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if password == "" || len(password) > 1024 {
		return errors.New("pass the password on standard input")
	}
	dsn, err := db.WithPassword(configured, password)
	if err != nil {
		return err
	}
	probe, err := db.Verify(ctx, dsn)
	if err != nil {
		return err
	}
	current, _, err := db.ReadStoredPassword(cfg.DataDir)
	if err != nil {
		return err
	}
	if err := db.WriteStoredPassword(cfg.DataDir, db.StoredPassword{Revision: current.Revision, Password: password}); err != nil {
		return err
	}
	if err := chownLikeDir(filepath.Join(cfg.DataDir, db.PasswordFile), cfg.DataDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in as %s on PostgreSQL %s and stored the password for this controller.\nRestart it: sudo systemctl restart samuori-controller\n", probe.User, probe.ServerVersion)
	return nil
}

// chownLikeDir gives path the owner of dir, so a file written by root stays readable by the controller's user.
func chownLikeDir(path, dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}

// loadEnvFile sets the KEY=VALUE lines of a systemd EnvironmentFile, such as /etc/samuori/env, without a shell.
func loadEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			if err := os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)); err != nil {
				return err
			}
		}
	}
	return nil
}
