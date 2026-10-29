package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// config exports and imports the organization's configuration (ADR 0069); import previews unless --apply is given.
func (e *env) config(ctx context.Context, args []string) error {
	return e.subcommand(args, "", map[string]func([]string) error{
		"export": func(a []string) error { return e.configExport(ctx, a) },
		"import": func(a []string) error { return e.configImport(ctx, a) },
	}, "config")
}

func (e *env) send(ctx context.Context, method, path, contentType string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := e.auth(ctx, req); err != nil {
		return 0, nil, err
	}
	resp, err := e.raw.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, out, err
}

func (e *env) configExport(ctx context.Context, args []string) error {
	fs := flags("config export", e)
	out := fs.String("o", "", "write to FILE instead of standard output")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return failf(ExitUsage, "usage: samuori config export [-o FILE]")
	}
	accept := "application/yaml"
	if e.json {
		accept = "application/json"
	}
	status, body, err := e.send(ctx, "GET", "/configuration/export", "", nil, map[string]string{"Accept": accept})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return problem("config export", status, body)
	}
	if *out == "" {
		_, err := e.out.Write(body)
		return err
	}
	if err := os.WriteFile(*out, body, 0o600); err != nil {
		return failf(ExitError, "%v", err)
	}
	e.note("wrote %s", *out)
	return nil
}

type configPlan struct {
	DryRun   bool `json:"dryRun"`
	Complete bool `json:"complete"`
	Items    []struct {
		Section, Name, Action, Reason, Outcome, Error string
	} `json:"items"`
	NeedsAttention []struct{ Section, Name, Kind, Message, Link string } `json:"needsAttention"`
	Summary        struct{ Create, Update, Unchanged, Skip, Failed int } `json:"summary"`
}

func (e *env) configImport(ctx context.Context, args []string) error {
	fs := flags("config import", e)
	apply := fs.Bool("apply", false, "apply the plan instead of previewing it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return failf(ExitUsage, "usage: samuori config import FILE [--apply]")
	}
	file, err := os.ReadFile(pos[0])
	if err != nil {
		return failf(ExitUsage, "%v", err)
	}
	contentType, headers, query := "application/yaml", map[string]string{}, "?dryRun=true"
	if json.Valid(file) {
		contentType = "application/json"
	}
	if *apply {
		headers["Idempotency-Key"], query = newKey("cli-config"), "?dryRun=false"
	}
	status, body, err := e.send(ctx, "POST", "/configuration/import"+query, contentType, file, headers)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		var p struct {
			Details []struct{ Field, Reason string } `json:"details"`
		}
		err := problem("config import", status, body)
		if json.Unmarshal(body, &p) == nil && len(p.Details) > 0 {
			var b strings.Builder
			b.WriteString(err.Error())
			for _, d := range p.Details {
				fmt.Fprintf(&b, "\n  %s: %s", d.Field, d.Reason)
			}
			return failf(ExitError, "%s", b.String())
		}
		return err
	}
	var plan configPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		return err
	}
	rows := make([][]string, 0, len(plan.Items))
	for _, it := range plan.Items {
		what := it.Reason
		if it.Error != "" {
			what = it.Error
		}
		rows = append(rows, []string{it.Section, it.Name, it.Action, cmp.Or(it.Outcome, "-"), what})
	}
	if err := e.table(plan, []string{"SECTION", "NAME", "ACTION", "OUTCOME", "DETAIL"}, rows); err != nil {
		return err
	}
	if !e.json {
		for _, x := range plan.NeedsAttention {
			fmt.Fprintf(e.out, "needs attention (%s): %s\n", x.Kind, x.Message)
		}
		s := plan.Summary
		if *apply {
			fmt.Fprintf(e.out, "applied: %d create, %d update, %d unchanged, %d skipped, %d of them failed\n", s.Create, s.Update, s.Unchanged, s.Skip, s.Failed)
		} else {
			fmt.Fprintf(e.out, "preview, nothing changed: %d to create, %d to update, %d unchanged, %d skipped; add --apply to apply\n", s.Create, s.Update, s.Unchanged, s.Skip)
		}
	}
	if !plan.Complete {
		return failf(ExitFailed, "%d item(s) failed; the others were applied", plan.Summary.Failed)
	}
	return nil
}
