package clusterjoin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Bundle is everything a controller needs to serve as a member of an existing cluster. It is sent once, over TLS
// pinned to the issuing controller, to the holder of a join token, and holds the cluster key and credentials: it is
// never logged or stored by the issuer.
type Bundle struct {
	Host            string   `json:"host"`
	ClusterKey      []byte   `json:"clusterKey"`
	DatabaseDSN     string   `json:"databaseDsn"`
	ExternalURL     string   `json:"externalUrl,omitempty"`
	WorkerHostnames []string `json:"workerHostnames"`
	NATSURLs        []string `json:"natsUrls"`
	NATSReplicas    int      `json:"natsReplicas"`
	// ExternalNATS means the controllers share a NATS cluster run elsewhere: no member runs on the controller, and
	// its CA and client credentials travel in Files (messaging-*).
	ExternalNATS bool              `json:"externalNats,omitempty"`
	Storage      map[string]string `json:"storage"`
	// Files are credential and trust files the settings refer to, by name: s3-credentials, oci-credentials,
	// backup-recipients and, for an external NATS cluster, messaging-ca, messaging-credentials, messaging-cert and
	// messaging-key.
	Files      map[string][]byte `json:"files"`
	BackupDir  string            `json:"backupDir,omitempty"`
	ClusterCA  []byte            `json:"clusterCa"`
	NATSServer Pair              `json:"natsServer"`
	NATSClient Pair              `json:"natsClient"`
	API        Pair              `json:"api"`
}

// NATSURL is the client URL of a NATS member on host.
func NATSURL(host string) string { return "tls://" + net.JoinHostPort(host, "4222") }

// ForHost completes a bundle for host: its own certificates and its NATS member in the URL list.
func (b Bundle) ForHost(ca *Authority, host string) (Bundle, error) {
	var err error
	b.Host = host
	if b.API, err = ca.Issue(API, host); err != nil {
		return b, err
	}
	b.ClusterCA = ca.CertPEM()
	if !contains(b.WorkerHostnames, host) {
		b.WorkerHostnames = append(append([]string{}, b.WorkerHostnames...), host)
	}
	if b.ExternalNATS {
		return b, nil
	}
	if b.NATSServer, err = ca.Issue(NATSServer, host); err != nil {
		return b, err
	}
	if b.NATSClient, err = ca.Issue(NATSClient, host); err != nil {
		return b, err
	}
	own := NATSURL(host)
	urls := []string{own}
	for _, u := range b.NATSURLs {
		if u != own {
			urls = append(urls, u)
		}
	}
	b.NATSURLs = urls
	return b, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Layout is where a controller keeps its configuration (the installer's defaults).
type Layout struct {
	ConfigDir string // /etc/samuori
	DataDir   string // /var/lib/samuori
	Owner     string // samuori: owns the controller's files
	NATSOwner string // nats: owns the NATS member's files
	UIDir     string
}

// Install writes server.yml, the environment file with the database DSN, the key and certificate files and, unless
// NATS is external, the NATS member's configuration. Secret files are mode 0600 and owned by the service that reads them.
func (b Bundle) Install(l Layout, bootstrap string) error {
	if len(b.ClusterKey) != 32 || b.DatabaseDSN == "" || b.Host == "" {
		return errors.New("clusterjoin: the bundle is incomplete")
	}
	samuori, err := lookup(l.Owner)
	if err != nil {
		return err
	}
	nats, err := lookup(l.NATSOwner)
	if err != nil {
		return err
	}
	natsDir := filepath.Join(l.ConfigDir, "nats")
	if err := mkdirMode(l.ConfigDir, 0o755); err != nil {
		return err
	}
	type file struct {
		path  string
		data  []byte
		owner owners
		mode  os.FileMode
	}
	files := []file{
		{filepath.Join(l.ConfigDir, "cluster.key"), b.ClusterKey, samuori, 0o600},
		{filepath.Join(l.ConfigDir, "env"), []byte("SAMUORI_DATABASE=" + b.DatabaseDSN + "\n"), samuori, 0o600},
		{filepath.Join(l.ConfigDir, "cluster-ca.crt"), b.ClusterCA, samuori, 0o644},
		{filepath.Join(l.ConfigDir, "api.crt"), b.API.Cert, samuori, 0o644},
		{filepath.Join(l.ConfigDir, "api.key"), b.API.Key, samuori, 0o600},
		{filepath.Join(l.ConfigDir, "server.yml"), []byte(b.serverConfig(l, bootstrap)), samuori, 0o640},
	}
	if !b.ExternalNATS {
		if err := mkdirMode(natsDir, 0o755); err != nil {
			return err
		}
		files = append(files,
			file{filepath.Join(l.ConfigDir, "nats-client.crt"), b.NATSClient.Cert, samuori, 0o644},
			file{filepath.Join(l.ConfigDir, "nats-client.key"), b.NATSClient.Key, samuori, 0o600},
			file{filepath.Join(natsDir, "ca.crt"), b.ClusterCA, nats, 0o644},
			file{filepath.Join(natsDir, "server.crt"), b.NATSServer.Cert, nats, 0o644},
			file{filepath.Join(natsDir, "server.key"), b.NATSServer.Key, nats, 0o600},
			file{filepath.Join(natsDir, "nats.conf"), []byte(b.natsConfig(natsDir)), nats, 0o644})
	}
	for name, data := range b.Files {
		if !fileNames[name] {
			return fmt.Errorf("clusterjoin: unexpected file %q in the bundle", name)
		}
		files = append(files, file{filepath.Join(l.ConfigDir, name), data, samuori, 0o600})
	}
	for _, f := range files {
		if err := writeOwned(f.path, f.data, f.mode, f.owner); err != nil {
			return err
		}
	}
	if !b.ExternalNATS {
		if _, err := EnsureSystemAccount(natsDir, l.NATSOwner); err != nil {
			return err
		}
	}
	return nil
}

var fileNames = map[string]bool{"s3-credentials": true, "oci-credentials": true, "backup-recipients": true, "database-ca": true,
	"messaging-ca": true, "messaging-credentials": true, "messaging-cert": true, "messaging-key": true}

type owners struct{ uid, gid int }

func lookup(name string) (owners, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return owners{}, fmt.Errorf("clusterjoin: user %s: %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return owners{uid, gid}, nil
}

func writeOwned(path string, data []byte, mode os.FileMode, o owners) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Chown(tmp, o.uid, o.gid); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// natsConfig routes to every known member, this one included: NATS ignores a route to itself, but a clustered
// JetStream member with no routes at all refuses to start, which the first member would otherwise have. Route
// verification starts off; the member's route sync fills the routes and then turns it on (routes.go).
func (b Bundle) natsConfig(dir string) string {
	var routes []string
	for _, u := range b.NATSURLs {
		pu, err := url.Parse(u)
		if err != nil {
			continue
		}
		routes = append(routes, "nats-route://"+net.JoinHostPort(pu.Hostname(), "6222"))
	}
	sort.Strings(routes)
	tls := fmt.Sprintf("tls { cert_file: %q, key_file: %q, ca_file: %q, verify: true }", filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), filepath.Join(dir, "ca.crt"))
	// Members advertise their names, which their certificates carry, instead of the IP addresses NATS gossips
	// by default; routes to gossiped IPs would fail certificate verification.
	return fmt.Sprintf(`server_name: %q
listen: 0.0.0.0:4222
client_advertise: %q
%s
jetstream { store_dir: "/var/lib/nats" }
cluster {
  name: samuori
  listen: 0.0.0.0:6222
  advertise: %q
  routes: [%s]
  %s
}
%s
`, b.Host, net.JoinHostPort(b.Host, "4222"), tls, net.JoinHostPort(b.Host, "6222"), strings.Join(routes, ", "), tls, systemInclude)
}

func (b Bundle) serverConfig(l Layout, bootstrap string) string {
	var s strings.Builder
	q := strconv.Quote
	fmt.Fprintf(&s, "role: controller\nlisten: 0.0.0.0:8443\ndataDir: %s\n", l.DataDir)
	if b.ExternalURL != "" {
		fmt.Fprintf(&s, "externalUrl: %s\n", q(b.ExternalURL))
	}
	fmt.Fprintf(&s, "log:\n  format: json\ntls:\n  certFile: %s\n  keyFile: %s\n", filepath.Join(l.ConfigDir, "api.crt"), filepath.Join(l.ConfigDir, "api.key"))
	if l.UIDir != "" {
		fmt.Fprintf(&s, "ui:\n  dir: %s\n", l.UIDir)
	}
	hosts := make([]string, len(b.WorkerHostnames))
	for i, h := range b.WorkerHostnames {
		hosts[i] = q(h)
	}
	fmt.Fprintf(&s, "worker:\n  listen: 0.0.0.0:8444\n  hostnames: [%s]\n", strings.Join(hosts, ", "))
	fmt.Fprintf(&s, "cluster:\n  enabled: \"true\"\n  keyFile: %s\n", filepath.Join(l.ConfigDir, "cluster.key"))
	fmt.Fprintf(&s, "messaging:\n  url: %s\n  replicas: \"%d\"\n", q(strings.Join(b.NATSURLs, ",")), max(b.NATSReplicas, 1))
	if b.ExternalNATS {
		for _, f := range []struct{ field, name string }{{"caFile", "messaging-ca"}, {"credentialsFile", "messaging-credentials"}, {"certFile", "messaging-cert"}, {"keyFile", "messaging-key"}} {
			if _, ok := b.Files[f.name]; ok {
				fmt.Fprintf(&s, "  %s: %s\n", f.field, filepath.Join(l.ConfigDir, f.name))
			}
		}
	} else {
		fmt.Fprintf(&s, "  caFile: %s\n  certFile: %s\n  keyFile: %s\n", filepath.Join(l.ConfigDir, "cluster-ca.crt"),
			filepath.Join(l.ConfigDir, "nats-client.crt"), filepath.Join(l.ConfigDir, "nats-client.key"))
	}
	storage := map[string]map[string]string{}
	set := func(sec, field, v string) {
		if storage[sec] == nil {
			storage[sec] = map[string]string{}
		}
		storage[sec][field] = v
	}
	for k, v := range b.Storage {
		sec, field, _ := strings.Cut(k, ".")
		set(sec, field, v)
	}
	if _, ok := b.Files["s3-credentials"]; ok {
		set("s3", "credentialsFile", filepath.Join(l.ConfigDir, "s3-credentials"))
	}
	if _, ok := b.Files["oci-credentials"]; ok {
		set("oci", "credentialsFile", filepath.Join(l.ConfigDir, "oci-credentials"))
	}
	if len(storage) > 0 {
		s.WriteString("storage:\n")
		for _, sec := range sortedKeys(storage) {
			fmt.Fprintf(&s, "  %s:\n", sec)
			for _, field := range sortedKeys(storage[sec]) {
				fmt.Fprintf(&s, "    %s: %s\n", field, q(storage[sec][field]))
			}
		}
	}
	if _, ok := b.Files["backup-recipients"]; ok {
		dir := b.BackupDir
		if dir == "" {
			dir = filepath.Join(l.DataDir, "backups")
		}
		fmt.Fprintf(&s, "backup:\n  dir: %s\n  recipientsFile: %s\n", dir, filepath.Join(l.ConfigDir, "backup-recipients"))
	}
	s.WriteString(bootstrap)
	return s.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mkdirMode sets the mode even when a restrictive umask (027, 077) is in force, so the nats user can still read its
// configuration.
func mkdirMode(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	return os.Chmod(dir, mode)
}

// sslRootCert is the CA file a PostgreSQL DSN names, if any.
func sslRootCert(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return ""
	}
	return u.Query().Get("sslrootcert")
}

// UseBundledDatabaseCA writes the database CA the issuing controller sent and points the DSN at it, so a joining
// host needs no CA copied by hand. The CA is public; it arrived over the pinned connection.
func (b *Bundle) UseBundledDatabaseCA(configDir string) error {
	ca, ok := b.Files["database-ca"]
	if !ok || sslRootCert(b.DatabaseDSN) == "" {
		return nil
	}
	if err := mkdirMode(configDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(configDir, "database-ca")
	if err := os.WriteFile(path, ca, 0o644); err != nil {
		return err
	}
	u, err := url.Parse(b.DatabaseDSN)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("sslrootcert", path)
	u.RawQuery = q.Encode()
	b.DatabaseDSN = u.String()
	return nil
}
