package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"samuori.invalid/samuori/internal/cluster"
	"samuori.invalid/samuori/internal/clusterjoin"
	"samuori.invalid/samuori/internal/config"
	"samuori.invalid/samuori/internal/conversion"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/setupcli"
	"samuori.invalid/samuori/internal/storage"
)

const (
	configDir     = "/etc/samuori"
	convertBackup = "/var/lib/samuori-convert"
	unitDir       = "/etc/systemd/system"
)

// singleServerUnits are the all-in-one services a controller install removes (install.sh); a rollback restores them.
var singleServerUnits = []string{"samuori-server.service", "samuori-local-registry.service"}

// clusterUnits are the services a clustered controller runs (install-controller.sh).
var clusterUnits = []string{"samuori-controller.service", "samuori-nats.service", "samuori-nats-forget.path", "samuori-nats-routes.path"}

// Settings each direction writes itself, replaces on purpose or carries into the new configuration; any other
// setting in the old server.yml would be lost and is reported.
var (
	toClusterCarried = []string{"role", "listen", "dataDir", "log.format", "ui.dir", "worker.listen", "worker.hostnames", "localWorker.", "externalUrl",
		"storage.s3.", "storage.oci.", "backup.recipientsFile", "bootstrap.", "database.dsn"}
	toSingleCarried = []string{"role", "listen", "dataDir", "log.format", "ui.dir", "worker.listen", "worker.hostnames", "localWorker.", "externalUrl",
		"tls.", "cluster.", "messaging.", "database.dsn", "storage.s3.", "storage.oci.", "storage.routes.", "bootstrap."}
)

type convertFlags struct {
	to, configFile, envFile, release, host, externalURL string
	dryRun, dropSettings, dedicatedVM                   bool
	dbHost, dbName, dbUser, dbCA, dbPasswordFile        string
	dbPort                                              int
	s3                                                  config.Config
	s3PathStyle, ociPrivate                             bool
}

// topologyCommand is `samuori-server topology convert`, run as root from the release folder with Samuori stopped (ADR 0046).
func topologyCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "convert" {
		return errors.New("usage: samuori-server topology convert --to cluster|single-server [--dry-run] ...")
	}
	fs := flag.NewFlagSet("topology convert", flag.ContinueOnError)
	var f convertFlags
	fs.StringVar(&f.to, "to", "", "cluster (this single server becomes a cluster's first controller) or single-server")
	fs.BoolVar(&f.dryRun, "dry-run", false, "run every check and report the counts; write nothing")
	fs.StringVar(&f.configFile, "config", filepath.Join(configDir, "server.yml"), "this host's server.yml")
	fs.StringVar(&f.envFile, "env-file", filepath.Join(configDir, "env"), "a clustered controller's environment file, when present")
	fs.StringVar(&f.release, "release", "", "the release folder holding install.sh (default: this binary's folder)")
	fs.StringVar(&f.host, "host", "", "this host's DNS name, as other controllers and workers reach it (default: as samuori-setup picks it)")
	fs.StringVar(&f.externalURL, "external-url", "", "portal URL browsers use (default: the current externalUrl)")
	fs.BoolVar(&f.dropSettings, "allow-dropped-settings", false, "convert even though some settings of server.yml are not carried over")
	fs.BoolVar(&f.dedicatedVM, "confirm-dedicated-vm", false, "confirm this host is a VM that runs nothing else (ADR 0027)")
	fs.StringVar(&f.dbHost, "database-host", "", "cluster: PostgreSQL host of the new, empty database")
	fs.IntVar(&f.dbPort, "database-port", 5432, "cluster: PostgreSQL port")
	fs.StringVar(&f.dbName, "database-name", "samuori", "cluster: database name")
	fs.StringVar(&f.dbUser, "database-user", "samuori", "cluster: database user that owns the schema")
	fs.StringVar(&f.dbCA, "database-ca-file", setupcli.DefaultCAFile, "cluster: CA bundle for the database certificate")
	fs.StringVar(&f.dbPasswordFile, "database-password-file", "", "cluster: file (mode 0600) holding the database password (default: asked on the terminal)")
	fs.StringVar(&f.s3.S3Bucket, "s3-bucket", "", "cluster: S3 bucket for shared blobs (default: the current storage.s3 settings)")
	fs.StringVar(&f.s3.S3Region, "s3-region", "", "S3 bucket region")
	fs.StringVar(&f.s3.S3Prefix, "s3-prefix", "", "key prefix in the bucket")
	fs.StringVar(&f.s3.S3Endpoint, "s3-endpoint", "", "S3-compatible endpoint URL")
	fs.BoolVar(&f.s3PathStyle, "s3-path-style", false, "address the bucket by path")
	fs.StringVar(&f.s3.S3AuthMethod, "s3-auth-method", "workload-identity", "workload-identity, assume-role or access-key")
	fs.StringVar(&f.s3.S3RoleARN, "s3-role-arn", "", "role to assume for S3")
	fs.StringVar(&f.s3.S3ExternalID, "s3-external-id", "", "external ID for the assumed role")
	fs.StringVar(&f.s3.S3CredentialsFile, "s3-credentials-file", "", "ACCESS_KEY_ID:SECRET_ACCESS_KEY file for access-key")
	fs.StringVar(&f.s3.OCIRepository, "oci-repository", "", "cluster: OCI repository for shared blobs, instead of S3")
	fs.StringVar(&f.s3.OCICredentialsFile, "oci-credentials-file", "", "USERNAME:PASSWORD file for the OCI repository")
	fs.BoolVar(&f.ociPrivate, "oci-allow-private-network", false, "allow a private address for the OCI repository")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("convert takes no arguments; the database password is read from --database-password-file or the terminal, never the command line")
	}
	f.s3.S3PathStyle, f.s3.OCIAllowPrivate = f.s3PathStyle, f.ociPrivate
	if os.Geteuid() != 0 {
		return errors.New("run as root from the release folder: sudo ./samuori-server topology convert ...")
	}
	if f.release == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		f.release = filepath.Dir(exe)
	}
	if _, err := os.Stat(filepath.Join(f.release, "install.sh")); err != nil {
		return fmt.Errorf("%s is not a release folder (no install.sh); run this from the release folder of the installed version", f.release)
	}
	switch f.to {
	case "cluster":
		return convertToCluster(ctx, f, stdin, stdout)
	case "single-server":
		return convertToSingle(ctx, f, stdout)
	}
	return errors.New("--to must be cluster or single-server")
}

func droppedSettings(origins map[string]config.Source, carried []string) []string {
	var out []string
	for k, src := range origins {
		if src != config.SourceFile {
			continue
		}
		if !slices.ContainsFunc(carried, func(c string) bool { return k == c || (strings.HasSuffix(c, ".") && strings.HasPrefix(k, c)) }) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func checkDropped(out io.Writer, f convertFlags, origins map[string]config.Source, carried []string) error {
	dropped := droppedSettings(origins, carried)
	if len(dropped) == 0 {
		return nil
	}
	fmt.Fprintf(out, "not carried over from %s: %s\n", f.configFile, strings.Join(dropped, ", "))
	if !f.dryRun && !f.dropSettings {
		return errors.New("some settings would be lost; set them again after the conversion and pass --allow-dropped-settings")
	}
	return nil
}

func printCounts(out io.Writer, tables []db.TableReport, blobs conversion.BlobReport) {
	for _, t := range tables {
		fmt.Fprintf(out, "  %-34s %10d rows\n", t.Name, t.Rows)
	}
	n, rows := conversion.Totals(tables)
	fmt.Fprintf(out, "source: %d tables, %d rows; %d blobs, %d bytes\n", n, rows, blobs.Blobs, blobs.Bytes)
}

func reportCopy(out io.Writer, tables []db.TableReport) string {
	for _, t := range tables {
		fmt.Fprintf(out, "verified %-30s %10d rows  sha256:%.16s\n", t.Name, t.Rows, t.Checksum)
	}
	n, rows := conversion.Totals(tables)
	return fmt.Sprintf("%d tables, %d rows", n, rows)
}

func requireStopped(ctx context.Context, src *db.DB) error {
	running, err := conversion.Running(ctx, src)
	if err != nil {
		return err
	}
	if len(running) > 0 {
		return fmt.Errorf("Samuori is still running on %s; stop it first (sudo systemctl stop samuori-server samuori-controller)", strings.Join(running, ", "))
	}
	return nil
}

func readKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must hold 32 bytes and be readable by its owner only", path)
	}
	return key, nil
}

func backupDir() (string, error) {
	dir := filepath.Join(convertBackup, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(convertBackup, 0o700)
}

func convertToCluster(ctx context.Context, f convertFlags, stdin io.Reader, out io.Writer) error {
	cfg, origins, _, err := loadConfig([]string{"--config", f.configFile})
	if err != nil {
		return err
	}
	if cfg.Role != config.RoleAllInOne || cfg.Cluster {
		return errors.New("this host is not a single server (role all-in-one); it is already a controller")
	}
	if dialect, _, err := db.ParseDSN(cfg.Database); err != nil || dialect != db.SQLite {
		return errors.New("this single server does not use SQLite; a cluster needs PostgreSQL shared by every controller")
	}
	if err := checkDropped(out, f, origins, toClusterCarried); err != nil {
		return err
	}
	if !f.dedicatedVM {
		return errors.New("the host keeps its worker, which builds uploaded source here, so it must stay a VM that runs nothing else (ADR 0027); confirm with --confirm-dedicated-vm")
	}
	if f.host == "" {
		f.host = (&setupHost{}).DefaultHostname(ctx)
	}
	if !clusterjoin.ValidHost(f.host) {
		return fmt.Errorf("--host %q must be this host's DNS name or IP address", f.host)
	}
	if _, err := os.Stat(filepath.Join(configDir, "cluster.key")); err == nil {
		return errors.New("/etc/samuori/cluster.key exists: this host is already a controller")
	}
	keyFile := filepath.Join(cfg.DataDir, "keys", "secrets.key")
	if _, err := readKey(keyFile); err != nil {
		return fmt.Errorf("the single server's secrets key: %w", err)
	}

	src, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := requireStopped(ctx, src); err != nil {
		return err
	}
	version, err := conversion.CheckSource(ctx, src)
	if err != nil {
		return err
	}

	target := f.s3
	if target.S3Bucket == "" && target.OCIRepository == "" {
		target = config.Config{S3Bucket: cfg.S3Bucket, S3Region: cfg.S3Region, S3Prefix: cfg.S3Prefix, S3Endpoint: cfg.S3Endpoint, S3PathStyle: cfg.S3PathStyle,
			S3AuthMethod: cfg.S3AuthMethod, S3RoleARN: cfg.S3RoleARN, S3ExternalID: cfg.S3ExternalID, S3CredentialsFile: cfg.S3CredentialsFile,
			OCIRepository: cfg.OCIRepository, OCICredentialsFile: cfg.OCICredentialsFile, OCIAllowPrivate: cfg.OCIAllowPrivate}
	}
	if target.S3Bucket != "" {
		target.OCIRepository = ""
	}
	if target.S3Bucket == "" && target.OCIRepository == "" {
		return errors.New("a cluster shares blob storage: pass --s3-bucket and --s3-region, or --oci-repository")
	}
	if target.S3Bucket != "" && target.S3Region == "" {
		return errors.New("--s3-bucket needs --s3-region")
	}
	remotes, err := openRemotes(ctx, target)
	if err != nil {
		return err
	}
	var remote storage.Remote
	for _, r := range remotes {
		remote = r
	}
	if err := remote.Check(ctx); err != nil {
		return fmt.Errorf("%s: %w", remote.Describe(), err)
	}

	if f.dbHost == "" {
		return errors.New("--database-host is required: the new, empty PostgreSQL database the cluster will share")
	}
	if _, err := os.Stat(f.dbCA); err != nil {
		return fmt.Errorf("--database-ca-file: %w", err)
	}
	d := setupcli.Database{Host: f.dbHost, Port: f.dbPort, Name: f.dbName, User: f.dbUser, CAFile: f.dbCA}
	if f.dbPasswordFile != "" {
		if d.Password, err = config.ReadPrivateFile(f.dbPasswordFile); err != nil {
			return err
		}
	} else if d.Password, err = newTerminal(stdin, out).Secret("Database password for " + f.dbUser + "@" + f.dbHost); err != nil {
		return err
	}
	if d.Password == "" {
		return errors.New("the database password is required")
	}
	dsn := d.DSN()
	if _, err := (&setupHost{}).CheckDatabase(ctx, dsn); err != nil {
		return err
	}
	dst, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := conversion.PrepareTarget(ctx, dst, version, false); err != nil {
		return err
	}

	counts, err := src.RowCounts(ctx)
	if err != nil {
		return err
	}
	blobRoot := filepath.Join(cfg.DataDir, "blobs")
	local, err := conversion.LocalBlobs(blobRoot)
	if err != nil {
		return err
	}
	printCounts(out, counts, local)
	fmt.Fprintf(out, "target: PostgreSQL %s/%s as %s (schema %d), blobs in %s; this host becomes controller %s\n", f.dbHost, f.dbName, f.dbUser, version, remote.Describe(), f.host)
	if f.dryRun {
		fmt.Fprintln(out, "dry run: every check passed; nothing was written")
		return nil
	}

	if err := conversion.PrepareTarget(ctx, dst, version, true); err != nil {
		return err
	}
	tables, err := conversion.CopyDatabase(ctx, src, dst, nil)
	if err != nil {
		return err
	}
	summary := reportCopy(out, tables)
	blobs, err := conversion.PushBlobs(ctx, blobRoot, remote, nil)
	if err != nil {
		return fmt.Errorf("%w; the database was copied, so drop and recreate it before converting again", err)
	}
	summary += fmt.Sprintf(", %d blobs (%d bytes, %d uploaded) copied and verified", blobs.Blobs, blobs.Bytes, blobs.Copied)
	fmt.Fprintf(out, "verified %d blobs in %s (%d uploaded)\n", blobs.Blobs, remote.Describe(), blobs.Copied)
	if err := conversion.Audit(ctx, dst, f.host, "single server to cluster", summary); err != nil {
		return err
	}
	src.Close()
	dst.Close()

	backup, err := backupDir()
	if err != nil {
		return err
	}
	if err := conversion.Snapshot(configDir, filepath.Join(backup, "etc-samuori")); err != nil {
		return err
	}
	for _, u := range singleServerUnits {
		if raw, err := os.ReadFile(filepath.Join(unitDir, u)); err == nil {
			if err := os.WriteFile(filepath.Join(backup, u), raw, 0o644); err != nil {
				return err
			}
		}
	}
	clusterPKI := filepath.Join(cfg.DataDir, "cluster-pki")
	if _, err := os.Stat(clusterPKI); err == nil {
		if err := os.Rename(clusterPKI, filepath.Join(backup, "cluster-pki")); err != nil {
			return err
		}
	}
	args := []string{"--release", f.release, "--role", "controller", "--host", f.host, "--confirm-dedicated-vm", "--keep-local-worker", "init", "--key-file", keyFile,
		"--worker-hostnames", strings.Join(mergeHosts([]string{"localhost", "127.0.0.1"}, cfg.WorkerHostnames), ",")}
	if u := or(f.externalURL, cfg.ExternalURL); u != "" {
		args = append(args, "--external-url", u)
	}
	args = append(args, storageArgs(target)...)
	if cfg.Backup.RecipientsFile != "" {
		args = append(args, "--backup-recipients-file", cfg.Backup.RecipientsFile)
	}
	_ = systemctl("disable", "--now", "samuori-server.service")
	env := append(withoutEnv(os.Environ(), "SAMUORI_DATABASE"), "SAMUORI_DATABASE="+dsn)
	if err := runInstaller(ctx, f.release, args, env, out); err != nil {
		for _, u := range clusterUnits {
			_ = systemctl("disable", "--now", u)
		}
		restoreErr := conversion.Restore(filepath.Join(backup, "etc-samuori"), configDir)
		_ = os.RemoveAll(clusterPKI)
		if _, serr := os.Stat(filepath.Join(backup, "cluster-pki")); serr == nil {
			restoreErr = errors.Join(restoreErr, os.Rename(filepath.Join(backup, "cluster-pki"), clusterPKI))
		}
		for _, u := range singleServerUnits {
			if raw, rerr := os.ReadFile(filepath.Join(backup, u)); rerr == nil {
				restoreErr = errors.Join(restoreErr, os.WriteFile(filepath.Join(unitDir, u), raw, 0o644))
			}
		}
		_ = systemctl("daemon-reload")
		for _, u := range singleServerUnits {
			if _, serr := os.Stat(filepath.Join(backup, u)); serr == nil {
				restoreErr = errors.Join(restoreErr, systemctl("enable", "--now", u))
			}
		}
		if restoreErr != nil {
			return fmt.Errorf("%w; restoring the single server also failed (%v): copy %s back to /etc/samuori and run sudo systemctl enable --now samuori-server", err, restoreErr, filepath.Join(backup, "etc-samuori"))
		}
		return fmt.Errorf("%w; this host runs as the single server again with its data unchanged. The PostgreSQL database and %s now hold a copy: drop and recreate the database before converting again", err, remote.Describe())
	}
	fmt.Fprintf(out, "\nConverted: %s is the first controller of a cluster (%s). Its local worker stays enrolled and updates with it.\n"+
		"The old configuration is kept in %s; the SQLite database and local blobs in %s are no longer used and can be\n"+
		"removed once the cluster is backed up. Add controllers under Administration > Servers.\n", f.host, summary, backup, cfg.DataDir)
	return nil
}

func convertToSingle(ctx context.Context, f convertFlags, out io.Writer) error {
	if !f.dedicatedVM {
		return errors.New("a single server builds uploaded source on this host, which must be a VM that runs nothing else (ADR 0027); confirm with --confirm-dedicated-vm")
	}
	if _, err := os.Stat(f.envFile); err == nil {
		if err := loadEnvFile(f.envFile); err != nil {
			return err
		}
	}
	cfg, origins, _, err := loadConfig([]string{"--config", f.configFile})
	if err != nil {
		return err
	}
	if !cfg.Cluster || cfg.Role != config.RoleController {
		return errors.New("this host is not a clustered controller")
	}
	if err := checkDropped(out, f, origins, toSingleCarried); err != nil {
		return err
	}
	if f.host == "" {
		f.host = controllerIdentity(cfg).Host
	}
	if f.host == "" {
		f.host, _ = os.Hostname()
	}
	key, err := readKey(cfg.ClusterKeyFile)
	if err != nil {
		return fmt.Errorf("the cluster key: %w", err)
	}
	keyFile := filepath.Join(cfg.DataDir, "keys", "secrets.key")
	if have, err := os.ReadFile(keyFile); err == nil && !bytes.Equal(have, key) {
		return fmt.Errorf("%s holds another key than the cluster key; move it away first", keyFile)
	}
	sqlitePath := filepath.Join(cfg.DataDir, "samuori.db")
	if _, err := os.Stat(sqlitePath); err == nil {
		return fmt.Errorf("%s already exists; move it away first: the conversion writes a fresh SQLite database", sqlitePath)
	}

	src, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := requireStopped(ctx, src); err != nil {
		return err
	}
	self, _ := os.Hostname()
	if err := conversion.SoleController(ctx, src, self); err != nil {
		return err
	}
	version, err := conversion.CheckSource(ctx, src)
	if err != nil {
		return err
	}
	remotes, err := openRemotes(ctx, cfg)
	if err != nil {
		return err
	}
	blobRoot := filepath.Join(cfg.DataDir, "blobs")
	var total conversion.BlobReport
	for _, r := range remotes {
		if err := r.Check(ctx); err != nil {
			return fmt.Errorf("%s: %w", r.Describe(), err)
		}
		c, err := conversion.RemoteBlobs(ctx, r, blobRoot)
		if err != nil {
			return fmt.Errorf("listing %s: %w", r.Describe(), err)
		}
		total.Blobs, total.Bytes, total.Missing = total.Blobs+c.Blobs, total.Bytes+c.Bytes, total.Missing+c.Missing
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(cfg.DataDir, &st); err == nil {
		if free := int64(st.Bavail) * int64(st.Bsize); free < total.Missing {
			return fmt.Errorf("%s has %d bytes free; the blobs not yet on this host need %d", cfg.DataDir, free, total.Missing)
		}
	}
	counts, err := src.RowCounts(ctx)
	if err != nil {
		return err
	}
	printCounts(out, counts, total)
	fmt.Fprintf(out, "target: SQLite %s (schema %d), blobs in %s (%d bytes to download); this host becomes a single server\n", sqlitePath, version, blobRoot, total.Missing)
	if f.dryRun {
		fmt.Fprintln(out, "dry run: every check passed; nothing was written")
		return nil
	}

	own := func(p string) error { return chownLikeDir(p, cfg.DataDir) }
	tmp := sqlitePath + ".converting"
	for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
		_ = os.Remove(p)
	}
	summary, err := copyToSQLite(ctx, src, tmp, f.host, out)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	svc, err := secrets.NewServiceWithKey(src, cfg.ClusterKeyFile)
	if err != nil {
		return err
	}
	pkiDir := filepath.Join(cfg.DataDir, "pki")
	shared, err := cluster.NewSharedDir(src, "pki", pkiDir, "topology-convert", svc.Derive("cluster-material"))
	if err != nil {
		return err
	}
	if _, err := shared.Pull(ctx); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("reading the internal CA from the cluster: %w", err)
	}
	if err := walkOwn(pkiDir, own); err != nil {
		return err
	}
	var blobs conversion.BlobReport
	for _, r := range remotes {
		b, err := conversion.PullBlobs(ctx, r, blobRoot, own, nil)
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		fmt.Fprintf(out, "verified %d blobs from %s (%d downloaded)\n", b.Blobs, r.Describe(), b.Copied)
		blobs.Blobs, blobs.Bytes, blobs.Copied = blobs.Blobs+b.Blobs, blobs.Bytes+b.Bytes, blobs.Copied+b.Copied
	}
	summary += fmt.Sprintf(", %d blobs (%d bytes, %d downloaded) copied and verified", blobs.Blobs, blobs.Bytes, blobs.Copied)
	src.Close()

	keyWritten := false
	if _, err := os.Stat(keyFile); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			return err
		}
		if err := errors.Join(own(filepath.Dir(keyFile)), own(keyFile)); err != nil {
			return err
		}
		keyWritten = true
	}
	backup, err := backupDir()
	if err != nil {
		return err
	}
	if err := conversion.Snapshot(configDir, filepath.Join(backup, "etc-samuori")); err != nil {
		return err
	}
	if err := os.Rename(tmp, sqlitePath); err != nil {
		return err
	}
	for _, u := range clusterUnits {
		_ = systemctl("disable", "--now", u)
	}
	for _, name := range []string{"cluster.key", "env", "api.crt", "api.key", "cluster-ca.crt", "nats-client.crt", "nats-client.key", "nats",
		"s3-credentials", "oci-credentials", "database-ca", "messaging-ca", "messaging-credentials", "messaging-cert", "messaging-key"} {
		_ = os.RemoveAll(filepath.Join(configDir, name))
	}
	args := []string{"--release", f.release, "--confirm-dedicated-vm", "--worker-listen", "0.0.0.0:8444",
		"--worker-hostnames", strings.Join(mergeHosts([]string{"localhost", "127.0.0.1", f.host}, cfg.WorkerHostnames), ",")}
	if u := or(f.externalURL, cfg.ExternalURL); u != "" {
		args = append(args, "--external-url", u)
	}
	if err := runInstaller(ctx, f.release, args, withoutEnv(os.Environ(), "SAMUORI_DATABASE"), out); err != nil {
		_ = systemctl("disable", "--now", "samuori-server.service")
		failed := sqlitePath + ".failed-" + filepath.Base(backup)
		restoreErr := errors.Join(os.Rename(sqlitePath, failed), conversion.Restore(filepath.Join(backup, "etc-samuori"), configDir))
		if keyWritten {
			_ = os.Remove(keyFile)
		}
		_ = systemctl("daemon-reload")
		for _, u := range clusterUnits {
			if _, serr := os.Stat(filepath.Join(configDir, "nats", "nats.conf")); serr != nil && u != "samuori-controller.service" {
				continue
			}
			restoreErr = errors.Join(restoreErr, systemctl("enable", "--now", u))
		}
		if restoreErr != nil {
			return fmt.Errorf("%w; restoring the controller also failed (%v): copy %s back to /etc/samuori and start samuori-nats and samuori-controller", err, restoreErr, filepath.Join(backup, "etc-samuori"))
		}
		return fmt.Errorf("%w; this host runs as the controller again, on the unchanged PostgreSQL database; the SQLite copy is kept as %s", err, failed)
	}
	fmt.Fprintf(out, "\nConverted: %s is a single server (%s). Separate workers stay enrolled and reach it at %s:8444.\n"+
		"The old configuration, which holds the database password, is kept in %s (root only); delete it once the single\n"+
		"server is backed up. The PostgreSQL database and the shared blob storage are no longer used by this host.\n", f.host, summary, f.host, backup)
	return nil
}

// copyToSQLite writes a fresh SQLite database at path from src, verifies it and audits the conversion there. The
// file leaves WAL mode before closing so it is complete on its own.
func copyToSQLite(ctx context.Context, src *db.DB, path, host string, out io.Writer) (string, error) {
	dst, err := db.Open(ctx, "sqlite://"+path)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	version, err := src.SchemaVersion(ctx)
	if err != nil {
		return "", err
	}
	if err := conversion.PrepareTarget(ctx, dst, version, true); err != nil {
		return "", err
	}
	tables, err := conversion.CopyDatabase(ctx, src, dst, nil)
	if err != nil {
		return "", err
	}
	summary := reportCopy(out, tables)
	if err := conversion.Audit(ctx, dst, host, "cluster to single server", summary); err != nil {
		return "", err
	}
	for _, q := range []string{`PRAGMA wal_checkpoint(TRUNCATE)`, `PRAGMA journal_mode=DELETE`} {
		if _, err := dst.SQL.ExecContext(ctx, q); err != nil {
			return "", err
		}
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	return summary, chownLikeDir(path, filepath.Dir(path))
}

func walkOwn(dir string, own func(string) error) error {
	return filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return own(p)
	})
}

func storageArgs(c config.Config) []string {
	var a []string
	add := func(flag, v string) {
		if v != "" {
			a = append(a, flag, v)
		}
	}
	if c.S3Bucket != "" {
		add("--s3-bucket", c.S3Bucket)
		add("--s3-region", c.S3Region)
		add("--s3-prefix", c.S3Prefix)
		add("--s3-endpoint", c.S3Endpoint)
		add("--s3-auth-method", c.S3AuthMethod)
		add("--s3-role-arn", c.S3RoleARN)
		add("--s3-external-id", c.S3ExternalID)
		add("--s3-credentials-file", c.S3CredentialsFile)
		if c.S3PathStyle {
			a = append(a, "--s3-path-style")
		}
		return a
	}
	add("--oci-repository", c.OCIRepository)
	add("--oci-credentials-file", c.OCICredentialsFile)
	if c.OCIAllowPrivate {
		a = append(a, "--oci-allow-private-network="+strconv.FormatBool(true))
	}
	return a
}

func mergeHosts(base, more []string) []string {
	out := append([]string{}, base...)
	for _, h := range more {
		if h = strings.TrimSpace(h); h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
