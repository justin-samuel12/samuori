// Package setupcli is the first-node setup behind `sudo ./samuori-setup` (ADR 0045, docs/operators/all-in-one.md).
package setupcli

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/topology"
)

const (
	Cluster      = "cluster"
	SingleServer = "single-server"

	DefaultCAFile = "/etc/samuori/rds-ca.pem"
)

type Answers struct {
	Deployment     string   `yaml:"deployment"`
	Controllers    int      `yaml:"controllers"`
	Workers        int      `yaml:"workers"`
	DedicatedVM    bool     `yaml:"dedicatedVm"`
	Host           string   `yaml:"host"`
	PortalURL      string   `yaml:"portalUrl"`
	Database       Database `yaml:"database"`
	S3             S3       `yaml:"s3"`
	Organization   string   `yaml:"organization"`
	Admin          Admin    `yaml:"admin"`
	ReleaseSigners string   `yaml:"releaseSigners"`
}

type Database struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	CAFile   string `yaml:"caFile"`
	// Replicated is the administrator's declaration, such as RDS Multi-AZ; Samuori cannot measure it (ADR 0046).
	Replicated bool `yaml:"replicated"`
}

type S3 struct {
	Bucket string `yaml:"bucket"`
	Region string `yaml:"region"`
	Prefix string `yaml:"prefix"`
}

type Admin struct {
	Username string `yaml:"username"`
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
}

// DSN is the PostgreSQL connection string; it carries the password, so it is never printed.
func (d Database) DSN() string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(d.User, d.Password), Host: net.JoinHostPort(d.Host, strconv.Itoa(d.Port)), Path: "/" + d.Name}
	u.RawQuery = url.Values{"sslmode": {"verify-full"}, "sslrootcert": {d.CAFile}}.Encode()
	return u.String()
}

func (a Answers) secrets() []string { return []string{a.Database.Password, a.Admin.Password} }

// LoadAnswers reads an answers file, which may hold passwords: it must be a regular file owned by uid with no
// group or other access.
func LoadAnswers(path string, uid uint32) (Answers, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Answers{}, err
	}
	if err := private(fi, uid); err != nil {
		return Answers{}, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Answers{}, err
	}
	var a Answers
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return Answers{}, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

func private(fi os.FileInfo, uid uint32) error {
	if !fi.Mode().IsRegular() {
		return errors.New("the answers file must be a regular file")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uid {
		return fmt.Errorf("the answers file must be owned by uid %d (root)", uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the answers file has mode %04o; it may hold passwords, so make it 0600", fi.Mode().Perm())
	}
	return nil
}

// Plan is the target topology these answers describe (ADR 0046).
func (a Answers) Plan() topology.Plan {
	p := topology.Plan{Deployment: a.Deployment, Controllers: a.Controllers, Workers: a.Workers, DatabaseReplicated: a.Database.Replicated}
	if a.Deployment == SingleServer {
		p.Controllers, p.Workers, p.DatabaseReplicated = 1, 1, false
	}
	return p
}

// Assessment is the tier the plan reaches with the storage and portal URL answered.
func (a Answers) Assessment() topology.Assessment {
	storage := topology.Local
	if a.S3.Bucket != "" {
		storage = topology.S3
	}
	host := ""
	if u, err := url.Parse(a.PortalURL); err == nil {
		host = u.Hostname()
	}
	onController := host == "" || strings.EqualFold(host, a.Host) || host == "localhost" || host == "127.0.0.1" || host == "::1"
	return topology.Assess(a.Plan().Facts(storage, onController))
}
