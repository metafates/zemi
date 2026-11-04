// Package keyring stores small secrets (an MCP server's OAuth grant, later a
// model API key) in the OS keychain, with a 0600 file as the fallback where
// no keychain answers - a headless server, a broken agent. The keychain
// itself is github.com/zalando/go-keyring: /usr/bin/security on darwin, the
// Credential Manager on windows, the Secret Service on linux.
package keyring

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	kc "github.com/zalando/go-keyring"

	"github.com/metafates/zemi/internal/config"
)

// Service is the service name of every keychain entry.
const Service = "zemi"

// ErrNotFound says the chain holds no entry; a test chain stands in with it.
var ErrNotFound = errors.New("not in the keychain")

// Chain is the OS keychain backend; tests swap it.
var Chain chain = osChain{}

type chain interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

type osChain struct{}

// chainDeadline is how long one OS keychain call may take. The call can
// block forever in some session contexts (the first-run audit's probe hung
// on kc.Set), where one hung call wedged every Go command queued behind it
// on the shared worker and the file fallback never ran. Past the deadline
// the caller sees the stall as a refusal and takes the path a refusal
// takes.
// NOTE: the ceiling is one leaked goroutine per timed-out call, held until
// the keychain answers; joining it would mean waiting out the stall this
// deadline exists to escape.
var chainDeadline = 3 * time.Second

// bound runs one OS keychain call to the deadline.
func bound(fn func() error) error {
	errc := make(chan error, 1)
	go func() { errc <- fn() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(chainDeadline):
		return fmt.Errorf("the OS keychain did not answer within %s", chainDeadline)
	}
}

func (osChain) Get(service, account string) (string, error) {
	s, err := kc.Get(service, account)
	if errors.Is(err, kc.ErrNotFound) {
		return "", ErrNotFound
	}
	return s, err
}

func (osChain) Set(service, account, secret string) error {
	return kc.Set(service, account, secret)
}

func (osChain) Delete(service, account string) error {
	err := kc.Delete(service, account)
	if errors.Is(err, kc.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Ref is one secret's store: Account in the keychain under Service, with
// File as the 0600 fallback. Legacy, when set, is the account the same
// secret lived under before the tree scope existed; the first read moves
// the value across.
type Ref struct {
	Account string // "mcp:<server>@<scope>", "key:<provider>@<scope>"
	Legacy  string // "mcp:<server>", "key:<provider>": the pre-scope account
	File    string
}

// Scoped refs the secret of account under the tree dataHome names. The
// keychain is machine-global, so an unscoped account lets a throwaway tree
// overwrite the real install's stored secret and then clear it; the scope
// is 12 hex of the data home's sha256, stable for the install, distinct
// per tree.
func Scoped(dataHome, account, file string) Ref {
	sum := sha256.Sum256([]byte(dataHome))
	return Ref{
		Account: fmt.Sprintf("%s@%x", account, sum[:6]),
		Legacy:  account,
		File:    file,
	}
}

// Get reads the secret, keychain first; false means neither store holds
// one, and chain is whether the keychain itself answered - where a key
// lives is what the UI says about it. A scoped miss falls to the legacy
// account once and moves the value over, so an upgrade loses nothing: the
// chain copy is authoritative there, as it was when it was written.
func (r Ref) Get() (secret string, chain, ok bool) {
	c := Chain // captured once: the call may outlive the deadline, and tests swap Chain under it
	var s string
	if err := bound(func() error {
		var err error
		s, err = c.Get(Service, r.Account)
		return err
	}); err == nil && s != "" {
		return s, true, true
	}
	if r.Legacy != "" {
		if err := bound(func() error {
			var err error
			s, err = c.Get(Service, r.Legacy)
			return err
		}); err == nil && s != "" {
			if setErr := bound(func() error { return c.Set(Service, r.Account, s) }); setErr == nil {
				_ = bound(func() error { return c.Delete(Service, r.Legacy) })
				_ = os.Remove(r.File)
			}
			return s, true, true
		}
	}
	if b, err := os.ReadFile(r.File); err == nil && len(b) > 0 {
		return string(b), false, true
	}
	return "", false, false
}

// Set stores the secret in the keychain, where it works, and removes the
// file, so a grant saved there before migrates on its next write. A keychain
// that refuses or stalls past the deadline falls to the file; the error is
// only when that fails too.
func (r Ref) Set(secret string) (inChain bool, err error) {
	c := Chain
	if err := bound(func() error { return c.Set(Service, r.Account, secret) }); err == nil {
		_ = os.Remove(r.File) // ErrNotExist is the usual case, a cleanup miss no news
		if r.Legacy != "" {   // a pre-scope copy would linger with the old value
			_ = bound(func() error { return c.Delete(Service, r.Legacy) })
		}
		return true, nil
	}
	// the chain refused this write, but an older copy it holds would shadow
	// the file's newer one forever; it goes only once the file holds the
	// secret, so a failed save never destroys the old key
	if err := config.WriteFileAtomic(r.File, []byte(secret)); err != nil {
		return false, err
	}
	_ = bound(func() error { return c.Delete(Service, r.Account) })
	if r.Legacy != "" {
		_ = bound(func() error { return c.Delete(Service, r.Legacy) })
	}
	return false, nil
}

// Delete removes the secret from both stores - the scoped account and, for
// a ref that has one, the pre-scope account a migration may not have
// reached; absent is fine. A chain that stalls past the deadline is an
// error: the copy may still be there.
func (r Ref) Delete() error {
	c := Chain
	for _, account := range [...]string{r.Account, r.Legacy} {
		if account == "" {
			continue
		}
		if err := bound(func() error { return c.Delete(Service, account) }); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if err := os.Remove(r.File); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
