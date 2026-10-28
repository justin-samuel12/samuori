package imagescan

import (
	"bytes"
	"encoding/pem"
	"path"
	"regexp"
	"strings"

	"samuori.invalid/samuori/internal/secretscan"
)

// RulesVersion identifies the built-in rule set; validation results record it.
const RulesVersion = "1.2.1"

// Rule classes and their default outcomes (ADR 0031).
const (
	ClassBuildSecret       = "build-secret"
	ClassPrivateKey        = "private-key"
	ClassDatabaseSecrets   = "database-secrets"
	ClassDatabaseData      = "database-data"
	ClassCredentialFile    = "credential-file"
	ClassCredentialContent = "credential-content"
	ClassMetadata          = "metadata"

	RuleBuildSecret     = "build-secret-bytes"
	RuleMetadataEnv     = "metadata-env"
	RuleMetadataHistory = "metadata-build-arg"
	RuleMetadataLabel   = "metadata-label"
	RuleMetadataAnnot   = "metadata-annotation"
)

var DefaultOutcomes = map[string]string{
	ClassBuildSecret:       "block",
	ClassPrivateKey:        "block",
	ClassDatabaseSecrets:   "require-exception",
	ClassDatabaseData:      "warn",
	ClassCredentialFile:    "require-exception",
	ClassCredentialContent: "require-exception",
	ClassMetadata:          "require-exception",
}

type Rule struct {
	ID         string
	Class      string
	Paths      []string
	Extensions []string
	// Content, when set, must match the start of the file; with Paths or Extensions both must match.
	Content *regexp.Regexp
	// Verify, when set, must also accept the content, to drop matches that cannot be real secrets.
	Verify func(head []byte) bool
}

func (r Rule) matches(name string, head []byte) bool {
	pathRule := len(r.Paths) > 0 || len(r.Extensions) > 0
	ok := !pathRule
	for _, p := range r.Paths {
		if Glob(p, name) {
			ok = true
		}
	}
	lower := strings.ToLower(name)
	for _, e := range r.Extensions {
		if strings.HasSuffix(lower, e) {
			ok = true
		}
	}
	if !ok {
		return false
	}
	if r.Content != nil {
		return head != nil && r.Content.Match(head) && (r.Verify == nil || r.Verify(head))
	}
	return true
}

func secretRule(code string) *regexp.Regexp {
	for _, r := range secretscan.Rules {
		if r.Code == code {
			return r.Regexp()
		}
	}
	panic("imagescan: unknown secretscan rule " + code)
}

// DefaultRules is the curated path and content rule set.
func DefaultRules() []Rule {
	return []Rule{
		{ID: "private-key-pem", Class: ClassPrivateKey, Content: secretRule("secret.private-key"), Verify: pemPrivateKey},
		{ID: "keystore-file", Class: ClassPrivateKey, Extensions: []string{".pvk", ".pfx", ".p12", ".jks", ".keystore", ".bcfks"}},
		{ID: "mssql-secrets", Class: ClassDatabaseSecrets, Paths: []string{"var/opt/mssql/secrets/**"}},
		{ID: "postgres-auth", Class: ClassDatabaseSecrets, Paths: []string{"var/lib/postgresql/**/pg_hba.conf", "var/lib/postgresql/**/server.key"}},
		{ID: "mysql-keyring", Class: ClassDatabaseSecrets, Paths: []string{"var/lib/mysql-keyring/**"}},
		{ID: "mssql-data", Class: ClassDatabaseData, Paths: []string{"var/opt/mssql/data/**"}},
		{ID: "postgres-data", Class: ClassDatabaseData, Paths: []string{"var/lib/postgresql/data/**"}},
		{ID: "mysql-data", Class: ClassDatabaseData, Paths: []string{"var/lib/mysql/**"}},
		{ID: "dotenv", Class: ClassCredentialFile, Paths: []string{"**/.env", ".env"}},
		{ID: "cloud-credentials", Class: ClassCredentialFile, Paths: []string{"**/.aws/credentials", "**/.config/gcloud/credentials.db", "**/.azure/accessTokens.json"}},
		{ID: "registry-auth", Class: ClassCredentialFile, Paths: []string{"**/.docker/config.json"}, Content: regexp.MustCompile(`"auths?"`)},
		{ID: "kubeconfig", Class: ClassCredentialFile, Paths: []string{"**/.kube/config"}},
		{ID: "ssh-private-key", Class: ClassCredentialFile, Paths: []string{"**/.ssh/id_rsa", "**/.ssh/id_dsa", "**/.ssh/id_ecdsa", "**/.ssh/id_ed25519"}},
		{ID: "netrc", Class: ClassCredentialFile, Paths: []string{"**/.netrc", "**/.git-credentials", "**/.pypirc"}},
		{ID: "npm-token", Class: ClassCredentialFile, Paths: []string{"**/.npmrc", ".npmrc"}, Content: regexp.MustCompile(`_auth(Token)?\s*=`)},
		{ID: "cloud-access-key", Class: ClassCredentialContent, Content: secretRule("secret.cloud-access-key")},
		{ID: "access-token", Class: ClassCredentialContent, Content: secretRule("secret.token")},
	}
}

// Glob matches slash-separated names against a pattern where "**" spans any number of segments.
func Glob(pattern, name string) bool {
	return glob(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func glob(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return len(n) > 0
			}
			for i := 0; i <= len(n); i++ {
				if glob(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], n[0]); !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}

var pemHeader = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

// pemPrivateKey accepts a header only when a PEM block with at least 32 bytes of key material follows it, as real
// keys have and documentation placeholders do not. Keys embedded in JSON or source strings with escaped newlines
// are decoded too.
func pemPrivateKey(head []byte) bool {
	for _, loc := range pemHeader.FindAllIndex(head, -1) {
		rest := head[loc[0]:]
		for _, candidate := range [][]byte{rest, bytes.ReplaceAll(rest, []byte(`\n`), []byte("\n"))} {
			if b, _ := pem.Decode(candidate); b != nil && strings.HasSuffix(b.Type, "PRIVATE KEY") && len(b.Bytes) >= 32 {
				return true
			}
		}
	}
	return false
}
