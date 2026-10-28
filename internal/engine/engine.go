// Package engine holds what every build engine adapter shares: requests, results and the runner contract
// (D05, docs/architecture/build-execution.md#engines).
package engine

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"
)

// SecretSource answers a request for one secret. Returning ErrSecretNotFound leaves the mount empty, as for an
// unbound optional secret; any other error fails the step (docs/architecture/secrets.md).
type SecretSource func(ctx context.Context, id string) ([]byte, error)

var ErrSecretNotFound = errors.New("secret not found")

// MirrorToken asks the controller for a token scoped to the attempt's allowed repositories on one mirror.
type MirrorToken func(ctx context.Context, mirror string, scopes []string) (token string, expiresIn int64, issuedAt time.Time, err error)

type Request struct {
	// Progress, when set, receives the build output as it happens, besides the result's log.
	Progress      io.Writer
	ContextDir    string
	DockerfileDir string
	Filename      string
	// Frontend is a gateway frontend image pinned by digest that reads Filename from DockerfileDir instead of the
	// Dockerfile frontend; FrontendOpts are passed to it unchanged, and BuildArgs and Proxy are not (ADR 0047).
	Frontend     string
	FrontendOpts map[string]string
	Target       string
	BuildArgs    map[string]string
	Platforms    []string
	Secrets      SecretSource
	// MirrorHosts are authorizing mirrors that MirrorToken authenticates pulls to; without them every registry is
	// anonymous.
	MirrorHosts []string
	MirrorToken MirrorToken
	// Proxy is the egress proxy for RUN steps; each engine passes it so that it never enters image history. NoProxy
	// lists hosts the engine reaches directly, such as mirrors.
	Proxy     string
	NoProxy   []string
	NoCache   bool
	OutputTar string
	// OutputDir, when set, receives the final stage's filesystem instead of an image archive in OutputTar.
	OutputDir string
	// NamedContexts are local directories the Dockerfile reads as named build contexts (COPY --from=<name>).
	NamedContexts map[string]string
	// ImageContexts replace FROM images: each key is BuildKit's named-context name for a reference, and its value
	// the repository@sha256 image to build from instead (ADR 0049).
	ImageContexts map[string]string
	// CacheImportDir holds a cache layout to import; CacheExportDir receives this build's cache. Both are set only for
	// builds whose plan has a controller-assigned cache scope.
	CacheImportDir string
	CacheExportDir string
	// Labels go into the image config of a Dockerfile-frontend build, overriding LABEL instructions with the same key;
	// gateway frontends do not receive them. Annotations go onto every image manifest of an OCI archive, and onto
	// the index of a multi-platform build.
	Labels      map[string]string
	Annotations map[string]string
}

type Result struct {
	ImageDigest string
	Log         string
}

// TestRequest runs a generated Dockerfile against an image from an OCI layout, exposed to the Dockerfile as the
// named context "samuoriimage", with a local named context "samuoriprobe".
type TestRequest struct {
	DockerfileDir string
	Layout        string
	Manifest      string
	ProbeDir      string
	OutputDir     string
	Platform      string
}

// ErrBuild is an application build failure; the log explains it.
var ErrBuild = errors.New("the build failed")

// Runner is one engine instance with private state, used for a single build and then stopped.
type Runner interface {
	Build(ctx context.Context, r Request) (Result, error)
	Test(ctx context.Context, r TestRequest) (Result, error)
	OutOfMemory() bool
	Stop() error
}

const (
	BuildKit = "buildkit"
	Buildah  = "buildah"
)

// DeliversSecrets reports whether an engine delivers secrets just in time through a session (ADR 0029). Buildah
// reads secrets from files, a delivery model that needs its own decision first (OD-26).
func DeliversSecrets(name string) bool { return name == BuildKit }

// AuthorizesMirrors reports whether an engine fetches build-scoped mirror tokens (OD-14); others pull anonymously.
func AuthorizesMirrors(name string) bool { return name == BuildKit }

// Known lists the engines adapters exist for.
func Known(name string) bool { return slices.Contains([]string{BuildKit, Buildah}, name) }
