package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// ProjectPermissionsStep loads the project's permissions file, the bash
// commands always allowed in a trusted project. Lines reuse the ask
// overlay's rule shapes (bashAlwaysPatterns), so a command is allowed as a
// whole, program plus subcommand, and a deleting or wrapper program offers
// nothing. Allow rules are safe here where project Lua is not: the file lives
// in zemi's state dir, where no repository can ship it and no sandbox can
// write it, and is only read when the project is trusted (DESIGN §5).
func (h *Host) ProjectPermissionsStep() lua.BootStep {
	return lua.BootStep{
		Name: "project permissions",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			if !h.ProjectTrusted() {
				return nil
			}
			h.projectPermissions()
			return nil
		},
	}
}

// projectPermissions reads the permissions file and registers its allow rules. A
// line an inner shell, assignment or malformed text (nothing a rule from it
// would match is what the user read) is skipped whole, as is one whose command
// a wrapper or a glob makes unsafe: no rule from a command the user did not
// see.
func (h *Host) projectPermissions() {
	b, err := os.ReadFile(h.projectPermissionsPath())
	if err != nil {
		return // no file is the common case
	}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n := perm.Normalize(line)
		if n.Fail != "" || n.ForceAsk != "" || n.Assign != "" || n.Opaque {
			h.permissionSkipped(line)
			continue
		}
		patterns, ok := bashRulePatterns(n)
		if !ok {
			h.permissionSkipped(line)
			continue
		}
		for _, ps := range patterns {
			for _, p := range ps {
				h.addPermissionRule(perm.Rule{Subject: perm.SubjectBash, Pattern: p, Decision: perm.Allow, Source: "permissions"})
			}
		}
	}
}

// bashRulePatterns turns one normalized command into the allow-rule patterns
// for each of its segments: shared by the project permissions file loader and
// the "for this project" ask answer. ok is false when any segment has no safe
// rule (a wrapper, glob or deleting program).
func bashRulePatterns(n perm.Normalized) (patterns [][]string, ok bool) {
	for _, seg := range n.Segments {
		ps, good := bashAlwaysPatterns(seg)
		if !good {
			return nil, false
		}
		patterns = append(patterns, ps)
	}
	return patterns, true
}

func (h *Host) permissionSkipped(line string) {
	h.Log.Warn("permissions", "skipping unsafe command", map[string]any{"command": line})
}

// projectPermissionsPath is the project's always-allow file, "" without a
// state dir.
func (h *Host) projectPermissionsPath() string {
	if h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(projectState(h.Dirs.StateHome, h.trustRoot()), "permissions")
}

// projectAllow answers the ask's "for this project" choice: append cmd to the
// project permissions file so it allows on every later boot, and register its
// rules now so it holds for this session too. A failed write still applies
// in-memory (the choice degrades to a session allow) but is logged.
func (h *Host) projectAllow(cmd string, rules []perm.Rule) {
	path := h.projectPermissionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.Log.Warn("permissions", "create dir failed", map[string]any{"error": err.Error()})
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.Log.Warn("permissions", "append failed", map[string]any{"error": err.Error()})
	} else {
		if _, werr := f.WriteString(strings.TrimSpace(cmd) + "\n"); werr != nil {
			h.Log.Warn("permissions", "append failed", map[string]any{"error": werr.Error()})
		}
		f.Close()
	}
	for _, r := range rules {
		r.Source = "permissions"
		h.addPermissionRule(r)
	}
}
