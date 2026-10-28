// Command secretprobe is the OD-15 spike client. See docs/spikes/buildkit-secrets.md.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/tonistiigi/fsutil"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type store struct {
	mu        sync.Mutex
	tag       string
	start     time.Time
	values    map[string][]byte
	denyAfter map[string]int
	calls     map[string]int
	onFirst   func(id string)
}

func (s *store) GetSecret(ctx context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[id]++
	n := s.calls[id]
	v, ok := s.values[id]
	switch {
	case !ok:
		s.logf("GetSecret id=%s call=%d -> not found", id, n)
		return nil, secrets.ErrNotFound
	case s.denyAfter[id] > 0 && n > s.denyAfter[id]:
		s.logf("GetSecret id=%s call=%d -> denied", id, n)
		return nil, fmt.Errorf("release denied for %s", id)
	}
	s.logf("GetSecret id=%s call=%d -> released sha256:%s", id, n, digest(v))
	if n == 1 && s.onFirst != nil {
		go s.onFirst(id)
	}
	return v, nil
}

func (s *store) logf(format string, args ...any) {
	fmt.Printf("[%7.3fs] %s %s\n", time.Since(s.start).Seconds(), s.tag, fmt.Sprintf(format, args...))
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

func main() {
	addr := flag.String("addr", "", "buildkitd address")
	dir := flag.String("context", ".", "build context directory")
	file := flag.String("file", "Dockerfile", "Dockerfile name inside the context")
	tag := flag.String("tag", "build", "log tag")
	noCache := flag.Bool("no-cache", false, "disable cache")
	ociOut := flag.String("oci", "", "export OCI tarball to this path")
	localOut := flag.String("local", "", "export final filesystem to this directory")
	cacheOut := flag.String("cache-export", "", "export cache to this directory")
	cacheIn := flag.String("cache-import", "", "import cache from this directory")
	provenance := flag.Bool("provenance", false, "request max-mode provenance")
	cancelAfter := flag.Duration("cancel-after-first-secret", 0, "cancel the solve this long after the first secret release")
	var secretFlags, denyFlags, argFlags multiFlag
	flag.Var(&secretFlags, "secret", "id=path (repeatable)")
	flag.Var(&denyFlags, "deny-after", "id=n: deny releases after n calls (repeatable)")
	flag.Var(&argFlags, "build-arg", "name=value (repeatable)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := &store{tag: *tag, start: time.Now(), values: map[string][]byte{}, denyAfter: map[string]int{}, calls: map[string]int{}}
	for _, s := range secretFlags {
		id, path, _ := strings.Cut(s, "=")
		b, err := os.ReadFile(path)
		must(err)
		st.values[id] = b
	}
	for _, d := range denyFlags {
		id, n, _ := strings.Cut(d, "=")
		v, err := strconv.Atoi(n)
		must(err)
		st.denyAfter[id] = v
	}
	if *cancelAfter > 0 {
		var once sync.Once
		st.onFirst = func(string) {
			once.Do(func() {
				time.Sleep(*cancelAfter)
				st.logf("cancelling solve")
				cancel()
			})
		}
	}

	c, err := client.New(ctx, *addr)
	must(err)
	defer c.Close()

	fs, err := fsutil.NewFS(*dir)
	must(err)
	attrs := map[string]string{"filename": *file}
	if *noCache {
		attrs["no-cache"] = ""
	}
	if *provenance {
		attrs["attest:provenance"] = "mode=max"
	}
	for _, a := range argFlags {
		k, v, _ := strings.Cut(a, "=")
		attrs["build-arg:"+k] = v
	}
	opt := client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalMounts:   map[string]fsutil.FS{"context": fs, "dockerfile": fs},
		Session:       []session.Attachable{secretsprovider.NewSecretProvider(st)},
	}
	switch {
	case *ociOut != "":
		opt.Exports = []client.ExportEntry{{Type: client.ExporterOCI, Output: func(map[string]string) (io.WriteCloser, error) { return os.Create(*ociOut) }}}
	case *localOut != "":
		opt.Exports = []client.ExportEntry{{Type: client.ExporterLocal, OutputDir: *localOut}}
	}
	if *cacheIn != "" {
		opt.CacheImports = []client.CacheOptionsEntry{{Type: "local", Attrs: map[string]string{"src": *cacheIn}}}
	}
	if *cacheOut != "" {
		opt.CacheExports = []client.CacheOptionsEntry{{Type: "local", Attrs: map[string]string{"dest": *cacheOut, "mode": "max"}}}
	}

	ch := make(chan *client.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := map[string]bool{}
		for s := range ch {
			for _, v := range s.Vertexes {
				key := v.Digest.String()
				switch {
				case v.Cached && !seen[key+"c"]:
					seen[key+"c"] = true
					st.logf("vertex CACHED   %s", v.Name)
				case v.Started != nil && !seen[key+"s"]:
					seen[key+"s"] = true
					st.logf("vertex started  %s", v.Name)
				}
				if v.Completed != nil && !seen[key+"d"] {
					seen[key+"d"] = true
					st.logf("vertex done     %s %s", v.Name, v.Error)
				}
			}
			for _, l := range s.Logs {
				for _, line := range strings.Split(strings.TrimRight(string(l.Data), "\n"), "\n") {
					st.logf("log | %s", line)
				}
			}
		}
	}()
	_, err = c.Solve(ctx, nil, opt, ch)
	<-done
	for id, n := range st.calls {
		st.logf("summary id=%s calls=%d", id, n)
	}
	if err != nil {
		st.logf("solve error: %v", err)
		os.Exit(2)
	}
	st.logf("solve ok")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
