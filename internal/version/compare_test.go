package version

import "testing"

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.1.9", 1},
		{"0.2.0-dev+abc", "0.2.0-dev+def", 0},
		{"0.2.0", "0.2.0-rc.1", 1},
		{"0.2.0-rc.2", "0.2.0-rc.10", -1},
		{"0.2.0-alpha", "0.2.0-alpha.1", -1},
		{"1.0.0", "0.9.9", 1},
	} {
		a, _ := Parse(c.a)
		b, _ := Parse(c.b)
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d; want %d", c.a, c.b, got, c.want)
		}
	}
}
