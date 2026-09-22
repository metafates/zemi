package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// ProjectPermissionsStep loads <project>/.zemi/permissions, a user-local and
// gitignored list of bash commands always allowed in a trusted project. Lines
// reuse the ask overlay's rule shapes (bashAlwaysPatterns), so a command is
// allowed as a whole, program plus subcommand, and a deleting or wrapper
// program offers nothing. Allow rules are safe here where project Lua is not:
// this file is not shared via git and is only read when the project is
// trusted (DESIGN §5).
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

// projectPermissions reads .zemi/permissions and registers its allow rules. A
// line an inner shell, assignment or malformed text (nothing a rule from it
// would match is what the user read) is skipped whole, as is one whose command
// a wrapper or a glob makes unsafe: no rule from a command the user did not
// see.
func (h *Host) projectPermissions() {
	path := filepath.Join(h.Dirs.ProjectConfigDir(), "permissions")
	b, err := os.ReadFile(path)
	if err != nil {
		return // no file is the common case
	}
	if err := h.Dirs.IgnoreLocal("permissions"); err != nil {
		h.Log.Warn("permissions", "gitignore append", map[string]any{"error": err.Error()})
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
		var patterns [][]string
		ok := true
		for _, seg := range n.Segments {
			ps, good := bashAlwaysPatterns(seg)
			if !good {
				ok = false
				break
			}
			patterns = append(patterns, ps)
		}
		if !ok {
			h.permissionSkipped(line)
			continue
		}
		for _, ps := range patterns {
			for _, p := range ps {
				h.addPermissionRule(perm.Rule{Subject: "tool.bash", Pattern: p, Decision: perm.Allow})
			}
		}
	}
}

func (h *Host) permissionSkipped(line string) {
	h.Log.Warn("permissions", "skipping unsafe command", map[string]any{"command": line})
}
