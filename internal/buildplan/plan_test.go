package buildplan

import (
	"strings"
	"testing"
)

// TestContextName follows BuildKit v0.34.0: resolveBaseImage normalizes the FROM name with its tag, and
// dockerui.Client.NamedContext looks it up in familiar form without ":latest".
func TestContextName(t *testing.T) {
	for ref, want := range map[string]string{
		"node:18-alpine":                             "node:18-alpine",
		"docker.io/library/node:18-alpine":           "node:18-alpine",
		"library/node:18-alpine":                     "node:18-alpine",
		"node":                                       "node",
		"node:latest":                                "node",
		"docker.io/bitnami/redis:7":                  "bitnami/redis:7",
		"ghcr.io/example/tool:1.2":                   "ghcr.io/example/tool:1.2",
		"localhost:5000/app":                         "localhost:5000/app",
		"mcr.microsoft.com/mssql/server:2022-latest": "mcr.microsoft.com/mssql/server:2022-latest",
	} {
		if got, err := ContextName(ref); err != nil || got != want {
			t.Errorf("ContextName(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := ContextName("Node:18"); err == nil {
		t.Error("an invalid reference was accepted")
	}
}

func TestPinsChangeThePlanDigest(t *testing.T) {
	plan := func(digest string) Plan {
		return Plan{Version: 1, Method: "dockerfile", Dockerfile: &PlanDockerfile{Context: ".", Path: "Dockerfile",
			BaseImagePins: []BaseImagePin{{Ref: "node:18-alpine", Image: "docker.io/library/node@sha256:" + strings.Repeat(digest, 64)}}}}
	}
	_, a := plan("a").Encode()
	_, again := plan("a").Encode()
	_, b := plan("b").Encode()
	unpinned := plan("a")
	unpinned.Dockerfile.BaseImagePins = nil
	_, none := unpinned.Encode()
	if a != again || a == b || a == none {
		t.Errorf("plan digests: %s %s %s %s", a, again, b, none)
	}
}
