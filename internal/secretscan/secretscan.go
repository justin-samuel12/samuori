// Package secretscan recognises high-signal credential formats. Matching is best effort: absence of a match
// never proves content is free of secrets. Callers report the rule, never the matched text.
package secretscan

import "regexp"

type Rule struct {
	Code    string
	Message string
	re      *regexp.Regexp
}

var Rules = []Rule{
	{"secret.private-key", "Looks like a private key. Remove it and use a secret reference instead.", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`)},
	{"secret.cloud-access-key", "Looks like an AWS access key ID. Remove it and use a secret reference instead.", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"secret.token", "Looks like an access token (GitHub, GitLab, Slack or npm). Remove it and use a secret reference instead.", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|glpat-[A-Za-z0-9_-]{20,}|xox[abpors]-[A-Za-z0-9-]{10,}|npm_[A-Za-z0-9]{36})\b`)},
	{"secret.url-credentials", "Contains a URL with an embedded password. Remove it and use a secret reference instead.", regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]+:[^\s/@]+@`)},
}

// Match returns the rules that match b.
func Match(b []byte) []Rule {
	var out []Rule
	for _, r := range Rules {
		if r.re.Match(b) {
			out = append(out, r)
		}
	}
	return out
}

// Regexp exposes the rule's pattern for scanners that match other inputs.
func (r Rule) Regexp() *regexp.Regexp { return r.re }
