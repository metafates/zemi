package hostapi

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/runtime"
)

// undocumented lists names the builtin skills leave out on purpose.
var undocumented = map[string]string{}

// TestSkillsCoverSurface fails when a setting, model tool, event, host
// action or slash command is missing from every builtin skill, so zemi's
// knowledge of itself cannot drift silently.
func TestSkillsCoverSurface(t *testing.T) {
	t.Parallel()
	text := skillsText(t)
	h, r := newTestHost(t)
	if err := r.Boot(t.Context(), []lua.BootStep{
		h.NamespacesStep(), h.BuiltinCommandsStep(),
	}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range config.NewStore().Schema() {
		names = append(names, string(s.Path))
	}
	for _, tool := range tools.All(&tools.Pipeline{}) {
		names = append(names, tool.Info().Name)
	}
	for ev := range eventCatalogue {
		names = append(names, ev)
	}
	for action := range h.hostActions {
		if action != "zemi.reload" {
			names = append(names, action)
		}
	}
	for _, c := range h.Commands.List() {
		names = append(names, "/"+c.Name)
	}
	for _, name := range names {
		if _, skip := undocumented[name]; !skip && !strings.Contains(text, name) {
			t.Errorf("no builtin skill mentions %q", name)
		}
	}
}

// TestBuiltinSkillDescriptionsFitIndex: a builtin description over the cap
// would fail discovery and vanish from the index.
func TestBuiltinSkillDescriptionsFitIndex(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.Dirs.DataHome = t.TempDir()
	if err := h.materializeBuiltinSkills(); err != nil {
		t.Fatal(err)
	}
	found, warns := skill.Discover(skill.Dir{Path: h.builtinSkillsDir(), Source: skill.Builtin})
	if len(found) == 0 || len(warns) > 0 {
		t.Fatalf("builtin skills: %d found, warnings %v", len(found), warns)
	}
	for _, s := range found {
		if n := len([]rune(s.Description)); n > skill.MaxDescriptionRunes {
			t.Errorf("%s description is %d runes, index shows %d", s.Name, n, skill.MaxDescriptionRunes)
		}
	}
}

// TestBuiltinSkillsFitTheSkillTool: a body over the tool's cap reaches the
// model cut in half, which is half a procedure.
func TestBuiltinSkillsFitTheSkillTool(t *testing.T) {
	for dir, body := range builtinSkillBodies(t) {
		if len(body) > tools.SkillMaxOut {
			t.Errorf("%s is %d bytes, the skill tool sends %d", dir, len(body), tools.SkillMaxOut)
		}
	}
}

// TestBuiltinSkillReferences: every reference file a builtin skill or a
// prompt names exists in the skill that names it, and every builtin skill's
// SKILL.md routes to every file beside it.
func TestBuiltinSkillReferences(t *testing.T) {
	t.Parallel()
	bodies := builtinSkillBodies(t)
	h, _ := newTestHost(t)
	// the lua tool names its file by the synced path
	lua := strings.ReplaceAll((&luaEvalTool{h: h}).Info().Description, filepath.Join(h.builtinSkillsDir(), "zemi")+"/", " ")
	// a ready prompt sends the model to the zemi skill's directory
	const zemi = "skills/zemi"
	rows := []struct{ name, text, dir string }{
		{"newPluginPrompt", newPluginPrompt, zemi},
		{"modifyPluginPrompt", modifyPluginPrompt, zemi},
		{"newAgentPrompt", newAgentPrompt, zemi},
		{"newWorkflowPrompt", newWorkflowPrompt, zemi},
		{"errorAskPrompt", errorAskPrompt, zemi},
		{"workflowToolDesc", workflowToolDesc, zemi},
		{"the lua tool", lua, zemi},
	}
	for path, body := range bodies {
		rows = append(rows, struct{ name, text, dir string }{path, body, filepath.Dir(path)})
	}
	// a bare name: commands/<name>.md is a path, AGENTS.md no reference file
	ref := regexp.MustCompile("(?:^|[\\s(`])([a-z][a-z0-9-]*\\.md)\\b")
	routed := map[string]bool{}
	for _, r := range rows {
		for _, m := range ref.FindAllStringSubmatch(r.text, -1) {
			file := m[1]
			if _, ok := bodies[r.dir+"/"+file]; !ok {
				t.Errorf("%s names %s, which %s does not have", r.name, file, r.dir)
			}
			if strings.HasSuffix(r.name, "/SKILL.md") {
				routed[r.dir+"/"+file] = true
			}
		}
	}
	for path := range bodies {
		if file := filepath.Base(path); file != "SKILL.md" && !routed[path] {
			t.Errorf("%s/SKILL.md never routes to %s", filepath.Dir(path), file)
		}
	}
}

// TestPluginsSkillNamesEveryTestMember: plugins.md is the zemi.test
// reference an agent reads, so it cannot lose a driver or an assertion.
func TestPluginsSkillNamesEveryTestMember(t *testing.T) {
	skill := builtinSkillBodies(t)["skills/zemi/plugins.md"]
	for _, m := range bootedNamespaces(t, true)["zemi.test"] {
		if !strings.Contains(skill, "test."+m+"(") {
			t.Errorf("plugins.md never shows test.%s(", m)
		}
	}
}

// skillsText is every builtin skill file joined.
func skillsText(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, body := range builtinSkillBodies(t) {
		b.WriteString(body)
	}
	return b.String()
}

// builtinSkillBodies is every embedded skill file by path.
func builtinSkillBodies(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(runtime.Skills, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(runtime.Skills, path)
		out[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
