package discovery

import (
	"fmt"
	"sort"
)

var detectors = []struct {
	id  string
	run func(*Tree) []Finding
}{
	{"node", detectNode}, {"python", detectPython}, {"powershell", detectPowerShell}, {"dotnet", detectDotnet},
}

// Analyse runs every detector over in; a detector that fails is reported as a warning, not a crash.
func Analyse(in Input) Result {
	t := NewTree(in)
	res := Result{Findings: []Finding{}, Dockerfiles: []DockerfileAnalysis{}, Warnings: []Warning{}}
	for _, d := range detectors {
		func() {
			defer func() {
				if r := recover(); r != nil {
					res.Warnings = append(res.Warnings, Warning{"detector-failed", fmt.Sprintf("The %s detector could not finish; its results are missing.", d.id), ""})
				}
			}()
			res.Findings = append(res.Findings, d.run(t)...)
		}()
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				res.Warnings = append(res.Warnings, Warning{"detector-failed", "Dockerfile analysis could not finish.", ""})
			}
		}()
		res.Dockerfiles = detectDockerfiles(t, in.Dockerfile)
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				res.Warnings = append(res.Warnings, Warning{"detector-failed", "Build readiness could not be predicted.", ""})
			}
		}()
		predict(t, &res, in)
	}()
	sort.SliceStable(res.Findings, func(i, j int) bool {
		if res.Findings[i].SourceSubpath != res.Findings[j].SourceSubpath {
			return res.Findings[i].SourceSubpath < res.Findings[j].SourceSubpath
		}
		return res.Findings[i].DetectorID < res.Findings[j].DetectorID
	})
	dirs := map[string]bool{}
	for _, f := range res.Findings {
		if f.ApplicationKind != "" {
			dirs[f.SourceSubpath] = true
		}
	}
	if len(dirs) > 1 {
		res.Warnings = append(res.Warnings, Warning{"multiple-applications", fmt.Sprintf("Found %d candidate applications; choose the one to build.", len(dirs)), ""})
	}
	if len(res.Findings) == 0 && len(res.Dockerfiles) == 0 {
		res.Warnings = append(res.Warnings, Warning{"nothing-detected", "No supported language or Dockerfile was recognised. You can still describe the build yourself.", ""})
	}
	return res
}
