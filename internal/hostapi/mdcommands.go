// mdcommands.go registers prompt commands from markdown: commands/<name>.md
// under the config dir (user origin), an enabled plugin (plugin origin) or a
// trusted repository's .agents (project origin). Running /<name> args sends the body to the agent with $ARGUMENTS
// and $1..$9 filled in, behind the typed "/<name> args". Files are read at boot; /reload picks up changes.
// The step also registers the /<name> commands of skills that ask for one.

package hostapi

import (
	"cmp"
	"context"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/agent"
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
			h.addMarkdownCommands(Gen(g.ID))
			return nil
		},
	}
}

func (h *Host) addMarkdownCommands(gen Gen) {
	h.layerFiles("commands", ".md", func(f layerFile) bool {
		name, path := f.name, f.path
		warn := func(msg string) {
			h.Log.Warn("commands", path+": "+msg, nil)
			h.RecordError("command "+name, msg)
		}
		block, body, err := readFrontmatterFile(path)
		if err != nil {
			warn(err.Error())
			return false
		}
		var fm struct {
			Description  string      `yaml:"description"`
			ArgumentHint skills.Hint `yaml:"argument-hint"`
		}
		if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
			warn("frontmatter: " + err.Error())
			return false
		}
		body = strings.TrimSpace(body)
		if body == "" {
			warn("empty prompt")
			return false
		}
		desc := listDesc(cmp.Or(fm.Description, clipRunes(strings.SplitN(body, "\n", 2)[0], 60)))
		desc = strings.TrimSpace(desc + " " + string(fm.ArgumentHint))
		_, err = h.Commands.Add(&Command{
			Name: name, NArgs: NArgsAny, Desc: desc, Path: path, Origin: f.origin, Gen: gen,
			// the typed line stays the user's words; the body is the file's
			GoFn: func(h *Host, args string) error {
				typed, stdin := agent.CutAttached(args)
				return h.AgentSend(strings.TrimSpace("/"+name+" "+typed) + agent.Attached("command", expandPromptArgs(body, typed)) + stdin)
			},
		})
		if err != nil {
			warn(err.Error())
			return false
		}
		return true
	})
	// last, so a skill that asks for a command yields to every one above
	for _, s := range h.enabledSkills() {
		if s.UserInvocable {
			h.addSkillCommand(s, gen)
		}
	}
}
