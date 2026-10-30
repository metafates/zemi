// health.go is /health (after neovim's :checkhealth): one row per check,
// so a piece failing quietly (no API key, a build that keeps failing) is
// visible in one place.

package hostapi

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// HealthLevel is how a /health row reads.
type HealthLevel string

const (
	HealthOK    HealthLevel = "ok"
	HealthWarn  HealthLevel = "warn"
	HealthError HealthLevel = "error"
)

var healthLevels = []HealthLevel{HealthOK, HealthWarn, HealthError}

// HealthRow is one /health check.
type HealthRow struct {
	Level        HealthLevel
	Name, Detail string
	Run          string // the command the row's detail action runs
}

// healthReg holds the checks Lua registered with zemi.health.add;
// HealthRows runs them after the built-in rows.
type healthReg struct {
	mu   sync.Mutex
	list []luaHealthCheck
}

type luaHealthCheck struct {
	name   string
	fn     *glua.LFunction
	origin lua.Origin
	path   string
	gen    Gen
}

func (r *healthReg) purge() {
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

// health.add(name, fn): fn returns { level = "ok"|"warn"|"error",
// detail = "..." } or a bare detail string. A name is one row: a less
// trusted origin is refused, anyone else replaces it.
func (h *Host) healthNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name, fn := L.CheckString(1), L.CheckFunction(2)
		if strings.TrimSpace(name) == "" {
			L.RaiseError("zemi.health.add: name is required")
		}
		c := luaHealthCheck{name: name, fn: fn, origin: h.RT.CurrentOrigin(L), path: h.RT.CurrentPath(L), gen: h.currentGen()}
		reg := &h.healthReg
		reg.mu.Lock()
		var err error
		switch i := slices.IndexFunc(reg.list, func(o luaHealthCheck) bool { return o.name == name }); {
		case i < 0:
			reg.list = append(reg.list, c)
		default:
			if err = mayReplace(h.PluginOf, "health row "+name, reg.list[i].origin, reg.list[i].path, c.origin, c.path); err == nil {
				reg.list[i] = c
			}
		}
		reg.mu.Unlock()
		if err != nil {
			L.RaiseError("zemi.health.add: %v", err)
		}
		return 0
	}))
	// list() -> the rows /health shows, so config written from Lua can be
	// checked without asking the user to run /health. Registered checks run
	// inline: list is already on the interpreter goroutine, where the queue
	// luaHealthRows uses would deadlock.
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, c := range append(h.goHealthRows(), h.inlineHealthRows(L)...) {
			rows = append(rows, map[string]any{"level": c.Level, "name": c.Name, "detail": c.Detail})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// inlineHealthRows runs the registered checks on the calling state.
func (h *Host) inlineHealthRows(L *glua.LState) []HealthRow {
	h.healthReg.mu.Lock()
	list := slices.Clone(h.healthReg.list)
	h.healthReg.mu.Unlock()
	gen := h.currentGen()
	var out []HealthRow
	for _, c := range list {
		if c.gen != gen {
			continue
		}
		row := HealthRow{Level: HealthOK, Name: c.name}
		func() {
			rets, err := h.callFn(L, luaCaller{gen: c.gen, origin: c.origin, path: c.path}, c.fn, 1)
			if err != nil {
				row.Level, row.Detail = HealthError, sanitize.String(err.Error())
				return
			}
			v, err := lua.ToGo(rets[0])
			if err == nil {
				row.Level, row.Detail, err = healthResult(v)
			}
			if err != nil {
				row.Level, row.Detail = HealthError, sanitize.String(err.Error())
			}
		}()
		out = append(out, row)
	}
	return out
}

// luaHealthRows runs the registered checks through the queue, bounded by
// the veto deadline: /health is Go-side, so the checks must not run on the
// calling goroutine.
func (h *Host) luaHealthRows() []HealthRow {
	h.healthReg.mu.Lock()
	n := len(h.healthReg.list)
	h.healthReg.mu.Unlock()
	if n == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lua.VetoCeiling(n, 1))
	defer cancel()
	vals, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, "health checks", func(L *glua.LState) ([]any, error) {
		return []any{h.inlineHealthRows(L)}, nil
	})
	if err != nil || len(vals) == 0 {
		return []HealthRow{{Level: HealthError, Name: "health checks", Detail: sanitize.String(fmt.Sprint(err))}}
	}
	rows, _ := vals[0].([]HealthRow)
	return rows
}

// healthResult reads what a check returned. A missing level is ok; a level
// that is no string or is outside healthLevels is the check's misuse and
// fails it like a raise (DESIGN §6.0), so a misspelt "warning" never reads
// as ok.
func healthResult(v any) (HealthLevel, string, error) {
	switch v := v.(type) {
	case string:
		return HealthOK, sanitize.String(v), nil
	case map[string]any:
		raw, set := v["level"]
		level := HealthOK
		if set {
			s, ok := raw.(string)
			if !ok {
				return "", "", fmt.Errorf("level is not a string: want one of %s", strings.Join(strs(healthLevels), ", "))
			}
			level = HealthLevel(s)
		}
		if !slices.Contains(healthLevels, level) {
			return "", "", fmt.Errorf("level %q is not one of %s", level, strings.Join(strs(healthLevels), ", "))
		}
		detail, _ := v["detail"].(string)
		return level, sanitize.String(detail), nil
	}
	return HealthOK, "", nil
}

// oscTerms are the TERM_PROGRAM and TERM values of terminals that show OSC 9
// as a desktop notification. TERM survives ssh, TERM_PROGRAM does not.
var oscTerms = []string{"iTerm.app", "ghostty", "WezTerm", "xterm-ghostty", "xterm-kitty", "wezterm"}

// statusTerms name terminals that follow the program status protocol
// (OSC 7501) and surface a record change themselves; auto leaves the
// notifying to such a terminal instead of doubling it.
var statusTerms = []string{"rex"}

// notifiers name the native notifier per GOOS.
var notifiers = map[string]string{"darwin": "osascript", "linux": "notify-send"}

// notifyStyle resolves the ui.notify setting to the style notifyIfAway
// uses, and why. has runs only when the answer needs it: it walks PATH.
// missing is the machine-read why: the native notifier the setting names is
// not there, which /health warns about.
func notifyStyle(setting string, env func(string) string, goos string, has func(string) bool) (style, why string, missing bool) {
	tool := notifiers[goos]
	switch setting {
	case config.NotifyNative:
		if tool != "" && has(tool) {
			return config.NotifyNative, "via " + tool, false
		}
		return config.NotifyBell, cmp.Or(tool, "a notifier") + " not found", true
	case config.NotifyAuto:
	default:
		return setting, "set in config", false
	}
	remote := slices.ContainsFunc([]string{"SSH_TTY", "SSH_CONNECTION"}, func(k string) bool { return env(k) != "" })
	native := tool != "" && !remote && has(tool)
	// tmux, GNU screen and zellij drop OSC 9 unless it is passed through
	muxed := slices.ContainsFunc([]string{"TMUX", "STY", "ZELLIJ"}, func(k string) bool { return env(k) != "" })
	if !muxed {
		for _, k := range []string{"TERM_PROGRAM", "TERM"} {
			v := env(k)
			if slices.Contains(oscTerms, v) {
				return config.NotifyOSC, "auto: " + k + "=" + v, false
			}
			if slices.Contains(statusTerms, v) {
				return config.NotifyOff, "auto: " + k + "=" + v + " follows program status (OSC 7501) itself", false
			}
		}
	}
	if native {
		return config.NotifyNative, "auto: via " + tool, false
	}
	return config.NotifyBell, "auto: no OSC 9 terminal or local notifier found", false
}

// NotifyStyle is notifyStyle for this process.
func (h *Host) NotifyStyle() (style, why string, missing bool) {
	return notifyStyle(h.Settings.Str(config.KeyUINotify), os.Getenv, runtime.GOOS, func(name string) bool {
		_, err := exec.LookPath(name)
		return err == nil
	})
}

// NotifyArgv is the command that shows text as a native notification. The
// text is argv, never spliced into AppleScript, and escaped for the Pango
// markup notify-send daemons read in a body.
// NOTE: macOS credits an osascript banner to Script Editor, whose icon it
// shows and whose setting mutes it; terminal-notifier is the upgrade.
func NotifyArgv(text string) []string {
	if notifiers[runtime.GOOS] == "osascript" {
		return []string{"osascript", "-e", "on run argv", "-e", `display notification (item 1 of argv) with title "zemi"`, "-e", "end run", text}
	}
	text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	return []string{"notify-send", "-a", "zemi", "--", "zemi", text}
}

// builderRow is the /health row of the summary tree's builder.
func (h *Host) builderRow() HealthRow {
	unbuilt := h.chat.tree.health.unbuilt.Load()
	failed := h.chat.tree.health.failed.Load()
	queued := h.chat.tree.health.queued.Load()
	detail := fmt.Sprintf("%d lines still placeholders, %d %s queued", unbuilt, queued, Plural(int(queued), "build", "builds"))
	how, cooling := "they retry when new messages land", false
	if until := h.chat.tree.health.coolingUntil.Load(); until > 0 {
		if left := time.Until(time.Unix(0, until)); left > 0 {
			how, cooling = fmt.Sprintf("the provider is cooling down, the next try is in %s", left.Round(time.Second)), true
		}
	}
	switch {
	case failed > 0:
		// a cooldown's failures are the rate limit's, not /errors': the row
		// itself carries the wait they are on
		if !cooling {
			how += " (see /errors)"
		}
		return HealthRow{Level: HealthWarn, Detail: fmt.Sprintf("%d builds failed, %s; %s", failed, detail, how)}
	case unbuilt > 4*leafWaitLines:
		return HealthRow{Level: HealthWarn, Detail: "the view holds " + detail + "; zoom still reads the raw messages"}
	}
	return HealthRow{Level: HealthOK, Detail: detail}
}

// goHealthRows are the Go-side checks, in the order /health shows them.
func (h *Host) goHealthRows() []HealthRow {
	var out []HealthRow
	add := func(level HealthLevel, name, detail string, run ...string) {
		row := HealthRow{Level: level, Name: name, Detail: detail}
		if len(run) > 0 {
			row.Run = run[0]
		}
		out = append(out, row)
	}

	if h.DB == nil || h.GlobalDB == nil {
		add(HealthError, "store", fmt.Sprintf("the chat log and run storage unavailable: %v", h.initErr))
	} else {
		add(HealthOK, "store", "the chat log and run storage on disk")
	}

	var names []string
	for _, s := range h.Providers.List() {
		switch {
		case !h.Providers.HasKey(s.ID): // a spec without one: openai_compat runs, the rest fail their first turn
			names = append(names, s.ID+" (no key)")
		case s.Builtin:
			names = append(names, s.ID+" (builtin)")
		default:
			names = append(names, s.ID)
		}
	}
	if len(names) == 0 {
		add(HealthWarn, "providers", "none usable")
	} else if n := len(h.Providers.Missing()); n > 0 {
		add(HealthOK, "providers", fmt.Sprintf("%s; %d builtin without a key exported", strings.Join(names, ", "), n))
	} else {
		add(HealthOK, "providers", strings.Join(names, ", "))
	}
	if info := h.Builtin.Load().Info; info.Err != nil {
		add(HealthWarn, "catalog", "the saved catalog did not load: "+info.Err.Error()+"; "+info.String()+" in use")
	} else {
		add(HealthOK, "catalog", info.String())
	}

	id := h.sessionModelID()
	out = append(out, h.modelRow())
	// a target's own model would otherwise fail only at its first run or
	// summary, where nobody looks
	ids := []string{id}
	for _, name := range h.targetNames()[1:] {
		m := h.roleModel(name)
		if !slices.Contains(ids, m) {
			ids = append(ids, m)
			if row := h.modelCheck(name+" model", m, h.classifies(name)); row.Level != HealthOK {
				out = append(out, row)
			}
		}
	}

	// the chat's context is the persisted view's, bounded by the builder; a
	// model's window sizes the /usage read and a subagent's history cap
	var unknown []string
	if id != "" && h.contextWindow(id) == 0 {
		unknown = append(unknown, id+": no context percent in /usage")
	}
	if m := h.roleModel("agents"); m != "" && m != id && h.contextWindow(m) == 0 {
		unknown = append(unknown, m+": subagents trim their history only after the provider refuses it")
	}
	if len(unknown) > 0 {
		add(HealthWarn, "context", "window unknown for "+strings.Join(unknown, "; ")+"; set context_window on the model entry")
	} else if id != "" {
		add(HealthOK, "context", "window known; the summary view keeps the chat's prompt bounded, subagents trim their history by it")
	}

	if _, err := exec.LookPath("git"); err != nil {
		add(HealthWarn, "git", "not found; the prompt has no git status, glob, grep and @ completion read no .gitignore, and git commands that print file content ask")
	} else {
		add(HealthOK, "git", "found")
	}

	add(HealthOK, "shell", proc.Shell())

	switch style, why, missing := h.NotifyStyle(); {
	case style == config.NotifyOff:
		add(HealthOK, "notify", "off")
	case missing:
		add(HealthWarn, "notify", "native wanted, bell used: "+why)
	default:
		add(HealthOK, "notify", style+", "+why+"; only while zemi is unfocused")
	}

	// the builder row: what the summary tree has and owes, so a dead builder
	// is visible in one place (risk 4 of the plan)
	if h.DB == nil {
		add(HealthWarn, "chat builder", "no store: the chat runs with no summary tree")
	} else {
		row := h.builderRow()
		add(row.Level, "chat builder", row.Detail)
	}

	// the project's own skills are always on and never in the registry, so
	// counting only the registry would disagree with the prompt
	proj := len(h.projectOnlySkills())
	msg := fmt.Sprintf("%d found, %d enabled", len(h.skillsReg.list())+proj, len(h.enabledSkills()))
	if proj > 0 {
		msg += fmt.Sprintf(" (%d from this project)", proj)
	}
	add(HealthOK, "skills", msg)

	if h.RT.IsHot() {
		add(HealthError, "lua", "stopped: a boot step failed or a call wedged; see /errors, then /reload")
	} else {
		add(HealthOK, "lua", "runtime healthy")
	}

	// a widget in a region no node draws is the first thing a relayout breaks
	drawn := h.Layout().regions()
	var undrawn []string
	for _, w := range h.Widgets.List() {
		if !drawn.Has(w.Region) {
			undrawn = append(undrawn, w.ID+" ("+w.Region+")")
		}
	}
	if len(undrawn) > 0 {
		add(HealthWarn, "layout", "the layout draws no region for: "+strings.Join(undrawn, ", "))
	} else {
		add(HealthOK, "layout", "every widget's region is drawn")
	}

	var loads []pluginLoad
	if p := h.pluginLoads.Load(); p != nil {
		loads = *p
	}
	failed := 0
	for _, l := range loads {
		if l.err != nil {
			failed++
		}
	}
	switch n := fmt.Sprintf("%d %s", len(loads), Plural(len(loads), "plugin", "plugins")); {
	case len(loads) == 0:
		add(HealthOK, "plugins", "none on", "plugins")
	case failed == 0:
		add(HealthOK, "plugins", n+" ready", "plugins")
	default:
		add(HealthWarn, "plugins", fmt.Sprintf("%s: %d failed, %d ready", n, failed, len(loads)-failed), "plugins")
	}

	h.errs.mu.Lock()
	nerr := len(h.errs.entries)
	var shown errEntry
	boot := false
	for _, e := range h.errs.entries {
		if strings.HasPrefix(e.source(), "boot") {
			shown, boot = e, true
		}
	}
	if nerr > 0 && !boot {
		shown = h.errs.entries[nerr-1]
	}
	h.errs.mu.Unlock()
	if nerr > 0 {
		// name one of them: a boot step that failed is an error, so the
		// checks do not read green over it
		level := HealthWarn
		if boot {
			level = HealthError
		}
		add(level, "errors", fmt.Sprintf("%d since start, %s: %s", nerr, shown.source(), Headline(shown.msg)), "errors")
	} else {
		add(HealthOK, "errors", "none since start")
	}

	if h.ProjectTrusted() {
		add(HealthOK, "trust", "trusted: "+perm.TildePath(h.Dirs.TrustRoot()), "trust")
	} else {
		add(HealthWarn, "trust", "untrusted: the project's config, instructions and language servers are off; /trust (or zemi --trust for one run)", "trust")
	}

	counts, servers := map[ServiceStatus]int{}, h.mcp.list()
	for _, s := range servers {
		s.mu.Lock()
		counts[s.status]++
		s.mu.Unlock()
	}
	switch {
	case len(servers) == 0:
		add(HealthOK, "mcp", "no servers added")
	case counts[StatusReady] == len(servers):
		add(HealthOK, "mcp", fmt.Sprintf("%d %s ready", len(servers), Plural(len(servers), "server", "servers")), "mcp")
	default:
		var parts []string
		for _, st := range slices.Sorted(maps.Keys(counts)) {
			n := counts[st]
			part := fmt.Sprintf("%d %s", n, st)
			if st == StatusSignIn { // the phrase agrees with its count
				part = fmt.Sprintf("%d %s sign-in", n, Plural(n, "needs", "need"))
			}
			parts = append(parts, part)
		}
		level := HealthWarn
		if counts[StatusReady]+counts[StatusStarting] == len(servers) {
			level = HealthOK
		}
		add(level, "mcp", fmt.Sprintf("%d %s: %s", len(servers), Plural(len(servers), "server", "servers"), strings.Join(parts, ", ")), "mcp")
	}

	var lsps []string
	for _, row := range h.lspRows() {
		st := string(row.Status)
		if row.Status == StatusIdle {
			st = "idle until a file it serves"
		}
		lsps = append(lsps, fmt.Sprintf("%s (%s)", row.Name, st))
	}
	switch {
	case len(lsps) > 0:
		add(HealthOK, "lsp", strings.Join(lsps, ", "))
	case !h.ProjectTrusted():
		add(HealthOK, "lsp", "the project's servers are off: untrusted (/trust)")
	default:
		add(HealthOK, "lsp", "none for this project: install "+builtinCommands()+", or zemi.lsp.add one")
	}
	return out
}

// modelRow is the conversation's model check. A reload toasts it, so a
// models.default nobody can run is not silent.
func (h *Host) modelRow() HealthRow { return h.modelCheck("model", h.sessionModelID(), false) }

// modelCheck is what a call on id would fail with, or an id the provider's
// list does not carry (a typo runs until the provider refuses it). classify is
// a classify slot, the one kind a decisions model is for: it checks that a
// call reaches the model, not that a turn could run on it.
func (h *Host) modelCheck(name, id string, classify bool) HealthRow {
	err := h.modelErr(id)
	if classify {
		err = h.modelRunErr(id)
	}
	_, known := h.modelSpec(id)
	switch {
	case err != nil:
		return HealthRow{Level: HealthError, Name: name, Detail: err.Error()}
	case !known:
		return HealthRow{Level: HealthWarn, Name: name, Detail: unlistedDetail(id)}
	}
	return HealthRow{Level: HealthOK, Name: name, Detail: id}
}

// unlistedDetail is the warning for an id the provider's models list
// does not carry: the pick is taken, since the provider decides.
func unlistedDetail(id string) string {
	return id + ": not in the provider's models list; the provider decides whether it exists"
}

// HealthRows runs every check; each has side effects and file IO, so a
// caller runs them once.
func (h *Host) HealthRows() []HealthRow {
	return append(h.goHealthRows(), h.luaHealthRows()...)
}

// WaitMCP returns once no MCP server is starting, or when ctx ends. A
// command-line run calls it first: its one send builds the tool set, and a
// server that is not ready then has no tools in it.
func (h *Host) WaitMCP(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // startMCP's own cap
	defer cancel()
	for slices.ContainsFunc(h.mcp.list(), func(s *mcpServer) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.status == StatusStarting
	}) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// healthActions labels the command a check's detail view runs; its initial
// is the key.
var healthActions = map[string]string{"errors": "Show errors", "trust": "Change trust", "mcp": "Open MCP servers", "plugins": "Open plugins"}

// healthAction is what a check's Run resolves to. A run the table does not
// label takes no action rather than reading a missing label.
func healthAction(run string) (label, key string, ok bool) {
	label, ok = healthActions[run]
	if !ok || label == "" {
		return "", "", false
	}
	return label, strings.ToLower(label[:1]), true
}

// OpenHealth shows the checks in the list overlay.
func (h *Host) OpenHealth() error {
	ui, err := h.Screen()
	if err != nil {
		return fmt.Errorf("%w; zemi health prints the checks", err)
	}
	// A Go command runs on the key loop, and the checks do file IO and call
	// Lua under the veto deadline: collect them off it, then open.
	h.GoSafe("health", func() {
		items := func() []OverlayItem {
			defer h.Busy("Checking health", nil)()
			return h.healthItems()
		}()
		ui.OpenOverlay(Overlay{ID: "health", Title: "Health", Origin: lua.OriginBuiltin, Items: items})
	})
	return nil
}

// healthMarks is a check's level as the mark the transcript uses for the same
// state, so /health reads in the one state vocabulary the product has.
var healthMarks = map[HealthLevel]MarkKind{HealthOK: tools.MarkOK, HealthWarn: tools.MarkWarn, HealthError: tools.MarkErr}

// healthItems are the /health rows, problems first.
func (h *Host) healthItems() []OverlayItem {
	var problems, ok []OverlayItem
	for _, c := range h.HealthRows() {
		item := OverlayItem{Text: c.Name, Desc: c.Detail, Section: "OK", Mark: healthMarks[c.Level]}
		// a detail view that only repeats a short, fine row leads nowhere
		if c.Run != "" || c.Level != HealthOK || utf8.RuneCountInString(c.Detail) > 48 {
			item.Detail = c.Detail
		}
		if c.Run != "" {
			if label, key, ok := healthAction(c.Run); ok {
				item.Keys = []ItemKey{{Key: key, Desc: label, Run: c.Run}}
			}
		}
		if c.Level == HealthOK {
			ok = append(ok, item)
			continue
		}
		item.Section = "Needs attention"
		problems = append(problems, item)
	}
	return append(problems, ok...)
}
