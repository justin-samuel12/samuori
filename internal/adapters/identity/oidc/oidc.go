// Package oidc implements identity.OIDCClient with coreos/go-oidc: discovery, the authorization code flow with
// PKCE, and ID token verification of signature, issuer, audience, expiry and nonce.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"samuori.invalid/samuori/internal/identity"
)

type Client struct {
	HTTP *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	p  *gooidc.Provider
	at time.Time
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second}, cache: map[string]cached{}}
}

// provider fetches discovery, which go-oidc rejects when the document names a different issuer. Results are cached
// for an hour; the key set refreshes itself when a token names an unknown key.
func (c *Client) provider(ctx context.Context, issuer string) (*gooidc.Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[issuer]; ok && time.Since(e.at) < time.Hour {
		return e.p, nil
	}
	p, err := gooidc.NewProvider(gooidc.ClientContext(ctx, c.HTTP), issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery at %s: %w", issuer, err)
	}
	c.cache[issuer] = cached{p, time.Now()}
	return p, nil
}

func (c *Client) config(prov *gooidc.Provider, p identity.Provider, secret, redirect string) oauth2.Config {
	return oauth2.Config{ClientID: p.OIDC.ClientID, ClientSecret: secret, Endpoint: prov.Endpoint(), RedirectURL: redirect, Scopes: p.OIDC.Scopes}
}

func (c *Client) AuthCodeURL(ctx context.Context, p identity.Provider, redirect, state, nonce, challenge string) (string, error) {
	prov, err := c.provider(ctx, p.OIDC.Issuer)
	if err != nil {
		return "", err
	}
	cfg := c.config(prov, p, "", redirect)
	return cfg.AuthCodeURL(state, gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", challenge), oauth2.SetAuthURLParam("code_challenge_method", "S256")), nil
}

func (c *Client) Exchange(ctx context.Context, p identity.Provider, secret, redirect, code, verifier, nonce string) (identity.Claims, error) {
	if code == "" {
		return identity.Claims{}, errors.New("the callback carried no authorization code")
	}
	prov, err := c.provider(ctx, p.OIDC.Issuer)
	if err != nil {
		return identity.Claims{}, err
	}
	ctx = gooidc.ClientContext(ctx, c.HTTP)
	cfg := c.config(prov, p, secret, redirect)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return identity.Claims{}, fmt.Errorf("the token exchange failed: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return identity.Claims{}, errors.New("the provider returned no ID token")
	}
	idt, err := prov.Verifier(&gooidc.Config{ClientID: p.OIDC.ClientID}).Verify(ctx, raw)
	if err != nil {
		return identity.Claims{}, fmt.Errorf("the ID token does not verify: %w", err)
	}
	if idt.Nonce != nonce {
		return identity.Claims{}, errors.New("the ID token nonce does not match this sign-in")
	}
	var claims map[string]json.RawMessage
	if err := idt.Claims(&claims); err != nil {
		return identity.Claims{}, err
	}
	out := identity.Claims{Subject: idt.Subject, Username: str(claims[p.OIDC.UsernameClaim]), DisplayName: str(claims[p.OIDC.DisplayNameClaim]), Groups: list(claims[p.OIDC.GroupsClaim])}
	return out, nil
}

func (c *Client) Check(ctx context.Context, p identity.Provider) error {
	c.mu.Lock()
	delete(c.cache, p.OIDC.Issuer)
	c.mu.Unlock()
	_, err := c.provider(ctx, p.OIDC.Issuer)
	return err
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func list(raw json.RawMessage) []string {
	var l []string
	if json.Unmarshal(raw, &l) == nil {
		return l
	}
	if s := str(raw); s != "" {
		return []string{s}
	}
	return nil
}
