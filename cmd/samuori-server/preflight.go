package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"samuori.invalid/samuori/internal/config"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/messaging"
)

// preflight checks what a controller needs before it starts, with real sign-ins rather than open ports: the data
// directory, key and certificate files, a PostgreSQL sign-in and schema state, NATS with JetStream, and a write,
// read and delete on shared storage. It changes nothing but a probe object it removes.
func preflight(args []string, out io.Writer) error {
	cfg, _, _, err := loadConfig(args)
	if err != nil {
		return err
	}
	failed := 0
	var ctx context.Context
	check := func(name string, fn func() error) {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ctx = c
		if err := fn(); err != nil {
			failed++
			fmt.Fprintf(out, "FAIL  %-16s %v\n", name, err)
			return
		}
		fmt.Fprintf(out, "ok    %s\n", name)
	}
	check("data directory", func() error { return writable(cfg.DataDir) })
	if cfg.TLSCertFile != "" {
		check("TLS certificate", func() error {
			_, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
			return err
		})
	}
	if cfg.Cluster {
		check("cluster key", func() error {
			v, err := config.ReadPrivateFile(cfg.ClusterKeyFile)
			if err == nil && len(v) != 32 {
				err = errors.New("the cluster key must be 32 bytes")
			}
			return err
		})
	}
	check("database", func() error {
		dsn := cfg.Database
		if dsn == "" {
			dsn = "sqlite://" + filepath.Join(cfg.DataDir, "samuori.db")
		}
		d, err := db.Open(ctx, dsn)
		if err != nil {
			return err
		}
		defer d.Close()
		if err := d.Ping(ctx); err != nil {
			return fmt.Errorf("signing in failed: %w", err)
		}
		applied, known, err := d.Schema(ctx)
		if err != nil {
			return fmt.Errorf("reading the schema: %w", err)
		}
		if applied > known {
			return fmt.Errorf("the schema (version %d) is newer than this release (%d)", applied, known)
		}
		return nil
	})
	if m := cfg.Messaging; len(m.URLs) > 0 {
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := messaging.Probe(pctx, messaging.External{URLs: m.URLs, CredentialsFile: m.CredentialsFile, CAFile: m.CAFile, CertFile: m.CertFile, KeyFile: m.KeyFile, Name: "samuori-preflight"})
		cancel()
		switch {
		case errors.Is(err, messaging.ErrNoJetStream):
			fmt.Fprintf(out, "WARN  %-16s %v (expected until a second controller joins a new cluster)\n", "messaging", err)
		default:
			check("messaging", func() error { return err })
		}
	}
	octx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	remotes, err := openRemotes(octx, cfg)
	if err != nil {
		failed++
		fmt.Fprintf(out, "FAIL  %-16s %v\n", "shared storage", err)
	}
	for name, r := range remotes {
		check("storage ("+name+")", func() error {
			if err := r.Check(ctx); err != nil {
				return err
			}
			body := make([]byte, 64)
			_, _ = rand.Read(body)
			sum := sha256.Sum256(body)
			key := "sha256/" + hex.EncodeToString(sum[:])
			if err := r.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
				return fmt.Errorf("writing: %w", err)
			}
			var got bytes.Buffer
			if err := r.Get(ctx, key, &got); err != nil || !bytes.Equal(got.Bytes(), body) {
				return errors.Join(errors.New("reading back what was written failed"), err)
			}
			return r.Delete(ctx, key)
		})
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	mode := string(cfg.Role)
	if cfg.Cluster {
		mode += ", clustered"
	}
	fmt.Fprintf(out, "all checks passed (%s)\n", mode)
	return nil
}
