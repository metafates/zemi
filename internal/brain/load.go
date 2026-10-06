package brain

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/perm"
)

// Scan ceilings (§3): the catalog is derived at render, so a pathological
// tree costs at most this much. Past the caps the catalog renders its cut
// marker naming the partition and a lint finding fires.
const (
	// MaxCatalogFiles caps a partition's non-reserved .md files.
	MaxCatalogFiles = 64
	// MaxFrontBytes caps the frontmatter text one scan parses.
	MaxFrontBytes = 256 << 10
	// FrontPeek is how much of a file the frontmatter parse reads;
	// frontmatter past it is an unterminated block.
	FrontPeek = 4 << 10
)

// MemFile is the reserved one-liner store's name, the memory tool's file
// (D1); every other .md is freeform and carries frontmatter.
const MemFile = "memories.md"

// Entry is one freeform brain file as the scan saw it. A file that passes
// the file-safety checks but cannot be read or parsed is still here, Err
// set: it is named, never silently dropped (D6).
type Entry struct {
	Name string // the file's name in the partition
	FM   Frontmatter
	Err  error // unreadable, unterminated block, or broken yaml
}

// Loaded is one partition's scan. A missing directory loads empty, never
// as an error (D1); the render and lint degrade from here.
type Loaded struct {
	Partition Partition
	Entries   []Entry // sorted by name, at most MaxCatalogFiles
	Over      bool    // the scan hit a ceiling with catalogable files left
	DirErr    error   // the directory could not be read (a missing one is empty, not this)
	Mem       []byte  // memories.md's bytes; nil when absent
	MemErr    error   // memories.md failed to read
}

// Load scans a partition's directory. Files that fail instructionFile's
// bar (prompt.go:485: a regular file, not secret-shaped, inside the
// partition) are left out of the catalog, so a symlink planted in a brain
// dir cannot feed a target outside it into the render (D3).
func Load(p Partition) Loaded {
	l := Loaded{Partition: p}
	des, err := os.ReadDir(p.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return l
	}
	if err != nil {
		l.DirErr = err
		return l
	}
	names := make([]string, 0, len(des))
	for _, e := range des {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	front := 0
	for _, name := range names {
		if name == MemFile {
			l.Mem, l.MemErr = readSafe(p, MemFile)
			continue
		}
		if !strings.HasSuffix(name, ".md") || !safe(p.Dir, name) {
			continue
		}
		if len(l.Entries) >= MaxCatalogFiles || front >= MaxFrontBytes {
			l.Over = true
			continue
		}
		e, n := readEntry(p.Dir, name)
		front += n
		l.Entries = append(l.Entries, e)
	}
	return l
}

// readEntry reads a file's first FrontPeek bytes and parses their
// frontmatter: the whole parse rides the peek.
func readEntry(dir, name string) (e Entry, peeked int) {
	e.Name = name
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		e.Err = err
		return e, 0
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, FrontPeek))
	if err != nil {
		e.Err = err
		return e, 0
	}
	e.FM, e.Err = ParseFrontmatter(string(data))
	return e, len(data)
}

// readSafe reads a brain file through the bar every cataloged file passes: a
// regular file, not secret-shaped, resolving inside the partition. A missing
// file is ErrNotExist as a plain read would give; anything planted - a link,
// a pipe - reads as fs.ErrInvalid, not the store.
func readSafe(p Partition, name string) ([]byte, error) {
	path := filepath.Join(p.Dir, name)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	if !safe(p.Dir, name) {
		return nil, fs.ErrInvalid
	}
	return os.ReadFile(path)
}

// safe is instructionFile's check (prompt.go:485-488): a regular file, not
// secret-shaped, inside the partition. Symlinks are followed, like a read
// would follow them, and must not lead out.
func safe(dir, name string) bool {
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if perm.PathIsSecret(name) {
		return false
	}
	real := perm.CanonPath(p)
	if real == "" || perm.PathIsSecret(real) {
		return false
	}
	root := perm.CanonPath(dir)
	if root == "" {
		return false
	}
	_, ok := perm.Under(root, real)
	return ok
}
