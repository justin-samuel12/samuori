package discovery

import (
	"regexp"
	"strconv"
	"strings"
)

type version [3]int

var versionPrefix = regexp.MustCompile(`^v?(\d+)(?:\.(\d+|[xX*]))?(?:\.(\d+|[xX*]))?`)

// parseVersion reads up to three numeric parts and how many were given; a wildcard part ends the count.
func parseVersion(s string) (version, int, bool) {
	m := versionPrefix.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return version{}, 0, false
	}
	var v version
	n := 0
	for i := 1; i <= 3; i++ {
		x, err := strconv.Atoi(m[i])
		if err != nil {
			break
		}
		v[i-1] = x
		n++
	}
	return v, n, true
}

func (a version) cmp(b version) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (a version) String() string {
	return strconv.Itoa(a[0]) + "." + strconv.Itoa(a[1]) + "." + strconv.Itoa(a[2])
}

// next returns the smallest version above every version that shares the first n parts with v.
func (v version) next(n int) version {
	switch n {
	case 0:
		return version{1 << 30}
	case 1:
		return version{v[0] + 1}
	case 2:
		return version{v[0], v[1] + 1}
	}
	return version{v[0], v[1], v[2] + 1}
}

var opSpace = regexp.MustCompile(`(>=|<=|==|!=|~=|>|<|=|\^|~)\s+`)

// satisfies evaluates npm and PEP 440 style ranges: "||" alternatives of space- or comma-separated comparators
// (>=, <=, >, <, =, ==, !=, ^, ~, ~=) and partial or wildcard versions such as 18, 18.x or 3.9.*. The second
// result is false for a range it cannot read, such as a git or file reference.
func satisfies(v version, rng string) (bool, bool) {
	rng = strings.TrimSpace(rng)
	if rng == "" || rng == "*" || rng == "x" || rng == "latest" {
		return true, true
	}
	any := false
	for _, alt := range strings.Split(rng, "||") {
		ok, understood := all(v, opSpace.ReplaceAllString(strings.TrimSpace(alt), "$1"))
		if !understood {
			return false, false
		}
		any = any || ok
	}
	return any, true
}

func all(v version, alt string) (bool, bool) {
	ok := true
	for _, c := range strings.FieldsFunc(alt, func(r rune) bool { return r == ' ' || r == ',' }) {
		match, understood := comparator(v, c)
		if !understood {
			return false, false
		}
		ok = ok && match
	}
	return ok, true
}

func comparator(v version, c string) (bool, bool) {
	op := ""
	for _, o := range []string{">=", "<=", "==", "!=", "~=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(c, o) {
			op, c = o, c[len(o):]
			break
		}
	}
	if c == "*" || c == "x" {
		return true, true
	}
	w, n, ok := parseVersion(c)
	if !ok {
		return false, false
	}
	switch op {
	case ">=":
		return v.cmp(w) >= 0, true
	case ">":
		if n < 3 {
			return v.cmp(w.next(n)) >= 0, true
		}
		return v.cmp(w) > 0, true
	case "<=":
		if n < 3 {
			return v.cmp(w.next(n)) < 0, true
		}
		return v.cmp(w) <= 0, true
	case "<":
		return v.cmp(w) < 0, true
	case "!=":
		return v.cmp(w) < 0 || v.cmp(w.next(n)) >= 0, true
	case "^":
		upper := w.next(1)
		switch {
		case w[0] == 0 && (n >= 2 && w[1] > 0 || n == 2):
			upper = w.next(2)
		case w[0] == 0 && w[1] == 0 && n == 3:
			upper = w.next(3)
		}
		return v.cmp(w) >= 0 && v.cmp(upper) < 0, true
	case "~":
		return v.cmp(w) >= 0 && v.cmp(w.next(max(min(n, 2), 1))) < 0, true
	case "~=":
		return v.cmp(w) >= 0 && v.cmp(w.next(max(n-1, 1))) < 0, true
	}
	return v.cmp(w) >= 0 && v.cmp(w.next(n)) < 0, true
}

// lowest is the smallest version a manifest range admits when the range starts from a version, such as ^0.32.1,
// ~1.2, >=2 or 3.1.0.
func lowest(rng string) (version, bool) {
	rng = strings.TrimSpace(opSpace.ReplaceAllString(rng, "$1"))
	if strings.Contains(rng, "||") || strings.HasPrefix(rng, "<") || strings.HasPrefix(rng, "!") {
		return version{}, false
	}
	first, _, _ := strings.Cut(rng, " ")
	first = strings.TrimLeft(first, ">=^~")
	v, _, ok := parseVersion(first)
	if !ok || strings.ContainsAny(first, ":/") {
		return version{}, false
	}
	return v, true
}

// admitsSeries reports whether any release in the series named by the first n parts of v, such as 18 or 3.9,
// satisfies rng. It probes the series' ends and every version the range names inside the series.
func admitsSeries(v version, n int, rng string) (bool, bool) {
	top := 1 << 20
	probe := []version{v}
	switch n {
	case 1:
		probe = append(probe, version{v[0], top, top})
	case 2:
		probe = append(probe, version{v[0], v[1], top})
	}
	for _, m := range versionLiteral.FindAllString(rng, -1) {
		if w, _, ok := parseVersion(m); ok && w.cmp(v) >= 0 && w.cmp(v.next(n)) < 0 {
			probe = append(probe, w)
		}
	}
	for _, p := range probe {
		ok, understood := satisfies(p, rng)
		if !understood {
			return false, false
		}
		if ok {
			return true, true
		}
	}
	return false, true
}

var versionLiteral = regexp.MustCompile(`\d+(?:\.\d+){0,2}`)
