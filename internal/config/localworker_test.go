package config

import (
	"strings"
	"testing"
)

func TestLocalWorkerOnlyWithItsHost(t *testing.T) {
	load := func(args ...string) error {
		_, _, err := Load(args, nil, func(string) ([]byte, error) { return nil, nil })
		return err
	}
	cluster := []string{"--role", "controller", "--cluster", "true", "--cluster-key-file", "/etc/samuori/cluster.key", "--database", "postgres://samuori@db.example/samuori",
		"--messaging-url", "nats://127.0.0.1:4222", "--s3-bucket", "b", "--s3-region", "r", "--local-worker-dir", "/var/lib/samuori-local"}
	if err := load(cluster...); err != nil {
		t.Fatalf("a cluster controller that keeps its host's worker was refused: %v", err)
	}
	if err := load("--local-worker-dir", "/var/lib/samuori-local"); err != nil {
		t.Fatalf("an all-in-one local worker was refused: %v", err)
	}
	if err := load("--role", "controller", "--local-worker-dir", "/var/lib/samuori-local"); err == nil || !strings.Contains(err.Error(), "localWorker.dir") {
		t.Errorf("a standalone controller took a local worker: %v", err)
	}
}
