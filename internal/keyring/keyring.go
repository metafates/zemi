// Package keyring stores small secrets (an MCP server's OAuth grant, later a
// model API key) in the OS keychain, with a 0600 file as the fallback where
// no keychain answers - a headless server, a broken agent. The keychain
// itself is github.com/zalando/go-keyring: /usr/bin/security on darwin, the
// Credential Manager on windows, the Secret Service on linux.
package keyring

import (
	"errors"
	"os"

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
// File as the 0600 fallback.
type Ref struct {
	Account string // "mcp:<server>", "key:<provider>"
	File    string
}

// Get reads the secret, keychain first; false means neither store holds one.
func (r Ref) Get() (string, bool) {
	if s, err := Chain.Get(Service, r.Account); err == nil && s != "" {
		return s, true
	}
	if b, err := os.ReadFile(r.File); err == nil && len(b) > 0 {
		return string(b), true
	}
	return "", false
}

// Set stores the secret in the keychain, where it works, and removes the
// file, so a grant saved there before migrates on its next write. A keychain
// that refuses falls to the file; the error is only when that fails too.
func (r Ref) Set(secret string) (inChain bool, err error) {
	if err := Chain.Set(Service, r.Account, secret); err == nil {
		_ = os.Remove(r.File) // ErrNotExist is the usual case, a cleanup miss no news
		return true, nil
	}
	// Get reads the chain first, so an older copy there would shadow the
	// file's newer one forever; the chain refused this write anyway.
	_ = Chain.Delete(Service, r.Account)
	return false, config.WriteFileAtomic(r.File, []byte(secret))
}

// Delete removes the secret from both stores; absent is fine.
func (r Ref) Delete() error {
	if err := Chain.Delete(Service, r.Account); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := os.Remove(r.File); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
