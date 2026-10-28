// Package openbao reads KV v2 secrets from OpenBao, or from Vault through the same API (ADR 0015), over plain HTTP:
// no SDK is linked. Token and AppRole authentication are supported.
package openbao

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"samuori.invalid/samuori/internal/secrets"
)

type Client struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func New() *Client { return &Client{tokens: map[string]cachedToken{}} }

func httpClient(cfg secrets.ExternalConfig) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACertPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, errors.New("the CA certificate is not valid PEM")
		}
		tc.RootCAs = pool
	}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment},
		// Redirects would carry X-Vault-Token to wherever they point.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string {
	switch e.status {
	case http.StatusForbidden:
		return "permission denied" + e.msg
	case http.StatusNotFound:
		return "not found" + e.msg
	}
	return fmt.Sprintf("HTTP %d%s", e.status, e.msg)
}

func (c *Client) call(ctx context.Context, cfg secrets.ExternalConfig, token, method, path string, body any, out any) error {
	hc, err := httpClient(cfg)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.Address, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Request", "true")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", cfg.Namespace)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Errors []string `json:"errors"`
		}
		msg := ""
		if json.Unmarshal(raw, &e) == nil && len(e.Errors) > 0 {
			msg = ": " + strings.Join(e.Errors, "; ")
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return &apiError{resp.StatusCode, msg}
	}
	return json.Unmarshal(raw, out)
}

// token returns the credential for token auth, or logs in with AppRole and caches the client token until shortly
// before its lease ends.
func (c *Client) token(ctx context.Context, cfg secrets.ExternalConfig, credential []byte) (string, error) {
	if cfg.AuthMethod != "approle" {
		return strings.TrimSpace(string(credential)), nil
	}
	sum := sha256.Sum256(credential)
	key := cfg.Address + "|" + cfg.Namespace + "|" + cfg.RoleID + "|" + hex.EncodeToString(sum[:])
	c.mu.Lock()
	cached, ok := c.tokens[key]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.token, nil
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	// Pasted IDs often carry a trailing newline, which Vault rejects as an invalid role or secret ID.
	login := map[string]string{"role_id": strings.TrimSpace(cfg.RoleID)}
	if secretID := strings.TrimSpace(string(credential)); secretID != "" {
		login["secret_id"] = secretID
	}
	if err := c.call(ctx, cfg, "", http.MethodPost, "/v1/auth/approle/login", login, &out); err != nil {
		return "", fmt.Errorf("AppRole login: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("AppRole login returned no token")
	}
	ttl := time.Duration(out.Auth.LeaseDuration)*time.Second - 30*time.Second
	if ttl > 0 {
		c.mu.Lock()
		c.tokens[key] = cachedToken{out.Auth.ClientToken, time.Now().Add(ttl)}
		c.mu.Unlock()
	}
	return out.Auth.ClientToken, nil
}

func (c *Client) Read(ctx context.Context, _ string, cfg secrets.ExternalConfig, credential []byte, reference, version string) ([]byte, error) {
	path, field, hasField := strings.Cut(reference, "#")
	if path == "" || (hasField && field == "") {
		return nil, errors.New("the reference must be path or path#field")
	}
	token, err := c.token(ctx, cfg, credential)
	if err != nil {
		return nil, err
	}
	u := "/v1/" + strings.Trim(cfg.Mount, "/") + "/data/" + escapePath(path)
	if version != "" {
		u += "?version=" + url.QueryEscape(version)
	}
	var out struct {
		Data struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"data"`
	}
	if err := c.call(ctx, cfg, token, http.MethodGet, u, nil, &out); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if !hasField {
		// The whole secret, for reading its key names; only the controller sees this.
		return json.Marshal(out.Data.Data)
	}
	raw, ok := out.Data.Data[field]
	if !ok {
		return nil, fmt.Errorf("%s has no field %s", path, field)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []byte(s), nil
	}
	return raw, nil
}

func (c *Client) Check(ctx context.Context, _ string, cfg secrets.ExternalConfig, credential []byte) (string, error) {
	token, err := c.token(ctx, cfg, credential)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			DisplayName string   `json:"display_name"`
			Policies    []string `json:"policies"`
		} `json:"data"`
	}
	if err := c.call(ctx, cfg, token, http.MethodGet, "/v1/auth/token/lookup-self", nil, &out); err != nil {
		return "", fmt.Errorf("token lookup: %w", err)
	}
	return fmt.Sprintf("authenticated as %s with policies %s", out.Data.DisplayName, strings.Join(out.Data.Policies, ", ")), nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// transitRef splits mount/key#version.
func transitRef(ref string) (mount, key, version string, err error) {
	path, version, _ := strings.Cut(ref, "#")
	i := strings.LastIndex(path, "/")
	if i <= 0 || i == len(path)-1 {
		return "", "", "", errors.New("the key reference must be mount/key, such as transit/samuori-release")
	}
	return path[:i], path[i+1:], version, nil
}

// TransitKey returns a Transit key's type, latest version and that version's public key.
func (c *Client) TransitKey(ctx context.Context, cfg secrets.ExternalConfig, credential []byte, ref string) (typ string, version int, publicPEM string, err error) {
	mount, key, _, err := transitRef(ref)
	if err != nil {
		return "", 0, "", err
	}
	token, err := c.token(ctx, cfg, credential)
	if err != nil {
		return "", 0, "", err
	}
	var out struct {
		Data struct {
			Type          string `json:"type"`
			LatestVersion int    `json:"latest_version"`
			Keys          map[string]struct {
				PublicKey string `json:"public_key"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err := c.call(ctx, cfg, token, http.MethodGet, "/v1/"+escapePath(mount)+"/keys/"+url.PathEscape(key), nil, &out); err != nil {
		return "", 0, "", fmt.Errorf("reading the Transit key: %w", err)
	}
	v := out.Data.Keys[fmt.Sprint(out.Data.LatestVersion)]
	return out.Data.Type, out.Data.LatestVersion, v.PublicKey, nil
}

// TransitSign signs a SHA-256 digest with a pinned Transit key version and returns the ASN.1 signature.
func (c *Client) TransitSign(ctx context.Context, cfg secrets.ExternalConfig, credential []byte, ref string, digest []byte) ([]byte, error) {
	mount, key, version, err := transitRef(ref)
	if err != nil {
		return nil, err
	}
	token, err := c.token(ctx, cfg, credential)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"input": base64.StdEncoding.EncodeToString(digest), "prehashed": true, "marshaling_algorithm": "asn1"}
	if version != "" {
		var n int
		if _, err := fmt.Sscanf(version, "%d", &n); err != nil || n < 1 {
			return nil, errors.New("the pinned key version is not a number")
		}
		body["key_version"] = n
	}
	var out struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err := c.call(ctx, cfg, token, http.MethodPost, "/v1/"+escapePath(mount)+"/sign/"+url.PathEscape(key)+"/sha2-256", body, &out); err != nil {
		return nil, fmt.Errorf("Transit signing: %w", err)
	}
	parts := strings.SplitN(out.Data.Signature, ":", 3)
	if len(parts) != 3 || parts[0] != "vault" {
		return nil, errors.New("Transit returned an unexpected signature format")
	}
	return base64.StdEncoding.DecodeString(parts[2])
}
