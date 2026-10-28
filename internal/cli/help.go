package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

type commandHelp struct{ usage, summary string }

var commandGroups = []struct {
	title string
	cmds  []commandHelp
}{
	{"Sign in", []commandHelp{
		{"login [--profile NAME] --url URL [--ca-file FILE]", "check an API token read from standard input or a hidden prompt, then store it with the URL"},
		{"logout [--profile NAME]", "forget a profile and its stored token (the token stays valid until revoked in Account)"},
		{"profiles", "list stored profiles; * marks the default"},
		{"whoami", "show the identity and roles behind the token"},
	}},
	{"Projects and sources", []commandHelp{
		{"projects [list] [--archived]", "list the projects the token can see"},
		{"projects create NAME --team TEAM", "create a project in a team"},
		{"projects show PROJECT", "show a project and its specs"},
		{"projects rename PROJECT NAME", "rename a project"},
		{"projects archive PROJECT --yes [--reason TEXT]", "archive a project; its grants are revoked and new work is refused"},
		{"upload --project PROJECT [--exclude PATTERN]... PATH", "upload a directory or archive; prints the snapshot ID"},
		{"analyze SNAPSHOT [--rerun] [--timeout 5m]", "summarize what discovery found in a snapshot, without running its code"},
	}},
	{"Build specs", []commandHelp{
		{"spec init [NAME] [--snapshot SNAPSHOT] [--method recipe|dockerfile|railpack|buildpacks] [-o FILE] [--force]", "write a starting spec file; with --snapshot, the server's suggestion from discovery"},
		{"spec list --project PROJECT", "list a project's specs"},
		{"spec show SPEC [--project PROJECT] [--revision N]", "print a spec revision's document"},
		{"spec validate FILE", "validate a spec file on the server without saving it"},
		{"spec check SPEC --snapshot SNAPSHOT [--project PROJECT] [--revision N]", "admission dry run of a saved revision; creates no build"},
		{"spec apply FILE --project PROJECT", "create the spec, or add a revision when its document changed"},
	}},
	{"Builds", []commandHelp{
		{"build --spec SPEC (--snapshot ID | --source PATH) [--project PROJECT] [--revision N] [--clean] [--wait] [--timeout 1h]", "start a build; --wait follows it through its checks"},
		{"builds [list] [--project PROJECT] [--state STATE] [--limit N]", "list builds, newest first"},
		{"status BUILD", "print a build as JSON"},
		{"logs BUILD [--follow]", "print a build's log; --follow keeps printing until the build ends"},
		{"cancel BUILD", "cancel a queued or running build"},
	}},
	{"Images and publication", []commandHelp{
		{"images [list] --project PROJECT", "list a project's images with their checks and publications"},
		{"download (BUILD | --artifact ID) [-o FILE]", "save the image as an archive for docker load or podman load"},
		{"deploy-files compose|helm (--build ID | --artifact ID | --project PROJECT) [--publication ID] [-o FILE]", "save a starting compose.yml or Helm chart; Samuori deploys nothing"},
		{"publish (--artifact ID | --build ID) --destination ID [--tag T]... [--wait]", "request a publication; approvals and the publish-time gate apply"},
		{"promote --publication ID --destination ID [--tag T]... [--wait]", "promote a published image to another destination without rebuilding"},
	}},
	{"Requests", []commandHelp{
		{"requests [list] --project PROJECT", "list the project's repository, base-image and egress requests"},
		{"request repository --project PROJECT --repository NAME --justification TEXT [--registry ID]", "ask administrators for a publication repository"},
		{"request base-image --project PROJECT --repository REPO --justification TEXT", "ask administrators to allow an outside base-image repository"},
		{"request egress --project PROJECT --justification TEXT (--preset P | --destination D)... EVIDENCE", "ask for build network access; EVIDENCE is --discovery ID, --build ID or --spec S --revision N --snapshot ID, with --code, --path and --line"},
		{"withdraw APPROVAL [--reason TEXT]", "withdraw a pending request by its approval ID"},
	}},
	{"Automation", []commandHelp{
		{"ci --spec SPEC [--project PROJECT] [--publish DESTINATION [--tag T]...] [--deploy-files DIR] [--timeout 1h] [PATH]", "upload PATH (default .), build, wait for the checks, then optionally publish and save deployment files"},
		{"pipeline run PIPELINE --ref REF [--wait] [--timeout 1h]", "start a pipeline run; --wait follows its build"},
		{"pipelines [list] [--project PROJECT]", "list pipelines"},
		{"api OPERATION_ID [--path K=V]... [--query K=V]... [--header K=V]... [--body FILE|-]", "call any API operation by its operationId and print the JSON answer; api --list lists them"},
	}},
	{"Other", []commandHelp{
		{"completion bash|zsh|fish", "print a shell completion script"},
		{"version", "print the client version"},
		{"help [COMMAND]", "show help for all commands or one"},
	}},
}

const globalHelp = `Global flags (before the command) and environment:
  --url URL          server URL (SAMUORI_URL)
  --token-file FILE  file holding the API token, mode 0600 (SAMUORI_TOKEN_FILE); or set SAMUORI_TOKEN
  --ca-file FILE     PEM CA bundle for a private certificate authority (SAMUORI_CA_FILE)
  --profile NAME     stored profile to use (SAMUORI_PROFILE)
  --json             print JSON results; every command also accepts --json
  Flags win over the environment, and the environment over the profile.

Exit codes: 0 success, 1 the build, checks, publication or run did not succeed (blocked and skipped included),
2 usage, 3 request or server error, 4 timed out while waiting.
`

func usageText() string {
	var b strings.Builder
	b.WriteString("samuori talks to a Samuori server with an API token.\n\nUsage: samuori [global flags] <command> [flags]\n")
	for _, g := range commandGroups {
		fmt.Fprintf(&b, "\n%s:\n", g.title)
		for _, c := range g.cmds {
			fmt.Fprintf(&b, "  %s\n      %s\n", c.usage, c.summary)
		}
	}
	b.WriteString("\n" + globalHelp)
	return b.String()
}

// printUsage prints the usage lines of every command whose usage starts with name.
func printUsage(w io.Writer, name string) bool {
	found := false
	for _, g := range commandGroups {
		for _, c := range g.cmds {
			if c.usage == name || strings.HasPrefix(c.usage, name+" ") {
				fmt.Fprintf(w, "usage: samuori %s\n  %s\n", c.usage, c.summary)
				found = true
			}
		}
	}
	return found
}

func (e *env) help(_ context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(e.out, usageText())
		return nil
	}
	if !printUsage(e.out, strings.Join(args, " ")) {
		return failf(ExitUsage, "no command %q; run samuori help", strings.Join(args, " "))
	}
	return nil
}

// commandTree lists top-level commands and their subcommands, for completion.
func commandTree() map[string][]string {
	tree := map[string][]string{}
	for _, g := range commandGroups {
		for _, c := range g.cmds {
			words := strings.Fields(c.usage)
			subs := tree[words[0]]
			if len(words) > 1 {
				for _, s := range strings.Split(strings.Trim(words[1], "[]()"), "|") {
					if s != "" && strings.ToLower(s) == s && !strings.HasPrefix(s, "-") {
						subs = append(subs, s)
					}
				}
			}
			tree[words[0]] = subs
		}
	}
	return tree
}

func (e *env) completion(_ context.Context, args []string) error {
	if len(args) != 1 {
		return failf(ExitUsage, "usage: samuori completion bash|zsh|fish")
	}
	tree := commandTree()
	var names []string
	for n := range tree {
		names = append(names, n)
	}
	sort.Strings(names)
	var cases strings.Builder
	switch args[0] {
	case "bash", "zsh":
		for _, n := range names {
			if len(tree[n]) > 0 {
				fmt.Fprintf(&cases, "    %s) words=%q ;;\n", n, strings.Join(tree[n], " "))
			}
		}
		script := `_samuori() {
  local cur=${COMP_WORDS[COMP_CWORD]} words
  if [ "$COMP_CWORD" -eq 1 ]; then
    words="` + strings.Join(names, " ") + `"
  elif [ "$COMP_CWORD" -eq 2 ]; then
    case ${COMP_WORDS[1]} in
` + cases.String() + `    esac
  fi
  COMPREPLY=($(compgen -W "$words" -- "$cur"))
}
complete -F _samuori samuori
`
		if args[0] == "zsh" {
			script = "autoload -U +X bashcompinit && bashcompinit\n" + script
		}
		fmt.Fprint(e.out, script)
	case "fish":
		fmt.Fprintf(e.out, "complete -c samuori -f -n __fish_use_subcommand -a %q\n", strings.Join(names, " "))
		for _, n := range names {
			if len(tree[n]) > 0 {
				fmt.Fprintf(e.out, "complete -c samuori -f -n '__fish_seen_subcommand_from %s' -a %q\n", n, strings.Join(tree[n], " "))
			}
		}
	default:
		return failf(ExitUsage, "usage: samuori completion bash|zsh|fish")
	}
	return nil
}
