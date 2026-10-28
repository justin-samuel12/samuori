//go:build linux

package cloudhypervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/egress/proxy"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/vmguest"
)

// handshakeListener plays Cloud Hypervisor's side of a hybrid vsock connection to the agent port.
type handshakeListener struct{ net.Listener }

func (l handshakeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	line, err := readLine(c)
	if err != nil || line != fmt.Sprintf("CONNECT %d", vmguest.AgentPort) {
		c.Close()
		return nil, fmt.Errorf("bad handshake %q", line)
	}
	_, err = c.Write([]byte("OK 1\n"))
	return c, err
}

func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for {
		if _, err := c.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
}

// guestHost dials one of the host's listeners the way Cloud Hypervisor forwards a guest connection.
func guestHost(socket string, port uint32) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("%s_%d", socket, port))
	}}}
}

type fakeReply struct {
	result  vmguest.RunResult
	entries []tar.Header
	bodies  [][]byte
}

func ociArchive(t *testing.T, content []byte) []byte {
	t.Helper()
	digest := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	var lt bytes.Buffer
	tw := tar.NewWriter(&lt)
	_ = tw.WriteHeader(&tar.Header{Name: "app/config", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(content)
	_ = tw.Close()
	var gzb bytes.Buffer
	zw := gzip.NewWriter(&gzb)
	_, _ = zw.Write(lt.Bytes())
	_ = zw.Close()
	layer := gzb.Bytes()
	cfg := []byte(`{"os":"linux","architecture":"arm64","rootfs":{"type":"layers","diff_ids":["` + digest(lt.Bytes()) + `"]}}`)
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(cfg), "size": len(cfg)},
		"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": digest(layer), "size": len(layer)}}})
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []map[string]any{{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest(manifest), "size": len(manifest)}}})
	var out bytes.Buffer
	ow := tar.NewWriter(&out)
	for name, b := range map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": index,
		"blobs/sha256/" + digest(cfg)[7:]: cfg, "blobs/sha256/" + digest(manifest)[7:]: manifest, "blobs/sha256/" + digest(layer)[7:]: layer} {
		_ = ow.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		_, _ = ow.Write(b)
	}
	_ = ow.Close()
	return out.Bytes()
}

// fakeVM starts a provider holding one environment whose "guest" is handler, served on the hybrid vsock socket.
func fakeVM(t *testing.T, handler func(socket string, w http.ResponseWriter, r *http.Request), spec isolation.Spec) (*Provider, isolation.Env) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "samuori-ch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := &Provider{cfg: Config{Dir: dir, Proxy: proxy.New(proxy.Options{}), Mirrors: map[string]string{"docker.io": "192.0.2.10:5000"}, MaxDiskBytes: 1 << 30},
		envs: map[string]*held{}, mirrors: []vmguest.Mirror{{Upstream: "docker.io", Index: 0}}}
	vdir := filepath.Join(dir, "vm-1")
	if err := os.Mkdir(vdir, 0o700); err != nil {
		t.Fatal(err)
	}
	v := &vm{id: "vm-1", dir: vdir, socket: filepath.Join(vdir, "vsock.sock"), limits: isolation.Limits{DiskBytes: 1 << 20}, exited: make(chan struct{}), handlers: &callbacks{}}
	if err := v.listen(p); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", v.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(v.socket, w, r) })}
	go func() { _ = srv.Serve(handshakeListener{l}) }()
	t.Cleanup(func() { srv.Close(); v.kill() })
	spec.Profile = isolation.VMPerJob
	if spec.SourceDir == "" {
		spec.SourceDir = t.TempDir()
	}
	if spec.OutputDir == "" {
		spec.OutputDir = t.TempDir()
	}
	e := isolation.Env{ID: v.id, AttemptID: spec.AttemptID, Profile: isolation.VMPerJob, Deadline: time.Now().Add(time.Minute)}
	p.envs[spec.AttemptID] = &held{vm: v, spec: spec, env: e}
	return p, e
}

func reply(w http.ResponseWriter, res vmguest.RunResult, entries ...any) {
	tw := tar.NewWriter(w)
	raw, _ := json.Marshal(res)
	_ = vmguest.WriteBytes(tw, vmguest.EntryResult, raw)
	for i := 0; i+1 < len(entries); i += 2 {
		_ = vmguest.WriteBytes(tw, entries[i].(string), entries[i+1].([]byte))
	}
	_ = tw.Close()
}

func sha(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

var secretPlan, _ = buildplan.Plan{Version: 1, Method: "dockerfile", Dockerfile: &buildplan.PlanDockerfile{Context: ".", Path: "Dockerfile"},
	Secrets: []buildplan.PlanSecret{{ID: "npmrc", BindingID: "b1"}}, Egress: buildplan.PlanEgress{Allow: []buildplan.EgressRule{}}}.Encode()

func TestHostTreatsGuestOutputAsUntrusted(t *testing.T) {
	t.Run("SEC-ISO-11", func(t *testing.T) {
		const secret = "s3cr3t-value-for-the-canary"
		release := func(context.Context, string) ([]byte, error) { return []byte(secret), nil }
		t.Run("released values are masked and found by the host's canary", func(t *testing.T) {
			image := ociArchive(t, []byte("token="+secret))
			p, e := fakeVM(t, func(socket string, w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				resp, err := guestHost(socket, vmguest.CallbackPort).Post("http://host/v1/secret", "application/json", strings.NewReader(`{"id":"npmrc"}`))
				if err != nil {
					t.Error(err)
					return
				}
				got, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				canary := isolation.Check{ID: isolation.CheckCanary, Outcome: "passed", Reason: "forged by the guest"}
				reply(w, vmguest.RunResult{Logs: "echo " + string(got), ArtifactDigest: sha(image), Checks: []isolation.Check{canary}}, vmguest.EntryImage, image)
			}, isolation.Spec{AttemptID: "a1", ReleaseSecret: release})
			res, err := p.Run(context.Background(), e, secretPlan)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(res.Logs, []byte(secret)) {
				t.Fatal("a released value reached the logs")
			}
			if len(res.Checks) != 1 || res.Checks[0].ID != isolation.CheckCanary || res.Checks[0].Outcome != "blocked" {
				t.Fatalf("checks = %+v", res.Checks)
			}
		})
		for name, c := range map[string]struct {
			handler func(w http.ResponseWriter)
			class   error
		}{
			"digest mismatch": {func(w http.ResponseWriter) {
				reply(w, vmguest.RunResult{ArtifactDigest: sha([]byte("other"))}, vmguest.EntryImage, []byte("image"))
			}, isolation.ErrInfrastructure},
			"path in entry name": {func(w http.ResponseWriter) {
				reply(w, vmguest.RunResult{ArtifactDigest: sha([]byte("x"))}, "../../escape", []byte("x"))
			}, isolation.ErrInfrastructure},
			"oversized image": {func(w http.ResponseWriter) {
				big := make([]byte, 2<<20)
				reply(w, vmguest.RunResult{ArtifactDigest: sha(big)}, vmguest.EntryImage, big)
			}, isolation.ErrInfrastructure},
			"duplicate image": {func(w http.ResponseWriter) {
				reply(w, vmguest.RunResult{ArtifactDigest: sha([]byte("a"))}, vmguest.EntryImage, []byte("a"), vmguest.EntryImage, []byte("b"))
			}, isolation.ErrInfrastructure},
			"no image": {func(w http.ResponseWriter) { reply(w, vmguest.RunResult{}) }, isolation.ErrInfrastructure},
			"guest limit": {func(w http.ResponseWriter) {
				reply(w, vmguest.RunResult{Class: vmguest.ClassResourceLimit, Detail: "the build exceeded its disk limit"})
			}, isolation.ErrResourceLimit},
		} {
			t.Run(name, func(t *testing.T) {
				out := t.TempDir()
				p, e := fakeVM(t, func(_ string, w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					c.handler(w)
				}, isolation.Spec{AttemptID: "a1", OutputDir: out})
				plan, _ := buildplan.Plan{Version: 1, Method: "dockerfile", Egress: buildplan.PlanEgress{Allow: []buildplan.EgressRule{}}}.Encode()
				if _, err := p.Run(context.Background(), e, plan); !errors.Is(err, c.class) {
					t.Fatalf("Run = %v, want %v", err, c.class)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(out), "escape")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("a guest entry was written outside the output directory")
				}
			})
		}
	})
}

func TestCallbacksServeOnlyTheAssignedAttempt(t *testing.T) {
	t.Run("SEC-ISO-11", func(t *testing.T) {
		var tokens []string
		spec := isolation.Spec{AttemptID: "a1",
			ReleaseSecret: func(context.Context, string) ([]byte, error) { return []byte("v"), nil },
			MirrorToken: func(_ context.Context, mirror string, _ []string) (string, int64, time.Time, error) {
				tokens = append(tokens, mirror)
				return "t", 60, time.Now(), nil
			}}
		p, _ := fakeVM(t, func(string, http.ResponseWriter, *http.Request) {}, spec)
		h := p.envs["a1"]
		c := guestHost(h.vm.socket, vmguest.CallbackPort)
		post := func(path, body string) int {
			resp, err := c.Post("http://host"+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		if got := post("/v1/secret", `{"id":"npmrc"}`); got != http.StatusForbidden {
			t.Fatalf("an unassigned VM got a secret: %d", got)
		}
		h.vm.handlers.assign(&h.spec, nil)
		if got := post("/v1/secret", `{"id":"npmrc"}`); got != http.StatusOK {
			t.Fatalf("assigned secret: %d", got)
		}
		if got := post("/v1/mirror-token", `{"mirror":"10.88.0.1:5000","scopes":["repository:library/node:pull"]}`); got != http.StatusOK || len(tokens) != 1 || tokens[0] != "192.0.2.10:5000" {
			t.Fatalf("mirror token: %d %v", got, tokens)
		}
		if got := post("/v1/mirror-token", `{"mirror":"203.0.113.1:443","scopes":[]}`); got != http.StatusForbidden {
			t.Fatalf("a token for a destination that is not a relay: %d", got)
		}
		h.vm.handlers.assign(nil, nil)
		if got := post("/v1/secret", `{"id":"npmrc"}`); got != http.StatusForbidden {
			t.Fatalf("a released VM got a secret: %d", got)
		}
	})
}
