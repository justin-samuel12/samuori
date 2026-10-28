package rootlesshost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"samuori.invalid/samuori/internal/buildplan"
)

// CheckRailpackPlan validates what railpack prepare wrote and returns the plan Samuori builds. Railpack can exit 0 with
// a near-empty plan, so the content decides, never the exit status (docs/spikes/railpack.md). The returned plan
// names only the approved, digest-pinned images and declares exactly secrets, the variables bound build secrets
// reach steps as.
func CheckRailpackPlan(planRaw, infoRaw []byte, rp *buildplan.PlanRailpack, secrets []string) ([]byte, error) {
	var info struct {
		Success *bool `json:"success"`
		Logs    []struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		} `json:"logs"`
	}
	if len(infoRaw) > 0 {
		if err := json.Unmarshal(infoRaw, &info); err != nil {
			return nil, errors.New("railpack wrote an unreadable railpack-info.json")
		}
	}
	if info.Success != nil && !*info.Success {
		var msgs []string
		for _, l := range info.Logs {
			if strings.EqualFold(l.Level, "error") {
				msgs = append(msgs, l.Msg)
			}
		}
		if len(msgs) == 0 {
			msgs = []string{"no reason given"}
		}
		return nil, errors.New("railpack could not plan this source: " + strings.Join(msgs, "; "))
	}
	if len(bytes.TrimSpace(planRaw)) == 0 {
		return nil, errors.New("railpack wrote no plan")
	}
	dec := json.NewDecoder(bytes.NewReader(planRaw))
	dec.UseNumber()
	var plan map[string]any
	if err := dec.Decode(&plan); err != nil || plan == nil {
		return nil, errors.New("railpack wrote an unreadable plan")
	}
	steps, _ := plan["steps"].([]any)
	if len(steps) == 0 {
		return nil, errors.New("the Railpack plan has no build steps")
	}
	names := map[string]bool{}
	commands := 0
	for i, raw := range steps {
		st, ok := raw.(map[string]any)
		name, _ := st["name"].(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("Railpack plan step %d has no name", i)
		}
		if names[name] {
			return nil, fmt.Errorf("the Railpack plan names step %q twice", name)
		}
		names[name] = true
		cmds, _ := st["commands"].([]any)
		commands += len(cmds)
	}
	if commands == 0 {
		return nil, errors.New("the Railpack plan runs no commands")
	}
	deploy, _ := plan["deploy"].(map[string]any)
	if deploy == nil {
		return nil, errors.New("the Railpack plan has no deploy section")
	}
	if start, _ := deploy["startCommand"].(string); strings.TrimSpace(start) == "" {
		return nil, errors.New("the Railpack plan has no start command; set spec.build.railpack.startCommand")
	}
	base, _ := deploy["base"].(map[string]any)
	if base == nil || (base["step"] == nil && base["image"] == nil) {
		return nil, errors.New("the Railpack plan has no base for the image")
	}
	var refErr error
	checkRef := func(layer map[string]any) {
		if s, ok := layer["step"].(string); ok && !names[s] && refErr == nil {
			refErr = fmt.Errorf("the Railpack plan refers to a missing step %q", s)
		}
	}
	approved := map[string]string{}
	for _, ref := range rp.Images {
		tagged, _, _ := strings.Cut(ref, "@")
		approved[tagged], approved[ref] = ref, ref
	}
	var imgErr error
	pin := func(m map[string]any) {
		img, ok := m["image"].(string)
		if !ok || imgErr != nil {
			return
		}
		ref, ok := approved[img]
		if !ok {
			imgErr = fmt.Errorf("the Railpack plan uses image %q, which is not an approved Railpack image (approved: %s)", img, strings.Join(rp.Images, ", "))
			return
		}
		m["image"] = ref
	}
	layers := func(v any) {
		list, _ := v.([]any)
		for _, l := range list {
			if m, ok := l.(map[string]any); ok {
				checkRef(m)
				pin(m)
			}
		}
	}
	for _, raw := range steps {
		st := raw.(map[string]any)
		layers(st["inputs"])
		layers(st["commands"])
	}
	checkRef(base)
	pin(base)
	layers(deploy["inputs"])
	if refErr != nil {
		return nil, refErr
	}
	if imgErr != nil {
		return nil, imgErr
	}
	list := make([]any, 0, len(secrets))
	for _, s := range slices.Sorted(slices.Values(secrets)) {
		list = append(list, s)
	}
	plan["secrets"] = list
	return json.MarshalIndent(plan, "", "  ")
}

// WithProxySecrets returns the plan the frontend runs: every step that runs commands also receives the egress proxy
// variables, which BuildKit delivers as secrets so they reach neither the image configuration nor the cache keys.
func WithProxySecrets(planRaw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(planRaw))
	dec.UseNumber()
	var plan map[string]any
	if err := dec.Decode(&plan); err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	list, _ := plan["secrets"].([]any)
	for _, s := range list {
		if v, ok := s.(string); ok {
			declared[v] = true
		}
	}
	for _, k := range ProxyEnv {
		if !declared[k] {
			list = append(list, k)
		}
	}
	plan["secrets"] = list
	steps, _ := plan["steps"].([]any)
	for _, raw := range steps {
		st, _ := raw.(map[string]any)
		if cmds, _ := st["commands"].([]any); len(cmds) == 0 {
			continue
		}
		have, _ := st["secrets"].([]any)
		var names []string
		for _, h := range have {
			if v, ok := h.(string); ok {
				names = append(names, v)
			}
		}
		if slices.Contains(names, "*") {
			continue
		}
		for _, k := range ProxyEnv {
			if !slices.Contains(names, k) {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		out := make([]any, len(names))
		for i, n := range names {
			out[i] = n
		}
		st["secrets"] = out
	}
	return json.MarshalIndent(plan, "", "  ")
}
