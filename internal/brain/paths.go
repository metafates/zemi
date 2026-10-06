// Package brain is the disk-native memory store (DESIGN §7, §10.7):
// one Markdown partition per scope, an append-only SQLite ledger beside
// each, and the render and lint that carry them into the prompt.
package brain

import (
	"path/filepath"

	"github.com/metafates/zemi/internal/store"
)

// Kind is which partition a store is: the user's cross-project brain or
// the private, TrustRoot-keyed one. Where the two differ - lock order,
// lint scoping - the kind carries it.
type Kind uint8

const (
	// User holds what is true in every project; its ledger is the global DB.
	User Kind = iota
	// Private holds one repository's maps and knowledge; its ledger is the
	// project DB.
	Private
)

// Scope is the memory scope a partition holds.
func (k Kind) Scope() store.Scope {
	if k == User {
		return store.ScopeUser
	}
	return store.ScopeProject
}

func (k Kind) String() string {
	if k == User {
		return "user"
	}
	return "private"
}

// Partition is one brain directory and the store its ledger lives in.
type Partition struct {
	Kind Kind
	Dir  string
	DB   string // the SQLite file whose memory_ops table is this partition's ledger
}

// UserPartition is <dataHome>/brain/user (DESIGN §7), whose ledger lives in the
// global store. The user partition is user-owned state a pull cannot
// write, so its catalog injects in every project (DESIGN §10.7).
func UserPartition(dataHome, stateHome string) Partition {
	if dataHome == "" || stateHome == "" {
		return Partition{Kind: User}
	}
	return Partition{
		Kind: User,
		Dir:  filepath.Join(dataHome, "brain", "user"),
		DB:   filepath.Join(stateHome, "zemi.db"),
	}
}

// PrivatePartition is <stateHome>/projects/<hash-of-TrustRoot>/brain, the
// same hash key and derivation as the project DB it sits beside (the
// recipes precedent), so one brain per repository survives across
// worktrees and no pull can write a byte of it (DESIGN §7).
func PrivatePartition(stateHome, trustRoot string) Partition {
	if stateHome == "" || trustRoot == "" {
		return Partition{Kind: Private}
	}
	db := store.ProjectDBPath(stateHome, trustRoot)
	return Partition{
		Kind: Private,
		Dir:  filepath.Join(filepath.Dir(db), "brain"),
		DB:   db,
	}
}

// Zero reports a partition with no home: no repository or no data home, so
// there is nothing to load, lint, lock or write.
func (p Partition) Zero() bool { return p.Dir == "" }

// memPath is the reserved one-liner store.
func (p Partition) memPath() string { return filepath.Join(p.Dir, MemFile) }
