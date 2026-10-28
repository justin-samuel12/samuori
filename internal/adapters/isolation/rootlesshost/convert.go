package rootlesshost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"samuori.invalid/samuori/internal/adapters/build/buildkit"
	"samuori.invalid/samuori/internal/engine"
	"samuori.invalid/samuori/internal/isolation"
	"samuori.invalid/samuori/internal/toolrun"
)

// ConversionDockerfile runs the tool runner as the only process of a step built FROM scratch: nothing is pulled, the
// step has no network, and the tool, its run specification and the input are read-only bind mounts (ADR 0048,
// docs/architecture/kubernetes-conversion.md#execution).
func ConversionDockerfile() string {
	run, _ := json.Marshal([]string{"/.samuori-tool/samuori-run", "tool-run", "@/.samuori-tool/" + toolrun.SpecFile})
	return "FROM scratch AS samuori-convert\nWORKDIR /work\n" +
		"RUN --network=none --mount=type=bind,from=samuoritool,target=/.samuori-tool --mount=type=bind,from=samuoriinput,target=/in " + string(run) +
		"\nFROM scratch\nCOPY --from=samuori-convert /out/ /\n"
}

func (p *Provider) ConvertReady() error {
	if p.cfg.Engine != engine.BuildKit {
		return errors.New("conversions need the BuildKit engine")
	}
	if p.cfg.ProbeBinary == "" {
		return errors.New("no static samuori-worker is configured as the tool runner (probe-binary)")
	}
	return staticProbe(p.cfg.ProbeBinary)
}

func (p *Provider) Convert(ctx context.Context, ie isolation.Env, s isolation.ConvertSpec) (isolation.ConvertResult, error) {
	e, err := p.get(ie.ID)
	if err != nil {
		return isolation.ConvertResult{}, err
	}
	if err := p.ConvertReady(); err != nil || e.daemon == nil {
		detail := "conversions need the BuildKit engine"
		if err != nil {
			detail = err.Error()
		}
		return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrUnsupported, Detail: detail}
	}
	root := filepath.Join(e.dir, "convert")
	dfDir, toolDir := filepath.Join(root, "dockerfile"), filepath.Join(root, "tool")
	for _, d := range []string{dfDir, toolDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
		}
	}
	// The tool runs as an unprivileged user in the step, so its read-only mount must be searchable.
	if err := os.Chmod(toolDir, 0o755); err != nil {
		return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	if err := copyFile(filepath.Join(s.ToolDir, "tool"), filepath.Join(toolDir, "tool"), 0o755); err != nil {
		return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "staging the tool: " + err.Error()}
	}
	if err := copyFile(p.cfg.ProbeBinary, filepath.Join(toolDir, "samuori-run"), 0o755); err != nil {
		return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "staging the tool runner: " + err.Error()}
	}
	if err := os.WriteFile(filepath.Join(toolDir, toolrun.SpecFile), s.Run, 0o644); err != nil {
		return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "staging the run specification: " + err.Error()}
	}
	if err := os.WriteFile(filepath.Join(dfDir, "Dockerfile"), []byte(ConversionDockerfile()), 0o600); err != nil {
		return isolation.ConvertResult{}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: err.Error()}
	}
	res, err := e.daemon.RunTool(ctx, buildkit.ToolRequest{DockerfileDir: dfDir, Contexts: map[string]string{"samuoritool": toolDir, "samuoriinput": s.InputDir},
		OutputDir: s.OutputDir, Platform: "linux/" + runtime.GOARCH})
	logs := []byte(res.Log)
	switch {
	case err != nil && e.daemon.OutOfMemory():
		return isolation.ConvertResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrResourceLimit, Detail: "the conversion exceeded its memory limit"}
	case ctx.Err() != nil:
		return isolation.ConvertResult{Logs: logs}, ctx.Err()
	case err != nil:
		return isolation.ConvertResult{Logs: logs}, &isolation.Failure{Class: isolation.ErrInfrastructure, Detail: "the conversion step did not run: " + err.Error()}
	}
	return isolation.ConvertResult{Logs: logs}, nil
}
