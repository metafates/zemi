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
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
	return readHead(filepath.Join(dir, ClaudeManifestFile), headMax) != nil
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

// ReadClaudeRaw is ReadClaude with the manifest bytes given instead of read
// from the file: an entry-carried manifest, whose source dir has none.
func ReadClaudeRaw(dir, dataDir string, raw []byte) (ClaudeManifest, error) {
	return readClaude(dir, dataDir, raw)
}

func readClaude(dir, dataDir string, raw []byte) (ClaudeManifest, error) {
	var m ClaudeManifest
	var j claudeManifestJSON
	if raw != nil {
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

// forEachClaudeComponent runs add over the plugin's three component sources:
// the conventional file, the manifest's paths and its inline objects,
// stopping at the first parse error.
func forEachClaudeComponent[T any](dir, file string, raw jsontext.Value, parse func(jsontext.Value) (T, error), add func(T)) error {
	var sources []jsontext.Value
	if f := readJSONFile(filepath.Join(dir, file)); f != nil {
		sources = append(sources, f)
	}
	paths, inline, err := componentField(raw)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if f := readJSONFile(filepath.Join(dir, p)); f != nil {
			sources = append(sources, f)
		}
	}
	sources = append(sources, inline...)
	for _, src := range sources {
		v, err := parse(src)
		if err != nil {
			return err
		}
		add(v)
	}
	return nil
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
	if err := forEachClaudeComponent(dir, ClaudeMCPFile, raw, claudeMCPCfgs, add); err != nil {
		return nil, err
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
	if err := forEachClaudeComponent(dir, ClaudeHooksFile, raw, claudeHookGroups, add); err != nil {
		return nil, err
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
	s = ExpandRootData(s, root, data)
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

// ExpandRootData substitutes what a Claude plugin's own paths stand for in s:
// its root and its data dir.
func ExpandRootData(s, root, data string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", root)
	return strings.ReplaceAll(s, "${CLAUDE_PLUGIN_DATA}", data)
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

// readJSONFile reads a JSON data file of a plugin or a marketplace, at most
// 4 MB: the official marketplace's manifest is past 190 KB.
func readJSONFile(path string) jsontext.Value {
	return jsontext.Value(readHead(path, 4<<20))
}

// ClaudeMarketplaceFile is the marketplace manifest inside ClaudeDir, one of
// the two files that make a repository a marketplace.
const ClaudeMarketplaceFile = ClaudeDir + "/marketplace.json"

// ZemiMarketplaceFile is the zemi-native marketplace manifest, preferred
// over Claude's when a repository holds both.
const ZemiMarketplaceFile = CodeDir + "/marketplace.json"

// MarketplaceKind says which manifest a Marketplace was read from. It
// decides how entry names match and what a synthesized manifest looks like.
type MarketplaceKind string

const (
	KindZemi   MarketplaceKind = "zemi"
	KindClaude MarketplaceKind = "claude"
)

// SourceKind is a marketplace entry's source kind; an unknown kind rides as
// data, for the "not supported" error to name.
type SourceKind string

const (
	SourceRelative  SourceKind = "relative"
	SourceGithub    SourceKind = "github"
	SourceGit       SourceKind = "git"
	SourceURL       SourceKind = "url"
	SourceGitSubdir SourceKind = "git-subdir"
)

// ClaudeSource is a marketplace entry's source.
type ClaudeSource struct {
	Kind SourceKind // an unknown kind rides as data, for the refusal to name
	URL  string     // the git URL to clone; github's is built from the repo
	Dir  string     // relative: the "./path" in the marketplace; git-subdir: the path inside the clone
	Ref  string
	SHA  string
}

// MarketplacePlugin is one plugins[] entry of a marketplace manifest. Its
// component fields ride as written: when the plugin dir has no plugin.json,
// the entry acts as the manifest, and Kind says which file that is.
type MarketplacePlugin struct {
	Kind                                 MarketplaceKind
	Name, Version, Description, Homepage string
	Category                             string
	Author                               ClaudeAuthor
	Source                               ClaudeSource
	Strict                               bool // Claude defaults an unwritten strict to true

	Commands, Agents, Skills, Hooks, MCPServers jsontext.Value
	Uses                                        []string // the zemi manifest's capability grants
}

// Marketplace is a marketplace manifest parsed; unknown fields are ignored,
// Claude's own rule.
type Marketplace struct {
	Name    string
	Kind    MarketplaceKind
	Renames map[string]string // an old entry name -> its current one
	Plugins []MarketplacePlugin
}

// find returns the entry a user's name picks: exact in a zemi marketplace,
// whose names are already zemi's, sanitized in a Claude one.
func (m Marketplace) find(name string) (MarketplacePlugin, bool) {
	match := func(e MarketplacePlugin) bool { return e.Name == name }
	if m.Kind == KindClaude {
		want := SanitizeName(name)
		match = func(e MarketplacePlugin) bool { return SanitizeName(e.Name) == want }
		if !slices.ContainsFunc(m.Plugins, match) {
			// an old name the manifest renamed still picks its entry
			if to := m.Renames[name]; to != "" {
				name = to
				match = func(e MarketplacePlugin) bool { return SanitizeName(e.Name) == SanitizeName(to) }
			}
		}
	}
	i := slices.IndexFunc(m.Plugins, match)
	if i < 0 {
		return MarketplacePlugin{}, false
	}
	return m.Plugins[i], true
}

// installName is the name the entry installs under: a zemi entry's name is
// already zemi's, a Claude one is folded into it.
func (e MarketplacePlugin) installName() string {
	if e.Kind == KindZemi {
		return e.Name
	}
	return SanitizeName(e.Name)
}

// ReadMarketplace reads dir's marketplace manifest, the zemi-native one
// preferred over Claude's; neither present is fs.ErrNotExist. It is
// data-only: an entry whose source zemi cannot install still comes back,
// its kind naming the reason.
func ReadMarketplace(dir string) (Marketplace, error) {
	if raw := readJSONFile(filepath.Join(dir, ZemiMarketplaceFile)); raw != nil {
		return readMarketplace(raw, KindZemi)
	}
	if raw := readJSONFile(filepath.Join(dir, ClaudeMarketplaceFile)); raw != nil {
		return readMarketplace(raw, KindClaude)
	}
	return Marketplace{}, fs.ErrNotExist
}

// readMarketplace parses one manifest of either kind: one shape, the zemi
// one stricter about names and validated on uses here, at the boundary.
func readMarketplace(raw jsontext.Value, kind MarketplaceKind) (Marketplace, error) {
	var j struct {
		Name    string            `json:"name"`
		Renames map[string]string `json:"renames"`
		Owner   struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			URL   string `json:"url"`
		} `json:"owner"`
		Plugins []struct {
			Name        string         `json:"name"`
			Source      jsontext.Value `json:"source"`
			Version     string         `json:"version"`
			Description string         `json:"description"`
			Category    string         `json:"category"`
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
			Uses       []string       `json:"uses"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return Marketplace{}, err
	}
	m := Marketplace{Name: j.Name, Kind: kind, Renames: j.Renames}
	if kind == KindZemi && !nameRe.MatchString(m.Name) {
		return Marketplace{}, fmt.Errorf("%q is not a marketplace name: lowercase letters, digits, '_' and '-'", m.Name)
	}
	installs := map[string]string{} // install name -> the first entry named it
	for _, e := range j.Plugins {
		if e.Name == "" {
			return Marketplace{}, errors.New("a plugin entry without a name")
		}
		if in := (MarketplacePlugin{Kind: kind, Name: e.Name}).installName(); in != "" {
			if prev, dup := installs[in]; dup {
				return Marketplace{}, fmt.Errorf("plugins %s and %s both install as %s", prev, e.Name, in)
			}
			installs[in] = e.Name
		}
		if kind == KindZemi {
			if !nameRe.MatchString(e.Name) {
				return Marketplace{}, fmt.Errorf("%q is not a plugin name: %s", e.Name, nameWhy)
			}
			if err := CheckUses(e.Uses); err != nil {
				return Marketplace{}, err
			}
		}
		src, err := claudeSource(e.Name, e.Source)
		if err != nil {
			return Marketplace{}, err
		}
		m.Plugins = append(m.Plugins, MarketplacePlugin{
			Kind: kind, Name: e.Name, Version: e.Version, Description: e.Description,
			Category: e.Category, Homepage: e.Homepage, Author: ClaudeAuthor(e.Author),
			Source: src, Strict: e.Strict == nil || *e.Strict,
			Commands: e.Commands, Agents: e.Agents, Skills: e.Skills,
			Hooks: e.Hooks, MCPServers: e.MCPServers, Uses: e.Uses,
		})
	}
	return m, nil
}

// climbsOut reports whether a source path resolves outside the repo root.
func climbsOut(dir string) bool {
	return !filepath.IsLocal(filepath.Clean(dir))
}

// localIn guards a git-subdir path, which installs as a copy: one that
// climbs out - or is the repo root itself, whose .git a copy must not
// carry - would put foreign code in the plugin tree. A relative source at
// the root is allowed; it installs as a git-kind clone instead.
func localIn(name, dir string) error {
	if climbsOut(dir) || filepath.Clean(dir) == "." {
		return fmt.Errorf("plugin %s: source path %q must stay inside the marketplace", name, dir)
	}
	return nil
}

// claudeSource parses an entry's source: a "./path" inside the marketplace
// clone, or an object naming a git URL.
func claudeSource(name string, raw jsontext.Value) (ClaudeSource, error) {
	var rel string
	if json.Unmarshal(raw, &rel) == nil {
		if !strings.HasPrefix(rel, "./") {
			return ClaudeSource{}, fmt.Errorf("plugin %s: source %q must start with %q", name, rel, "./")
		}
		if climbsOut(rel) {
			return ClaudeSource{}, fmt.Errorf("plugin %s: source path %q must stay inside the marketplace", name, rel)
		}
		return ClaudeSource{Kind: SourceRelative, Dir: rel}, nil
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
	switch SourceKind(o.Source) {
	case SourceGithub:
		return ClaudeSource{Kind: SourceGithub, URL: "https://github.com/" + o.Repo, Ref: o.Ref, SHA: o.SHA}, nil
	case SourceGit, SourceURL:
		return ClaudeSource{Kind: SourceKind(o.Source), URL: o.URL, Ref: o.Ref, SHA: o.SHA}, nil
	case SourceGitSubdir:
		if err := localIn(name, o.Path); err != nil {
			return ClaudeSource{}, err
		}
		return ClaudeSource{Kind: SourceGitSubdir, URL: o.URL, Dir: o.Path, Ref: o.Ref, SHA: o.SHA}, nil
	case "":
		return ClaudeSource{}, fmt.Errorf("plugin %s: source names no kind", name)
	default:
		return ClaudeSource{Kind: SourceKind(o.Source)}, nil
	}
}

// manifestJSON renders the .claude-plugin/plugin.json for a dir whose entry
// acts as the manifest: the entry's scalars and its component fields as
// written.
func (e MarketplacePlugin) manifestJSON() ([]byte, error) {
	var author *ClaudeAuthor
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

// ClaudeHookEvents is the Claude hook events zemi runs; anything else a
// plugin declares is refused at load, so the display summary leaves it out.
var ClaudeHookEvents = []string{
	"PermissionDenied", "PostToolUse", "SessionEnd", "SessionStart",
	"Stop", "SubagentStart", "SubagentStop",
}

// claudeProvides summarizes what a Claude-layout plugin contributes, for
// display: the layered dirs' component counts, the MCP servers and the hook
// events. Not a Uses list - CheckUses would refuse every word here.
func claudeProvides(dir string, raw []byte) []string {
	m, err := ReadClaudeRaw(dir, "", raw)
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
		{"workflow", false, nil, luaFile},
	} {
		roots := c.paths
		if !c.set {
			// workflows layer under CodeDir like load layers them; the markdown ones sit at the root
			where := c.what + "s"
			if c.what == "workflow" {
				where = filepath.Join(CodeDir, where)
			}
			roots = []string{filepath.Join(dir, where)}
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
		var events []string
		seen := map[string]bool{}
		for _, g := range m.Hooks {
			if seen[g.Event] || !slices.Contains(ClaudeHookEvents, g.Event) {
				continue
			}
			seen[g.Event] = true
			events = append(events, g.Event)
		}
		if len(events) > 0 {
			out = append(out, "hooks: "+strings.Join(events, ","))
		}
	}
	return out
}

func mdFile(name string) bool { return strings.HasSuffix(name, ".md") }

func luaFile(name string) bool { return strings.HasSuffix(name, ".lua") }

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
		// a missing root counts nothing, which is the answer wanted here
		_ = fs.WalkDir(os.DirFS(root), ".", func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && keep(e.Name()) {
				n++
			}
			return nil
		})
	}
	return n
}
