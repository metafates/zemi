package perm

import "testing"

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
