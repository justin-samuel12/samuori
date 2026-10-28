package admission

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeTags map[string]string

func (f fakeTags) ResolveTag(_ context.Context, ref string) (string, error) {
	if d, ok := f[ref]; ok {
		return d, nil
	}
	return "", errors.New("the tag does not exist")
}

// TestPin: only FROM images that nothing else replaces are pinned, private sources and unresolvable tags are refused
// with a reason, and the impact preview never queries a registry (ADR 0049).
func TestPin(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	c := &Checker{tags: fakeTags{"node:18-alpine": digest, "ghcr.io/example/tool:1": digest}}
	from := fromImages{
		images:   map[string]bool{"node:18-alpine": true, "ghcr.io/example/tool:1": true, "python:3.12": true, "missing:1": true, "builder": true},
		contexts: map[string]bool{"redis:7": true},
		names:    map[string]bool{"python:3.12": true, "builder": true},
	}
	ctx := context.Background()

	for ref, want := range map[string]string{
		"node:18-alpine":         "docker.io/library/node@" + digest,
		"ghcr.io/example/tool:1": "ghcr.io/example/tool@" + digest,
	} {
		if pin, why := c.pin(ctx, ref, from, false, true); why != "" || pin.Ref != ref || pin.Image != want {
			t.Errorf("pin %s = %+v %q", ref, pin, why)
		}
	}
	for ref, reason := range map[string]string{
		"redis:7":     "only Dockerfile FROM images",
		"python:3.12": "would replace it",
		"builder":     "would replace it",
		"missing:1":   "could not resolve the tag",
	} {
		if pin, why := c.pin(ctx, ref, from, false, true); pin.Image != "" || !strings.Contains(why, reason) {
			t.Errorf("pin %s = %+v %q; want a refusal mentioning %q", ref, pin, why, reason)
		}
	}
	if _, why := c.pin(ctx, "node:18-alpine", from, true, true); !strings.Contains(why, "private image source") {
		t.Errorf("a private source tag = %q", why)
	}
	if pin, why := (&Checker{}).pin(ctx, "node:18-alpine", from, false, false); why != "" || pin.Image != "" {
		t.Errorf("impact preview = %+v %q; want pinnable without a lookup", pin, why)
	}
	if _, why := (&Checker{}).pin(ctx, "node:18-alpine", from, false, true); !strings.Contains(why, "cannot resolve tags") {
		t.Errorf("no resolver = %q", why)
	}
}
