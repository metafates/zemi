// mdcommands.go registers prompt commands from markdown: commands/<name>.md
// under the config dir (user origin) or the trusted .zemi dir (project
// origin). Running /<name> args sends the body to the agent with $ARGUMENTS
// and $1..$9 filled in. Files are read at boot; /Reload picks up changes.
package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skills"
)

var positionalArg = regexp.MustCompile(`\$([1-9])`)

// expandPromptArgs fills $ARGUMENTS with the raw argument string and $1..$9
// with whitespace-separated fields (missing ones become empty).
func expandPromptArgs(body, args string) string {
	fields := strings.Fields(args)
	body = strings.ReplaceAll(body, "$ARGUMENTS", strings.TrimSpace(args))
	return positionalArg.ReplaceAllStringFunc(body, func(m string) string {
		if i := int(m[1] - '1'); i < len(fields) {
			return fields[i]
		}
		return ""
	})
}

// MarkdownCommandsStep registers the markdown commands. It runs after the
// project config step, which decides whether project files are trusted.
func (h *Host) MarkdownCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "markdown commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			if h.Dirs.ConfigHome != "" {
				h.addMarkdownCommands(filepath.Join(h.Dirs.ConfigHome, "commands"), lua.OriginUser, Gen(g.ID))
			}
			if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && h.projectTrusted.Load() {
				h.addMarkdownCommands(filepath.Join(pdir, "commands"), lua.OriginProject, Gen(g.ID))
			}
			return nil
		},
	}
}

func (h *Host) addMarkdownCommands(dir string, origin lua.Origin, gen Gen) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		warn := func(msg string) {
			h.Log.Warn("commands", path+": "+msg, nil)
			h.RecordError("command "+name, msg)
		}
		if !skills.ValidName(name) {
			warn("file name must match [a-z0-9][a-z0-9-]*")
			continue
		}
		block, body, err := readFrontmatterFile(path)
		if err != nil {
			warn(err.Error())
			continue
		}
		var fm struct {
			Description  string `yaml:"description"`
			ArgumentHint string `yaml:"argument-hint"`
		}
		if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
			warn("frontmatter: " + err.Error())
			continue
		}
		body = strings.TrimSpace(body)
		if body == "" {
			warn("empty prompt")
			continue
		}
		desc := fm.Description
		if desc == "" {
			desc = truncRunes(strings.SplitN(body, "\n", 2)[0], 60)
		}
		if fm.ArgumentHint != "" {
			desc += " " + fm.ArgumentHint
		}
		_, err = h.Commands.Add(&Command{
			Name: name, NArgs: "*", Desc: desc, Path: path, Origin: origin, Gen: gen,
			GoFn: func(h *Host, args string) error { return h.AgentSend(expandPromptArgs(body, args)) },
		})
		if err != nil {
			warn(err.Error())
		}
	}
}
