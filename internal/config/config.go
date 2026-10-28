// Package config resolves controller settings with precedence: flags, then SAMUORI_* environment, then the
// config file, then defaults (docs/product/administration.md).
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/isolation"
)

type Role string

const (
	RoleController Role = "controller"
	RoleAllInOne   Role = "all-in-one"
)

type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

type Config struct {
	Role            Role
	Listen          string
	DataDir         string
	LogLevel        string
	LogFormat       string
	ShutdownTimeout time.Duration
	TLSCertFile     string
	TLSKeyFile      string
	Database        string
	MigrationDB     string
	UIDir           string
	WorkerListen    string
	WorkerHostnames []string
	BootstrapOrg    string
	BootstrapAdmin  string
	BootstrapPWFile string
	BootstrapEmail  string
	// DevWeakPasswords relaxes the password policy for local accounts on a local all-in-one development server
	// (docs/developers/local-dev.md).
	DevWeakPasswords bool
	// LocalWorkerDir, in all-in-one mode, receives the single-use enrollment token and CA pin of the host worker.
	LocalWorkerDir     string
	LocalWorkerProfile string
	// VulnerabilityScanner names an operator-installed scanner (grype or trivy); none by default (ADR 0021).
	VulnerabilityScanner string
	// ExternalURL is how browsers reach Samuori, such as https://samuori.example; OIDC redirects are built from it.
	ExternalURL string
	// ConfigFile is the file the configuration was read from, if any.
	ConfigFile string
	// Object storage, when a bucket is set, keeps blobs durably; the data directory becomes a cache.
	S3Bucket, S3Prefix, S3Region, S3Endpoint string
	S3PathStyle                              bool
	S3AuthMethod, S3RoleARN, S3ExternalID    string
	S3CredentialsFile                        string
	CacheEvictAfter                          time.Duration
	// OCI storage keeps blobs in a registry repository; Routes maps a data class to local, s3 or oci.
	OCIRepository, OCICredentialsFile string
	OCIPlainHTTP, OCIAllowPrivate     bool
	StorageRoutes                     map[string]string
	// BuildCacheTTL and BuildCacheMaxScopeBytes bound shared build caches (docs/architecture/build-cache.md).
	BuildCacheTTL           time.Duration
	BuildCacheMaxScopeBytes int64
	// SharedMirrors lets one authorizing mirror serve several teams (OD-14); off until its tests pass in CI.
	SharedMirrors bool
	// SMTP serves email notification channels; empty Host disables email.
	SMTPHost, SMTPFrom, SMTPUsername, SMTPPasswordFile string
	SMTPPort                                           int
	// Observability outputs are optional; Samuori's own diagnostics and audit search need none of them (ADR 0016).
	MetricsListen      string
	MetricsTokenFile   string
	TracingEndpoint    string
	TracingSampleRatio float64
	AuditExportDir     string
	// Cluster runs this controller as one of several sharing a PostgreSQL database, an external NATS cluster,
	// shared blob storage and one cluster key (ADR 0034).
	Cluster        bool
	ClusterKeyFile string
	Messaging      Messaging
	Backup         Backup
	// Federation serves peer installations on its own mutual-TLS listener (ADR 0037); empty Listen disables it.
	FederationListen       string
	FederationHostnames    []string
	FederationPrivatePeers bool
}

// Backup configures admin-managed, encrypted backups (D27). An empty Dir disables them; Interval zero means only
// backups started by an administrator.
type Backup struct {
	Dir            string
	RecipientsFile string
	Interval       time.Duration
	Keep           int
	PGDump         string
	PGRestore      string
}

// Messaging selects an external NATS cluster; an empty URL list runs NATS embedded in the controller.
type Messaging struct {
	URLs                      []string
	CredentialsFile           string
	CAFile, CertFile, KeyFile string
	Replicas                  int
}

func (c Config) TLSEnabled() bool { return c.TLSCertFile != "" }

type setting struct {
	key, flag, env, def, usage string
}

var settings = []setting{
	{"role", "role", "SAMUORI_ROLE", string(RoleAllInOne), "controller or all-in-one"},
	{"listen", "listen", "SAMUORI_LISTEN", "127.0.0.1:8080", "HTTP listen address"},
	{"dataDir", "data-dir", "SAMUORI_DATA_DIR", "/var/lib/samuori", "data directory"},
	{"log.level", "log-level", "SAMUORI_LOG_LEVEL", "info", "debug, info, warn or error"},
	{"log.format", "log-format", "SAMUORI_LOG_FORMAT", "json", "json or text"},
	{"shutdownTimeout", "shutdown-timeout", "SAMUORI_SHUTDOWN_TIMEOUT", "30s", "graceful shutdown limit"},
	{"tls.certFile", "tls-cert-file", "SAMUORI_TLS_CERT_FILE", "", "TLS certificate (PEM)"},
	{"tls.keyFile", "tls-key-file", "SAMUORI_TLS_KEY_FILE", "", "TLS private key (PEM)"},
	{"database.dsn", "database", "SAMUORI_DATABASE", "", "metadata database DSN (default: SQLite in the data directory)"},
	{"database.migrationDsn", "database-migration", "SAMUORI_DATABASE_MIGRATION", "", "DSN used for schema migrations (default: database)"},
	{"worker.listen", "worker-listen", "SAMUORI_WORKER_LISTEN", "127.0.0.1:8444", "worker endpoint (gRPC over mTLS); empty disables it"},
	{"worker.hostnames", "worker-hostnames", "SAMUORI_WORKER_HOSTNAMES", "localhost,127.0.0.1", "comma-separated names in the worker endpoint certificate"},
	{"ui.dir", "ui-dir", "SAMUORI_UI_DIR", "", "directory holding the built web UI (empty: API only)"},
	{"bootstrap.organizationName", "bootstrap-organization", "SAMUORI_BOOTSTRAP_ORGANIZATION", "", "organization created by config-as-code bootstrap"},
	{"bootstrap.adminUsername", "bootstrap-admin", "SAMUORI_BOOTSTRAP_ADMIN", "", "first administrator created by config-as-code bootstrap"},
	{"bootstrap.adminPasswordFile", "bootstrap-admin-password-file", "SAMUORI_BOOTSTRAP_ADMIN_PASSWORD_FILE", "", "file holding the first administrator password (mode 0600)"},
	{"bootstrap.adminEmail", "bootstrap-admin-email", "SAMUORI_BOOTSTRAP_ADMIN_EMAIL", "", "first administrator's email address"},
	{"dev.weakPasswords", "dev-weak-passwords", "SAMUORI_DEV_WEAK_PASSWORDS", "false", "local development only: local accounts may have passwords below the policy; refused unless all-in-one on a loopback address"},
	{"localWorker.dir", "local-worker-dir", "SAMUORI_LOCAL_WORKER_DIR", "", "all-in-one, or a cluster controller that keeps its host's worker: private directory where the controller hands that worker its enrollment token (empty disables it)"},
	{"validation.vulnerabilityScanner", "vulnerability-scanner", "SAMUORI_VULNERABILITY_SCANNER", "", "operator-installed vulnerability scanner: grype or trivy (none by default)"},
	{"localWorker.profile", "local-worker-profile", "SAMUORI_LOCAL_WORKER_PROFILE", "rootless-dedicated-vm", "all-in-one: isolation profile of the local worker pool"},
	{"storage.s3.bucket", "s3-bucket", "SAMUORI_S3_BUCKET", "", "S3 or S3-compatible bucket for blobs (empty keeps them in the data directory only)"},
	{"storage.s3.prefix", "s3-prefix", "SAMUORI_S3_PREFIX", "", "key prefix inside the bucket"},
	{"storage.s3.region", "s3-region", "SAMUORI_S3_REGION", "", "bucket region"},
	{"storage.s3.endpoint", "s3-endpoint", "SAMUORI_S3_ENDPOINT", "", "S3-compatible endpoint URL (empty for AWS)"},
	{"storage.s3.pathStyle", "s3-path-style", "SAMUORI_S3_PATH_STYLE", "false", "address buckets by path, as most S3-compatible services need"},
	{"storage.s3.authMethod", "s3-auth-method", "SAMUORI_S3_AUTH_METHOD", "workload-identity", "workload-identity, assume-role or access-key"},
	{"storage.s3.roleArn", "s3-role-arn", "SAMUORI_S3_ROLE_ARN", "", "role to assume for object storage"},
	{"storage.s3.externalId", "s3-external-id", "SAMUORI_S3_EXTERNAL_ID", "", "external ID for the assumed role"},
	{"storage.s3.credentialsFile", "s3-credentials-file", "SAMUORI_S3_CREDENTIALS_FILE", "", "file holding ACCESS_KEY_ID:SECRET_ACCESS_KEY for access-key (mode 0600)"},
	{"storage.cacheEvictAfter", "cache-evict-after", "SAMUORI_CACHE_EVICT_AFTER", "24h", "with object storage, remove local copies older than this"},
	{"storage.oci.repository", "oci-storage-repository", "SAMUORI_OCI_STORAGE_REPOSITORY", "", "OCI registry repository (host[:port]/path) for blobs"},
	{"storage.oci.credentialsFile", "oci-storage-credentials-file", "SAMUORI_OCI_STORAGE_CREDENTIALS_FILE", "", "file holding USERNAME:PASSWORD for the storage registry (mode 0600)"},
	{"storage.oci.plainHttp", "oci-storage-plain-http", "SAMUORI_OCI_STORAGE_PLAIN_HTTP", "false", "plain HTTP to a loopback storage registry"},
	{"storage.oci.allowPrivateNetwork", "oci-storage-allow-private-network", "SAMUORI_OCI_STORAGE_ALLOW_PRIVATE_NETWORK", "false", "allow loopback and private addresses for the storage registry"},
	{"storage.routes.source", "storage-route-source", "SAMUORI_STORAGE_ROUTE_SOURCE", "", "backend for source snapshots: local, s3 or oci (default: s3 if set, else oci if set, else local)"},
	{"storage.routes.image", "storage-route-image", "SAMUORI_STORAGE_ROUTE_IMAGE", "", "backend for built images"},
	{"storage.routes.report", "storage-route-report", "SAMUORI_STORAGE_ROUTE_REPORT", "", "backend for SBOMs and reports"},
	{"buildCache.ttl", "build-cache-ttl", "SAMUORI_BUILD_CACHE_TTL", "168h", "shared build caches older than this are not imported and are removed"},
	{"buildCache.maxScopeBytes", "build-cache-max-scope-bytes", "SAMUORI_BUILD_CACHE_MAX_SCOPE_BYTES", "10737418240", "largest shared build cache kept for one scope, in bytes"},
	{"smtp.host", "smtp-host", "SAMUORI_SMTP_HOST", "", "SMTP server for email notifications (STARTTLS required off loopback)"},
	{"smtp.port", "smtp-port", "SAMUORI_SMTP_PORT", "587", "SMTP port"},
	{"smtp.from", "smtp-from", "SAMUORI_SMTP_FROM", "", "sender address of notification email"},
	{"smtp.username", "smtp-username", "SAMUORI_SMTP_USERNAME", "", "SMTP user"},
	{"smtp.passwordFile", "smtp-password-file", "SAMUORI_SMTP_PASSWORD_FILE", "", "file holding the SMTP password (mode 0600)"},
	{"imageSources.sharedMirrors", "shared-mirrors", "SAMUORI_SHARED_MIRRORS", "false", "let one authorizing mirror serve several teams (OD-14)"},
	{"externalUrl", "external-url", "SAMUORI_EXTERNAL_URL", "", "URL browsers use to reach Samuori, such as https://samuori.example (needed for OIDC sign-in)"},
	{"observability.metricsListen", "metrics-listen", "SAMUORI_METRICS_LISTEN", "", "Prometheus metrics listen address (empty disables it)"},
	{"observability.metricsTokenFile", "metrics-token-file", "SAMUORI_METRICS_TOKEN_FILE", "", "file holding the bearer token scrapes must send (mode 0600); required off loopback"},
	{"observability.tracing.endpoint", "tracing-endpoint", "SAMUORI_TRACING_ENDPOINT", "", "OTLP/HTTP traces URL, such as http://collector:4318/v1/traces (empty disables tracing)"},
	{"observability.tracing.sampleRatio", "tracing-sample-ratio", "SAMUORI_TRACING_SAMPLE_RATIO", "1", "fraction of new traces sampled, from 0 to 1"},
	{"cluster.enabled", "cluster", "SAMUORI_CLUSTER", "false", "run as one of several controllers sharing one PostgreSQL database (needs messaging.url, cluster.keyFile and shared blob storage)"},
	{"cluster.keyFile", "cluster-key-file", "SAMUORI_CLUSTER_KEY_FILE", "", "file holding the 32-byte cluster key every controller shares (mode 0600)"},
	{"messaging.url", "messaging-url", "SAMUORI_MESSAGING_URL", "", "comma-separated URLs of an external NATS cluster (empty runs NATS inside the controller)"},
	{"messaging.credentialsFile", "messaging-credentials-file", "SAMUORI_MESSAGING_CREDENTIALS_FILE", "", "NATS credentials (.creds) for the external cluster (mode 0600)"},
	{"messaging.caFile", "messaging-ca-file", "SAMUORI_MESSAGING_CA_FILE", "", "CA certificates that sign the NATS servers' certificates"},
	{"messaging.certFile", "messaging-cert-file", "SAMUORI_MESSAGING_CERT_FILE", "", "client certificate for mutual TLS with NATS"},
	{"messaging.keyFile", "messaging-key-file", "SAMUORI_MESSAGING_KEY_FILE", "", "client key for mutual TLS with NATS (mode 0600)"},
	{"messaging.replicas", "messaging-replicas", "SAMUORI_MESSAGING_REPLICAS", "3", "JetStream replicas of Samuori's stream on the external cluster"},
	{"federation.listen", "federation-listen", "SAMUORI_FEDERATION_LISTEN", "", "listener for federation peers (mutual TLS); empty disables it"},
	{"federation.hostnames", "federation-hostnames", "SAMUORI_FEDERATION_HOSTNAMES", "", "comma-separated names peers use to reach the federation listener"},
	{"federation.allowPrivatePeers", "federation-allow-private-peers", "SAMUORI_FEDERATION_ALLOW_PRIVATE_PEERS", "false", "allow peers on loopback and private networks (link-local and metadata addresses are always refused)"},
	{"backup.dir", "backup-dir", "SAMUORI_BACKUP_DIR", "", "directory for encrypted backups (empty disables backups)"},
	{"backup.recipientsFile", "backup-recipients-file", "SAMUORI_BACKUP_RECIPIENTS_FILE", "", "age recipients (public keys, one per line) that backups are encrypted to"},
	{"backup.interval", "backup-interval", "SAMUORI_BACKUP_INTERVAL", "0", "time between scheduled backups, such as 24h (0: only on request)"},
	{"backup.keep", "backup-keep", "SAMUORI_BACKUP_KEEP", "7", "number of backups kept in backup.dir"},
	{"backup.pgDump", "backup-pg-dump", "SAMUORI_BACKUP_PG_DUMP", "", "pg_dump binary for PostgreSQL backups (default: pg_dump on PATH)"},
	{"backup.pgRestore", "backup-pg-restore", "SAMUORI_BACKUP_PG_RESTORE", "", "pg_restore binary (default: pg_restore on PATH)"},
	{"observability.auditExportDir", "audit-export-dir", "SAMUORI_AUDIT_EXPORT_DIR", "", "directory for scheduled audit exports (default: audit-export in the data directory)"},
}

type fileConfig struct {
	Role         string `yaml:"role"`
	Listen       string `yaml:"listen"`
	ExternalURL  string `yaml:"externalUrl"`
	ImageSources struct {
		SharedMirrors string `yaml:"sharedMirrors"`
	} `yaml:"imageSources"`
	SMTP struct {
		Host         string `yaml:"host"`
		Port         string `yaml:"port"`
		From         string `yaml:"from"`
		Username     string `yaml:"username"`
		PasswordFile string `yaml:"passwordFile"`
	} `yaml:"smtp"`
	Storage struct {
		CacheEvictAfter string `yaml:"cacheEvictAfter"`
		S3              struct {
			Bucket          string `yaml:"bucket"`
			Prefix          string `yaml:"prefix"`
			Region          string `yaml:"region"`
			Endpoint        string `yaml:"endpoint"`
			PathStyle       string `yaml:"pathStyle"`
			AuthMethod      string `yaml:"authMethod"`
			RoleARN         string `yaml:"roleArn"`
			ExternalID      string `yaml:"externalId"`
			CredentialsFile string `yaml:"credentialsFile"`
		} `yaml:"s3"`
		OCI struct {
			Repository          string `yaml:"repository"`
			CredentialsFile     string `yaml:"credentialsFile"`
			PlainHTTP           string `yaml:"plainHttp"`
			AllowPrivateNetwork string `yaml:"allowPrivateNetwork"`
		} `yaml:"oci"`
		Routes struct {
			Source string `yaml:"source"`
			Image  string `yaml:"image"`
			Report string `yaml:"report"`
		} `yaml:"routes"`
	} `yaml:"storage"`
	BuildCache struct {
		TTL           string `yaml:"ttl"`
		MaxScopeBytes string `yaml:"maxScopeBytes"`
	} `yaml:"buildCache"`
	DataDir         string `yaml:"dataDir"`
	ShutdownTimeout string `yaml:"shutdownTimeout"`
	Log             struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"log"`
	TLS struct {
		CertFile string `yaml:"certFile"`
		KeyFile  string `yaml:"keyFile"`
	} `yaml:"tls"`
	Database struct {
		DSN          string `yaml:"dsn"`
		MigrationDSN string `yaml:"migrationDsn"`
	} `yaml:"database"`
	UI struct {
		Dir string `yaml:"dir"`
	} `yaml:"ui"`
	Worker struct {
		Listen    string   `yaml:"listen"`
		Hostnames []string `yaml:"hostnames"`
	} `yaml:"worker"`
	LocalWorker struct {
		Dir     string `yaml:"dir"`
		Profile string `yaml:"profile"`
	} `yaml:"localWorker"`
	Validation struct {
		VulnerabilityScanner string `yaml:"vulnerabilityScanner"`
	} `yaml:"validation"`
	Observability struct {
		MetricsListen    string `yaml:"metricsListen"`
		MetricsTokenFile string `yaml:"metricsTokenFile"`
		AuditExportDir   string `yaml:"auditExportDir"`
		Tracing          struct {
			Endpoint    string `yaml:"endpoint"`
			SampleRatio string `yaml:"sampleRatio"`
		} `yaml:"tracing"`
	} `yaml:"observability"`
	Federation struct {
		Listen            string   `yaml:"listen"`
		Hostnames         []string `yaml:"hostnames"`
		AllowPrivatePeers string   `yaml:"allowPrivatePeers"`
	} `yaml:"federation"`
	Backup struct {
		Dir            string `yaml:"dir"`
		RecipientsFile string `yaml:"recipientsFile"`
		Interval       string `yaml:"interval"`
		Keep           string `yaml:"keep"`
		PGDump         string `yaml:"pgDump"`
		PGRestore      string `yaml:"pgRestore"`
	} `yaml:"backup"`
	Cluster struct {
		Enabled string `yaml:"enabled"`
		KeyFile string `yaml:"keyFile"`
	} `yaml:"cluster"`
	Messaging struct {
		URL             string `yaml:"url"`
		CredentialsFile string `yaml:"credentialsFile"`
		CAFile          string `yaml:"caFile"`
		CertFile        string `yaml:"certFile"`
		KeyFile         string `yaml:"keyFile"`
		Replicas        string `yaml:"replicas"`
	} `yaml:"messaging"`
	Bootstrap struct {
		OrganizationName  string `yaml:"organizationName"`
		AdminUsername     string `yaml:"adminUsername"`
		AdminPasswordFile string `yaml:"adminPasswordFile"`
		AdminEmail        string `yaml:"adminEmail"`
	} `yaml:"bootstrap"`
}

func (f fileConfig) get(key string) string {
	switch key {
	case "role":
		return f.Role
	case "listen":
		return f.Listen
	case "externalUrl":
		return f.ExternalURL
	case "imageSources.sharedMirrors":
		return f.ImageSources.SharedMirrors
	case "smtp.host":
		return f.SMTP.Host
	case "smtp.port":
		return f.SMTP.Port
	case "smtp.from":
		return f.SMTP.From
	case "smtp.username":
		return f.SMTP.Username
	case "smtp.passwordFile":
		return f.SMTP.PasswordFile
	case "storage.s3.bucket":
		return f.Storage.S3.Bucket
	case "storage.s3.prefix":
		return f.Storage.S3.Prefix
	case "storage.s3.region":
		return f.Storage.S3.Region
	case "storage.s3.endpoint":
		return f.Storage.S3.Endpoint
	case "storage.s3.pathStyle":
		return f.Storage.S3.PathStyle
	case "storage.s3.authMethod":
		return f.Storage.S3.AuthMethod
	case "storage.s3.roleArn":
		return f.Storage.S3.RoleARN
	case "storage.s3.externalId":
		return f.Storage.S3.ExternalID
	case "storage.s3.credentialsFile":
		return f.Storage.S3.CredentialsFile
	case "federation.listen":
		return f.Federation.Listen
	case "federation.hostnames":
		return strings.Join(f.Federation.Hostnames, ",")
	case "federation.allowPrivatePeers":
		return f.Federation.AllowPrivatePeers
	case "backup.dir":
		return f.Backup.Dir
	case "backup.recipientsFile":
		return f.Backup.RecipientsFile
	case "backup.interval":
		return f.Backup.Interval
	case "backup.keep":
		return f.Backup.Keep
	case "backup.pgDump":
		return f.Backup.PGDump
	case "backup.pgRestore":
		return f.Backup.PGRestore
	case "cluster.enabled":
		return f.Cluster.Enabled
	case "cluster.keyFile":
		return f.Cluster.KeyFile
	case "messaging.url":
		return f.Messaging.URL
	case "messaging.credentialsFile":
		return f.Messaging.CredentialsFile
	case "messaging.caFile":
		return f.Messaging.CAFile
	case "messaging.certFile":
		return f.Messaging.CertFile
	case "messaging.keyFile":
		return f.Messaging.KeyFile
	case "messaging.replicas":
		return f.Messaging.Replicas
	case "storage.cacheEvictAfter":
		return f.Storage.CacheEvictAfter
	case "storage.oci.repository":
		return f.Storage.OCI.Repository
	case "storage.oci.credentialsFile":
		return f.Storage.OCI.CredentialsFile
	case "storage.oci.plainHttp":
		return f.Storage.OCI.PlainHTTP
	case "storage.oci.allowPrivateNetwork":
		return f.Storage.OCI.AllowPrivateNetwork
	case "storage.routes.source":
		return f.Storage.Routes.Source
	case "storage.routes.image":
		return f.Storage.Routes.Image
	case "storage.routes.report":
		return f.Storage.Routes.Report
	case "buildCache.ttl":
		return f.BuildCache.TTL
	case "buildCache.maxScopeBytes":
		return f.BuildCache.MaxScopeBytes
	case "dataDir":
		return f.DataDir
	case "log.level":
		return f.Log.Level
	case "log.format":
		return f.Log.Format
	case "shutdownTimeout":
		return f.ShutdownTimeout
	case "tls.certFile":
		return f.TLS.CertFile
	case "tls.keyFile":
		return f.TLS.KeyFile
	case "database.dsn":
		return f.Database.DSN
	case "database.migrationDsn":
		return f.Database.MigrationDSN
	case "ui.dir":
		return f.UI.Dir
	case "worker.listen":
		return f.Worker.Listen
	case "worker.hostnames":
		return strings.Join(f.Worker.Hostnames, ",")
	case "bootstrap.organizationName":
		return f.Bootstrap.OrganizationName
	case "bootstrap.adminUsername":
		return f.Bootstrap.AdminUsername
	case "bootstrap.adminPasswordFile":
		return f.Bootstrap.AdminPasswordFile
	case "bootstrap.adminEmail":
		return f.Bootstrap.AdminEmail
	case "localWorker.dir":
		return f.LocalWorker.Dir
	case "localWorker.profile":
		return f.LocalWorker.Profile
	case "validation.vulnerabilityScanner":
		return f.Validation.VulnerabilityScanner
	case "observability.metricsListen":
		return f.Observability.MetricsListen
	case "observability.metricsTokenFile":
		return f.Observability.MetricsTokenFile
	case "observability.tracing.endpoint":
		return f.Observability.Tracing.Endpoint
	case "observability.tracing.sampleRatio":
		return f.Observability.Tracing.SampleRatio
	case "observability.auditExportDir":
		return f.Observability.AuditExportDir
	}
	return ""
}

// Load resolves the configuration and reports where each value came from.
func Load(args []string, environ []string, readFile func(string) ([]byte, error)) (Config, map[string]Source, error) {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}

	fs := flag.NewFlagSet("samuori-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", env["SAMUORI_CONFIG"], "config file (.yml)")
	flagValues := map[string]*string{}
	for _, s := range settings {
		flagValues[s.key] = fs.String(s.flag, "", s.usage)
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, nil, err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	var file fileConfig
	if *configPath != "" {
		raw, err := readFile(*configPath)
		if err != nil {
			return Config{}, nil, fmt.Errorf("read config file: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, nil, fmt.Errorf("parse config file %s: %w", *configPath, err)
		}
	}

	values := map[string]string{}
	sources := map[string]Source{}
	for _, s := range settings {
		switch {
		case explicit[s.flag]:
			values[s.key], sources[s.key] = *flagValues[s.key], SourceFlag
		case env[s.env] != "":
			values[s.key], sources[s.key] = env[s.env], SourceEnv
		case file.get(s.key) != "":
			values[s.key], sources[s.key] = file.get(s.key), SourceFile
		default:
			values[s.key], sources[s.key] = s.def, SourceDefault
		}
	}

	timeout, err := time.ParseDuration(values["shutdownTimeout"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("shutdownTimeout: %w", err)
	}
	evict, err := time.ParseDuration(values["storage.cacheEvictAfter"])
	if err != nil || evict < time.Hour {
		return Config{}, nil, fmt.Errorf("storage.cacheEvictAfter %q: must be a duration of at least 1h", values["storage.cacheEvictAfter"])
	}
	smtpPort, err := strconv.Atoi(values["smtp.port"])
	if err != nil || smtpPort < 1 || smtpPort > 65535 {
		return Config{}, nil, fmt.Errorf("smtp.port %q: must be a port number", values["smtp.port"])
	}
	devWeak, err := strconv.ParseBool(values["dev.weakPasswords"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("dev.weakPasswords: %w", err)
	}
	shared, err := strconv.ParseBool(values["imageSources.sharedMirrors"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("imageSources.sharedMirrors: %w", err)
	}
	pathStyle, err := strconv.ParseBool(values["storage.s3.pathStyle"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("storage.s3.pathStyle: %w", err)
	}
	ociPlain, err := strconv.ParseBool(values["storage.oci.plainHttp"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("storage.oci.plainHttp: %w", err)
	}
	ociPrivate, err := strconv.ParseBool(values["storage.oci.allowPrivateNetwork"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("storage.oci.allowPrivateNetwork: %w", err)
	}
	cacheTTL, err := time.ParseDuration(values["buildCache.ttl"])
	if err != nil || cacheTTL < time.Hour {
		return Config{}, nil, fmt.Errorf("buildCache.ttl %q: must be a duration of at least 1h", values["buildCache.ttl"])
	}
	cacheMax, err := strconv.ParseInt(values["buildCache.maxScopeBytes"], 10, 64)
	if err != nil || cacheMax < 1<<20 {
		return Config{}, nil, fmt.Errorf("buildCache.maxScopeBytes %q: must be at least 1048576", values["buildCache.maxScopeBytes"])
	}
	routes := map[string]string{}
	for _, c := range []string{"source", "image", "report"} {
		if v := values["storage.routes."+c]; v != "" {
			routes[c] = v
		}
	}
	ratio, err := strconv.ParseFloat(values["observability.tracing.sampleRatio"], 64)
	if err != nil || ratio < 0 || ratio > 1 {
		return Config{}, nil, fmt.Errorf("observability.tracing.sampleRatio %q: must be a number from 0 to 1", values["observability.tracing.sampleRatio"])
	}
	clustered, err := strconv.ParseBool(values["cluster.enabled"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("cluster.enabled: %w", err)
	}
	replicas, err := strconv.Atoi(values["messaging.replicas"])
	if err != nil || replicas < 1 || replicas > 5 {
		return Config{}, nil, fmt.Errorf("messaging.replicas %q: must be from 1 to 5", values["messaging.replicas"])
	}
	interval, err := time.ParseDuration(values["backup.interval"])
	if err != nil || (interval != 0 && interval < time.Hour) {
		return Config{}, nil, fmt.Errorf("backup.interval %q: must be 0 or a duration of at least 1h", values["backup.interval"])
	}
	keep, err := strconv.Atoi(values["backup.keep"])
	if err != nil || keep < 1 {
		return Config{}, nil, fmt.Errorf("backup.keep %q: must be at least 1", values["backup.keep"])
	}
	privatePeers, err := strconv.ParseBool(values["federation.allowPrivatePeers"])
	if err != nil {
		return Config{}, nil, fmt.Errorf("federation.allowPrivatePeers: %w", err)
	}
	cfg := Config{
		FederationListen: values["federation.listen"], FederationHostnames: splitList(values["federation.hostnames"]), FederationPrivatePeers: privatePeers,
		Backup: Backup{Dir: values["backup.dir"], RecipientsFile: values["backup.recipientsFile"], Interval: interval, Keep: keep,
			PGDump: values["backup.pgDump"], PGRestore: values["backup.pgRestore"]},
		Cluster:        clustered,
		ClusterKeyFile: values["cluster.keyFile"],
		Messaging: Messaging{URLs: splitList(values["messaging.url"]), CredentialsFile: values["messaging.credentialsFile"],
			CAFile: values["messaging.caFile"], CertFile: values["messaging.certFile"], KeyFile: values["messaging.keyFile"], Replicas: replicas},
		Role:            Role(values["role"]),
		ConfigFile:      *configPath,
		Listen:          values["listen"],
		DataDir:         values["dataDir"],
		LogLevel:        values["log.level"],
		LogFormat:       values["log.format"],
		ShutdownTimeout: timeout,
		TLSCertFile:     values["tls.certFile"],
		TLSKeyFile:      values["tls.keyFile"],
		Database:        values["database.dsn"],
		MigrationDB:     values["database.migrationDsn"],
		UIDir:           values["ui.dir"],
		WorkerListen:    values["worker.listen"],
		WorkerHostnames: splitList(values["worker.hostnames"]),
		BootstrapOrg:    values["bootstrap.organizationName"],
		BootstrapAdmin:  values["bootstrap.adminUsername"],
		BootstrapPWFile: values["bootstrap.adminPasswordFile"],
		BootstrapEmail:  values["bootstrap.adminEmail"],

		DevWeakPasswords: devWeak,

		LocalWorkerDir:     values["localWorker.dir"],
		LocalWorkerProfile: values["localWorker.profile"],

		VulnerabilityScanner: values["validation.vulnerabilityScanner"],

		ExternalURL:             strings.TrimRight(values["externalUrl"], "/"),
		S3Bucket:                values["storage.s3.bucket"],
		S3Prefix:                values["storage.s3.prefix"],
		S3Region:                values["storage.s3.region"],
		S3Endpoint:              values["storage.s3.endpoint"],
		S3PathStyle:             pathStyle,
		S3AuthMethod:            values["storage.s3.authMethod"],
		S3RoleARN:               values["storage.s3.roleArn"],
		S3ExternalID:            values["storage.s3.externalId"],
		S3CredentialsFile:       values["storage.s3.credentialsFile"],
		CacheEvictAfter:         evict,
		OCIRepository:           values["storage.oci.repository"],
		OCICredentialsFile:      values["storage.oci.credentialsFile"],
		OCIPlainHTTP:            ociPlain,
		OCIAllowPrivate:         ociPrivate,
		StorageRoutes:           routes,
		BuildCacheTTL:           cacheTTL,
		BuildCacheMaxScopeBytes: cacheMax,
		SharedMirrors:           shared,
		SMTPHost:                values["smtp.host"],
		SMTPPort:                smtpPort,
		SMTPFrom:                values["smtp.from"],
		SMTPUsername:            values["smtp.username"],
		SMTPPasswordFile:        values["smtp.passwordFile"],
		MetricsListen:           values["observability.metricsListen"],
		MetricsTokenFile:        values["observability.metricsTokenFile"],
		TracingEndpoint:         values["observability.tracing.endpoint"],
		TracingSampleRatio:      ratio,
		AuditExportDir:          values["observability.auditExportDir"],
	}
	if cfg.AuditExportDir == "" {
		cfg.AuditExportDir = filepath.Join(cfg.DataDir, "audit-export")
	}
	if cfg.Database == "" {
		cfg.Database = "sqlite://" + filepath.Join(cfg.DataDir, "samuori.db")
	}
	if cfg.MigrationDB == "" {
		cfg.MigrationDB = cfg.Database
	}
	return cfg, sources, cfg.validate()
}

func (c Config) validate() error {
	var errs []error
	if c.Role != RoleController && c.Role != RoleAllInOne {
		errs = append(errs, fmt.Errorf("role %q: must be controller or all-in-one", c.Role))
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		errs = append(errs, fmt.Errorf("listen %q: %w", c.Listen, err))
	} else if !c.TLSEnabled() && !isLoopback(host) {
		errs = append(errs, fmt.Errorf("listen %q: plaintext HTTP is only allowed on loopback; configure tls.certFile and tls.keyFile", c.Listen))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("tls.certFile and tls.keyFile must be set together"))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("dataDir must not be empty"))
	}
	// A cluster controller converted from a single server keeps its host's worker (ADR 0046, OD-28).
	if c.LocalWorkerDir != "" && c.Role != RoleAllInOne && !(c.Role == RoleController && c.Cluster) {
		errs = append(errs, errors.New("localWorker.dir is only for the all-in-one role or a cluster controller that keeps its host's worker"))
	}
	if c.LocalWorkerDir != "" && !isolation.Profile(c.LocalWorkerProfile).Valid() {
		errs = append(errs, fmt.Errorf("localWorker.profile %q: unknown isolation profile", c.LocalWorkerProfile))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: must be debug, info, warn or error", c.LogLevel))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("log.format %q: must be json or text", c.LogFormat))
	}
	for name, dsn := range map[string]string{"database.dsn": c.Database, "database.migrationDsn": c.MigrationDB} {
		if !strings.HasPrefix(dsn, "sqlite://") && !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
			errs = append(errs, fmt.Errorf("%s: must start with sqlite:// or postgres://", name))
		}
	}
	if (c.BootstrapAdmin == "") != (c.BootstrapPWFile == "") || (c.BootstrapAdmin != "" && c.BootstrapOrg == "") {
		errs = append(errs, errors.New("bootstrap: organizationName, adminUsername and adminPasswordFile must be set together"))
	}
	if c.DevWeakPasswords {
		host, _, _ := net.SplitHostPort(c.Listen)
		if c.Role != RoleAllInOne || c.Cluster || !isLoopback(host) {
			errs = append(errs, errors.New("dev.weakPasswords is for local development: it needs role all-in-one, no cluster and a loopback listen address"))
		}
	}
	if c.ExternalURL != "" {
		u, err := url.Parse(c.ExternalURL)
		switch {
		case err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
			errs = append(errs, fmt.Errorf("externalUrl %q: must be a scheme and host, such as https://samuori.example", c.ExternalURL))
		case u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())):
			errs = append(errs, fmt.Errorf("externalUrl %q: must use https (http only on loopback)", c.ExternalURL))
		}
	}
	if c.S3Bucket != "" {
		if c.S3Region == "" {
			errs = append(errs, errors.New("storage.s3.region is required with a bucket"))
		}
		switch c.S3AuthMethod {
		case "workload-identity", "assume-role":
		case "access-key":
			if c.S3CredentialsFile == "" {
				errs = append(errs, errors.New("storage.s3.credentialsFile is required for access-key"))
			}
		default:
			errs = append(errs, fmt.Errorf("storage.s3.authMethod %q: must be workload-identity, assume-role or access-key", c.S3AuthMethod))
		}
		if c.S3Endpoint != "" {
			u, err := url.Parse(c.S3Endpoint)
			if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
				errs = append(errs, fmt.Errorf("storage.s3.endpoint %q: must be https (http only on loopback)", c.S3Endpoint))
			}
		}
	}
	for class, backend := range c.StorageRoutes {
		switch {
		case backend == "s3" && c.S3Bucket == "", backend == "oci" && c.OCIRepository == "":
			errs = append(errs, fmt.Errorf("storage.routes.%s %q: that backend is not configured", class, backend))
		case backend != "local" && backend != "s3" && backend != "oci":
			errs = append(errs, fmt.Errorf("storage.routes.%s %q: must be local, s3 or oci", class, backend))
		}
	}
	if c.OCIRepository != "" && strings.Contains(c.OCIRepository, "://") {
		errs = append(errs, fmt.Errorf("storage.oci.repository %q: give host[:port]/path without a scheme", c.OCIRepository))
	}
	if c.SMTPHost != "" && c.SMTPFrom == "" {
		errs = append(errs, errors.New("smtp.from is required with an SMTP host"))
	}
	if c.MetricsListen != "" {
		host, _, err := net.SplitHostPort(c.MetricsListen)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("observability.metricsListen %q: %w", c.MetricsListen, err))
		case !isLoopback(host) && c.MetricsTokenFile == "":
			errs = append(errs, fmt.Errorf("observability.metricsListen %q: a metrics token file is required off loopback", c.MetricsListen))
		}
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("shutdownTimeout must be positive"))
	}
	if (c.Backup.Dir == "") != (c.Backup.RecipientsFile == "") {
		errs = append(errs, errors.New("backup.dir and backup.recipientsFile must be set together"))
	}
	if c.Backup.Interval > 0 && c.Backup.Dir == "" {
		errs = append(errs, errors.New("backup.interval needs backup.dir"))
	}
	if c.FederationListen != "" {
		if _, _, err := net.SplitHostPort(c.FederationListen); err != nil {
			errs = append(errs, fmt.Errorf("federation.listen %q: %w", c.FederationListen, err))
		}
		if len(c.FederationHostnames) == 0 {
			errs = append(errs, errors.New("federation.listen needs federation.hostnames: the names peers reach this installation by"))
		}
	}
	errs = append(errs, c.validateMessaging()...)
	errs = append(errs, c.validateCluster()...)
	return errors.Join(errs...)
}

func (c Config) validateMessaging() []error {
	var errs []error
	m := c.Messaging
	secure := m.CAFile != "" && (m.CredentialsFile != "" || m.CertFile != "")
	for _, raw := range m.URLs {
		u, err := url.Parse(raw)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls"):
			errs = append(errs, fmt.Errorf("messaging.url %q: must be nats://host:port or tls://host:port", raw))
		case u.User != nil:
			errs = append(errs, fmt.Errorf("messaging.url %q: put credentials in messaging.credentialsFile, not the URL", u.Redacted()))
		case !secure && !isLoopback(u.Hostname()):
			errs = append(errs, fmt.Errorf("messaging.url %q: off loopback, NATS needs TLS (messaging.caFile) and credentials (messaging.credentialsFile or a client certificate)", raw))
		}
	}
	if (m.CertFile == "") != (m.KeyFile == "") {
		errs = append(errs, errors.New("messaging.certFile and messaging.keyFile must be set together"))
	}
	if len(m.URLs) == 0 && (m.CredentialsFile != "" || m.CAFile != "" || m.CertFile != "") {
		errs = append(errs, errors.New("messaging credentials and certificates need messaging.url"))
	}
	return errs
}

// StorageClasses are the blob data classes; in a cluster none may stay in one controller's data directory.
var StorageClasses = []string{"source", "image", "report", "other"}

func (c Config) validateCluster() []error {
	if !c.Cluster {
		return nil
	}
	var errs []error
	if !strings.HasPrefix(c.Database, "postgres://") && !strings.HasPrefix(c.Database, "postgresql://") {
		errs = append(errs, errors.New("cluster.enabled needs a PostgreSQL database.dsn"))
	}
	if len(c.Messaging.URLs) == 0 {
		errs = append(errs, errors.New("cluster.enabled needs an external NATS cluster (messaging.url)"))
	}
	if c.ClusterKeyFile == "" {
		errs = append(errs, errors.New("cluster.enabled needs cluster.keyFile"))
	}
	if c.Role != RoleController {
		errs = append(errs, errors.New("cluster.enabled needs role controller; the all-in-one role is one controller with its own worker"))
	}
	for _, class := range StorageClasses {
		if c.StorageRoute(class) == "local" {
			errs = append(errs, fmt.Errorf("cluster.enabled needs shared blob storage: %s blobs are routed to this controller's data directory", class))
		}
	}
	return errs
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func OSReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// StorageRoute returns the backend for a data class: its route, else s3 when a bucket is set, else oci when a
// repository is set, else local.
func (c Config) StorageRoute(class string) string {
	if r := c.StorageRoutes[class]; r != "" {
		return r
	}
	switch {
	case c.S3Bucket != "":
		return "s3"
	case c.OCIRepository != "":
		return "oci"
	}
	return "local"
}
