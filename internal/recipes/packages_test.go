package recipes

import (
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/recipes/catalog"
)

func TestDistro(t *testing.T) {
	pin := "@sha256:" + strings.Repeat("a", 64)
	for image, want := range map[string]string{
		"docker.io/library/node:24-bookworm-slim" + pin:               "debian",
		"docker.io/library/python:3.13-slim-trixie" + pin:             "debian",
		"docker.io/nginxinc/nginx-unprivileged:1.29-alpine" + pin:     "alpine",
		"docker.io/library/alpine:3.22" + pin:                         "alpine",
		"mcr.microsoft.com/powershell:7.5-ubuntu-24.04" + pin:         "ubuntu",
		"docker.io/library/ubuntu:noble" + pin:                        "ubuntu",
		"mcr.microsoft.com/dotnet/sdk:8.0" + pin:                      "",
		"docker.io/library/node:24" + pin:                             "",
		"registry.example/alpine-tools/debian:12" + pin:               "debian",
		"registry.example/team/builder:alpine-to-bookworm" + pin:      "",
		"registry.example/debian-mirror/alpine-tools:1" + pin:         "alpine",
		"registry.example/alpinefan/image:1" + pin:                    "",
		"registry.example/team/node:22-bookworm-and-ubuntu-mix" + pin: "",
	} {
		if got := Distro(image, ""); got != want {
			t.Errorf("Distro(%s) = %q, want %q", image, got, want)
		}
	}
	if got := Distro("mcr.microsoft.com/dotnet/sdk:8.0"+pin, "debian"); got != "debian" {
		t.Errorf("a declared distribution is not used: %q", got)
	}
	if got := Distro("mcr.microsoft.com/dotnet/sdk:8.0"+pin, "fedora"); got != "" {
		t.Errorf("an unsupported declared distribution is used: %q", got)
	}
}

// TestSystemPackagesNeedAKnownManager: each list needs a known package manager on its own image only.
func TestSystemPackagesNeedAKnownManager(t *testing.T) {
	pin := "@sha256:" + strings.Repeat("a", 64)
	rt := Runtime{Runtime: catalog.Runtime{BuildImage: "docker.io/library/node:24" + pin, RunImage: "docker.io/library/node:24-alpine" + pin}}
	if _, presets, problems := rt.SystemPackages("worker", &buildspec.SystemPackages{Runtime: []string{"curl"}}); len(problems) != 0 || len(presets) != 1 || presets[0] != "alpine" {
		t.Fatalf("runtime packages on an Alpine run image = %v %v", presets, problems)
	}
	_, _, problems := rt.SystemPackages("worker", &buildspec.SystemPackages{Build: []string{"git"}})
	if len(problems) != 1 || problems[0].Path != "/spec/build/recipe/systemPackages/build" || !strings.Contains(problems[0].Message, "node:24@") {
		t.Fatalf("build packages on an image of unknown distribution = %v", problems)
	}
	if sp, presets, problems := rt.SystemPackages("worker", &buildspec.SystemPackages{}); sp != nil || presets != nil || problems != nil {
		t.Fatal("empty lists produced a plan entry")
	}
}
