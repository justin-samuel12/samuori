package kubeconvert

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"samuori.invalid/samuori/internal/convertjob"
)

// Kompose converts Compose files (kompose convert). It always runs with --build none, so it never builds or pushes
// an image, and without its annotations, so no input path leaks into the output.
type Kompose struct{}

func (Kompose) Name() string      { return "kompose" }
func (Kompose) Formats() []string { return []string{FormatManifests, FormatHelm, FormatKustomize} }
func (Kompose) Inputs() []string  { return []string{InputCompose, InputBuild, InputPublication} }

func (k Kompose) Plan(in PlanInput) (ToolPlan, error) {
	if !convertjob.LocalPath(in.ComposeFile) {
		return ToolPlan{}, invalid("the Compose file path must stay inside the input")
	}
	dir := "manifests"
	if in.Options.Format == FormatHelm {
		dir = in.Name
	}
	args := []string{"convert", "-f", convertjob.InputRoot + "/" + in.ComposeFile, "-o", convertjob.OutputRoot + "/" + dir,
		"--provider", "kubernetes", "--build", "none", "--push-image=false", "--with-kompose-annotation=false",
		"--replicas", strconv.Itoa(in.Options.Replicas), "--volumes", in.Options.Storage.Volumes}
	if in.Options.Namespace != "" {
		args = append(args, "--namespace", in.Options.Namespace)
	}
	if in.Options.Storage.Size != "" {
		args = append(args, "--pvc-request-size", in.Options.Storage.Size)
	}
	if in.Options.Format == FormatHelm {
		args = append(args, "--chart")
	}
	return ToolPlan{Args: args, Mkdirs: []string{dir}, OutputDir: dir}, nil
}

var (
	ansi      = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	logLine   = regexp.MustCompile(`^(WARN|INFO|FATA|ERRO|DEBU)\s+(.*)$`)
	inputPath = regexp.MustCompile(regexp.QuoteMeta(convertjob.InputRoot) + `(/[^\s"']*)?`)
)

// cleanLog strips colour codes and replaces sandbox input paths with paths relative to the input.
func cleanLog(s string) string {
	s = ansi.ReplaceAllString(s, "")
	return inputPath.ReplaceAllStringFunc(s, func(m string) string {
		rel := strings.TrimPrefix(strings.TrimPrefix(m, convertjob.InputRoot), "/")
		if rel == "" {
			return "."
		}
		return rel
	})
}

func (k Kompose) Interpret(in PlanInput, plan ToolPlan, out ToolOutput) (Interpretation, error) {
	var res Interpretation
	var fatal []string
	for _, raw := range strings.Split(cleanLog(out.Stderr), "\n") {
		m := logLine.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue
		}
		msg := strings.TrimSpace(m[2])
		switch m[1] {
		case "WARN":
			res.Findings = append(res.Findings, komposeWarning(msg))
		case "FATA", "ERRO":
			fatal = append(fatal, msg)
		}
	}
	if out.TimedOut {
		return res, invalid("Kompose did not finish within its time limit")
	}
	if out.ExitCode != 0 {
		detail := strings.Join(fatal, "; ")
		if detail == "" {
			detail = fmt.Sprintf("exit status %d", out.ExitCode)
		}
		return res, invalid("Kompose failed: %s", clipString(detail, 500))
	}
	names := make([]string, 0, len(out.Files))
	for name := range out.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		bundle := name
		switch in.Options.Format {
		case FormatHelm:
			bundle = path.Join(in.Name, name)
		default:
			bundle = path.Join("manifests", name)
		}
		res.Files = append(res.Files, newFile(bundle, out.Files[name]))
	}
	if len(res.Files) == 0 {
		return res, invalid("Kompose produced no files")
	}
	return res, nil
}

// komposeWarning keeps every Kompose warning; the ones that drop or change configuration need review.
func komposeWarning(msg string) Finding {
	f := Finding{Severity: SeverityWarning, Code: "kompose-warning", Source: SourceProvider, Message: "Kompose: " + msg}
	switch {
	case strings.Contains(msg, "isn't supported - ignoring path on the host"):
		f.Severity, f.Code = SeverityReview, "host-volume-not-carried"
		f.Message = "Kompose: " + msg + ". The bind mount became a volume claim; data on the Compose host is not carried over."
	case strings.Contains(msg, "won't be created because 'ports' is not specified"):
		f.Severity, f.Code = SeverityReview, "no-service-created"
	case strings.Contains(strings.ToLower(msg), "unsupported"), strings.Contains(msg, "not supported"):
		f.Severity, f.Code = SeverityReview, "compose-option-unsupported"
	}
	return f
}

func clipString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
