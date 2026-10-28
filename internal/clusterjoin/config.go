package clusterjoin

import (
	"bytes"
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"

	"samuori.invalid/samuori/internal/config"
)

var dnsName = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ValidHost accepts a DNS name or an IP address for a controller's certificates.
func ValidHost(h string) bool {
	return len(h) <= 253 && (net.ParseIP(h) != nil || dnsName.MatchString(h))
}

// MembersOnControllers reports whether the controllers run the cluster's NATS members, which the cluster CA signs,
// rather than sharing an external NATS cluster.
func MembersOnControllers(c config.Config, clusterCA []byte) bool {
	trusted, err := os.ReadFile(c.Messaging.CAFile)
	return c.Cluster && len(c.Messaging.URLs) > 0 && err == nil && len(bytes.TrimSpace(clusterCA)) > 0 && bytes.Contains(trusted, bytes.TrimSpace(clusterCA))
}

// FromConfig builds the part of a bundle every member shares from this controller's configuration. NATS whose
// servers the cluster CA does not sign is external: the joining controller gets this controller's NATS credentials
// instead of a member of its own.
func FromConfig(c config.Config, ca *Authority) (Bundle, error) {
	if !c.Cluster || len(c.Messaging.URLs) == 0 {
		return Bundle{}, errors.New("clusterjoin: this controller is not a cluster member")
	}
	external := !MembersOnControllers(c, ca.CertPEM())
	key, err := os.ReadFile(c.ClusterKeyFile)
	if err != nil {
		return Bundle{}, err
	}
	b := Bundle{ClusterKey: key, DatabaseDSN: c.Database, ExternalURL: c.ExternalURL, WorkerHostnames: c.WorkerHostnames, NATSURLs: c.Messaging.URLs,
		NATSReplicas: c.Messaging.Replicas, ExternalNATS: external, Storage: map[string]string{}, Files: map[string][]byte{}, BackupDir: c.Backup.Dir}
	put := func(k, v string) {
		if v != "" {
			b.Storage[k] = v
		}
	}
	if c.S3Bucket != "" {
		put("s3.bucket", c.S3Bucket)
		put("s3.prefix", c.S3Prefix)
		put("s3.region", c.S3Region)
		put("s3.endpoint", c.S3Endpoint)
		put("s3.pathStyle", strconv.FormatBool(c.S3PathStyle))
		put("s3.authMethod", c.S3AuthMethod)
		put("s3.roleArn", c.S3RoleARN)
		put("s3.externalId", c.S3ExternalID)
	}
	if c.OCIRepository != "" {
		put("oci.repository", c.OCIRepository)
		put("oci.plainHttp", strconv.FormatBool(c.OCIPlainHTTP))
		put("oci.allowPrivateNetwork", strconv.FormatBool(c.OCIAllowPrivate))
	}
	for class, backend := range c.StorageRoutes {
		put("routes."+class, backend)
	}
	files := map[string]string{"s3-credentials": c.S3CredentialsFile, "oci-credentials": c.OCICredentialsFile, "backup-recipients": c.Backup.RecipientsFile,
		"database-ca": sslRootCert(c.Database)}
	if external {
		m := c.Messaging
		files["messaging-ca"], files["messaging-credentials"], files["messaging-cert"], files["messaging-key"] = m.CAFile, m.CredentialsFile, m.CertFile, m.KeyFile
	}
	for name, path := range files {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Bundle{}, err
		}
		b.Files[name] = data
	}
	return b, nil
}
