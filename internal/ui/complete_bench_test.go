package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// benchTree grows a deterministic tree under root: fan directories per
// directory for deep levels, files in every deepest directory, and a few
// files at the root itself.
func benchTree(b *testing.B, root string, fan, deep, files int) {
	b.Helper()
	var grow func(dir string, level int)
	grow = func(dir string, level int) {
		if level == deep {
			for i := range files {
				if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i%26))+".txt"), []byte("x"), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			return
		}
		for i := range fan {
			sub := filepath.Join(dir, "d"+string(rune('a'+i)))
			if err := os.MkdirAll(sub, 0o700); err != nil {
				b.Fatal(err)
			}
			grow(sub, level+1)
		}
	}
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(root, "top"+string(rune('a'+i))+".go"), []byte("x"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	for i := range fan {
		sub := filepath.Join(root, "d"+string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0o700); err != nil {
			b.Fatal(err)
		}
		grow(sub, 1)
	}
}

func BenchmarkWalkFiles(b *testing.B) {
	root := b.TempDir()
	benchTree(b, root, 4, 4, 20)
	// 4 top dirs, fan 4 per level, leaves at level 4: 4*(4+16+64)
	// directories, the 4*64 deepest holding the files
	if got := walkFiles(context.Background(), root); len(got) != 4*64*20+10 {
		b.Fatalf("walk found %d files", len(got))
	}
	b.ResetTimer()
	for b.Loop() {
		walkFiles(context.Background(), root)
	}
}
