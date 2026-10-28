package discovery

// Score applies the documented confidence rules: high needs a score of at least 5, one strong item and no
// contradiction; medium needs at least 3; any contradiction caps the level at medium.
func Score(ev []Evidence) string {
	total, strong, contradicted := 0, false, false
	for _, e := range ev {
		total += e.Weight
		strong = strong || e.Weight == Strong
		contradicted = contradicted || e.Weight < 0
	}
	switch {
	case total >= 5 && strong && !contradicted:
		return "high"
	case total >= 3:
		return "medium"
	default:
		return "low"
	}
}

type finding struct{ Finding }

func newFinding(id, version, dir string) *finding {
	return &finding{Finding{DetectorID: id, DetectorVersion: version, SourceSubpath: dir, Evidence: []Evidence{}, RuntimeConstraints: map[string]string{}, SuggestedCommands: map[string]string{}}}
}

func (f *finding) add(weight int, kind, p, pointer, observation string) {
	f.Evidence = append(f.Evidence, Evidence{Kind: kind, Path: p, Pointer: pointer, Observation: observation, Weight: weight})
}

func (f *finding) warn(code, msg, p string) {
	f.Warnings = append(f.Warnings, Warning{Code: code, Message: msg, Path: p})
}

func (f *finding) done() Finding {
	f.Confidence = Score(f.Evidence)
	for _, e := range f.Evidence {
		if e.Weight < 0 {
			f.warn("contradicting-evidence", "Some evidence conflicts; confirm the detected settings.", e.Path)
			break
		}
	}
	if len(f.RuntimeConstraints) == 0 {
		f.RuntimeConstraints = nil
	}
	if len(f.SuggestedCommands) == 0 {
		f.SuggestedCommands = nil
	}
	return f.Finding
}
