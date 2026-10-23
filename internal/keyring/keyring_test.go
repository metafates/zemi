package keyring

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubChain answers from fields; Set keeps the secret in stored.
type stubChain struct {
	val            string
	getErr, setErr error
	delErr         error
	stored         string
}

func (s *stubChain) Get(string, string) (string, error) { return s.val, s.getErr }

func (s *stubChain) Set(_, _, secret string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.stored = secret
	return nil
}

func (s *stubChain) Delete(string, string) error {
	if s.delErr == nil {
		s.stored = ""
	}
	return s.delErr
}

// TestRef: the keychain answers first, the file covers its misses and
// refusals, and a write that reaches the keychain takes the file with it.
func TestRef(t *testing.T) {
	tests := []struct {
		name   string
		op     string // get, set, delete
		chain  *stubChain
		file   string // the fallback file's content before, "" when absent
		secret string // set
		noDir  bool   // set: File under a missing directory
		want   string // get
		wantOK bool   // get: found; set: stored in the chain
		noErr  bool   // set and delete: no error
	}{
		{name: "the keychain wins over the file", op: "get", chain: &stubChain{val: "kc"}, file: "filedata", want: "kc", wantOK: true, noErr: true},
		{name: "a keychain miss reads the file", op: "get", chain: &stubChain{getErr: ErrNotFound}, file: "filedata", want: "filedata", wantOK: true, noErr: true},
		{name: "neither store holds one", op: "get", chain: &stubChain{getErr: ErrNotFound}},
		{name: "a keychain save clears the file", op: "set", chain: &stubChain{}, file: "old", secret: "grant", wantOK: true, noErr: true},
		{name: "a keychain refusal falls to the file", op: "set", chain: &stubChain{setErr: errors.New("locked")}, secret: "grant", noErr: true},
		{name: "both stores fail", op: "set", chain: &stubChain{setErr: errors.New("locked")}, secret: "grant", noDir: true},
		{name: "a delete clears both", op: "delete", chain: &stubChain{}, file: "filedata", noErr: true},
		{name: "an absent chain entry still clears the file", op: "delete", chain: &stubChain{delErr: ErrNotFound}, file: "filedata", noErr: true},
		{name: "a delete failure reports", op: "delete", chain: &stubChain{delErr: errors.New("locked")}, file: "filedata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "grant.json")
			if tc.noDir {
				// a file where WriteFileAtomic needs a directory: no store can win
				if err := os.WriteFile(filepath.Join(dir, "blocker"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(dir, "blocker", "grant.json")
			}
			if tc.file != "" {
				if err := os.WriteFile(file, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			old := Chain
			t.Cleanup(func() { Chain = old })
			Chain = tc.chain
			ref := Ref{Account: "mcp:test", File: file}

			switch tc.op {
			case "get":
				got, ok := ref.Get()
				if got != tc.want || ok != tc.wantOK {
					t.Errorf("Get = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
				}
			case "set":
				inChain, err := ref.Set(tc.secret)
				if (err == nil) != tc.noErr || inChain != tc.wantOK {
					t.Errorf("Set = (%v, %v), want inChain %v, no err %v", inChain, err, tc.wantOK, tc.noErr)
				}
				if tc.wantOK {
					if tc.chain.stored != tc.secret {
						t.Errorf("chain holds %q, want %q", tc.chain.stored, tc.secret)
					}
					if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("the fallback file survived a keychain save: %v", err)
					}
				} else if err == nil {
					fi, err := os.Stat(file)
					if err != nil {
						t.Fatal(err)
					}
					if fi.Mode().Perm() != 0o600 {
						t.Errorf("fallback file mode = %v, want 0600", fi.Mode().Perm())
					}
				}
			case "delete":
				err := ref.Delete()
				if (err == nil) != tc.noErr {
					t.Errorf("Delete = %v, want no err %v", err, tc.noErr)
				}
				if tc.noErr {
					if tc.chain.stored != "" {
						t.Errorf("chain still holds %q", tc.chain.stored)
					}
					if tc.file != "" {
						if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("the file survived a delete: %v", err)
						}
					}
				} else if tc.file != "" {
					if _, err := os.Stat(file); err != nil {
						t.Errorf("a failed delete dropped the file: %v", err)
					}
				}
			}
		})
	}
}
