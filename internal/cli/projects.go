package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"samuori.invalid/samuori/internal/apiclient"
)

// table prints aligned columns, or items as JSON with --json.
func (e *env) table(items any, header []string, rows [][]string) error {
	if e.json {
		return e.printJSON(items)
	}
	if len(rows) == 0 {
		e.note("none")
		return nil
	}
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

func when(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func str[T ~string](p *T) string {
	if p == nil {
		return "-"
	}
	return string(*p)
}

func (e *env) allProjects(ctx context.Context, archived bool) ([]apiclient.Project, error) {
	limit := 200
	params := &apiclient.ListProjectsParams{Limit: &limit}
	if archived {
		inc := apiclient.ListProjectsParamsInclude("archived")
		params.Include = &inc
	}
	var out []apiclient.Project
	for {
		r, err := e.api.ListProjectsWithResponse(ctx, params)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, problem("listing projects", r.StatusCode(), r.Body)
		}
		out = append(out, r.JSON200.Items...)
		if r.JSON200.NextCursor == nil || *r.JSON200.NextCursor == "" {
			return out, nil
		}
		params.Cursor = r.JSON200.NextCursor
	}
}

// projectID accepts a project ID or a name that only one visible project has.
func (e *env) projectID(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", failf(ExitUsage, "--project is required")
	}
	if isID(ref) {
		return ref, nil
	}
	list, err := e.allProjects(ctx, false)
	if err != nil {
		return "", err
	}
	var found []string
	for _, p := range list {
		if p.Name == ref {
			found = append(found, p.Id)
		}
	}
	switch len(found) {
	case 0:
		return "", failf(ExitError, "no project named %q is visible to this token", ref)
	case 1:
		return found[0], nil
	}
	return "", failf(ExitUsage, "%d projects are named %q (%s); use the project ID", len(found), ref, strings.Join(found, ", "))
}

// specRef resolves a spec ID, or a spec name in the given project or, without one, in the only visible project that has it.
func (e *env) specRef(ctx context.Context, ref, project string) (*apiclient.BuildSpec, error) {
	if ref == "" {
		return nil, failf(ExitUsage, "--spec is required")
	}
	if isID(ref) {
		r, err := e.api.GetSpecWithResponse(ctx, ref)
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			return nil, problem("reading the spec", r.StatusCode(), r.Body)
		}
		return r.JSON200, nil
	}
	var projects []string
	if project != "" {
		id, err := e.projectID(ctx, project)
		if err != nil {
			return nil, err
		}
		projects = []string{id}
	} else {
		list, err := e.allProjects(ctx, false)
		if err != nil {
			return nil, err
		}
		for _, p := range list {
			projects = append(projects, p.Id)
		}
	}
	var found []apiclient.BuildSpec
	for _, p := range projects {
		r, err := e.api.ListSpecsWithResponse(ctx, p, &apiclient.ListSpecsParams{Name: &ref})
		if err != nil {
			return nil, err
		}
		if r.JSON200 == nil {
			if project != "" {
				return nil, problem("finding the spec", r.StatusCode(), r.Body)
			}
			continue
		}
		found = append(found, r.JSON200.Items...)
	}
	switch len(found) {
	case 0:
		return nil, failf(ExitError, "no spec named %q is visible to this token", ref)
	case 1:
		return &found[0], nil
	}
	return nil, failf(ExitUsage, "%d projects have a spec named %q; add --project", len(found), ref)
}

func (e *env) projects(ctx context.Context, args []string) error {
	return e.subcommand(args, "list", map[string]func([]string) error{
		"list": func(args []string) error {
			fs := flags("projects", e)
			archived := fs.Bool("archived", false, "include archived and purged projects")
			if _, err := parse(fs, args); err != nil {
				return err
			}
			list, err := e.allProjects(ctx, *archived)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, p := range list {
				rows = append(rows, []string{p.Id, p.Name, str(p.TeamName), str(p.State)})
			}
			return e.table(list, []string{"ID", "NAME", "TEAM", "STATE"}, rows)
		},
		"create": func(args []string) error {
			fs := flags("projects create", e)
			team := fs.String("team", "", "team ID or name")
			pos, err := parse(fs, args)
			if err != nil {
				return err
			}
			if len(pos) != 1 || *team == "" {
				return failf(ExitUsage, "usage: samuori projects create NAME --team TEAM")
			}
			teamID, err := e.teamID(ctx, *team)
			if err != nil {
				return err
			}
			r, err := e.api.CreateProjectWithResponse(ctx, nil, apiclient.CreateProjectJSONRequestBody{Name: pos[0], TeamId: teamID})
			if err != nil {
				return err
			}
			if r.JSON201 == nil {
				return problem("creating the project", r.StatusCode(), r.Body)
			}
			return e.result(r.JSON201, r.JSON201.Id)
		},
		"show": func(args []string) error {
			pos, err := parse(flags("projects show", e), args)
			if err != nil {
				return err
			}
			if len(pos) != 1 {
				return failf(ExitUsage, "usage: samuori projects show PROJECT")
			}
			id, err := e.projectID(ctx, pos[0])
			if err != nil {
				return err
			}
			r, err := e.api.GetProjectWithResponse(ctx, id)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return problem("reading the project", r.StatusCode(), r.Body)
			}
			if e.json {
				return e.printJSON(r.JSON200)
			}
			p := r.JSON200
			fmt.Fprintf(e.out, "ID:           %s\nName:         %s\nTeam:         %s\nState:        %s\nPublication:  %s\nCreated:      %s\n",
				p.Id, p.Name, str(p.TeamName), str(p.State), str(p.PublicationMode), when(p.CreatedAt))
			specs, err := e.api.ListSpecsWithResponse(ctx, id, nil)
			if err != nil || specs.JSON200 == nil {
				return nil
			}
			fmt.Fprintln(e.out, "\nSpecs:")
			var rows [][]string
			for _, s := range specs.JSON200.Items {
				rows = append(rows, []string{s.Id, s.Name, fmt.Sprint(s.LatestRevision)})
			}
			return e.table(nil, []string{"ID", "NAME", "REVISION"}, rows)
		},
		"rename": func(args []string) error {
			pos, err := parse(flags("projects rename", e), args)
			if err != nil {
				return err
			}
			if len(pos) != 2 {
				return failf(ExitUsage, "usage: samuori projects rename PROJECT NAME")
			}
			id, err := e.projectID(ctx, pos[0])
			if err != nil {
				return err
			}
			r, err := e.api.UpdateProjectWithResponse(ctx, id, nil, apiclient.UpdateProjectJSONRequestBody{Name: &pos[1]})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return problem("renaming the project", r.StatusCode(), r.Body)
			}
			return e.result(r.JSON200, r.JSON200.Name)
		},
		"archive": func(args []string) error {
			fs := flags("projects archive", e)
			yes := fs.Bool("yes", false, "confirm: grants are revoked and not restored")
			reason := fs.String("reason", "", "reason recorded in the audit trail")
			pos, err := parse(fs, args)
			if err != nil {
				return err
			}
			if len(pos) != 1 {
				return failf(ExitUsage, "usage: samuori projects archive PROJECT --yes [--reason TEXT]")
			}
			if !*yes {
				return failf(ExitUsage, "archiving revokes the project's grants and cancels its unfinished builds; add --yes to confirm")
			}
			id, err := e.projectID(ctx, pos[0])
			if err != nil {
				return err
			}
			body := apiclient.ArchiveProjectJSONRequestBody{}
			if *reason != "" {
				body.Reason = reason
			}
			r, err := e.api.ArchiveProjectWithResponse(ctx, id, nil, body)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return problem("archiving the project", r.StatusCode(), r.Body)
			}
			return e.result(r.JSON200, "archived "+id)
		},
	}, "projects")
}

func (e *env) teamID(ctx context.Context, ref string) (string, error) {
	if isID(ref) {
		return ref, nil
	}
	limit := 200
	r, err := e.api.ListTeamsWithResponse(ctx, &apiclient.ListTeamsParams{Limit: &limit})
	if err != nil {
		return "", err
	}
	if r.JSON200 == nil {
		return "", problem("listing teams", r.StatusCode(), r.Body)
	}
	for _, t := range r.JSON200.Items {
		if t.Name == ref {
			return t.Id, nil
		}
	}
	return "", failf(ExitError, "no team named %q is visible to this token", ref)
}
