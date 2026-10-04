package perm

import (
	"fmt"
	"syscall"
	"testing"
	"time"
)

func BenchmarkNormalize(b *testing.B) {
	cmds := []string{
		"go test ./... 2>&1 | tail -20",
		"git status --short",
		"rg -n 'func \\w+\\(' -t go internal | head -40",
		"cd internal/perm && go vet ./... && go test -run TestNormalize ./...",
		"D=internal/web; ls $D; grep -n 'func ' $D/main.go",
		`for f in $(git ls-files '*.go'); do grep -c TODO "$f" || true; done | sort | uniq -c`,
		"git commit -m \"$(cat <<'EOF'\nfix the parser\n\nit's done\nEOF\n)\"",
		"find . -name '*.tmp' -exec rm {} +",
		`sh -c 'npm ci && npm run build' > build.log 2>&1`,
		"cat > notes.md <<'EOF'\n# Notes\n\n- one\n- two\nEOF",
	}
	for b.Loop() {
		for _, c := range cmds {
			Normalize(c)
		}
	}
}

// cpuTime is this process's CPU time, for a test that budgets work instead of
// wall time (it runs alone, no t.Parallel).
func cpuTime() time.Duration {
	var u syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

// TestNormalizeNestedCheap: a rewalk after a reassignment and a failed
// body's line by line reads reach a heredoc body again, which must not
// multiply the work per level of nesting. The budget is the
// process's CPU time; the test runs alone (no t.Parallel).
func TestNormalizeNestedCheap(t *testing.T) {
	cmd := "ls"
	for i := range 20 {
		cmd = fmt.Sprintf("sh <<E%d\n%s\nE%d", i, cmd, i)
	}
	start := cpuTime()
	Normalize(cmd)
	if d := cpuTime() - start; d > 100*time.Millisecond {
		t.Fatalf("20 nested heredocs took %s of CPU", d)
	}
}
