---@meta
---
--- Type annotations for zemi's Lua API, in the LuaLS (lua-language-server)
--- format. zemi rewrites this file on every start, so do not edit it: it is
--- a description of the API, never part of it. See the zemi-lua skill for
--- the rules behind the shapes.
---
--- zemi runs Lua 5.1 (gopher-lua) in a sandbox: no `io`, no `os.execute`,
--- no C modules. Filesystem, network and processes go through `zemi.fs`,
--- `zemi.http` and `zemi.job`, each behind the permission engine.
---
--- Error convention: misuse (a bad argument, a call an origin may not make)
--- raises; a runtime failure of a synchronous call returns `nil, err`;
--- asynchronous work reports through its callback or handle.

-- ---------------------------------------------------------------------------
-- The sandboxed standard library. The .luarc.json zemi writes turns off
-- LuaLS's own basic, io, os and debug libraries, so these are the globals an
-- editor offers; string, table, math, coroutine and require are LuaLS's.

_G = {}

---@type string
_VERSION = "Lua 5.1"

---@generic T
---@param v? T
---@param message? any
---@return T
function assert(v, message, ...) end

---@param opt? "collect"|"stop"|"restart"|"count"|"step"
---@param arg? integer
---@return any
function collectgarbage(opt, arg) end

---@param message any
---@param level? integer
function error(message, level) end

---@param object any
---@return table|nil metatable
function getmetatable(object) end

---@generic T: table, V
---@param t T
---@return fun(table: V[], i?: integer): integer, V
---@return T
---@return integer i
function ipairs(t) end

--- Compile a chunk; it runs under the caller's origin.
---@param func fun(): string?
---@param chunkname? string
---@return function? fn
---@return string? err
function load(func, chunkname) end

--- Compile a string; it runs under the caller's origin.
---@param text string
---@param chunkname? string
---@return function? fn
---@return string? err
function loadstring(text, chunkname) end

---@generic K, V
---@param table table<K, V>
---@param index? K
---@return K?
---@return V?
function next(table, index) end

---@generic T: table, K, V
---@param t T
---@return fun(table: table<K, V>, index?: K): K, V
---@return T
function pairs(t) end

---@param f function
---@return boolean ok
---@return any ...
function pcall(f, ...) end

--- Writes to zemi's log file, tagged with the caller's origin.
function print(...) end

---@param table table
---@param index any
---@return any
function rawget(table, index) end

---@param index integer|"#"
---@return any
function select(index, ...) end

--- In plugin and project code a handler must be that code's own Lua function (for a plugin,
--- one of its own plugin), and the handlers are read at the call.
---@generic T: table
---@param table T
---@param metatable? table
---@return T
function setmetatable(table, metatable) end

---@param e any
---@param base? integer
---@return number?
function tonumber(e, base) end

---@param v any
---@return string
function tostring(v) end

---@alias type "nil"|"number"|"string"|"boolean"|"table"|"function"|"thread"|"userdata"

---@param v any
---@return type
function type(v) end

---@generic T
---@param list T[]
---@param i? integer
---@param j? integer
---@return T ...
function unpack(list, i, j) end

---@param f function
---@param msgh function
---@return boolean ok
---@return any ...
function xpcall(f, msgh) end

---@param proxy boolean|userdata
---@return userdata
function newproxy(proxy) end

---@deprecated The sandbox removes dofile; use require.
function dofile(...) end

---@deprecated The sandbox removes loadfile; use require.
function loadfile(...) end

---@deprecated The sandbox removes getfenv.
function getfenv(...) end

---@deprecated The sandbox removes setfenv.
function setfenv(...) end

---@deprecated The sandbox removes rawset: zemi tables are frozen.
function rawset(...) end

---@deprecated The sandbox removes rawequal.
function rawequal(...) end

---@deprecated The sandbox removes module; return a table from the file instead.
function module(...) end

---@class sandbox.os
os = {}

--- Seconds since zemi started.
---@return number
function os.clock() end

--- Unix seconds now, or of the date `t` describes.
---@param t? { year: integer, month: integer, day: integer, hour?: integer, min?: integer, sec?: integer }
---@return integer
function os.time(t) end

--- The date as text in a strftime `format` ("%c" by default; a leading "!"
--- means UTC), or as a table with "*t".
---@param format? string
---@param time? integer Unix seconds; now by default.
---@return string|{ year: integer, month: integer, day: integer, hour: integer, min: integer, sec: integer, wday: integer, yday: integer, isdst: boolean }
function os.date(format, time) end

---@class sandbox.debug
debug = {}

---@param msg? string
---@return string
function debug.traceback(msg) end

---@alias zemi.Origin
---| "user"     # ~/.config/zemi
---| "builtin"  # shipped with zemi
---| "plugin"   # <data>/plugins/{installed,local}/<name>; code runs as the least trusted function on its stack
---| "project"  # <repo>/.agents

---@alias zemi.Level "debug"|"info"|"warn"|"error"
---@alias zemi.HealthLevel "ok"|"warn"|"error"
---@alias zemi.Scope "user"|"project"
--- The regions the default layout draws; a layout may draw any other name.
---@alias zemi.Region "header"|"chat"|"working"|"statusline.left"|"statusline.right"|string

--- One styled run of text in a widget or overlay row. `hl` names a
--- highlight group; omit it for the default foreground.
---@class zemi.Segment
---@field text string
---@field hl? string
---@field click? string Widget rows only: the id a click passes to the widget's `on_click`.
---@field effect? "shimmer" A bright band sweeps across the text every frame, blending `hl` toward Normal (static with ui.animations off).
---@field copy? boolean `false` marks decoration (a gutter bar, a bullet, a border, a line number): a selection neither lights nor copies it.
---@field hover? boolean `false` marks a clickable body row (a diff line, a detail line): the pointer still shows a click acts, but hovering it restyles nothing.
---@field soft_wrap? boolean The line continues on the next one: a copy joins the two and drops what follows this segment on the row. Go's own wrapping sets it.

--- A widget row: the segments it is drawn from, left to right.
---@alias zemi.Line zemi.Segment[]

zemi = {}

-- ---------------------------------------------------------------------------
-- zemi.api

---@class zemi.api
---@field version string The API version, bumped whenever the surface grows.
zemi.api = {}

-- ---------------------------------------------------------------------------
-- zemi.config

---@class zemi.SettingSchema
---@field path string Dotted key, e.g. "ui.theme".
---@field kind string "string", "bool", "int" or "float".
---@field default any
---@field desc string

---@class zemi.config
zemi.config = {}

--- Read a setting, or nil when it is not set and has no default.
---@param path string
---@return any
function zemi.config.get(path) end

--- Write a setting. User config only; raises for an unknown path or a wrong
--- kind. A reload that writes the same value as the last
--- load leaves the setting alone, so a runtime pick (/model, /effort) stays.
---@param path string
---@param value any
function zemi.config.set(path, value) end

--- Call fn whenever a setting changes. A path outside the schema raises.
---@param path string
---@param fn fun(new: any, old: any)
---@return string watch_id
function zemi.config.watch(path, fn) end

--- Drop a watch by the id `zemi.config.watch` returned.
---@param id string
---@return boolean removed
function zemi.config.unwatch(id) end

--- Every known setting with its default.
---@return zemi.SettingSchema[]
function zemi.config.schema() end

--- Replay the boot steps, so config just written takes effect. Same as
--- /reload. It runs once the caller returns, so what follows the call still
--- runs. Refused while config is still loading. User or builtin origin only.
---@return true|nil ok
---@return string|nil err
function zemi.config.reload() end

-- ---------------------------------------------------------------------------
-- zemi.keymap

---@class zemi.KeymapOpts
---@field desc? string Shown in /help.
---@field focus? string A focusable layout node id: the binding works only while that node has the keys (zemi.ui.focus), and there it may start with a printable key ("j", "g g").

---@class zemi.Keymap
---@field key string
---@field desc string
---@field origin zemi.Origin
---@field action string "/command", a host action, or "<function>".
---@field focus? string The node it is scoped to; nil for a global binding.

--- The host actions a keymap may name; some take an argument after a space.
---@alias zemi.HostAction
---| "zemi.ui.help"
---| "zemi.ui.reasoning.toggle"
---| "zemi.ui.scroll.up"
---| "zemi.ui.scroll.down"
---| "zemi.ui.scroll.top"
---| "zemi.ui.scroll.bottom"
---| "zemi.ui.scroll.line_up"
---| "zemi.ui.scroll.line_down"
---| "zemi.ui.focus.set"
---| "zemi.ui.focus.next"
---| "zemi.ui.focus.prev"
---| "zemi.model.pick"
---| "zemi.composer.submit"
---| "zemi.composer.insert"
---| "zemi.composer.history_prev"
---| "zemi.composer.history_next"
---| "zemi.composer.paste"
---| "zemi.composer.edit"
---| "zemi.permission.mode.next"
---| "zemi.permission.mode.prev"
---| "zemi.agent.profile.next"
---| "zemi.agent.profile.prev"

---@class zemi.keymap
zemi.keymap = {}

--- Bind a key or a key sequence. The right-hand side is a function, a
--- command name with or without its slash ("review", "/review", so what
--- `get` hands back rebinds as is), or a host action ("zemi.ui.scroll.up"), which may take an
--- argument after a space ("zemi.ui.scroll.up sidebar" scrolls the layout
--- node with that id; without one, the stage's scrolled region). Keys may be
--- sequences that start with a non-printable key: "ctrl+x s", "<leader>gs"
--- (`<leader>` is the `keymap.leader` setting, ctrl+x by default). A key the
--- terminal never reports ("ctrl-q", "control+q") or zemi keeps (ctrl+c)
--- raises rather than binding something nothing can press. Setting
--- the same key again from the same origin rebinds it; a key (or a prefix
--- or extension of a key sequence) a more trusted origin bound raises.
--- With `opts.focus` the binding is scoped to a focusable node: while it has
--- the keys its bindings come first, then the global ones, unless a more
--- trusted global binding holds the key. It may not take a key a more
--- trusted global binding holds. The composer's own editing keys (ctrl+a,
--- ctrl+e, ctrl+k, ctrl+u, ctrl+w, home, end) bind like any other, and the
--- composer then no longer gets them.
---
--- Host actions: zemi.composer.{submit,history_prev,history_next,paste,edit},
--- zemi.composer.insert <text> (these user config only),
--- zemi.ui.help, zemi.ui.reasoning.toggle, zemi.model.pick,
--- zemi.ui.scroll.{up,down,top,bottom} [id], zemi.ui.focus.set [id],
--- zemi.ui.focus.next, zemi.ui.focus.prev, zemi.permission.mode.{next,prev}
--- and zemi.agent.profile.{next,prev} (user config only).
---@param key string
---@param action zemi.HostAction|string|fun()
---@param opts? zemi.KeymapOpts
function zemi.keymap.set(key, action, opts) end

--- Unbind a key the caller's origin or a less trusted one set. A key
--- nothing bound is `false`, not an error, so a reload unbinds idempotently.
---@param key string
---@param opts? { focus?: string }
---@return boolean removed
function zemi.keymap.del(key, opts) end

--- The binding a key runs, while `opts.focus` has the keys when given.
---@param key string
---@param opts? { focus?: string }
---@return zemi.Keymap|nil
function zemi.keymap.get(key, opts) end

---@return zemi.Keymap[]
function zemi.keymap.list() end

-- ---------------------------------------------------------------------------
-- zemi.command

--- What a command handler receives: the raw argument string, with piped
--- stdin attached behind what was typed, and the typed words.
---@class zemi.CommandArgs
---@field args string
---@field list string[]

---@class zemi.CommandOpts
---@field desc? string Shown in /help and the slash palette.
---@field nargs? ""|"?"|"*"|"+" Arguments the command takes: "" none (the default), "?" at most one, "+" one or more, "*" any. A line with the wrong count is refused and the handler never runs.
---@field complete? fun(lead: string, line: string): string[]|zemi.CompletionItem[] Runs off the key path; the palette keeps the rows that start with `lead`, and is not called past the last argument `nargs` takes. An error lands in /errors.

---@class zemi.Command
---@field name string
---@field desc string
---@field nargs string
---@field origin zemi.Origin

---@class zemi.command
zemi.command = {}

--- Register a slash command. The name is typed without the slash.
---@param name string
---@param fn fun(args: zemi.CommandArgs)
---@param opts? zemi.CommandOpts
function zemi.command.add(name, fn, opts) end

---@param name string
---@return boolean removed
function zemi.command.del(name) end

--- Run a registered command as if it were typed.
---@param name string
---@param args? string
---@return true|nil ok
---@return string|nil err Not a command, bad arguments, the command's own error, or a command the caller's origin may not run (a Go command below builtin, a more trusted origin's Lua command).
function zemi.command.run(name, args) end

---@return zemi.Command[]
function zemi.command.list() end

-- ---------------------------------------------------------------------------
-- zemi.event

---@alias zemi.EventName
---| "app.ready" | "app.quit" | "config.reloaded"
---| "session.new" | "session.open" | "session.close"
---| "agent.start" | "agent.step" | "agent.finish" | "agent.error" | "agent.assemble"
---| "agent.subagent.start" | "agent.subagent.finish"
---| "agent.text_delta" | "agent.reasoning_delta"
---| "tool.pre" | "tool.post" | "tool.denied"
---| "permission.request" | "permission.resolved"
---| "ui.resize" | "ui.theme"
---| "composer.change" | "composer.submit"
---| "memory.written" | "reflector.start" | "reflector.done"
---| "job.exit" | "lsp.request"

--- The payload every handler receives, plus the event's own fields.
---@class zemi.Event
---@field name string
---@field from string "host" or the origin that emitted it.

-- NOTE: the payload classes are kept by hand from the EmitAsync and
-- EmitVetoable calls in internal/; only the event names are pinned by a test.

---@class zemi.ev.Session: zemi.Event
---@field id string
---@field title? string Not on session.close.

---@class zemi.ev.Text: zemi.Event
---@field text string agent.start: the message; the deltas: the new text.

---@class zemi.ev.AgentError: zemi.Event
---@field error string

---@class zemi.ev.AgentFinish: zemi.Event
---@field session string
---@field model string
---@field usage zemi.Usage This turn's own model calls; subagents and a step resent after compaction are only in `zemi.agent.usage()`.

--- One model call of a turn, a subagent's included.
---@class zemi.ev.AgentStep: zemi.Event
---@field session string
---@field model string
---@field step integer 1-based within the send.
---@field ms integer From the call's start to its end.
---@field first_token_ms integer To the first text, reasoning or tool argument; 0 when none came.
---@field tools integer Tool calls it made.
---@field usage { in: integer, out: integer, cache_read: integer, cache_write: integer, cost: number }

---@class zemi.ev.Assemble: zemi.Event
---@field cwd string

---@class zemi.ev.Subagent: zemi.Event
---@field agent string
---@field session string The run's child session.
---@field description? string agent.subagent.start only.
---@field error? string agent.subagent.finish only: "" when it succeeded.

---@class zemi.ev.ToolPost: zemi.Event
---@field tool string
---@field input table
---@field ok boolean
---@field output string The full text the model gets, as the handler before this one left it.
---@field ms integer The call's time from the permission gate, an ask's wait included.
---@field changed string[] Absolute paths of the files the call changed: the file a write or edit wrote, and what a bash command changed in its repository (ignored files and other tools are not tracked). Commands running at the same time each list the other's changes too.

---@class zemi.ev.ToolDenied: zemi.Event
---@field tool string
---@field subject string
---@field arg string
---@field by string Who decided, such as "user" or "mode:auto".
---@field reason string

---@class zemi.ev.PermissionRequest: zemi.Event
---@field subject string
---@field arg string
---@field reason? string
---@field post_hoc? boolean A Lua API call already refused for want of an ask; a veto changes nothing.

---@class zemi.ev.PermissionResolved: zemi.Event
---@field subject string
---@field arg string
---@field decision "allow"|"deny"
---@field by string

---@class zemi.ev.Resize: zemi.Event
---@field width integer
---@field height integer

---@class zemi.ev.Theme: zemi.Event
---@field theme string

---@class zemi.ev.ComposerChange: zemi.Event
---@field text string
---@field cursor integer

---@class zemi.ev.ComposerSubmit: zemi.Event
---@field text string

---@class zemi.ev.MemoryWritten: zemi.Event
---@field id string
---@field scope zemi.Scope
---@field origin "user"|"learned"
---@field text string

---@class zemi.ev.ReflectorStart: zemi.Event
---@field session string

---@class zemi.ev.ReflectorDone: zemi.Event
---@field session string
---@field error? string Set when the run failed, and then no counts are.
---@field stored? integer
---@field queued? integer Proposals waiting in /memory.
---@field superseded? integer
---@field skipped? integer

---@class zemi.ev.JobExit: zemi.Event
---@field id integer
---@field cmd string
---@field code integer

---@class zemi.ev.LspRequest: zemi.Event
---@field server string
---@field path string
---@field kind "diagnostics"|"tool"

--- What a vetoable handler returns to veto: `decision = "deny"` with a
--- reason. Any other return lets the event through.
---@class zemi.Veto
---@field decision "deny"
---@field reason? string

---@class zemi.EventOpts
---@field id? string Name the handler so `zemi.event.off` can drop it.
---@field once? boolean Drop the handler after it fires.

---@class zemi.event
zemi.event = {}

--- Subscribe to an event. Vetoable events (`permission.request`,
--- `app.quit`, `composer.submit`, `tool.pre`) give the handler 50 ms and
--- then take their logged default; return `{ decision = "deny", reason = "..." }`
--- to veto one (`tool.pre` also takes "halt" and "rewrite", see zemi.ToolOverride).
--- A `tool.post` handler runs as a task and may return `{ output = "..." }`
--- to replace what the model gets.
--- `app.quit` vetoes from project code are ignored.
---@param name zemi.EventName|string A catalogued name or `user.*`; anything else raises.
---@param fn fun(ev: zemi.Event): any
---@param opts? zemi.EventOpts
---@return string id
---@overload fun(name: "session.new"|"session.open"|"session.close", fn: (fun(ev: zemi.ev.Session): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.start"|"agent.text_delta"|"agent.reasoning_delta", fn: (fun(ev: zemi.ev.Text): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.finish", fn: (fun(ev: zemi.ev.AgentFinish): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.error", fn: (fun(ev: zemi.ev.AgentError): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.assemble", fn: (fun(ev: zemi.ev.Assemble): string|{ title?: string, text: string }|nil), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.subagent.start"|"agent.subagent.finish", fn: (fun(ev: zemi.ev.Subagent): any), opts?: zemi.EventOpts): string
---@overload fun(name: "tool.pre", fn: (fun(ev: zemi.ToolPreEvent): zemi.ToolVeto?), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.step", fn: (fun(ev: zemi.ev.AgentStep): any), opts?: zemi.EventOpts): string
---@overload fun(name: "tool.post", fn: (fun(ev: zemi.ev.ToolPost): zemi.ToolPostResult?), opts?: zemi.EventOpts): string
---@overload fun(name: "tool.denied", fn: (fun(ev: zemi.ev.ToolDenied): any), opts?: zemi.EventOpts): string
---@overload fun(name: "permission.request", fn: (fun(ev: zemi.ev.PermissionRequest): zemi.Veto?), opts?: zemi.EventOpts): string
---@overload fun(name: "permission.resolved", fn: (fun(ev: zemi.ev.PermissionResolved): any), opts?: zemi.EventOpts): string
---@overload fun(name: "app.quit", fn: (fun(ev: zemi.Event): zemi.Veto?), opts?: zemi.EventOpts): string
---@overload fun(name: "ui.resize", fn: (fun(ev: zemi.ev.Resize): any), opts?: zemi.EventOpts): string
---@overload fun(name: "ui.theme", fn: (fun(ev: zemi.ev.Theme): any), opts?: zemi.EventOpts): string
---@overload fun(name: "composer.change", fn: (fun(ev: zemi.ev.ComposerChange): any), opts?: zemi.EventOpts): string
---@overload fun(name: "composer.submit", fn: (fun(ev: zemi.ev.ComposerSubmit): zemi.Veto?), opts?: zemi.EventOpts): string
---@overload fun(name: "memory.written", fn: (fun(ev: zemi.ev.MemoryWritten): any), opts?: zemi.EventOpts): string
---@overload fun(name: "reflector.start", fn: (fun(ev: zemi.ev.ReflectorStart): any), opts?: zemi.EventOpts): string
---@overload fun(name: "reflector.done", fn: (fun(ev: zemi.ev.ReflectorDone): any), opts?: zemi.EventOpts): string
---@overload fun(name: "job.exit", fn: (fun(ev: zemi.ev.JobExit): any), opts?: zemi.EventOpts): string
---@overload fun(name: "lsp.request", fn: (fun(ev: zemi.ev.LspRequest): any), opts?: zemi.EventOpts): string
function zemi.event.on(name, fn, opts) end

---@param id string
---@return boolean removed
function zemi.event.off(id) end

--- Emit a `user.*` event, the only names Lua may emit from any origin.
--- Handlers get a copy made the way JSON copies: a table that is not a
--- sequence arrives with string keys (`{ [7] = "x" }` reads as `ev["7"]`).
---@param name string
---@param payload? table
function zemi.event.emit(name, payload) end

---@class zemi.EventLogOpts
---@field name? string Only this event.
---@field limit? integer At most this many.

---@class zemi.EventLogEntry: zemi.Event
---@field at string "15:04:05".
---@field ts number Unix seconds.

--- Recent events newest first, each the table its handlers saw (or would
--- have: events are logged whether or not anything listens), the last 500.
--- The streaming deltas are counted in stats only.
---@param opts? zemi.EventLogOpts
---@return zemi.EventLogEntry[]
function zemi.event.log(opts) end

--- Emissions per event name since zemi started; diff two snapshots for a
--- session or a turn.
---@return table<string, integer>
function zemi.event.stats() end

-- ---------------------------------------------------------------------------
-- zemi.ui

--- The live turn, as the chat widget sees it.
---@class zemi.AgentCtx
---@field active boolean
---@field model string
---@field text string Reply text so far.
---@field reasoning string Thinking text so far.
---@field tools zemi.ToolCall[]
---@field steering string[] Messages queued mid-turn; while ctx.view is set, the ones queued for that agent run.
---@field thought number Seconds spent reasoning.
---@field elapsed number Seconds since the turn started.
---@field reports integer Background agent reports among steering; they stay queued, so the rule does not count them as queued.
---@field compacting boolean History is being summarized.
---@field retry? { attempt: integer, why: string, next_in: number } Set while a provider retry waits or is under way (next_in 0).

---@class zemi.ToolCall
---@field name string
---@field input string
---@field status "wait"|"run"|"ok"|"err"|"stopped"|"bg" "wait" until the call starts: its arguments stream, or a serial call ahead of it runs. "stopped": the user denied, dismissed or interrupted it. "bg": its work goes on in the background (a job, a background agent).
---@field detail string Subagent and workflow progress.
---@field run string Agent calls: the run id (`/agents view <id>`), else "".
---@field label string The model's description of the call.
---@field elapsed number
---@field diff string
---@field output string

--- One transcript entry.
---@class zemi.TranscriptRow
---@field role "user"|"assistant"
---@field kind ""|"reasoning"|"tool"|"shell"|"compact"|"notice"|"error"
---@field text string
---@field tool string
---@field status string Tool and shell rows: "wait", "run", "ok", "err", "stopped" (the user denied, dismissed or interrupted it) or "bg" (it goes on in the background); error rows: "cancelled" for an interrupt.
---@field label string
---@field detail string
---@field run string Agent calls: the run id, else "".
---@field diff string
---@field output string Last lines of a tool result.
---@field ["in"] integer Prompt tokens.
---@field out integer Completion tokens.
---@field thought number
---@field elapsed number

---@class zemi.RecentSession
---@field id string
---@field title string
---@field ago string "now", "5m ago", "2d ago".

---@class zemi.Todo
---@field content string
---@field status "pending"|"in_progress"|"done"
---@field active_form string

---@class zemi.Usage
---@field ["in"] integer
---@field out integer
---@field total integer
---@field cost number

---@class zemi.ContextUsage
---@field tokens integer
---@field window integer
---@field pct number

--- What a widget's render function is given. Everything is a snapshot:
--- render must be pure and finish in about 10 ms: only the read-only
--- `zemi.*` calls other than `zemi.fs.*` and a handle's `wait` work inside
--- it, any other raises. `transcript`, `agent`, `todos`, `recent` and
--- `instructions_missing` are built on first read, so a widget that does
--- not read them does not pay for them; kept past the render, a ctx reads
--- nil for the ones render did not read.
---@class zemi.WidgetCtx
---@field width integer Cells of the layout node drawing the widget's region.
---@field height integer Rows of that node; for a content-sized node, the most it could get.
---@field time string "15:04".
---@field version string
---@field cwd string
---@field project string The repository root trust is decided for, ~-shortened.
---@field trusted boolean Whether the user trusts this project (/trust).
---@field init_error string First line of the failure the user init.lua raised this boot, "" when it loaded.
---@field init_path string The user init.lua, ~-shortened.
---@field model_hint string How to get a model when `model` is "", short enough for one row: the first key to export, or init.lua.
---@field model string
---@field profile string
---@field profile_read_only boolean Whether the active agent is read-only (plans, presents for approval).
---@field effort string The reasoning level `model` runs at: the /effort pick fitted to the model, or its catalog default; "" sends none.
---@field permission_mode string
---@field permission_mode_base string
---@field agent_active boolean
---@field agent zemi.AgentCtx
---@field view? { id: string, agent: string, description: string, status: string } The agent run the chat shows instead of the session; a submit messages it while it is running.
---@field transcript zemi.TranscriptRow[]
---@field transcript_count integer Rows in `transcript`, without building it.
---@field transcript_epoch integer Changes when the transcript is replaced; 0 while an agent run is shown, so rows are worth caching only when it is stable.
---@field todos zemi.Todo[]
---@field recent? zemi.RecentSession[] Recent sessions, only while the transcript is empty.
---@field instructions_missing boolean The project is trusted, is not $HOME or /, and has no AGENTS.md or CLAUDE.md for the prompt: what /init writes.
---@field usage zemi.Usage
---@field context zemi.ContextUsage
---@field show_reasoning boolean
---@field frame integer The frame clock: wall time in 50 ms steps. While a turn runs every frame renders, so it advances by one per render; divide it for a slower cadence. Always 0 with animations off, which leaves every animation on its first frame.
---@field ended ""|"ok"|"error"|"cancelled" How the turn that ended a moment ago ended, so the mascot can show it while the working row is otherwise gone; "" once that moment passed.
---@field busy? { label: string, elapsed: number, cancellable: boolean } The newest job the user waits on that is not a turn (/btw, a plugin install, the reflector): its label, seconds it has run, and whether a double esc stops it (esc stops every such job). nil when none runs.
---@field pending_memory integer
---@field agents_running integer Background agents of this session still running; their reports arrive as messages.
---@field jobs_running integer Background jobs (job_start, or a bash call that outlived its timeout) still running.
---@field quit_armed boolean A second ctrl+c quits.
---@field interrupt_armed boolean A second esc interrupts the turn.
---@field key_seq string Keys of a bound sequence typed so far, space-separated; "" when none.
---@field rows_below integer Rows under the stage's scrolled region (the chat by default) while it is scrolled up; 0 when it follows the tail.
---@field cancelling boolean An interrupt was sent and the turn has not ended yet.
---@field editing string Title of the file open in the user's editor, else "".
---@field waiting string "approval" or "answer" while a prompt holds the turn, else "".
---@field focus string Id of the layout node that has the keys, "" while the composer has them.

--- `render` may return true as a second result to render again next frame:
--- return what is ready and do the rest then. Never change a line table after
--- returning it, one seen in the last render is converted only once: build a
--- new table for a line that changed.
---@class zemi.WidgetSpec
---@field id string
---@field region zemi.Region
---@field render fun(ctx: zemi.WidgetCtx): zemi.Line[], boolean?
---@field priority? integer Lower draws first within a region; `statusline.right` draws from its right edge, and a statusline too short for its widgets drops the highest first.
---@field wrap? boolean Word-wrap instead of truncating.
---@field on_click? fun(ev: zemi.WidgetClick) Runs under the widget's origin when a segment with a `click` id is clicked.

---@class zemi.WidgetClick
---@field id string The segment's `click` id.
---@field button "left"|"right"|"middle"
---@field clicks integer Presses on the same cell within 400 ms, counting this one.
---@field mods { shift: boolean, ctrl: boolean, alt: boolean }
---@field x integer Screen column, 0-based.
---@field y integer Screen row, 0-based.

---@class zemi.WidgetInfo
---@field id string
---@field region string
---@field priority integer
---@field origin zemi.Origin

---@alias zemi.Underline "none"|"single"|"double"|"curl"|"dotted"|"dashed"

--- A highlight group. The hover groups `Hover` (clickable widget segments,
--- statusline pills) and `HoverItem` (list rows) draw
--- over the text's own style: keys left out keep it, false and "NONE" clear it.
---@class zemi.HLSpec
---@field link? string Draw as this group instead; of the other fields only bold and italic then apply, over it. Chains longer than 8 or cycles draw unstyled.
---@field fg? string Hex ("#9aa5f2"), a palette name, an ANSI color name ("red", "green", ...), or "NONE".
---@field bg? string
---@field sp? string Underline color.
---@field bold? boolean
---@field italic? boolean
---@field underline? boolean|zemi.Underline true is "single", false "none".
---@field reverse? boolean
---@field brighten? number Hover groups: mix the fg this far (-1..1) toward white, or toward black on a lighter background.
---@field replace? boolean Hover groups: draw as this group alone instead of over the text's style.

---@class zemi.ThemeSpec
---@field palette? table<string, string>
---@field hl? table<string, zemi.HLSpec>

--- One overlay row. Enter on an item with `detail` or `actions` opens a
--- detail view (never on_select); otherwise it fills, runs or selects the
--- item, and does nothing when none applies. `run` is honored only for
--- user config.
---@class zemi.OverlayItem
---@field text string
---@field desc? string dim column after the text
---@field key? string right-aligned tag
---@field on? boolean draws the on/off switch in the tag's place
---@field locked? boolean draws the switch on with nothing to switch
---@field section? string a change of section starts with a header row; the filter matches it too
---@field detail? string body of the detail view
---@field actions? zemi.OverlayAction[] listed under the detail
---@field fill? string put in the composer (ignored while the overlay has an on_select, which takes enter instead)
---@field run? string a command line such as "mcp restart gitlab" (user and builtin overlays only; ignored while the overlay has an on_select)
---@field hotkey? string one character that acts like enter on this row (not while filtering; j, k and / stay the list's)
---@field copy? string the row's value: y copies it in place of the row's text, and enter copies it, keeping the list open, when the row does nothing else (no on_select, run, fill or detail); the filter matches it too
---@field verb? string enter's action in the hint ("open"); space passes the row by in a toggle list, and an "open" row's run leaves the list under the screen it opens

--- A detail view action; with no id, fill or run it goes back.
---@class zemi.OverlayAction
---@field text string
---@field id? string hands the action to the overlay's on_action, which takes it over fill and run
---@field hl? string highlight group of the label, such as "Danger"
---@field hotkey? string one character that acts like enter on this action; with none, its position ("1", "2", ...)
---@field detail? string enter opens it as a detail view of its own, over this one
---@field fill? string
---@field run? string

---@class zemi.OverlayKey
---@field desc? string named next to the key in the footer
---@field fn fun(item: any) Gets the selected item, nil on an empty list.

---@class zemi.OverlaySpec
---@field id string
---@field title? string
---@field body? string|zemi.Line[] text above the items, or styled lines (text and hl only); with no items a read-only view
---@field markdown? boolean render a string `body` and every item's `detail` as markdown
---@field empty? string the line shown when there are no items, under the body when both are set
---@field items? zemi.OverlayItem[]|any[]
---@field render_item? fun(item: any): string|zemi.Line A line's segments keep their `hl` (text, hl only); unstyled ones take the row's.
---@field selected? integer 1-based item selected when it opens.
---@field anchor? "bottom-left"|"bottom-right" In line with the composer frame's edge, by the statusline pill that opened it; nil centers it.
---@field on_select? fun(item: any) Takes enter for every item, so the items' own `fill` and `run` no longer act (a `reload` list runs both).
---@field on_action? fun(item: any, action: zemi.OverlayAction) Runs for a detail action with an `id`; the list closes like on on_select, a `reload` list rebuilds.
---@field keys? table<string, zemi.OverlayKey> One character each (j, k, /, y and Y are the list's and raise, a row's hotkey wins): a second verb on the selected item, which closes or rebuilds the list like on_action.
---@field on_close? fun()
---@field reload? fun(): any[] Makes a toggle list: enter and space run on_select (or the row's `run`) and keep the overlay open, then the items this returns replace the list, rendered again, keeping selection and filter. Enter on a row with `fill` runs on_select, fills the composer and closes the list. It runs again when an overlay opened over the list closes. Both run off the key path; an error keeps the old items.

---@class zemi.SelectOpts
---@field prompt? string
---@field format_item? fun(item: any): string|zemi.Line

---@class zemi.InputOpts
---@field prompt? string
---@field default? string

---@class zemi.ui
zemi.ui = {}

---@class zemi.ui.widget
zemi.ui.widget = {}

--- Register a widget in a region. Registering the same id again from the
--- same origin replaces it; a lower-trust origin is refused.
---@param spec zemi.WidgetSpec
function zemi.ui.widget.add(spec) end

--- Remove a widget the caller's origin or a less trusted one registered.
---@param id string
---@return boolean removed false when no widget has that id
function zemi.ui.widget.del(id) end

---@return zemi.WidgetInfo[]
function zemi.ui.widget.list() end

--- A size along the parent box's direction: whole cells, "N%" of the box's
--- cells after separators, "fill" (a share of what is left, by `weight`) or
--- "content" (the rows a region, bar or composer draws; in a col only).
---@alias zemi.Size integer|string

---@alias zemi.LayoutNode
---| zemi.LayoutBox
---| zemi.LayoutRegion
---| zemi.LayoutBar
---| zemi.LayoutComposer
---| zemi.LayoutSpacer

--- Fields every node takes. A box too small for its children takes cells
--- from "fill" children first, then percent, content and cells, the last
--- child first: down to `min`, then to 0.
---@class zemi.LayoutBase
---@field id? string Unique; names the node for rects, scroll actions and float `relative`.
---@field size? zemi.Size Default: "fill" for a box and anything in a row; 1 for a spacer; "content" otherwise.
---@field weight? integer A "fill" node's share, default 1.
---@field min? integer
---@field max? integer
---@field hidden? boolean Takes no cells and draws nothing.
---@field collapse? boolean Hidden when its box cannot give every child its `min` (the last collapsing child first), instead of squeezing them.
---@field stage? boolean Prompts, overlays, toasts and completion popups go over this node (the screen when none, or when it is under 40 by 12 cells). At most one.

--- Children laid out left to right ("row") or top to bottom ("col").
---@class zemi.LayoutBox: zemi.LayoutBase
---@field kind "box"
---@field dir "row"|"col"
---@field sep? boolean A one-cell separator between children.
---@field children zemi.LayoutNode[]

--- The widgets registered in `region`, stacked by priority. A region is drawn at most once.
---@class zemi.LayoutRegion: zemi.LayoutBase
---@field kind "region"
---@field region zemi.Region
---@field scroll? "top"|"bottom" Scrollable (needs an id): the wheel over it and the zemi.ui.scroll.* actions move it; "bottom" follows the tail.
---@field focusable? boolean Can take the keys (needs an id): zemi.ui.focus, a click on it, and keymaps with `focus` set to its id.

--- One row: the first line of each widget in `left`, then Go's pills (boot
--- failed, stuck, errors), then `right` aligned to the right edge, truncated
--- first. Without a bar the pills draw over the bottom-left cell.
---@class zemi.LayoutBar: zemi.LayoutBase
---@field kind "bar"
---@field left string Region name.
---@field right string Region name.

--- zemi's own editor. Exactly one per layout, never hidden. With `frame` it
--- draws inside a square frame two rows taller, whose rules carry the first
--- line of the widgets in `top`, `left` and `right` (the last two like a
--- bar's clusters, Go's pills included), so the mascot and the statusline
--- cost no row of their own.
---@class zemi.LayoutComposer: zemi.LayoutBase
---@field kind "composer"
---@field frame? boolean
---@field top? string Region name; needs `frame`. Drawn flush from the left corner at `ctx.width` cells, the whole rule: a widget fills it with its own "─" in `Rule`, or leaves the rest to zemi.
---@field left? string Region name; needs `frame`.
---@field right? string Region name; needs `frame`.

---@class zemi.LayoutSpacer: zemi.LayoutBase
---@field kind "spacer"

--- A region drawn over the tree in list order, like a neovim float: its
--- `anchor` corner sits at the same corner of the `relative` node, moved
--- `row` rows down and `col` columns right, kept on screen. It never covers
--- the composer or the statusline's alarm pills.
---@class zemi.LayoutFloat
---@field id? string
---@field region zemi.Region
---@field relative? string A node id, "stage" (default) or "screen".
---@field anchor? "NW"|"NE"|"SW"|"SE" Default "NW".
---@field row? integer
---@field col? integer
---@field width? integer|string Cells, "N%" of the relative node, or "content" (default: its widest row).
---@field height? integer|string Cells, "N%", or "content" (default: its rows).
---@field hidden? boolean
---@field focusable? boolean Can take the keys, like a region node (needs an id).

---@class zemi.Layout
---@field root zemi.LayoutBox
---@field floats? zemi.LayoutFloat[]

--- Cells on the last frame, 0-based.
---@class zemi.Rect
---@field x integer
---@field y integer
---@field width integer
---@field height integer

---@class zemi.ui.layout
zemi.ui.layout = {}

--- Replace the screen layout (DESIGN §8). The shipped UI sets the default
--- (tui/layout.lua) and user config may replace it; plugin and
--- project code are refused. Raises on a malformed layout: an unknown
--- kind or field, no composer or two, a region drawn twice, a duplicate id,
--- a bad size, min over max, more than 64 nodes or 8 levels. `/reload`
--- keeps the old layout until the replay sets one; a replay that sets none
--- draws zemi's built-in fallback.
---@param layout zemi.Layout
function zemi.ui.layout.set(layout) end

--- The layout table last set, nil when the built-in fallback draws.
---@return zemi.Layout|nil
function zemi.ui.layout.get() end

--- Where the last frame drew each node and float with an id, plus "stage"
--- and "composer".
---@return table<string, zemi.Rect>
function zemi.ui.layout.rects() end

--- Keyboard focus. The composer has the keys unless a focusable node does.
--- While a node has them, every key but ctrl+c goes to the keymaps scoped
--- to it and then the global ones; an unbound key does nothing and an
--- unbound esc gives the keys back to the composer. Prompts and overlays
--- still take keys first. A click on a focusable node focuses it, a click
--- on the composer focuses the composer, and the keys come back. A node the
--- frame does not draw gives its keys to the composer.
---@class zemi.ui.focus
zemi.ui.focus = {}

--- Set the terminal tab's title (escapes stripped, whitespace collapsed);
--- nil gives it back to zemi, which names it after the session. A reload
--- that does not set it again gives it back too. User or builtin origin only.
---@param text? string
function zemi.ui.title(text) end

--- Render every widget again on the next frame. Keymaps, clicks, one-shot
--- timers, job, http and agent callbacks and event handlers redraw
--- on their own when they return; a repeating timer or a widget fed by
--- other state calls this. Raises inside render.
function zemi.ui.redraw() end

--- Scroll a layout node, or the stage's scrolled region (the chat) without
--- `id`, as the zemi.ui.scroll.* host actions do: for a click. Raises inside
--- render.
---@param action "up"|"down"|"top"|"bottom"|"line_up"|"line_down"
---@param id? string
function zemi.ui.scroll(action, id) end

--- Give the keys to a focusable node of the layout, or to the composer with
--- nil. Raises when the layout has no focusable node with that id. User or
--- builtin origin only, like zemi.ui.layout.set.
---@param id? string
function zemi.ui.focus.set(id) end

--- The focused node's id, nil for the composer.
---@return string|nil
function zemi.ui.focus.get() end

--- Move the focus to the next drawn focusable node, the composer counting
--- as one, in tree order and then float order. User or builtin origin only.
function zemi.ui.focus.next() end

--- Move the focus back one. User or builtin origin only.
function zemi.ui.focus.prev() end

---@class zemi.ui.theme
zemi.ui.theme = {}

---@param name string
---@param spec zemi.ThemeSpec
function zemi.ui.theme.add(name, spec) end

--- Make a registered theme the active one. User or builtin origin only.
---@param name string
function zemi.ui.theme.set(name) end

--- The theme drawing now: the set one, or its "-light" variant on a light
--- terminal when one is registered.
---@return string
function zemi.ui.theme.get() end

--- Every registered theme, by name.
---@return { name: string, origin: string }[]
function zemi.ui.theme.list() end

---@class zemi.ui.overlay
zemi.ui.overlay = {}

--- Opening the id of an open overlay replaces it in place, keeping selection
--- and filter, when the caller may close it (see close); otherwise it opens
--- over it.
---@param spec zemi.OverlaySpec
function zemi.ui.overlay.open(spec) end

--- Close an overlay and run its on_close. Closes nothing unless user
--- config, builtin code or the origin that opened it calls it.
---@param id string
function zemi.ui.overlay.close(id) end

--- A picker in the shape of `vim.ui.select`. on_choice gets the item on
--- enter, nil on cancel. Items take the zemi.OverlayItem keys too.
---@param items any[]
---@param opts zemi.SelectOpts
---@param on_choice fun(item: any|nil)
function zemi.ui.select(items, opts, on_choice) end

--- A one-line prompt in the shape of `vim.ui.input`.
---@param opts zemi.InputOpts
---@param on_confirm fun(text: string|nil)
function zemi.ui.input(opts, on_confirm) end

---@class zemi.EditOpts
---@field line? integer 1-based line to start at
---@field title? string working-row label while the editor is open (default: the file name)

--- Open a file in the project dir, anywhere in a trusted repository (`zemi.project.root()`), or under
--- `zemi.fs.data_dir`, in the user's editor (editor.command, $VISUAL, $EDITOR)
--- fullscreen over zemi, and call on_done when it exits: on_done(),
--- or on_done(err) when it failed or exited non-zero.
--- Never blocks the interpreter. Raises for a path outside those.
---@param path string
---@param opts? zemi.EditOpts
---@param on_done fun(err: string|nil)
function zemi.ui.edit(path, opts, on_done) end

---@class zemi.ui.hl
zemi.ui.hl = {}

--- Change one highlight group on top of the active theme, and any theme
--- set later. `zemi.ui.theme.add` builds a whole theme instead.
--- User or builtin origin only.
---@param group string
---@param spec zemi.HLSpec
function zemi.ui.hl.set(group, spec) end

--- How a group draws under the active theme: links followed, palette names
--- resolved to hex, keys the group leaves unset left out. Nil when
--- nothing defines the group.
---@param group string
---@return zemi.HLSpec|nil
function zemi.ui.hl.get(group) end

--- Cells `text` draws in, measured the way the renderer does: a wide glyph
--- (CJK, emoji) is two, a combining mark zero. Use it to clip, pad and
--- align, never `#text`.
---@param text string
---@return integer
function zemi.ui.width(text) end

--- Render markdown into widget rows of at most `width` cells.
--- Decoration glamour draws (bullets, quote bars, code margins) comes back
--- marked `copy = false`, and a row that wrapped ends in `soft_wrap`, so a
--- selection copies the text the model wrote.
---@param text string
---@param width integer
---@return zemi.Line[]
function zemi.ui.markdown(text, width) end

--- Highlight code in `lang` (a chroma lexer name, like "bash" or "go") into
--- one widget row per line, in the groups markdown's code blocks draw in
--- (`Accent` keywords, `Dim` comments and punctuation, `Normal` the rest),
--- each wrapped to `width` cells when given. A `lang` with no lexer comes
--- back plain. Every segment copies, so a selection copies the code.
---@param text string
---@param lang string
---@param width? integer
---@return zemi.Line[]
function zemi.ui.highlight(text, lang, width) end

--- Wrap plain text into widget rows of at most `width` cells, a new row at
--- each line break; a row that wrapped ends in `soft_wrap`.
---@param text string
---@param width integer
---@return zemi.Line[]
function zemi.ui.wrap(text, width) end

--- Render a unified diff into widget rows at most `width` cells wide: one
--- line-number column and the sign in `DiffAddNum`, `DiffDelNum` or
--- `DiffCtxNum`, the text wrapped in `DiffAdd`, `DiffDel` or `DiffCtx` with
--- its tint out to the edge, so a diff line's rows are exactly `width` wide,
--- and "⋯" between hunks. Only the text copies.
---@param text string
---@param width integer
---@return zemi.Line[]
function zemi.ui.diff(text, width) end

--- The drawn screen as plain text, one line per row, styling and trailing
--- padding dropped. This is how zemi reads its own UI instead of asking what
--- the user sees.
---@return string|nil screen
---@return string|nil err
function zemi.ui.screenshot() end

--- Open the /help overlay.
function zemi.ui.help() end

---@class zemi.ui.reasoning
zemi.ui.reasoning = {}

--- Show or hide finished reasoning and every tool call (ctrl+o).
function zemi.ui.reasoning.toggle() end

-- ---------------------------------------------------------------------------
-- zemi.notify and zemi.log

---@class zemi.NotifySpec
---@field title? string
---@field body? string
---@field level? zemi.Level

--- A toast. Callable: `zemi.notify({ title = "Plugin installed", body = "foo v1.2.0" })`.
--- The title says what happened, in sentence case with no period: "<Kind>
--- <past participle>" with the name in the body, "Could not <verb> <object>"
--- with the cause in the body, "Nothing to <verb> [yet]" with a reason only
--- when it is not obvious. A lone title or body draws as one row.
---@class zemi.notify
---@overload fun(spec: zemi.NotifySpec)
---@overload fun(body: string, title?: string)
zemi.notify = {}

---@param body string
---@param title? string
function zemi.notify.info(body, title) end

---@param body string
---@param title? string
function zemi.notify.warn(body, title) end

---@param body string
---@param title? string
function zemi.notify.error(body, title) end

---@class zemi.NotifyEntry
---@field at string "15:04:05".
---@field ts number Unix seconds.
---@field level string
---@field title string
---@field body string

--- Recent notifications from zemi.notify and the host, newest first (the
--- last 100).
---@return zemi.NotifyEntry[]
function zemi.notify.list() end

--- The run's log file, one record per line. Callable:
--- `zemi.log("message", { key = value })`.
---@class zemi.log
---@overload fun(message: string, fields?: table)
zemi.log = {}

---@param message string
---@param fields? table
function zemi.log.debug(message, fields) end

---@param message string
---@param fields? table
function zemi.log.info(message, fields) end

---@param message string
---@param fields? table
function zemi.log.warn(message, fields) end

---@param message string
---@param fields? table
function zemi.log.error(message, fields) end

---@class zemi.LogEntry
---@field at string "15:04:05".
---@field ts number Unix seconds.
---@field source string The origin that printed, or "<origin> <level>".
---@field message string

--- Recent print() and zemi.log lines, newest first (the last 100), the
--- same list `/messages` shows.
---@return zemi.LogEntry[]
function zemi.log.list() end

-- ---------------------------------------------------------------------------
-- zemi.provider and zemi.model

---@class zemi.ModelSpec
---@field id string
---@field display? string
---@field context_window? integer Without it there is no context percent and no auto compaction.
---@field cost_per_1m_in? number
---@field cost_per_1m_out? number
---@field cost_per_1m_in_cached? number
---@field cost_per_1m_out_cached? number
---@field max_output? integer Reply token cap; unset, anthropic models get 32000 and other kinds the provider default.
---@field images? boolean false for a model that refuses image input; read then returns a text error and @image attachments are omitted with a note.

---@class zemi.ProviderSpec
---@field id string
---@field kind string One of `zemi.provider.kind`.
---@field api_key? string Read it with `zemi.env.get`; never write a key into a file.
---@field base_url? string Required for openai_compat, refused for openrouter; overrides the default endpoint for the other kinds.
---@field headers? table<string, string> Sent with every request; as secret as the key.
---@field body? table<string, any> Extra request-body fields, merged verbatim into every request (service_tier, a gateway's routing block). Not sent by openai or google.
---@field models zemi.ModelSpec[]

---@class zemi.ProviderKind
---@field ANTHROPIC "anthropic"
---@field OPENAI "openai"
---@field OPENAI_COMPAT "openai_compat"
---@field GOOGLE "google"
---@field OPENROUTER "openrouter"
---@field VERCEL "vercel"

---@class zemi.ProviderInfo
---@field id string
---@field kind string
---@field base_url string
---@field builtin boolean From the builtin catalog; false once `zemi.provider.add` replaced it.

---@alias zemi.ModelRole "chat"|"agents"|"titles"|"compact"|"memory"

---@class zemi.ModelInfo
---@field id string "provider/model".
---@field display string
---@field has_key boolean
---@field roles zemi.ModelRole[] The roles that run on this model. "chat" is models.chat, also while the active profile runs the conversation on its own model.

---@class zemi.provider
---@field kind zemi.ProviderKind
zemi.provider = {}

--- Register a provider and its models. User config only. An id a builtin
--- provider has replaces it whole (a gateway in front of it keeps its
--- base_url); the builtin ones are on while their key is exported.
---@param spec zemi.ProviderSpec
function zemi.provider.add(spec) end

--- Remove a provider, builtin or added; an id nothing registered is a
--- no-op, so init.lua works on a machine where that key is not exported.
--- User config only. `providers.builtin = false` drops every builtin one.
---@param id string
function zemi.provider.del(id) end

---@return zemi.ProviderInfo[]
function zemi.provider.list() end

---@class zemi.model
zemi.model = {}

---@return zemi.ModelInfo[]
function zemi.model.list() end

--- Switch the model the main conversation runs on: it writes models.chat.
--- Raises for a provider nobody registered, and while the active profile
--- sets its own model; an id the provider does not list is taken and warned
--- about. User or builtin origin only. Another role is plain config:
--- `zemi.config.set("models.titles", id)`.
---@param id string
function zemi.model.set(id) end

---@class zemi.ModelAsk
---@field prompt string
---@field system? string
---@field model? string A role name or a "provider/model" id; "chat" (models.chat, not a profile's own model) by default.
---@field max_output? integer The reply's token cap, 1024 by default.
---@field on_done? fun(text: string?, err: string?) Runs as a task when the reply is in.

---@class zemi.ModelHandle
---@field wait fun(self: zemi.ModelHandle): string|nil, string|nil Suspends the task until the reply, 10 minutes at most; raises outside a task and under pcall, like `zemi.HTTPHandle.wait`.
---@field cancel fun(self: zemi.ModelHandle)

--- One tool-less model call, run off the interpreter and billed to the
--- live session; a reload cancels it. Plugin code needs a user rule for
--- `lua.model.ask` with its principal; project code raises.
---@param spec zemi.ModelAsk
---@return zemi.ModelHandle
function zemi.model.ask(spec) end

--- Open a model picker and return at once: with no role `/model`, else that
--- role's list from `/model roles`. Choosing a model runs `/model <id>`, or
--- `/model <role> <id>` for another role. Raises for an unknown role.
---@param role? zemi.ModelRole
function zemi.model.pick(role) end

-- ---------------------------------------------------------------------------
-- zemi.agent

---@class zemi.ProfileSpec
---@field model? string A role name (the model that role runs on) or a "provider/model" id.
---@field system? string Replaces the system prompt.
---@field temperature? number
---@field tools? string[] Allowed tool names; "-name" removes one and keeps the rest.
---@field context? { files: string[] } Files pinned into the prompt.
---@field read_only? boolean A read-only (planning) agent: the project is read-only to it and it presents plans for approval.

---@class zemi.AgentCurrent
---@field id string Session id.
---@field model string
---@field profile string

---@class zemi.AgentUsage: zemi.Usage
---@field context_tokens integer
---@field context_window integer
---@field context_pct number

---@class zemi.AgentDef
---@field name string
---@field description string
---@field source string

---@class zemi.AgentRunSpec
---@field prompt string
---@field agent? string A name from `zemi.agent.list()`; "general" by default.
---@field description? string Shown while it runs.

---@class zemi.RunHandle
---@field cancel fun(self: zemi.RunHandle)

---@class zemi.agent
zemi.agent = {}

---@class zemi.agent.profile
zemi.agent.profile = {}

--- Define a named profile (model, prompt, tool set). User or builtin origin
--- only. The `agent.profile` config key names the active one.
---@param name string
---@param spec zemi.ProfileSpec
function zemi.agent.profile.add(name, spec) end

---@return zemi.AgentCurrent
function zemi.agent.current() end

--- Send a message. Returns at once; the turn runs off the interpreter, so
--- the error is what can be known before it starts (no model yet); during
--- a turn it joins at the next step. Plugin code needs a user rule for
--- `lua.agent.send` with its principal, and raises in a print run (-p); its
--- text reaches the model as the plugin's, never the user's words. Project
--- code raises.
---@param text string
---@return true?, string?
function zemi.agent.send(text) end

--- Interrupt the running turn.
function zemi.agent.cancel() end

---@return zemi.AgentUsage
function zemi.agent.usage() end

--- The system prompt the last send assembled, "" before the first one.
--- `/prompt` shows the same text.
---@return string
function zemi.agent.prompt() end

--- The subagents this project can run: builtin, `agents/*.md`, and trusted
--- project definitions.
---@return zemi.AgentDef[]
function zemi.agent.list() end

--- Run a subagent off the interpreter. User config only.
---@param spec zemi.AgentRunSpec
---@param on_done fun(text: string|nil, err: string|nil)
---@return zemi.RunHandle
function zemi.agent.run(spec, on_done) end

-- ---------------------------------------------------------------------------
-- zemi.workflow

---@class zemi.WorkflowRunSpec
---@field script? string Workflow source to run.
---@field name? string A script saved under workflows/ instead.
---@field args? any Passed to the script as the `args` global.

---@class zemi.WorkflowInfo
---@field name string
---@field description string The file's leading comment.
---@field source "user"|"builtin"|"plugin"|"project"
---@field path string

---@class zemi.workflow
zemi.workflow = {}

---@return zemi.WorkflowInfo[]
function zemi.workflow.list() end

--- Run a workflow in its own Lua state. User or builtin origin only.
---@param spec zemi.WorkflowRunSpec
---@param on_done? fun(result: string|nil, err: string|nil, log: string[]) A result that is not a string arrives as JSON.
---@return zemi.RunHandle|nil handle
---@return string|nil err No such saved workflow, or one in an untrusted project.
function zemi.workflow.run(spec, on_done) end

-- ---------------------------------------------------------------------------
-- zemi.session

---@class zemi.SessionInfo
---@field id string
---@field title string
---@field updated_at integer
---@field ago string
---@field tokens integer
---@field cost number
---@field current boolean

---@class zemi.StoredMessage
---@field id string
---@field role string
---@field text string The message's text parts joined.
---@field json string The provider wire format, as stored. Unstable: prefer `text`.

---@class zemi.session
zemi.session = {}

---@return string id
function zemi.session.id() end

---@return string title
function zemi.session.title() end

---@param title string
function zemi.session.rename(title) end

---@return zemi.StoredMessage[]
function zemi.session.messages() end

---@param opts? { limit: integer }
---@return zemi.SessionInfo[]
function zemi.session.list(opts) end

--- User or builtin origin only.
---@return true|nil ok
---@return string|nil err
function zemi.session.new() end

--- User or builtin origin only.
---@param id string
---@return true|nil ok
---@return string|nil err
function zemi.session.resume(id) end

---@return zemi.Todo[]
function zemi.session.todos() end

--- Restore the files a turn changed and cut the conversation back to the
--- message with this id. User or builtin origin only.
---@param message_id string
---@return integer|nil restored
---@return string|nil err
function zemi.session.rewind(message_id) end

-- ---------------------------------------------------------------------------
-- zemi.composer

---@class zemi.ComposerState
---@field text string
---@field cursor integer 0-based character offset.

---@class zemi.CompletionItem
---@field text string
---@field desc? string

--- A completion source: `trigger` completes the word after that character
--- where it starts a word ("#12"; "@" rows go ahead of the file list), or
--- `command` completes that slash command's arguments after its own
--- candidates. Set one of the two.
---@class zemi.CompletionSpec
---@field id string Registering the id again from the same origin replaces it.
---@field trigger? string One punctuation character other than "/".
---@field command? string A command name.
---@field items fun(lead: string, line: string): string[]|zemi.CompletionItem[] `lead` is the typed word after the trigger (or the argument under the cursor), `line` the draft up to the cursor. Runs off the key path, on every edit of the word; filter by `lead` yourself. At most 50 items across a popup's sources.

---@class zemi.composer
zemi.composer = {}

--- Register a completion source. Accepting a row puts its text after the
--- trigger, or in place of the argument, followed by a space. Raises on a
--- bad spec or an id a more trusted origin registered.
---@param spec zemi.CompletionSpec
function zemi.composer.complete(spec) end

---@return zemi.ComposerState
function zemi.composer.get() end

--- User or builtin origin only.
---@param text string
---@param cursor? integer -1, the default, puts the cursor at the end.
function zemi.composer.set(text, cursor) end

--- User or builtin origin only.
---@param text string
function zemi.composer.insert(text) end

--- Submit the draft, as if enter was pressed. User or builtin origin only.
function zemi.composer.submit() end

--- The previous sent message, like up on a one-line draft. User or builtin
--- origin only.
function zemi.composer.history_prev() end

--- The next sent message, back to an empty draft past the newest. User or
--- builtin origin only.
function zemi.composer.history_next() end

--- Paste the clipboard: an image becomes an attachment token, text is
--- inserted (ctrl+v). User or builtin origin only.
function zemi.composer.paste() end

--- Open the draft in the user's editor (ctrl+g when no keymap starts with
--- it). User or builtin origin only.
function zemi.composer.edit() end

-- ---------------------------------------------------------------------------
-- zemi.permission

---@alias zemi.Decision "allow"|"ask"|"deny"

---@class zemi.RuleSpec
---@field subject string "tool.bash", "fs.read", "fs.write", "net.<host>", "lua.<ns.call>", "mcp.<server>".
---@field pattern string Glob over the subject's argument.
---@field decision zemi.Decision Allow rules need user config.
---@field match string Required: an argument the rule must match, checked at registration.
---@field not_match? string A glob that falls through and forces an ask.
---@field not_match_example? string An argument `not_match` must catch; required with `not_match`.
---@field scope? "session" The only scope: a rule lasts until zemi exits, and init.lua adds it again on the next start.
---@field principal? string "plugin:<name>": only that plugin's Lua calls match. User config only.

---@class zemi.Rule
---@field subject string
---@field pattern string
---@field decision zemi.Decision
---@field not_match string
---@field principal? string

---@class zemi.PermModeSpec
---@field base? string The mode this one starts from.
---@field description? string
---@field prompt? string A system prompt section while the mode is active.
---@field rules? table[] `{ subject, pattern, decision }` rules that answer what would otherwise ask while the mode is on.
---@field decide? fun(req: zemi.PermRequest): "allow"|"deny"|nil Answers an ask the rules left open; anything else leaves it to the user. It runs as a task for at most 60s, so it may wait on a model call (zemi.http), which frees the interpreter.

---@class zemi.PermRequest
---@field subject string
---@field arg string Path relative to the project, url or command.
---@field tool string
---@field reason string
---@field preview string
---@field description string

---@class zemi.PermMode
---@field name string
---@field base string
---@field description string
---@field active boolean

---@class zemi.permission
zemi.permission = {}

---@class zemi.permission.rule
zemi.permission.rule = {}

--- Add a permission rule. Any origin may add deny and ask rules; allow rules
--- need user config. Ordering is deny > ask > allow, then
--- fewest wildcards, then longest literal prefix.
---@param spec zemi.RuleSpec
function zemi.permission.rule.add(spec) end

--- The rules in force, in evaluation order. There is no `del`: a session
--- rule dies with the session and config rules are declarative file state.
---@return zemi.Rule[]
function zemi.permission.rule.list() end

---@class zemi.permission.mode
zemi.permission.mode = {}

--- Define a permission mode. User config only.
---@param name string
---@param spec zemi.PermModeSpec
function zemi.permission.mode.add(name, spec) end

---@param name string
function zemi.permission.mode.del(name) end

--- Switch the active mode. User config only.
---@param name string
---@return true|nil ok, string? err
function zemi.permission.mode.set(name) end

---@return zemi.PermMode[]
function zemi.permission.mode.list() end

-- ---------------------------------------------------------------------------
-- zemi.tool

---@class zemi.ToolSpec
---@field name string
---@field description string What the model reads to decide when to call it. Required.
---@field input? table The parameters, flat: a name to its JSON Schema. A `properties` key here raises.
---@field required? string[] Which of `input`'s parameters the model must send.
---@field prompt? string A system prompt section only a run that has the tool gets. zemi adds the heading ("## Tool <name>"), so write the body only. Fixed text: what changes between turns costs the provider's prompt cache. Ignored from project code.
---@field execute fun(args: table, run: zemi.ToolRun): any, string? A string, a table whose `output` is the string, or anything else sent as JSON. `nil, err` fails the call with `err`. It runs as a task: a handle's `wait` in it suspends it and frees the interpreter.
---@field timeout_ms? integer Wall time for one call, waits included; 60000 by default.
---@field parallel? boolean Safe to run beside other tool calls.

--- The call `execute` runs for.
---@class zemi.ToolRun
---@field name string
---@field origin zemi.Origin
---@field progress fun(self: zemi.ToolRun, text: string) One line under the call's row, sanitized and cut to 120 characters.

---@class zemi.ToolPreEvent
---@field name "tool.pre"
---@field from string
---@field tool string
---@field input table

--- What a `tool.pre` handler returns: "deny" refuses the call, "halt" also
--- ends the turn, "rewrite" merges `input` into the call's input (ignored
--- for project code). nil lets the call through.
---@class zemi.ToolVeto
---@field decision "deny"|"halt"|"rewrite"
---@field reason? string
---@field input? table

--- What a `tool.post` handler returns to replace the output the model gets
--- (ignored for project code); nil keeps it.
---@class zemi.ToolPostResult
---@field output string

--- One of `pre` and `post` is required. Not available to project code.
---@class zemi.ToolOverride
---@field pre? fun(ev: zemi.ToolPreEvent): zemi.ToolVeto?
---@field post? fun(ev: zemi.ev.ToolPost): zemi.ToolPostResult? Runs after the global `tool.post` handlers, as a task.

---@class zemi.ToolInfo
---@field name string
---@field builtin boolean
---@field origin? zemi.Origin Lua tools only.

---@class zemi.tool
zemi.tool = {}

--- Register a tool the model can call.
---@param spec zemi.ToolSpec
function zemi.tool.add(spec) end

--- Hook one tool's calls: `pre` before it runs (a rewritten input re-enters
--- at the permission gate), `post` after, to rewrite its output.
---@param name string
---@param spec zemi.ToolOverride
function zemi.tool.override(name, spec) end

---@return zemi.ToolInfo[]
function zemi.tool.list() end

-- ---------------------------------------------------------------------------
-- zemi.fs

---@class zemi.DirEntry
---@field name string
---@field type "file"|"dir"|"link"
---@field size integer

---@class zemi.FileStat
---@field type "file"|"dir"|"link"
---@field size integer
---@field mtime integer Unix milliseconds.

---@class zemi.GrepHit
---@field path string
---@field line integer
---@field text string

--- Paths resolve against the project directory and must stay inside the
--- jail; reads pass the `lua.fs.read` gate and changes `lua.fs.write`.
---@class zemi.fs
zemi.fs = {}

---@param path string
---@return string|nil content
---@return string|nil err
function zemi.fs.read(path) end

---@param path string
---@param content string
---@return true|nil ok
---@return string|nil err
function zemi.fs.write(path, content) end

---@param path string
---@return zemi.DirEntry[]|nil entries
---@return string|nil err
function zemi.fs.list(path) end

---@param path string
---@return zemi.FileStat|nil stat
---@return string|nil err
function zemi.fs.stat(path) end

---@param path string
---@return boolean
function zemi.fs.exists(path) end

--- `<data>/lua/<name>`, where config code keeps its files: inside the
--- `zemi.fs` jail and accepted by `zemi.ui.edit` for user config only, and
--- outside the config tree, so a write never reloads the config. Reads
--- settle; writes need a `lua.fs.write` rule like any other. Nothing is
--- created: `write` makes the parents, and a job's `cwd` needs `mkdir` first.
---@param name string One directory name, no separators.
---@return string path
function zemi.fs.data_dir(name) end

---@param path string
---@return true|nil ok
---@return string|nil err
function zemi.fs.mkdir(path) end

--- Delete one file or empty directory, never a tree.
---@param path string
---@return true|nil ok
---@return string|nil err
function zemi.fs.remove(path) end

--- Match project-relative paths. `*` and `?` stay inside one segment,
--- `**` spans directories ("**/*.go"), `{a,b}` alternates, `[...]` is a class.
---@param pattern string
---@return string[] paths
function zemi.fs.glob(pattern) end

--- Search the project for a Go regular expression.
---@param pattern string
---@return zemi.GrepHit[]
function zemi.fs.grep(pattern) end

-- ---------------------------------------------------------------------------
-- zemi.http

---@class zemi.HTTPSpec
---@field url string
---@field method? string GET by default.
---@field headers? table<string, string>
---@field body? string
---@field timeout_ms? integer 30000 by default.
---@field on_done? fun(res: zemi.HTTPDone)

---@class zemi.HTTPResponse
---@field status integer
---@field body string At most 1 MiB.
---@field headers table<string, string>

--- What `on_done` gets: the response fields, or `error` when the request failed.
---@class zemi.HTTPDone
---@field url string
---@field status? integer
---@field body? string
---@field headers? table<string, string>
---@field error? string

---@class zemi.HTTPHandle
---@field wait fun(self: zemi.HTTPHandle): zemi.HTTPResponse|nil, string|nil Suspends the task until the response, 120 seconds at most; raises outside a task (a tool's execute, a command, a keymap, an overlay callback, an on_done or on_exit, a mode's decide) and under pcall.

--- http and https only, no userinfo. Loopback, RFC1918 and metadata
--- addresses are refused at connect time, on every redirect hop; an
--- explicit user allow rule for the host is the localhost opt-in.
---@class zemi.http
zemi.http = {}

---@param spec zemi.HTTPSpec
---@return zemi.HTTPHandle
function zemi.http.request(spec) end

---@param url string
---@param opts? { on_done: fun(res: zemi.HTTPDone) }
---@return zemi.HTTPHandle
function zemi.http.get(url, opts) end

-- ---------------------------------------------------------------------------
-- zemi.job

---@class zemi.JobSpec
---@field cmd string
---@field args? string[]
---@field env? table<string, string>
---@field cwd? string The project dir by default; any dir `zemi.fs` reads. A missing dir is the returned error.
---@field sandbox? boolean Run under the sandbox policy a mode-settled bash command gets; raises when it cannot be confined.
---@field stdin? boolean Needed before `write`.
---@field timeout_ms? integer `zemi.job.wait` only; 60000 by default.
---@field on_stdout? fun(lines: string[]) Lines as they arrive, batched; the result's `stdout` fills all the same.
---@field on_stderr? fun(lines: string[])
---@field on_exit? fun(res: zemi.JobResult) Runs after the last output callback, also for a job `kill` ended (code -1).

---@class zemi.JobResult
---@field id integer
---@field stdout string The first 1 MiB; the rest is dropped.
---@field stderr string As stdout.
---@field code integer -1 when a signal ended it.
---@field error string

---@class zemi.JobHandle
---@field id integer
---@field wait fun(self: zemi.JobHandle, ms?: integer): zemi.JobResult|nil, string|nil Suspends the task until the job ends, 30000 ms by default; err is "timeout". Raises outside a task and under pcall, like `zemi.HTTPHandle.wait`.
---@field kill fun(self: zemi.JobHandle)
---@field write fun(self: zemi.JobHandle, s: string): true|nil, string|nil
---@field close_stdin fun(self: zemi.JobHandle)

--- Child processes run in their own session and process group, with the
--- project directory as their working directory. Needs `lua.job.start`
--- allowed for the command line: rules match it normalized like `tool.bash`
--- patterns (`go test *`), with cmd and args as its words.
---@class zemi.job
zemi.job = {}

---@param spec zemi.JobSpec
---@return zemi.JobHandle|nil handle
---@return string|nil err A process that could not start.
function zemi.job.start(spec) end

--- Start a job and suspend the task until it finishes (spec.timeout_ms,
--- default 60000, then kills it). Raises outside a task, like a handle's
--- `wait`; there use `start` with `on_exit`.
---@param spec zemi.JobSpec
---@return zemi.JobResult|nil res
---@return string|nil err
function zemi.job.wait(spec) end

-- ---------------------------------------------------------------------------
-- zemi.timer

---@class zemi.TimerHandle
---@field stop fun(self: zemi.TimerHandle)

---@class zemi.timer
zemi.timer = {}

--- Run fn once after ms. `after(0, fn)` runs it on the next queue turn.
---@param ms integer
---@param fn fun()
---@return zemi.TimerHandle
function zemi.timer.after(ms, fn) end

--- Run fn every ms until stopped. Unlike `after`, it does not redraw when
--- fn returns, so a widget that changes on its own (a clock) calls
--- `zemi.ui.redraw`.
---@param ms integer
---@param fn fun()
---@return zemi.TimerHandle
function zemi.timer.every(ms, fn) end

-- ---------------------------------------------------------------------------
-- zemi.state and zemi.env

--- A persisted key-value store, global to zemi. Values round-trip as JSON.
--- Each plugin and project code get their own key space and never see
--- another's, user or builtin keys; user config sees every key, stored as
--- `plugin:<name>:<key>` and `project:<key>` for those.
---@class zemi.state
zemi.state = {}

---@param key string
---@return any value A key nobody set is nil with no error.
---@return string? err
function zemi.state.get(key) end

---@param key string
---@param value any
---@return true|nil ok
---@return string|nil err
function zemi.state.set(key, value) end

--- Drop a key.
---@param key string
---@return boolean|nil removed
---@return string|nil err
function zemi.state.del(key) end

--- The keys set, sorted, filtered by prefix when given.
---@param prefix? string
---@return string[]|nil keys
---@return string|nil err
function zemi.state.keys(prefix) end

---@class zemi.env
zemi.env = {}

--- Read an environment variable, nil when it is not set. Secret-shaped
--- names resolve for user and builtin origin only; config fields hold
--- values, never variable names.
---@param name string
---@return string?
function zemi.env.get(name) end

-- ---------------------------------------------------------------------------
-- zemi.json

---@class zemi.json
zemi.json = {}

---@param value any
---@return string
function zemi.json.encode(value) end

---@param s string
---@return any|nil value
---@return string|nil err
function zemi.json.decode(s) end

-- ---------------------------------------------------------------------------
-- zemi.memory, zemi.learn, zemi.skills

---@class zemi.Memory
---@field id string
---@field scope zemi.Scope
---@field origin "user"|"learned"
---@field text string
---@field quote string The user's own words a memory about them rests on; "" otherwise.

---@class zemi.MemoryOpts
---@field scope? zemi.Scope "project" by default; "user" holds in every project.
---@field quote? string The user's exact words from this session, at most 160 characters.
---@field replaces? string The memory this one rewrites: its id, the first 8 characters or more.

---@class zemi.MemoryAddResult
---@field stored? boolean Written straight away under auto autonomy.
---@field id? string
---@field queued? boolean Waiting for review in /memory.
---@field reason? string Why it waits, "" when the autonomy setting alone holds it.
---@field duplicate? string Id of the live or pending memory it repeats; nothing was written.
---@field dropped? boolean The quote is not in the session's messages; nothing was written.

---@class zemi.memory
zemi.memory = {}

--- Propose a memory of at most 300 characters. It goes through intake and
--- the autonomy gate, so it is stored or queued for review, never written
--- blindly. User config writes its own memory (origin user) and works
--- before the first message; the model's Lua tool writes a learned one.
--- Raises for project code.
---@param text string
---@param opts? zemi.MemoryOpts
---@return zemi.MemoryAddResult|nil res
---@return string|nil err
function zemi.memory.add(text, opts) end

---@param include_superseded? boolean
---@return zemi.Memory[]|nil memories
---@return string|nil err
function zemi.memory.list(include_superseded) end

--- Delete a memory by id (an 8-character prefix works). User or builtin
--- origin only.
---@param id string
---@return true|nil ok
---@return string|nil err
function zemi.memory.del(id) end

---@class zemi.Proposal
---@field id string
---@field scope zemi.Scope
---@field origin string
---@field text string
---@field quote string
---@field forget? true Accepting deletes the stored memory this text is a copy of.

---@class zemi.learn
zemi.learn = {}

--- Memory proposals awaiting review, the rows /memory shows.
---@return zemi.Proposal[]
function zemi.learn.list() end

--- Decide one proposal by id. User or builtin origin only.
---@param id string
---@param action "accept"|"reject"
---@return true|nil ok
---@return string|nil err
function zemi.learn.decide(id, action) end

---@class zemi.Skill
---@field name string
---@field description string
---@field source "user"|"builtin"|"plugin"|"project"|"recipe" A project skill or recipe is always enabled and cannot be toggled.
---@field enabled boolean
---@field path string

---@class zemi.skills
zemi.skills = {}

---@return zemi.Skill[]
function zemi.skills.list() end

--- Enable a disabled skill. User or builtin origin only.
---@param name string
---@return true|nil ok
---@return string|nil err
function zemi.skills.enable(name) end

---@param name string
---@return true|nil ok
---@return string|nil err
function zemi.skills.disable(name) end

-- ---------------------------------------------------------------------------
-- zemi.mcp and zemi.lsp

---@class zemi.MCPSpec
---@field command? string A stdio server.
---@field args? string[]
---@field env? table<string, string>
---@field url? string A streamable HTTP server instead.
---@field headers? table<string, string>
---@field client_id? string An OAuth client registered with the server by hand, for a server without dynamic client registration.
---@field client_secret? string
---@field callback_port? integer Fixed localhost port for the sign-in redirect, for a server that matches the registered redirect URI exactly.
---@field enabled_tools? string[]
---@field disabled_tools? string[]
---@field timeout? integer Seconds.

---@class zemi.MCPInfo
---@field name string
---@field status string "starting", "ready", "failed" or "stopped"; a server waiting for a sign-in is "failed" with the error "needs sign-in".
---@field error string
---@field tools string[] The tools the model gets, after enabled_tools and disabled_tools.

---@class zemi.mcp
zemi.mcp = {}

--- Add an MCP server. User config only. Its tools arrive as
--- `mcp_<server>_<tool>` and are gated as subject `mcp.<server>`.
---@param name string
---@param spec zemi.MCPSpec
function zemi.mcp.add(name, spec) end

---@return zemi.MCPInfo[]
function zemi.mcp.list() end

---@class zemi.LSPSpec
---@field command string
---@field args? string[]
---@field env? table<string, string>
---@field filetypes? string[] Extensions the server handles.
---@field settings? table Workspace settings: answers the server's configuration requests by section and is pushed after start (e.g. { gopls = { staticcheck = true } }).
---@field init_options? table Sent as initializationOptions.

---@class zemi.LSPInfo
---@field name string
---@field command string
---@field filetypes string
---@field status string "idle" (starts on its first file), "starting", "ready", "failed" or "stopped".
---@field error string

---@class zemi.lsp
zemi.lsp = {}

--- Add a language server. User config only; it starts on the first
--- matching file. Adding a builtin's name (gopls, pyright,
--- typescript, clangd) replaces it, which is how a builtin gets args, env
--- or settings. A server that never comes up only means no diagnostics.
---@param name string
---@param spec zemi.LSPSpec
function zemi.lsp.add(name, spec) end

--- Never start this server: a builtin you do not want, or one added above.
--- User config only.
---@param name string
function zemi.lsp.del(name) end

--- The servers this project can use and their state.
---@return zemi.LSPInfo[]
function zemi.lsp.list() end

-- ---------------------------------------------------------------------------
-- zemi.health and zemi.errors

--- What a /health check returns. Any other level reads as "ok".
---@class zemi.HealthResult
---@field level zemi.HealthLevel
---@field detail string

---@class zemi.health
zemi.health = {}

--- Add a row to /health. The check runs under the 50 ms handler deadline
--- and may return a detail string instead of a table. Adding a name again
--- replaces its row; a name a more trusted origin holds raises.
---@param name string
---@param fn fun(): zemi.HealthResult|string
function zemi.health.add(name, fn) end

--- One row per /health check, built-in rows first: this is how config
--- written from Lua is verified without asking the user to run /health.
---@class zemi.HealthRow
---@field level zemi.HealthLevel
---@field name string
---@field detail string

---@return zemi.HealthRow[]
function zemi.health.list() end

---@class zemi.ErrorEntry
---@field index integer
---@field at string "15:04:05".
---@field source string
---@field message string

---@class zemi.errors
zemi.errors = {}

--- What /errors shows, newest first: boot and reload failures, widget,
--- keymap and event-handler errors, and recovered panics.
---@return zemi.ErrorEntry[]
function zemi.errors.list() end

---@class zemi.project
zemi.project = {}

--- Whether the user trusts this project. Untrusted, zemi loads nothing from
--- it and runs no language server; a plugin should not run its code either.
---@return boolean
function zemi.project.trusted() end

--- The repository root (the nearest `.git` at or above the launch dir, never
--- `$HOME` or above), or the launch dir outside a repository.
---@return string
function zemi.project.root() end
