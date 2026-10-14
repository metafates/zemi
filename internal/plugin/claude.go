// Claude layout support: a directory holding .claude-plugin/plugin.json is a
// Claude Code plugin. Its manifest is read beside zemi's own and the two
// contribution types zemi lacks - MCP servers and hooks - are parsed here;
// the layered dirs (skills/, agents/, commands/) load as they are. The names
// are Claude's, so a plugin can carry manifests for several agents at once.
// Nothing here runs Lua or needs a Host, so the CLI shares it.

package plugin

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// ClaudeDir marks a plugin as a Claude Code plugin; the manifest is
// ClaudeManifestFile. ClaudeHooksFile and ClaudeMCPFile are the conventional
// component files, read in addition to what the manifest names.
const (
	ClaudeDir          = ".claude-plugin"
	ClaudeManifestFile = ClaudeDir + "/plugin.json"
	ClaudeHooksFile    = "hooks/hooks.json"
	ClaudeMCPFile      = ".mcp.json"
)

// IsClaude reports whether dir holds a Claude manifest.
func IsClaude(dir string) bool {
	return readHead(filepath.Join(dir, ClaudeManifestFile)) != nil
}

// ClaudeAuthor is a manifest's author.
type ClaudeAuthor struct {
	Name, Email, URL string
}

// ClaudeMCPServer is one server of .mcp.json or the manifest's mcpServers,
// its string values already expanded.
type ClaudeMCPServer struct {
	Name         string
	Type         string // "stdio" when Command is set, else the url type
	Command      string
	Args         []string
	Env          map[string]string
	URL          string
	Headers      map[string]string
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	CallbackPort int    `json:"callbackPort"`
}

// ClaudeHookGroup is one event's matcher groups of a hooks file.
type ClaudeHookGroup struct {
	Event    string // the Claude event name, e.g. PreToolUse
	Matchers []ClaudeMatcher
}

// ClaudeMatcher is one matcher's hooks. Claude's three matcher forms: ""
// or "*" matches everything, "Edit|Write" is an exact list, anything else is
// a regular expression (an invalid one matches nothing).
type ClaudeMatcher struct {
	Matcher string
	Hooks   []ClaudeHook
}

// ClaudeHook is one hook command; command hooks are the only type zemi runs.
type ClaudeHook struct {
	Type    string
	Command string
	Timeout int    // seconds; 0 keeps Claude's default
	If      string // a permission-rule pattern; zemi refuses such entries
}

// ClaudeManifest is ClaudeManifestFile parsed, with the conventional
// component files merged in and string values expanded. When the manifest
// declares a component field, zemi does not scan the conventional dir for
// that type (Claude's rule).
type ClaudeManifest struct {
	Name, Version, Description, Homepage, Repository string
	Author                                           ClaudeAuthor

	Commands, Agents, Skills          []string // extra component paths
	CommandsSet, AgentsSet, SkillsSet bool

	MCP   []ClaudeMCPServer // .mcp.json merged under the manifest's mcpServers
	Hooks []ClaudeHookGroup // hooks/hooks.json merged with the manifest's hooks
}

// claudeManifestJSON is the manifest as written; component fields accept a
// "./path", an array of those, or an inline object.
type claudeManifestJSON struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
	Repository  string `json:"repository"`
	Author      struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		URL   string `json:"url"`
	} `json:"author"`

	Commands   jsontext.Value `json:"commands"`
	Agents     jsontext.Value `json:"agents"`
	Skills     jsontext.Value `json:"skills"`
	Hooks      jsontext.Value `json:"hooks"`
	MCPServers jsontext.Value `json:"mcpServers"`
}

// ReadClaude reads the Claude manifest of the plugin in dir; dataDir is what
// ${CLAUDE_PLUGIN_DATA} stands for. A missing manifest is the zero manifest,
// its components still read; a malformed one is an error carrying the zero
// manifest, for the caller to log and load nothing from.
func ReadClaude(dir, dataDir string) (ClaudeManifest, error) {
	var m ClaudeManifest
	var j claudeManifestJSON
	if raw := readHead(filepath.Join(dir, ClaudeManifestFile)); raw != nil {
		if err := json.Unmarshal(raw, &j); err != nil {
			return ClaudeManifest{}, err
		}
		m = ClaudeManifest{
			Name: j.Name, Version: j.Version, Description: j.Description,
			Homepage: j.Homepage, Repository: j.Repository,
			Author: ClaudeAuthor(j.Author),
		}
	}
	var err error
	if m.Commands, err = componentPaths(dir, j.Commands); err != nil {
		return ClaudeManifest{}, err
	}
	m.CommandsSet = len(j.Commands) > 0
	if m.Agents, err = componentPaths(dir, j.Agents); err != nil {
		return ClaudeManifest{}, err
	}
	m.AgentsSet = len(j.Agents) > 0
	if m.Skills, err = componentPaths(dir, j.Skills); err != nil {
		return ClaudeManifest{}, err
	}
	m.SkillsSet = len(j.Skills) > 0
	if m.MCP, err = claudeMCP(dir, dataDir, j.MCPServers); err != nil {
		return ClaudeManifest{}, err
	}
	if m.Hooks, err = claudeHooks(dir, dataDir, j.Hooks); err != nil {
		return ClaudeManifest{}, err
	}
	return m, nil
}

// componentPaths resolves a component field: a "./path" or an array of them,
// joined to dir. An inline object is a form zemi does not use for these.
func componentPaths(dir string, raw jsontext.Value) ([]string, error) {
	paths, _, err := componentField(raw)
	for i, p := range paths {
		paths[i] = filepath.Join(dir, p)
	}
	return paths, err
}

// componentField resolves a manifest component field into its paths and
// whatever inline objects it holds.
func componentField(raw jsontext.Value) (paths []string, inline []jsontext.Value, err error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil, nil
	}
	var list []jsontext.Value
	if json.Unmarshal(raw, &list) == nil {
		for _, e := range list {
			var es string
			if json.Unmarshal(e, &es) == nil {
				paths = append(paths, es)
				continue
			}
			var obj map[string]jsontext.Value
			if json.Unmarshal(e, &obj) == nil {
				inline = append(inline, e)
			}
		}
		return paths, inline, nil
	}
	var obj map[string]jsontext.Value
	if json.Unmarshal(raw, &obj) == nil {
		return nil, []jsontext.Value{raw}, nil
	}
	return nil, nil, errors.New("not a path, an array or an object")
}

// claudeMCP merges the plugin's MCP servers: the root .mcp.json lowest, the
// manifest's mcpServers on top, a later source winning a name.
func claudeMCP(dir, dataDir string, raw jsontext.Value) ([]ClaudeMCPServer, error) {
	var servers []ClaudeMCPServer
	add := func(cfgs map[string]claudeMCPCfg) {
		for _, name := range slices.Sorted(maps.Keys(cfgs)) {
			s := cfgs[name].server(name)
			expandClaudeServer(&s, dir, dataDir)
			servers = replaceNamed(servers, s)
		}
	}
	if f := readJSONFile(filepath.Join(dir, ClaudeMCPFile)); f != nil {
		cfgs, err := claudeMCPCfgs(f)
		if err != nil {
			return nil, err
		}
		add(cfgs)
	}
	paths, inline, err := componentField(raw)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		f := readJSONFile(filepath.Join(dir, p))
		if f == nil {
			continue
		}
		cfgs, err := claudeMCPCfgs(f)
		if err != nil {
			return nil, err
		}
		add(cfgs)
	}
	for _, one := range inline {
		cfgs, err := claudeMCPCfgs(one)
		if err != nil {
			return nil, err
		}
		add(cfgs)
	}
	return servers, nil
}

// claudeMCPCfgs parses a document as {"mcpServers": {name: cfg}} or the bare
// {name: cfg} form. An entry that is not a server object is dropped, not
// fatal: one bad server must not sink its neighbors.
func claudeMCPCfgs(raw jsontext.Value) (map[string]claudeMCPCfg, error) {
	var doc map[string]jsontext.Value
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if inner, ok := doc["mcpServers"]; ok {
		var wrapped map[string]jsontext.Value
		if err := json.Unmarshal(inner, &wrapped); err != nil {
			return nil, err
		}
		doc = wrapped
	}
	cfgs := make(map[string]claudeMCPCfg, len(doc))
	for name, v := range doc {
		var c claudeMCPCfg
		if json.Unmarshal(v, &c) == nil {
			cfgs[name] = c
		}
	}
	return cfgs, nil
}

type claudeMCPCfg struct {
	Type         string            `json:"type"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	ClientID     string            `json:"clientId"`
	ClientSecret string            `json:"clientSecret"`
	CallbackPort int               `json:"callbackPort"`
	Oauth        *struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		CallbackPort int    `json:"callbackPort"`
	} `json:"oauth"`
}

func (c claudeMCPCfg) server(name string) ClaudeMCPServer {
	s := ClaudeMCPServer{
		Name: name, Type: c.Type,
		Command: c.Command, Args: c.Args,
		Env: c.Env, URL: c.URL, Headers: c.Headers,
		ClientID: c.ClientID, ClientSecret: c.ClientSecret, CallbackPort: c.CallbackPort,
	}
	if c.Oauth != nil {
		s.ClientID, s.ClientSecret, s.CallbackPort = c.Oauth.ClientID, c.Oauth.ClientSecret, c.Oauth.CallbackPort
	}
	return s
}

// expandClaudeServer expands a server's strings; an entry with a command and
// no type is stdio, Claude's default.
func expandClaudeServer(s *ClaudeMCPServer, dir, dataDir string) {
	if s.Type == "" && s.Command != "" {
		s.Type = "stdio"
	}
	s.Command = ExpandClaude(s.Command, dir, dataDir)
	s.URL = ExpandClaude(s.URL, dir, dataDir)
	s.ClientID = ExpandClaude(s.ClientID, dir, dataDir)
	for i, a := range s.Args {
		s.Args[i] = ExpandClaude(a, dir, dataDir)
	}
	s.Env = expandClaudeMap(s.Env, dir, dataDir)
	s.Headers = expandClaudeMap(s.Headers, dir, dataDir)
}

func expandClaudeMap(m map[string]string, dir, dataDir string) map[string]string {
	for k, v := range m {
		m[k] = ExpandClaude(v, dir, dataDir)
	}
	return m
}

// claudeHooks merges the plugin's hooks: the conventional hooks/hooks.json,
// then the manifest's hooks paths and inline objects.
func claudeHooks(dir, dataDir string, raw jsontext.Value) ([]ClaudeHookGroup, error) {
	var groups []ClaudeHookGroup
	add := func(gs []ClaudeHookGroup) {
		for _, g := range gs {
			for i := range g.Matchers {
				for j := range g.Matchers[i].Hooks {
					g.Matchers[i].Hooks[j].Command = ExpandClaude(g.Matchers[i].Hooks[j].Command, dir, dataDir)
				}
			}
			groups = append(groups, g)
		}
	}
	if f := readJSONFile(filepath.Join(dir, ClaudeHooksFile)); f != nil {
		gs, err := claudeHookGroups(f)
		if err != nil {
			return nil, err
		}
		add(gs)
	}
	paths, inline, err := componentField(raw)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		f := readJSONFile(filepath.Join(dir, p))
		if f == nil {
			continue
		}
		gs, err := claudeHookGroups(f)
		if err != nil {
			return nil, err
		}
		add(gs)
	}
	for _, one := range inline {
		gs, err := claudeHookGroups(one)
		if err != nil {
			return nil, err
		}
		add(gs)
	}
	return groups, nil
}

// claudeHookGroups parses a hooks document: {"hooks": {event: [...]}} or the
// bare {event: [...]} form.
func claudeHookGroups(raw jsontext.Value) ([]ClaudeHookGroup, error) {
	var wrapped struct {
		Hooks map[string][]claudeMatcherJSON `json:"hooks"`
	}
	var byEvent map[string][]claudeMatcherJSON
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Hooks != nil {
		byEvent = wrapped.Hooks
	} else {
		var bare map[string][]claudeMatcherJSON
		if err := json.Unmarshal(raw, &bare); err != nil {
			return nil, err
		}
		byEvent = bare
	}
	var groups []ClaudeHookGroup
	for _, event := range slices.Sorted(maps.Keys(byEvent)) {
		g := ClaudeHookGroup{Event: event}
		for _, mj := range byEvent[event] {
			m := ClaudeMatcher{Matcher: mj.Matcher}
			for _, h := range mj.Hooks {
				m.Hooks = append(m.Hooks, ClaudeHook{Type: h.Type, Command: h.Command, Timeout: h.Timeout, If: h.If})
			}
			g.Matchers = append(g.Matchers, m)
		}
		groups = append(groups, g)
	}
	return groups, nil
}

type claudeMatcherJSON struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
		If      string `json:"if"`
	} `json:"hooks"`
}

// claudeVarRe matches ${NAME}, ${NAME:-default} and ${NAME:-}; the second
// group participates only when a default was written.
var claudeVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandClaude substitutes the Claude plugin variables in s: the plugin root
// and its data dir first, so the environment cannot override them, then
// ${VAR} and ${VAR:-default} from the process environment. A variable with
// no value stays literal.
func ExpandClaude(s, root, data string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", root)
	s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_DATA}", data)
	return claudeVarRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := claudeVarRe.FindStringSubmatch(match)
		if v, ok := os.LookupEnv(parts[1]); ok {
			return v
		}
		if len(parts[2]) >= 2 { // a ":-" was written, so the default counts
			return parts[3]
		}
		return match
	})
}

// SanitizeName folds a Claude name into zemi's: lowercase, runs of anything
// outside [a-z0-9-] become one '-', leading and trailing '-' trimmed. "" for
// a name with no usable rune.
func SanitizeName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}

// replaceNamed swaps a server of the same name in, else appends; the slice
// stays sorted by name.
func replaceNamed(servers []ClaudeMCPServer, s ClaudeMCPServer) []ClaudeMCPServer {
	if i := slices.IndexFunc(servers, func(x ClaudeMCPServer) bool { return x.Name == s.Name }); i >= 0 {
		servers[i] = s
		return servers
	}
	return append(servers, s)
}

// readJSONFile reads a JSON data file of a plugin, at most 64 KB, with
// readHead's regular-file guard: a clone may hold a link to /dev/zero.
func readJSONFile(path string) jsontext.Value {
	lfi, err := os.Lstat(path)
	if err != nil || !lfi.Mode().IsRegular() {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !os.SameFile(lfi, fi) {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	return jsontext.Value(data)
}

// ClaudeMarketplaceFile is the marketplace manifest inside ClaudeDir, the
// one file that makes a repository a marketplace.
const ClaudeMarketplaceFile = ClaudeDir + "/marketplace.json"

// ClaudeSource is a marketplace entry's source.
type ClaudeSource struct {
	Kind string // relative, github, git, url, git-subdir; npm, pip and anything unknown are unsupported
	URL  string // the git URL to clone; github's is built from the repo
	Dir  string // relative: the "./path" in the marketplace; git-subdir: the path inside the clone
	Ref  string
	SHA  string
}

// ClaudeMarketplaceEntry is one plugins[] entry of a marketplace.json. Its
// component fields ride as written: when the plugin dir has no plugin.json,
// the entry acts as the manifest and they pass through.
type ClaudeMarketplaceEntry struct {
	Name, Version, Description, Homepage string
	Author                               ClaudeAuthor
	Source                               ClaudeSource
	Strict                               bool // Claude defaults an unwritten strict to true

	Commands, Agents, Skills, Hooks, MCPServers jsontext.Value
}

// ClaudeMarketplace is marketplace.json parsed; unknown fields are ignored,
// Claude's own rule.
type ClaudeMarketplace struct {
	Name    string
	Owner   ClaudeAuthor
	Plugins []ClaudeMarketplaceEntry
}

// ReadClaudeMarketplace reads dir's marketplace.json. It is data-only: an
// entry whose source zemi cannot install still comes back, its kind naming
// the reason.
func ReadClaudeMarketplace(dir string) (ClaudeMarketplace, error) {
	raw := readJSONFile(filepath.Join(dir, ClaudeMarketplaceFile))
	if raw == nil {
		return ClaudeMarketplace{}, fs.ErrNotExist
	}
	var j struct {
		Name  string `json:"name"`
		Owner struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			URL   string `json:"url"`
		} `json:"owner"`
		Plugins []claudeMarketplaceEntryJSON `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return ClaudeMarketplace{}, err
	}
	m := ClaudeMarketplace{Name: j.Name, Owner: ClaudeAuthor(j.Owner)}
	for _, e := range j.Plugins {
		if e.Name == "" {
			return ClaudeMarketplace{}, errors.New("a plugin entry without a name")
		}
		src, err := claudeSource(e.Name, e.Source)
		if err != nil {
			return ClaudeMarketplace{}, err
		}
		m.Plugins = append(m.Plugins, ClaudeMarketplaceEntry{
			Name: e.Name, Version: e.Version, Description: e.Description,
			Homepage: e.Homepage, Author: ClaudeAuthor(e.Author),
			Source: src, Strict: e.Strict == nil || *e.Strict,
			Commands: e.Commands, Agents: e.Agents, Skills: e.Skills,
			Hooks: e.Hooks, MCPServers: e.MCPServers,
		})
	}
	return m, nil
}

type claudeMarketplaceEntryJSON struct {
	Name        string         `json:"name"`
	Source      jsontext.Value `json:"source"`
	Version     string         `json:"version"`
	Description string         `json:"description"`
	Homepage    string         `json:"homepage"`
	Author      struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		URL   string `json:"url"`
	} `json:"author"`
	Strict *bool `json:"strict"`

	Commands   jsontext.Value `json:"commands"`
	Agents     jsontext.Value `json:"agents"`
	Skills     jsontext.Value `json:"skills"`
	Hooks      jsontext.Value `json:"hooks"`
	MCPServers jsontext.Value `json:"mcpServers"`
}

// claudeSource parses an entry's source: a "./path" inside the marketplace
// clone, or an object naming a git URL.
func claudeSource(name string, raw jsontext.Value) (ClaudeSource, error) {
	var rel string
	if json.Unmarshal(raw, &rel) == nil {
		if !strings.HasPrefix(rel, "./") {
			return ClaudeSource{}, fmt.Errorf("plugin %s: source %q must start with %q", name, rel, "./")
		}
		return ClaudeSource{Kind: "relative", Dir: rel}, nil
	}
	var o struct {
		Source string `json:"source"`
		URL    string `json:"url"`
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return ClaudeSource{}, fmt.Errorf("plugin %s: %w", name, err)
	}
	switch o.Source {
	case "github":
		return ClaudeSource{Kind: o.Source, URL: "https://github.com/" + o.Repo, Ref: o.Ref, SHA: o.SHA}, nil
	case "git", "url":
		return ClaudeSource{Kind: o.Source, URL: o.URL, Ref: o.Ref, SHA: o.SHA}, nil
	case "git-subdir":
		return ClaudeSource{Kind: o.Source, URL: o.URL, Dir: o.Path, Ref: o.Ref, SHA: o.SHA}, nil
	case "":
		return ClaudeSource{}, fmt.Errorf("plugin %s: source names no kind", name)
	default:
		return ClaudeSource{Kind: o.Source}, nil
	}
}

// manifestJSON renders the plugin.json for a dir whose entry acts as the
// manifest: the entry's scalars and its component fields as written.
func (e ClaudeMarketplaceEntry) manifestJSON() ([]byte, error) {	var author *ClaudeAuthor
	if e.Author != (ClaudeAuthor{}) {
		author = &e.Author
	}
	return json.Marshal(struct {
		Name        string         `json:"name"`
		Version     string         `json:"version,omitempty"`
		Description string         `json:"description,omitempty"`
		Homepage    string         `json:"homepage,omitempty"`
		Author      *ClaudeAuthor  `json:"author,omitempty"`
		Commands    jsontext.Value `json:"commands,omitempty"`
		Agents      jsontext.Value `json:"agents,omitempty"`
		Skills      jsontext.Value `json:"skills,omitempty"`
		Hooks       jsontext.Value `json:"hooks,omitempty"`
		MCPServers  jsontext.Value `json:"mcpServers,omitempty"`
	}{
		Name: e.Name, Version: e.Version, Description: e.Description,
		Homepage: e.Homepage, Author: author,
		Commands: e.Commands, Agents: e.Agents, Skills: e.Skills,
		Hooks: e.Hooks, MCPServers: e.MCPServers,
	})
}

// claudeProvides summarizes what a Claude-layout plugin contributes, for
// display: the layered dirs' component counts, the MCP servers and the hook
// events. Not a Uses list - CheckUses would refuse every word here.
func claudeProvides(dir string) []string {
	m, err := ReadClaude(dir, "")
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range []struct {
		what  string
		set   bool
		paths []string
		keep  func(string) bool
	}{
		{"skill", m.SkillsSet, m.Skills, func(n string) bool { return n == "SKILL.md" }},
		{"agent", m.AgentsSet, m.Agents, mdFile},
		{"command", m.CommandsSet, m.Commands, mdFile},
	} {
		roots := c.paths
		if !c.set {
			roots = []string{filepath.Join(dir, c.what+"s")}
		}
		if n := countClaude(roots, c.keep); n > 0 {
			what := c.what
			if n > 1 {
				what += "s"
			}
			out = append(out, fmt.Sprintf("%d %s", n, what))
		}
	}
	if len(m.MCP) > 0 {
		names := make([]string, len(m.MCP))
		for i, s := range m.MCP {
			names[i] = s.Name
		}
		out = append(out, "mcp: "+strings.Join(names, ","))
	}
	if len(m.Hooks) > 0 {
		events := make([]string, len(m.Hooks))
		for i, g := range m.Hooks {
			events[i] = g.Event
		}
		out = append(out, "hooks: "+strings.Join(events, ","))
	}
	return out
}

func mdFile(name string) bool { return strings.HasSuffix(name, ".md") }

// countClaude counts the component files under roots, where a root may be a
// single file; 0 for a root that is not there.
func countClaude(roots []string, keep func(string) bool) int {
	n := 0
	for _, root := range roots {
		if fi, err := os.Lstat(root); err == nil && fi.Mode().IsRegular() {
			if keep(filepath.Base(root)) {
				n++
			}
			continue
		}
		fs.WalkDir(os.DirFS(root), ".", func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && keep(e.Name()) {
				n++
			}
			return nil
		})
	}
	return n
}
