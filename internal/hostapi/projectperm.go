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
// commands always allowed in a trusted project. Allow rules are safe here
// where project Lua is not: the file lives in zemi's state dir, where no
// repository can ship it and no sandbox can write it (DESIGN §5).
func (h *Host) ProjectPermissionsStep() lua.BootStep {
	return lua.BootStep{
		Name: "project permissions",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			if !h.ProjectTrusted() {
				return nil
			}
			for _, line := range permissionLines(h.projectPermissionsPath()) {
				rules, ok := projectRules(perm.Normalize(line))
				if !ok {
					h.Log.Warn("skipping unsafe command", "origin", "permissions", "command", line)
					continue
				}
				for _, r := range rules {
					h.addPermissionRule(r)
				}
			}
			return nil
		},
	}
}

// permissionLines are the commands a permissions file lists.
func permissionLines(path string) []string {
	b, _ := os.ReadFile(path) // no file is the common case
	var out []string
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// projectRules are the allow rules a permissions file line loads, each
// segment in the shape the "always" answer offers (bashAlwaysPatterns). ok
// is false, and the line loads nothing, when any part has no safe rule: an
// inner shell, an assignment, text that does not parse, a wrapper, a glob.
func projectRules(n perm.Normalized) (rules []perm.Rule, ok bool) {
	if n.Fail != "" || n.ForceAsk != "" || n.Assign != "" || n.Opaque {
		return nil, false
	}
	for _, seg := range n.Segments {
		ps, ok := bashAlwaysPatterns(seg)
		if !ok {
			return nil, false
		}
		for _, p := range ps {
			rules = append(rules, perm.Rule{Subject: perm.SubjectBash, Pattern: p, Decision: perm.Allow, Source: "permissions"})
		}
	}
	return rules, true
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
// project permissions file for every later boot, and register its rules now.
// A failed write still applies for this session, and is logged.
func (h *Host) projectAllow(cmd string, rules []perm.Rule) {
	path := h.projectPermissionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.Log.Warn("create dir failed", "origin", "permissions", "error", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.Log.Warn("append failed", "origin", "permissions", "error", err)
	} else {
		if _, werr := f.WriteString(strings.TrimSpace(cmd) + "\n"); werr != nil {
			h.Log.Warn("append failed", "origin", "permissions", "error", werr)
		}
		f.Close()
	}
	for _, r := range rules {
		h.addPermissionRule(r)
	}
}
