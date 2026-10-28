package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/clusterjoin"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/pki"
)

// clusterCommand handles `samuori-server cluster init|join|nats-system|forget|routes`, run as root by the installer and its
// helpers (docs/operators/high-availability.md).
func clusterCommand(sub string, args []string, stdin io.Reader, stdout io.Writer) error {
	switch sub {
	case "init":
		return clusterInit(args, stdout)
	case "join":
		return clusterJoinCommand(args, stdin, stdout)
	case "nats-system":
		return natsSystemCommand(args, stdout)
	case "forget":
		return forgetCommand(args, stdout)
	case "routes":
		return routesCommand(args, stdout)
	}
	return fmt.Errorf("unknown subcommand %q; use init, join, nats-system, forget or routes", sub)
}

type layoutFlags struct {
	configDir, dataDir, uiDir, owner, natsOwner, host string
}

func (l *layoutFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&l.host, "host", "", "this controller's DNS name or IP address, as other controllers and workers reach it (required)")
	fs.StringVar(&l.configDir, "config-dir", "/etc/samuori", "where server.yml, keys and certificates are written")
	fs.StringVar(&l.dataDir, "data-dir", "/var/lib/samuori", "the controller's data directory")
	fs.StringVar(&l.uiDir, "ui-dir", "/usr/local/share/samuori/ui", "the web UI directory")
	fs.StringVar(&l.owner, "owner", "samuori", "the user samuori-server runs as")
	fs.StringVar(&l.natsOwner, "nats-owner", "nats", "the user the NATS member runs as")
}

func (l layoutFlags) layout() clusterjoin.Layout {
	return clusterjoin.Layout{ConfigDir: l.configDir, DataDir: l.dataDir, UIDir: l.uiDir, Owner: l.owner, NATSOwner: l.natsOwner}
}

func (l layoutFlags) check() error {
	if !clusterjoin.ValidHost(l.host) {
		return errors.New("--host must be this controller's DNS name or IP address")
	}
	if _, err := os.Stat(filepath.Join(l.configDir, "cluster.key")); err == nil {
		return fmt.Errorf("%s already holds a cluster key: this controller is already set up", l.configDir)
	}
	return nil
}

// clusterInit sets up the first controller: a new cluster key and cluster CA, the controller's and NATS member's
// certificates, and the configuration that points at the shared database and storage. The administrator named by
// --admin-username is created on the first start; later controllers join and never create one.
func clusterInit(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cluster init", flag.ContinueOnError)
	var l layoutFlags
	l.register(fs)
	dsn := fs.String("database", os.Getenv("SAMUORI_DATABASE"), "PostgreSQL DSN, for example postgres://samuori:PASSWORD@db.example:5432/samuori?sslmode=verify-full&sslrootcert=/etc/samuori/rds-ca.pem (or SAMUORI_DATABASE)")
	externalURL := fs.String("external-url", "", "URL browsers use, such as https://samuori.example (the load balancer)")
	workerHosts := fs.String("worker-hostnames", "", "extra names workers use to reach controllers, comma-separated")
	replicas := fs.Int("replicas", 3, "JetStream replicas the stream grows to as members join")
	s3Bucket := fs.String("s3-bucket", "", "S3 bucket for shared blobs")
	s3Region := fs.String("s3-region", "", "S3 bucket region")
	s3Prefix := fs.String("s3-prefix", "", "key prefix in the bucket")
	s3Endpoint := fs.String("s3-endpoint", "", "S3-compatible endpoint URL (empty for AWS)")
	s3PathStyle := fs.Bool("s3-path-style", false, "address the bucket by path (most S3-compatible services)")
	s3Auth := fs.String("s3-auth-method", "workload-identity", "workload-identity (instance role), assume-role or access-key")
	s3Role := fs.String("s3-role-arn", "", "role to assume for S3")
	s3ExternalID := fs.String("s3-external-id", "", "external ID for the assumed role")
	s3Creds := fs.String("s3-credentials-file", "", "ACCESS_KEY_ID:SECRET_ACCESS_KEY file for access-key")
	ociRepo := fs.String("oci-repository", "", "OCI repository for shared blobs, instead of S3")
	ociPrivate := fs.Bool("oci-allow-private-network", false, "allow a private or loopback address for the OCI repository")
	ociCreds := fs.String("oci-credentials-file", "", "USERNAME:PASSWORD file for the OCI repository")
	localWorker := fs.String("local-worker-dir", "", "keep this host's worker, handing it enrollment tokens in this directory (install.sh --keep-local-worker)")
	keyFile := fs.String("key-file", "", "use this 32-byte key as the cluster key instead of a new one (a converted single server's data-dir/keys/secrets.key)")
	natsURL := fs.String("nats-url", "", "tls://host:port URLs of an external NATS cluster, comma-separated (default: a NATS member on each controller)")
	natsCA := fs.String("nats-ca-file", "", "CA certificates that sign the external NATS servers' certificates")
	natsCreds := fs.String("nats-credentials-file", "", "NATS credentials (.creds) for the external cluster")
	natsCert := fs.String("nats-cert-file", "", "client certificate for mutual TLS with the external cluster")
	natsKey := fs.String("nats-key-file", "", "client key for mutual TLS with the external cluster")
	backupRecipients := fs.String("backup-recipients-file", "", "age recipients for encrypted backups")
	org := fs.String("organization", "", "organization created on the first start")
	admin := fs.String("admin-username", "", "first administrator, created on the first start")
	adminPW := fs.String("admin-password-file", "", "file holding the first administrator's password")
	adminEmail := fs.String("admin-email", "", "first administrator's email address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := l.check(); err != nil {
		return err
	}
	switch {
	case !strings.HasPrefix(*dsn, "postgres://") && !strings.HasPrefix(*dsn, "postgresql://"):
		return errors.New("--database must be a PostgreSQL DSN: clustered controllers share one PostgreSQL database")
	case *s3Bucket == "" && *ociRepo == "":
		return errors.New("--s3-bucket or --oci-repository is required: clustered controllers share blob storage")
	case (*admin == "") != (*adminPW == ""):
		return errors.New("--admin-username and --admin-password-file go together")
	case *natsURL == "" && *natsCA+*natsCreds+*natsCert+*natsKey != "":
		return errors.New("--nats-ca-file, --nats-credentials-file, --nats-cert-file and --nats-key-file need --nats-url")
	case *natsURL != "" && (*natsCA == "" || (*natsCreds == "" && *natsCert == "") || (*natsCert == "") != (*natsKey == "")):
		return errors.New("an external NATS cluster needs --nats-ca-file and credentials: --nats-credentials-file, or --nats-cert-file with --nats-key-file")
	}
	if err := checkDatabase(*dsn, true); err != nil {
		return err
	}
	key := make([]byte, 32)
	if *keyFile != "" {
		v, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(*keyFile); err != nil || fi.Mode().Perm()&0o077 != 0 || len(v) != 32 {
			return fmt.Errorf("--key-file %s must hold 32 bytes and be readable by its owner only", *keyFile)
		}
		key = v
	} else if _, err := rand.Read(key); err != nil {
		return err
	}
	b := clusterjoin.Bundle{ClusterKey: key, DatabaseDSN: *dsn, ExternalURL: *externalURL, NATSReplicas: *replicas, Storage: map[string]string{}, Files: map[string][]byte{}}
	for _, h := range strings.Split(*workerHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			b.WorkerHostnames = append(b.WorkerHostnames, h)
		}
	}
	files := map[string]string{"s3-credentials": *s3Creds, "oci-credentials": *ociCreds, "backup-recipients": *backupRecipients}
	if *natsURL != "" {
		b.ExternalNATS = true
		for _, u := range strings.Split(*natsURL, ",") {
			if u = strings.TrimSpace(u); u != "" {
				if !strings.HasPrefix(u, "tls://") {
					return fmt.Errorf("--nats-url %q: an external NATS cluster must use tls://", u)
				}
				b.NATSURLs = append(b.NATSURLs, u)
			}
		}
		files["messaging-ca"], files["messaging-credentials"], files["messaging-cert"], files["messaging-key"] = *natsCA, *natsCreds, *natsCert, *natsKey
	}
	put := func(k, v string) {
		if v != "" {
			b.Storage[k] = v
		}
	}
	if *s3Bucket != "" {
		put("s3.bucket", *s3Bucket)
		put("s3.region", *s3Region)
		put("s3.prefix", *s3Prefix)
		put("s3.endpoint", *s3Endpoint)
		put("s3.pathStyle", strconv.FormatBool(*s3PathStyle))
		put("s3.authMethod", *s3Auth)
		put("s3.roleArn", *s3Role)
		put("s3.externalId", *s3ExternalID)
	} else {
		put("oci.repository", *ociRepo)
		put("oci.allowPrivateNetwork", strconv.FormatBool(*ociPrivate))
	}
	for name, path := range files {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.Files[name] = bytes.TrimSpace(data)
	}
	pkiDir := filepath.Join(l.dataDir, "cluster-pki")
	authority, err := clusterjoin.Create(pkiDir)
	if err != nil {
		return err
	}
	if err := chownTree(pkiDir, l.owner); err != nil {
		return err
	}
	if b, err = b.ForHost(authority, l.host); err != nil {
		return err
	}
	bootstrap := ""
	if *admin != "" {
		pw, err := os.ReadFile(*adminPW)
		if err != nil {
			return err
		}
		pwFile := filepath.Join(l.configDir, "admin-password")
		if err := os.MkdirAll(l.configDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(pwFile, bytes.TrimSpace(pw), 0o600); err != nil {
			return err
		}
		if err := chownTree(pwFile, l.owner); err != nil {
			return err
		}
		bootstrap = fmt.Sprintf("bootstrap:\n  organizationName: %q\n  adminUsername: %q\n  adminPasswordFile: %s\n", *org, *admin, pwFile)
		if *adminEmail != "" {
			bootstrap += fmt.Sprintf("  adminEmail: %q\n", *adminEmail)
		}
	}
	if *localWorker != "" {
		bootstrap = fmt.Sprintf("localWorker:\n  dir: %s\n", *localWorker) + bootstrap
	}
	if err := b.Install(l.layout(), bootstrap); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "cluster initialized on %s; start nats and samuori-server, then add controllers under Administration > Servers\n", l.host)
	return nil
}

// clusterJoinCommand redeems a join token at an existing controller, checking its TLS certificate against the pin
// the administrator was shown, waits for an administrator to accept the request, and writes this controller's
// configuration (docs/operators/high-availability.md#join).
func clusterJoinCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("cluster join", flag.ContinueOnError)
	var l layoutFlags
	l.register(fs)
	code := fs.String("code", "", "the join code shown in the portal (samuoric_...); replaces --url, --pin and --token-file")
	url := fs.String("url", "", "an existing controller's address, as shown with the token (https://host:8443)")
	pin := fs.String("pin", "", "the controller certificate pin shown with the token (sha256:...)")
	tokenFile := fs.String("token-file", "", "file holding the join token (default: read from standard input)")
	wait := fs.Duration("wait", clusterjoin.RequestTTL, "how long to wait for an administrator to accept the request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := l.check(); err != nil {
		return err
	}
	var token string
	if *code != "" {
		var err error
		if *url, *pin, token, err = clusterjoin.DecodeCode(*code); err != nil {
			return err
		}
	} else {
		if !strings.HasPrefix(*url, "https://") || !strings.HasPrefix(*pin, "sha256:") {
			return errors.New("--code, or --url (https://...) and --pin (sha256:...), are required")
		}
		if *tokenFile != "" {
			raw, err := os.ReadFile(*tokenFile)
			if err != nil {
				return err
			}
			token = strings.TrimSpace(string(raw))
		} else {
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			token = strings.TrimSpace(line)
		}
	}
	base := strings.TrimSuffix(*url, "/") + "/api/v1/cluster/join"
	client := pinnedClient(*pin)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	sum := sha256.Sum256([]byte(secret))

	var pending struct {
		RequestID        string `json:"requestId"`
		VerificationCode string `json:"verificationCode"`
	}
	status, raw, err := postJSON(client, base, map[string]string{"token": token, "host": l.host, "secretHash": hex.EncodeToString(sum[:])})
	if err != nil {
		return fmt.Errorf("reaching %s: %w", *url, err)
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("the controller refused the join (%d): %s", status, problemDetail(raw))
	}
	if err := json.Unmarshal(raw, &pending); err != nil || pending.RequestID == "" {
		return errors.New("the controller sent an unexpected answer to the join request")
	}
	fmt.Fprintf(stdout, "join request for %s sent\nverification code: %s\nWaiting for an administrator to accept it under Administration > Servers…\n", l.host, pending.VerificationCode)

	deadline := time.Now().Add(*wait)
	pollURL := base + "/requests/" + pending.RequestID + "/poll"
	for {
		status, raw, err := postJSON(client, pollURL, map[string]string{"secret": secret})
		switch {
		case err != nil || status == http.StatusTooManyRequests || status >= 500:
		case status != http.StatusOK:
			return fmt.Errorf("the join was not accepted (%d): %s", status, problemDetail(raw))
		default:
			var res struct {
				State  string              `json:"state"`
				Bundle *clusterjoin.Bundle `json:"bundle"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("reading the join answer: %w", err)
			}
			if res.State == clusterjoin.StateAccepted && res.Bundle != nil {
				return installJoinBundle(*res.Bundle, l, stdout)
			}
		}
		if time.Now().After(deadline) {
			return errors.New("no administrator accepted the join request in time; create a new token and try again")
		}
		time.Sleep(3 * time.Second)
	}
}

func installJoinBundle(b clusterjoin.Bundle, l layoutFlags, stdout io.Writer) error {
	if b.Host != l.host {
		return errors.New("the join bundle is for another host")
	}
	if err := b.UseBundledDatabaseCA(l.configDir); err != nil {
		return err
	}
	if err := checkDatabase(b.DatabaseDSN, false); err != nil {
		return err
	}
	if err := b.Install(l.layout(), ""); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "accepted; joined the cluster as %s; start nats and samuori-server\n", l.host)
	return nil
}

func postJSON(client *http.Client, url string, v any) (int, []byte, error) {
	body, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

func problemDetail(raw []byte) string {
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Detail
}

// pinnedClient trusts exactly the server certificate whose public key matches pin.
func pinnedClient(pin string) *http.Client {
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			c, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if pki.Pin(c) != pin {
				return errors.New("the controller's certificate does not match the pin")
			}
			return nil
		}}}}
}

// checkDatabase signs in to PostgreSQL and reads its schema state. A first controller needs a database that holds
// no cluster yet; a joining one needs the cluster's.
func checkDatabase(dsn string, first bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := db.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer d.Close()
	if err := d.Ping(ctx); err != nil {
		return fmt.Errorf("database: signing in failed: %w", err)
	}
	applied, known, err := d.Schema(ctx)
	if err != nil {
		return fmt.Errorf("database: reading the schema: %w", err)
	}
	var clustered int
	if applied > 0 {
		_ = d.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM cluster_material WHERE name = 'cluster-pki'`).Scan(&clustered)
	}
	switch {
	case applied > known:
		return fmt.Errorf("database: the schema (version %d) is newer than this release knows (%d)", applied, known)
	case first && clustered > 0:
		return errors.New("database: it already belongs to a cluster; join that cluster instead of initializing a new one")
	case !first && clustered == 0:
		return errors.New("database: it holds no cluster yet; start the first controller before joining")
	}
	return nil
}

func chownTree(path, owner string) error {
	u, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}
