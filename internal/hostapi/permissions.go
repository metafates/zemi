// permissions.go persists permanent rules (DESIGN §6.1): permissions.lua is
// Go-generated data, read back by a strict parser that never executes Lua.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

const permissionsDumpVersion = 1

// permanentRulesPath is the permissions.lua dump location (DESIGN §5:
// ~/.config/zemi/permissions.lua). Empty when there is no config home
// (tests, headless embeds).
func (h *Host) permanentRulesPath() string {
	if h.Dirs.ConfigHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.ConfigHome, "permissions.lua")
}

// permanentRules returns the permanent rules in stable dump order.
func permanentRules(all []perm.Rule) []perm.Rule {
	var out []perm.Rule
	for _, r := range all {
		if r.Scope == perm.ScopePermanent {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b perm.Rule) int {
		return cmp.Or(cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Pattern, b.Pattern),
			cmp.Compare(a.Decision, b.Decision), cmp.Compare(a.NotMatch, b.NotMatch), cmp.Compare(a.Principal, b.Principal))
	})
	return out
}

// luaQuote renders one double-quoted Lua string literal with escapes the
// strict parser below accepts.
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// permDump renders the permanent rules as permissions.lua content: a data
// table with a version and one row per rule, deterministic across runs.
func permDump(rules []perm.Rule) []byte {
	var b strings.Builder
	b.WriteString("-- zemi permissions.lua: Go-generated data, not code (DESIGN 6.1).\n")
	b.WriteString("-- The permission rule store is the source of truth; this file is its\n")
	b.WriteString("-- dump of permanent rules. Manage rules through zemi, or delete the file.\n")
	b.WriteString("return {\n  version = " + strconv.Itoa(permissionsDumpVersion) + ",\n  rules = {\n")
	for _, r := range rules {
		b.WriteString("    { subject = " + luaQuote(r.Subject) +
			", pattern = " + luaQuote(r.Pattern) +
			", decision = " + luaQuote(string(r.Decision)) +
			", not_match = " + luaQuote(r.NotMatch) +
			", principal = " + luaQuote(r.Principal) +
			", scope = " + luaQuote(string(r.Scope)) + " },\n")
	}
	b.WriteString("  },\n}\n")
	return []byte(b.String())
}

// permRegenerate rewrites permissions.lua from the current permanent
// rules (atomic replace, so a crash never leaves a half-written dump).
func (h *Host) permRegenerate() error {
	path := h.permanentRulesPath()
	if path == "" {
		return nil
	}
	data := permDump(permanentRules(h.Perm.Rules()))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// persistPermanentRule rewrites permissions.lua and commits it as one
// revision. A failed commit is only logged: the rule and the dump stay.
func (h *Host) persistPermanentRule(r perm.Rule, title, by string) error {
	if err := h.permRegenerate(); err != nil {
		return err
	}
	changes, _ := json.Marshal(r)
	if err := h.commitConfigRevision(title, string(changes), []string{"permissions.lua"}, by); err != nil {
		h.Log.Warn("config-repo", "permanent-rule commit failed", map[string]any{"error": err.Error()})
	}
	return nil
}

// parsePermissionsDump parses permissions.lua without executing any of
// it. Only the exact generated shape is accepted: line comments, then
// `return { version = <int>, rules = { { field = "string", ... }, ... } }`
// with a fixed field whitelist; anything else (functions, concatenation,
// unknown fields, missing keys) is an error, which is what keeps the dump
// free of smuggled executable Lua.
func parsePermissionsDump(data []byte) ([]perm.Rule, error) {
	p := &permParser{src: string(data)}
	p.skipSpaceAndComments()
	if err := p.expectWord("return"); err != nil {
		return nil, err
	}
	p.skipSpaceAndComments()
	var (
		rules              []perm.Rule
		seenVersion, seenB bool
	)
	err := p.list(func() error {
		key, err := p.readKey()
		if err != nil {
			return err
		}
		switch key {
		case "version":
			v, err := p.readInt()
			if err != nil {
				return err
			}
			if v != permissionsDumpVersion {
				return fmt.Errorf("dump version %d, want %d", v, permissionsDumpVersion)
			}
			seenVersion = true
		case "rules":
			if err := p.list(func() error {
				row, err := p.expectStringFields()
				if err != nil {
					return err
				}
				r, err := rowToRule(row)
				if err != nil {
					return err
				}
				rules = append(rules, r)
				return nil
			}); err != nil {
				return err
			}
			seenB = true
		default:
			return fmt.Errorf("unknown top-level key %q", key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !seenVersion || !seenB {
		return nil, errors.New("dump missing version or rules")
	}
	p.skipSpaceAndComments()
	if err := p.expectEnd(); err != nil {
		return nil, err
	}
	return rules, nil
}

func rowToRule(row map[string]string) (perm.Rule, error) {
	want := map[string]bool{
		"subject": true, "pattern": true, "decision": true,
		"not_match": true, "principal": true, "scope": true,
	}
	for k := range row {
		if !want[k] {
			return perm.Rule{}, fmt.Errorf("unknown rule field %q", k)
		}
	}
	for _, req := range []string{"subject", "pattern", "decision", "scope"} {
		if row[req] == "" {
			return perm.Rule{}, fmt.Errorf("rule missing %q", req)
		}
	}
	r := perm.Rule{
		Subject:   row["subject"],
		Pattern:   row["pattern"],
		Decision:  perm.Decision(row["decision"]),
		NotMatch:  row["not_match"],
		Principal: row["principal"],
		Scope:     perm.Scope(row["scope"]),
	}
	switch r.Decision {
	case perm.Allow, perm.Ask, perm.Deny:
	default:
		return perm.Rule{}, errors.New("rule decision must be allow, ask or deny")
	}
	if r.Scope != perm.ScopePermanent {
		return perm.Rule{}, fmt.Errorf("rule scope must be %q, got %q", perm.ScopePermanent, r.Scope)
	}
	return r, nil
}

// permParser is a deliberately tiny reader for the generated dump shape:
// it accepts only word keys, string literals, integers, and table
// constructors, and refuses everything else before any evaluation could
// happen.
type permParser struct {
	src string
	pos int
}

func (p *permParser) skipSpaceAndComments() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++
			continue
		}
		if c == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '-' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		return
	}
}

func (p *permParser) peek() byte {
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

func (p *permParser) expectEnd() error {
	if p.pos < len(p.src) {
		return fmt.Errorf("trailing content at offset %d", p.pos)
	}
	return nil
}

func (p *permParser) expectWord(w string) error {
	if !strings.HasPrefix(p.src[p.pos:], w) {
		return fmt.Errorf("expected %q at offset %d", w, p.pos)
	}
	p.pos += len(w)
	return nil
}

func (p *permParser) expect(b byte) error {
	if p.peek() != b {
		return fmt.Errorf("expected %q at offset %d", string(b), p.pos)
	}
	p.pos++
	return nil
}

func (p *permParser) readInt() (int, error) {
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("expected integer at offset %d", start)
	}
	v, err := strconv.Atoi(p.src[start:p.pos])
	if err != nil {
		return 0, fmt.Errorf("bad integer: %w", err)
	}
	return v, nil
}

func (p *permParser) readString() (string, error) {
	if p.peek() != '"' {
		return "", fmt.Errorf("expected string at offset %d", p.pos)
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return b.String(), nil
		case '\\':
			if p.pos+1 >= len(p.src) {
				return "", fmt.Errorf("dangling escape at offset %d", p.pos)
			}
			p.pos++
			switch e := p.src[p.pos]; e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'x':
				if p.pos+2 >= len(p.src) {
					return "", fmt.Errorf("bad \\x escape at offset %d", p.pos)
				}
				v, err := strconv.ParseUint(p.src[p.pos+1:p.pos+3], 16, 8)
				if err != nil {
					return "", fmt.Errorf("bad \\x escape at offset %d", p.pos)
				}
				b.WriteByte(byte(v))
				p.pos += 2
			default:
				return "", fmt.Errorf("unknown escape \\%c at offset %d", e, p.pos)
			}
			p.pos++
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	return "", fmt.Errorf("unterminated string at offset %d", p.pos)
}

// list parses `{ item, item, ... }` (trailing separator allowed); item
// consumes one entry and refuses anything it does not expect.
func (p *permParser) list(item func() error) error {
	if err := p.expect('{'); err != nil {
		return err
	}
	for {
		p.skipSpaceAndComments()
		if p.peek() == '}' {
			p.pos++
			return nil
		}
		if err := item(); err != nil {
			return err
		}
		p.skipSpaceAndComments()
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return nil
		default:
			return fmt.Errorf("expected , or } at offset %d", p.pos)
		}
	}
}

// readKey reads `word =` and leaves the parser at the value.
func (p *permParser) readKey() (string, error) {
	start := p.pos
	for p.pos < len(p.src) && isWordByte(p.src[p.pos]) {
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("expected key at offset %d", start)
	}
	key := p.src[start:p.pos]
	p.skipSpaceAndComments()
	if err := p.expect('='); err != nil {
		return "", err
	}
	p.skipSpaceAndComments()
	return key, nil
}

// expectStringFields parses `{ key = "string", ... }` into a map,
// refusing duplicate keys and non-string values.
func (p *permParser) expectStringFields() (map[string]string, error) {
	out := map[string]string{}
	err := p.list(func() error {
		key, err := p.readKey()
		if err != nil {
			return err
		}
		v, err := p.readString()
		if err != nil {
			return err
		}
		if _, dup := out[key]; dup {
			return fmt.Errorf("duplicate field %q", key)
		}
		out[key] = v
		return nil
	})
	return out, err
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// PermissionsLoadStep returns the boot step that loads permissions.lua as
// data before any user or plugin Lua runs. A missing file is fine (no
// permanent rules yet); an unreadable or non-conforming file fails the
// boot fail-closed with the permanent store empty.
func (h *Host) PermissionsLoadStep() lua.BootStep {
	return lua.BootStep{
		Name: "permissions load",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.sweepPendingRevisions()
			rules, err := h.readPermanentRules()
			// a reload replays this step onto the same store; copies would
			// also land in the next dump
			for _, rule := range rules {
				if !slices.Contains(h.Perm.Rules(), rule) {
					h.Perm.Add(rule)
				}
			}
			return err
		},
	}
}

// readPermanentRules loads permissions.lua; no config home or no file
// means no rules.
func (h *Host) readPermanentRules() ([]perm.Rule, error) {
	path := h.permanentRulesPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("permissions.lua: %w", err)
	}
	rules, err := parsePermissionsDump(data)
	if err != nil {
		return nil, fmt.Errorf("permissions.lua refused (fix or delete the file): %w", err)
	}
	return rules, nil
}
