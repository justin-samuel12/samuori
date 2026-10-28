// Package ldap implements identity.Directory with go-ldap: a service bind, an escaped user search, a bind as the user
// to check the password, and a group lookup.
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	goldap "github.com/go-ldap/ldap/v3"

	"samuori.invalid/samuori/internal/identity"
)

type Client struct {
	Timeout time.Duration
}

func New() *Client { return &Client{Timeout: 10 * time.Second} }

var errCredentials = errors.New("invalid credentials")

// UserFilter substitutes the escaped username into the configured filter (SEC-AUTH-06).
func UserFilter(c *identity.LDAPConfig, username string) string {
	return strings.Replace(c.UserFilter, "{username}", goldap.EscapeFilter(username), 1)
}

// GroupFilter substitutes the escaped user DN into the configured group filter.
func GroupFilter(c *identity.LDAPConfig, dn string) string {
	return strings.ReplaceAll(c.GroupFilter, "{dn}", goldap.EscapeFilter(dn))
}

// connect tries each configured server in order and returns the first that answers.
func (c *Client) connect(ctx context.Context, cfg *identity.LDAPConfig) (*goldap.Conn, error) {
	var errs []error
	for _, raw := range cfg.URLs() {
		conn, err := c.dial(ctx, cfg, raw)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, errors.New("no server URL is configured")
	}
	return nil, errors.Join(errs...)
}

func (c *Client) dial(ctx context.Context, cfg *identity.LDAPConfig, raw string) (*goldap.Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if cfg.CACertPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, errors.New("the CA certificate is not valid PEM")
		}
		tc.RootCAs = pool
	}
	deadline := c.Timeout
	if d, ok := ctx.Deadline(); ok && time.Until(d) < deadline {
		deadline = time.Until(d)
	}
	conn, err := goldap.DialURL(raw, goldap.DialWithTLSConfig(tc), goldap.DialWithDialer(&net.Dialer{Timeout: deadline}))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", u.Host, err)
	}
	conn.SetTimeout(deadline)
	if cfg.StartTLS && u.Scheme == "ldap" {
		if err := conn.StartTLS(tc); err != nil {
			conn.Close()
			return nil, fmt.Errorf("StartTLS with %s: %w", u.Host, err)
		}
	}
	return conn, nil
}

func (c *Client) serviceBind(ctx context.Context, cfg *identity.LDAPConfig, bindPassword string) (*goldap.Conn, error) {
	conn, err := c.connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if bindPassword == "" {
		conn.Close()
		return nil, errors.New("the bind password is empty")
	}
	if err := conn.Bind(cfg.BindDN, bindPassword); err != nil {
		conn.Close()
		return nil, fmt.Errorf("the service account bind failed: %w", err)
	}
	return conn, nil
}

func (c *Client) Authenticate(ctx context.Context, p identity.Provider, bindPassword, username, password string) (identity.Claims, error) {
	cfg := p.LDAP
	if password == "" {
		return identity.Claims{}, errCredentials
	}
	conn, err := c.serviceBind(ctx, cfg, bindPassword)
	if err != nil {
		return identity.Claims{}, err
	}
	defer conn.Close()
	attrs := []string{cfg.UsernameAttribute, cfg.DisplayNameAttribute, "memberOf", "entryUUID", "objectGUID"}
	res, err := conn.Search(goldap.NewSearchRequest(cfg.UserBaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases, 2, 10, false, UserFilter(cfg, username), attrs, nil))
	if err != nil && !goldap.IsErrorWithCode(err, goldap.LDAPResultSizeLimitExceeded) {
		return identity.Claims{}, fmt.Errorf("the user search failed: %w", err)
	}
	if res == nil || len(res.Entries) == 0 {
		return identity.Claims{}, fmt.Errorf("%w: no user matched %s under %s", errCredentials, UserFilter(cfg, username), cfg.UserBaseDN)
	}
	if len(res.Entries) > 1 {
		return identity.Claims{}, fmt.Errorf("%w: more than one user matched %s under %s", errCredentials, UserFilter(cfg, username), cfg.UserBaseDN)
	}
	e := res.Entries[0]
	if err := conn.Bind(e.DN, password); err != nil {
		return identity.Claims{}, errCredentials
	}
	claims := identity.Claims{Subject: subject(e), Username: e.GetAttributeValue(cfg.UsernameAttribute), DisplayName: e.GetAttributeValue(cfg.DisplayNameAttribute)}
	for _, dn := range e.GetAttributeValues("memberOf") {
		claims.Groups = append(claims.Groups, dn)
		if cn := firstRDN(dn); cn != "" {
			claims.Groups = append(claims.Groups, cn)
		}
	}
	if cfg.GroupFilter != "" {
		if err := conn.Bind(cfg.BindDN, bindPassword); err != nil {
			return identity.Claims{}, fmt.Errorf("re-binding the service account: %w", err)
		}
		g, err := conn.Search(goldap.NewSearchRequest(cfg.GroupBaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases, 500, 10, false, GroupFilter(cfg, e.DN), []string{cfg.GroupNameAttribute}, nil))
		if err != nil {
			return identity.Claims{}, fmt.Errorf("the group search failed: %w", err)
		}
		for _, ge := range g.Entries {
			if n := ge.GetAttributeValue(cfg.GroupNameAttribute); n != "" {
				claims.Groups = append(claims.Groups, n)
			}
		}
	}
	return claims, nil
}

func (c *Client) Check(ctx context.Context, p identity.Provider, bindPassword string) error {
	conn, err := c.serviceBind(ctx, p.LDAP, bindPassword)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Search(goldap.NewSearchRequest(p.LDAP.UserBaseDN, goldap.ScopeBaseObject, goldap.NeverDerefAliases, 1, 10, false, "(objectClass=*)", []string{"dn"}, nil)); err != nil {
		return fmt.Errorf("the user base DN is not readable: %w", err)
	}
	return nil
}

// subject prefers the directory's immutable entry ID, so a renamed entry stays the same Samuori user.
func subject(e *goldap.Entry) string {
	if v := e.GetAttributeValue("entryUUID"); v != "" {
		return "entryUUID:" + v
	}
	if v := e.GetRawAttributeValue("objectGUID"); len(v) > 0 {
		return "objectGUID:" + hex.EncodeToString(v)
	}
	return "dn:" + strings.ToLower(e.DN)
}

func firstRDN(dn string) string {
	parsed, err := goldap.ParseDN(dn)
	if err != nil || len(parsed.RDNs) == 0 || len(parsed.RDNs[0].Attributes) == 0 {
		return ""
	}
	return parsed.RDNs[0].Attributes[0].Value
}
