// boot_bench_test.go measures the boot path (internal/cli/boot.go,
// internal/lua/runtime.go's step loop): the first paint and every
// ForceReload pay it, and nothing measured it before this. One no-config
// tree serves every iteration, so the number is the warm restart cost.

package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
)

func BenchmarkBootHeadless(b *testing.B) {
	home := b.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		b.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base := b.TempDir()
	b.Chdir(base)
	b.Setenv("PWD", base)
	dirs, err := config.ResolveDirs()
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		_, done, err := bootHeadless(options{dirs: dirs}, false, io.Discard)
		if err != nil {
			b.Fatal(err)
		}
		done()
	}
}
