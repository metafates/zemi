package glob

import "testing"

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, rel string
		want         bool
	}{
		{"*.go", "a/b/main.go", true},
		{"src/**/*.ts", "src/x/y/a.ts", true},
		{"src/**/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/x/a.ts", false},
		{"*.{ts,tsx}", "a.tsx", true},
		{"[!a]*", "b.go", true},
		{"[!a]*", "a.go", false},
		{"[a!]*", "!x", true},
		{"[a!]*", "b", false},
	} {
		match, err := Match(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := match(c.rel); got != c.want {
			t.Errorf("%q on %q = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}
