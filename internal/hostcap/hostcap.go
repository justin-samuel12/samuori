// Package hostcap measures a host's disk, memory and CPUs for the capacity view (docs/operators/capacity.md).
package hostcap

import (
	"runtime"
	"strings"
	"unicode/utf8"
)

// Dir is a directory to measure, named by what it holds.
type Dir struct {
	Label, Path string
}

// Volume is one filesystem; Label joins the labels of every measured directory on it.
type Volume struct {
	Label      string `json:"label"`
	Path       string `json:"path"`
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
}

// Report is a host's capacity; zero memory means it could not be read on this platform.
type Report struct {
	CPUs                 int      `json:"cpus"`
	MemoryTotalBytes     uint64   `json:"memoryTotalBytes"`
	MemoryAvailableBytes uint64   `json:"memoryAvailableBytes"`
	Volumes              []Volume `json:"volumes"`
}

const (
	MaxVolumes  = 8
	maxLabelLen = 120
)

// Measure reports the filesystems holding dirs, once per filesystem, in the order given. Missing directories are
// skipped.
func Measure(dirs []Dir) Report {
	r := Report{CPUs: runtime.NumCPU(), Volumes: []Volume{}}
	r.MemoryTotalBytes, r.MemoryAvailableBytes = memory()
	seen := map[uint64]int{}
	for _, d := range dirs {
		dev, total, free, err := statfs(d.Path)
		if err != nil || total == 0 {
			continue
		}
		if i, ok := seen[dev]; ok {
			r.Volumes[i].Label += ", " + d.Label
			continue
		}
		seen[dev] = len(r.Volumes)
		r.Volumes = append(r.Volumes, Volume{Label: d.Label, Path: d.Path, TotalBytes: total, FreeBytes: free})
	}
	return r
}

// Clean bounds a report received from a worker before it is stored.
func Clean(r Report) Report {
	r.CPUs = min(max(r.CPUs, 0), 4096)
	r.MemoryAvailableBytes = min(r.MemoryAvailableBytes, r.MemoryTotalBytes)
	out := []Volume{}
	for _, v := range r.Volumes {
		if len(out) == MaxVolumes {
			break
		}
		if v.TotalBytes == 0 {
			continue
		}
		v.FreeBytes = min(v.FreeBytes, v.TotalBytes)
		v.Label, v.Path = text(v.Label), text(v.Path)
		out = append(out, v)
	}
	r.Volumes = out
	return r
}

func text(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	for len(s) > maxLabelLen {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}
