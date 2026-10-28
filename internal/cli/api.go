package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"samuori.invalid/samuori/internal/api/apigen"
)

type operation struct {
	ID, Method, Path string
	idempotent       bool
}

// operations reads every operation from the OpenAPI contract embedded in the build.
func operations() ([]operation, error) {
	raw, err := apigen.GetSpecJSON()
	if err != nil {
		return nil, err
	}
	type param struct {
		Ref  string `json:"$ref"`
		Name string `json:"name"`
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	idempotent := func(ps []param) bool {
		for _, p := range ps {
			if strings.HasSuffix(p.Ref, "/IdempotencyKey") || p.Name == "Idempotency-Key" {
				return true
			}
		}
		return false
	}
	var ops []operation
	for path, item := range spec.Paths {
		var shared []param
		if p, ok := item["parameters"]; ok {
			_ = json.Unmarshal(p, &shared)
		}
		for method, body := range item {
			switch method {
			case "get", "put", "post", "patch", "delete":
			default:
				continue
			}
			var op struct {
				ID         string  `json:"operationId"`
				Parameters []param `json:"parameters"`
			}
			if err := json.Unmarshal(body, &op); err != nil || op.ID == "" {
				continue
			}
			ops = append(ops, operation{op.ID, strings.ToUpper(method), path, idempotent(append(shared, op.Parameters...))})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops, nil
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

func pairs(list []string, what string) (map[string]string, []string, error) {
	m := map[string]string{}
	var keys []string
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, nil, failf(ExitUsage, "--%s takes KEY=VALUE, not %q", what, kv)
		}
		m[k] = v
		keys = append(keys, k)
	}
	return m, keys, nil
}

func (e *env) apiCall(ctx context.Context, args []string) error {
	fs := flags("api", e)
	var pathArgs, queryArgs, headerArgs multiFlag
	fs.Var(&pathArgs, "path", "path parameter KEY=VALUE (repeatable)")
	fs.Var(&queryArgs, "query", "query parameter KEY=VALUE (repeatable)")
	fs.Var(&headerArgs, "header", "request header KEY=VALUE, such as If-Match=\"2\" (repeatable)")
	bodyFile := fs.String("body", "", "JSON request body file, or - for standard input")
	listOps := fs.Bool("list", false, "list every operation")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	ops, err := operations()
	if err != nil {
		return err
	}
	if *listOps {
		var rows [][]string
		for _, o := range ops {
			rows = append(rows, []string{o.ID, o.Method, "/api/v1" + o.Path})
		}
		return e.table(ops, []string{"OPERATION", "METHOD", "PATH"}, rows)
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori api OPERATION_ID [--path K=V]... [--query K=V]... [--header K=V]... [--body FILE|-]; samuori api --list")
	}
	var op *operation
	for i := range ops {
		if strings.EqualFold(ops[i].ID, pos[0]) {
			op = &ops[i]
		}
	}
	if op == nil {
		return failf(ExitUsage, "no operation %q; samuori api --list lists them", pos[0])
	}
	params, _, err := pairs(pathArgs, "path")
	if err != nil {
		return err
	}
	var missing []string
	path := pathParam.ReplaceAllStringFunc(op.Path, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := params[name]
		if !ok {
			missing = append(missing, name)
		}
		delete(params, name)
		return url.PathEscape(v)
	})
	if len(missing) > 0 {
		return failf(ExitUsage, "%s needs --path %s=…", op.ID, strings.Join(missing, "=… --path "))
	}
	if len(params) > 0 {
		return failf(ExitUsage, "%s has no path parameter %s", op.ID, strings.Join(slices.Sorted(maps.Keys(params)), ", "))
	}
	query, queryKeys, err := pairs(queryArgs, "query")
	if err != nil {
		return err
	}
	q := url.Values{}
	for _, k := range queryKeys {
		q.Add(k, query[k])
	}
	headers, headerKeys, err := pairs(headerArgs, "header")
	if err != nil {
		return err
	}
	var body io.Reader
	switch *bodyFile {
	case "":
	case "-":
		body = e.in
	default:
		f, err := os.Open(*bodyFile)
		if err != nil {
			return failf(ExitUsage, "%v", err)
		}
		defer f.Close()
		body = f
	}
	target := e.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, op.Method, target, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if op.idempotent {
		req.Header.Set("Idempotency-Key", newKey("cli-api"))
	}
	for _, k := range headerKeys {
		req.Header.Set(k, headers[k])
	}
	// Set last so a --header cannot replace the token or send it elsewhere.
	if err := e.auth(ctx, req); err != nil {
		return err
	}
	resp, err := e.raw.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return problem(op.ID, resp.StatusCode, raw)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_, err := io.Copy(e.out, resp.Body)
		return err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		_, err := e.out.Write(raw)
		return err
	}
	pretty.WriteByte('\n')
	_, err = pretty.WriteTo(e.out)
	return err
}
