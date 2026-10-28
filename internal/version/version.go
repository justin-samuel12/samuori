// Package version identifies this build and decides which versions may work together (D28). Releases set Version
// with -ldflags; development builds keep the default.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

var Version = "0.1.0-dev"

// Credit names Samuori's creator, maintainer and license, as NOTICE does.
const Credit = "Samuori, created and maintained by Justin Samuel <jsamuel12@gmail.com>. Apache License 2.0."

// Semver is a parsed MAJOR.MINOR.PATCH[-PRE][+BUILD] version.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

func Parse(v string) (Semver, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Semver{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return Semver{}, false
		}
		n[i] = x
	}
	return Semver{n[0], n[1], n[2], pre}, true
}

func (s Semver) Development() bool { return s.Pre != "" }

func (s Semver) String() string {
	out := fmt.Sprintf("%d.%d.%d", s.Major, s.Minor, s.Patch)
	if s.Pre != "" {
		out += "-" + s.Pre
	}
	return out
}

// WorkerCompatible applies the version matrix: a worker runs the controller's major and minor version or the minor
// version before it, so controllers upgrade first and workers follow (rolling upgrades). A worker newer than its
// controller is refused. Development builds work only with development builds.
func WorkerCompatible(controller, worker string) (bool, string) {
	c, okc := Parse(controller)
	w, okw := Parse(worker)
	switch {
	case !okc:
		return false, "the controller version " + controller + " is not a release version"
	case c.Development() && (!okw || w.Development()):
		return true, ""
	case !okw:
		return false, "the worker reports no release version"
	case c.Development() != w.Development():
		return false, "development and release builds do not mix"
	case w.Major != c.Major:
		return false, fmt.Sprintf("worker %s and controller %s differ in major version", w, c)
	case w.Minor > c.Minor:
		return false, fmt.Sprintf("worker %s is newer than controller %s; upgrade controllers first", w, c)
	case c.Minor-w.Minor > 1:
		return false, fmt.Sprintf("worker %s is more than one minor version behind controller %s", w, c)
	}
	return true, ""
}

// ControllersCompatible reports whether controllers of two versions may share a database during a rolling upgrade:
// the same major version and at most one minor version apart.
func ControllersCompatible(a, b string) bool {
	x, okx := Parse(a)
	y, oky := Parse(b)
	if !okx || !oky {
		return a == b
	}
	d := x.Minor - y.Minor
	return x.Major == y.Major && d >= -1 && d <= 1
}

// Compare orders versions by semantic-version precedence: -1, 0 or 1. Build metadata is ignored.
func Compare(a, b Semver) int {
	for _, d := range [3]int{a.Major - b.Major, a.Minor - b.Minor, a.Patch - b.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	x, y := strings.Split(a.Pre, "."), strings.Split(b.Pre, ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		if c := comparePre(x[i], y[i]); c != 0 {
			return c
		}
	}
	return sign(len(x) - len(y))
}

func comparePre(a, b string) int {
	m, errA := strconv.Atoi(a)
	n, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return sign(m - n)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}
