---@meta
---
--- Type annotations for zemi's Lua API, in the LuaLS (lua-language-server)
--- format. zemi rewrites this file on every start, so do not edit it: it is
--- a description of the API, never part of it. See the zemi skill for the
--- rules behind the shapes.
---
--- zemi runs Lua 5.1 (gopher-lua) in a sandbox: no `io`, no `os.execute`,
--- no C modules. Filesystem, network and processes go through `zemi.fs`,
--- `zemi.http` and `zemi.job`, each behind the permission gate.
---
--- Error convention: misuse (a bad argument, a call a render may not make)
--- raises; a runtime failure of a synchronous call returns `nil, err`;
--- asynchronous work reports through its callback or handle.

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

--- A metatable takes any handler, Lua or Go, and the handlers are read at the call: a handler
--- runs as the code that triggers it.
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
--- means UTC; the specifiers are %Y, %m, %d, %H, %M, %S, %p, %c, %x, %X and
--- %%, and any other raises), or as a table with "*t".
---@overload fun(format: "*t"|"!*t", time?: integer): { year: integer, month: integer, day: integer, hour: integer, min: integer, sec: integer, wday: integer, yday: integer, isdst: boolean }
---@param format? string
---@param time? integer Unix seconds; now by default.
---@return string
function os.date(format, time) end

---@class sandbox.debug
debug = {}

---@param msg? string
---@return string
function debug.traceback(msg) end

---@alias zemi.Origin
---| "user"     # ~/.config/zemi
---| "builtin"  # shipped with zemi
---| "plugin"   # <data>/plugins/{installed,local}/<name>; a call runs as the innermost file on its stack
---| "project"  # <repo>/.agents

---@alias zemi.Level
---| "info"
---| "warn"
---| "error"

---@alias zemi.HealthLevel
---| "ok"
---| "warn"
---| "error"

--- One styled run of text in an overlay row. `hl` names a highlight
--- group; omit it for the default foreground.
---@class zemi.Segment
---@field text string
---@field hl? string
---@field link? string The URL the text stands for: drawn as a terminal hyperlink and opened by a click. Only `http`, `https` and `mailto` are kept.
---@field copy? boolean `false` marks decoration (a bullet, a border): a selection neither lights nor copies it.

--- One row: the segments it is drawn from, left to right.
---@alias zemi.Line zemi.Segment[]

zemi = {}

-- zemi.api

---@class zemi.api
---@field version string The API version, bumped whenever the surface changes.
zemi.api = {}

-- zemi.config

---@alias zemi.SettingKind
---| "string"
---| "number"
---| "bool"
---| "list"
---| "duration"

---@class zemi.SettingSpec
---@field kind zemi.SettingKind
---@field default any
---@field desc string What /config shows: a lowercase fragment.
---@field choices? string[] The only values a string key takes; nil takes any. `zemi.config.set` also takes "default" for a "" choice.

---@class zemi.SettingSchema: zemi.SettingSpec
---@field path string Dotted key, e.g. "ui.theme"; a plugin's are "plugins.<name>.<key>".

---@class zemi.config
zemi.config = {}

--- Read a setting, or nil when it is not set and has no default. Plugin
--- code names its own settings by the bare key.
---@param path string
---@return any
function zemi.config.get(path) end

--- Write a setting. Raises for an unknown path; a wrong kind or a value outside
--- the key's `choices` is recorded in /errors and the key keeps its default.
--- A reload that writes the same value as the last load leaves the setting
--- alone, so a runtime pick (/model, /effort) stays. A call made after boot
--- (a keymap, a timer, an event handler) is a runtime pick: zemi keeps it for
--- every later start, in every project, unless init.lua sets that key.
--- A setting of a plugin that is off or failed to load is skipped, so
--- turning a plugin off never breaks init.lua.
---@param path string
---@param value any
function zemi.config.set(path, value) end

--- Register one of the calling plugin's settings, from its init.lua. The
--- key is a lowercase letter, then up to 31 lowercase letters, digits and
--- `_`; the plugin's own code reads and watches it by that key, and the user
--- sets it as `plugins.<plugin>.<key>`. It shows in /config under the
--- plugin, keeps a pick across starts, takes `zemi.config.set` from the
--- user's init.lua, and leaves when the plugin is turned off. Plugins load
--- before the user's init.lua, so read it with `zemi.config.get` where it is
--- used, or watch it, never once at load. Raises past load, or for a default
--- the kind or choices refuse. Never a secret:
--- /config shows it, the store keeps it in plain text and any code, the
--- model's Lua included, reads it; a key comes from the environment through
--- `zemi.env.get` in the user's init.lua.
---
--- ```lua
--- zemi.config.add("interval", { kind = "number", default = 30000, desc = "how often the branch is read, in ms" })
--- ```
---@param key string
---@param spec zemi.SettingSpec
function zemi.config.add(key, spec) end

--- Call fn whenever a setting changes. A path outside the schema raises,
--- except a setting of a plugin that is off or failed to load, which
--- returns nil. Plugin code names its own settings by the bare key.
---
--- ```lua
--- local tick = zemi.timer.every(zemi.config.get("interval"), poll)
--- zemi.config.watch("interval", function(ms)
--- 	tick:stop()
--- 	tick = zemi.timer.every(ms, poll)
--- end)
--- ```
---@param path string
---@param fn fun(new: any, old: any)
---@return string? watch_id
function zemi.config.watch(path, fn) end

--- Drop a watch by the id `zemi.config.watch` returned; a malformed id raises.
---@param id string
---@return boolean removed
function zemi.config.off(id) end

--- Every known setting with its default.
---@return zemi.SettingSchema[]
function zemi.config.schema() end

-- zemi.keymap

---@class zemi.KeymapOpts
---@field desc? string Shown in /help.
---@field list? boolean A list alias: while a list, a composer popup or a prompt has the keys, the key makes the move the action names ("up", "down", "pgup", "pgdown", "home", "end", "ctrl+d", "ctrl+u"), resolved without calling Lua. One non-printable key, not one the lists answer with.

---@class zemi.Keymap
---@field key string
---@field desc string
---@field origin zemi.Origin
---@field action string "/command", a host action, or "<function>".
---@field list? boolean A list alias; its action is the move.

--- The host actions a keymap may name; some take an argument after a space.
---@alias zemi.HostAction
---| "zemi.ui.help"
---| "zemi.ui.scroll.up"
---| "zemi.ui.scroll.down"
---| "zemi.ui.scroll.top"
---| "zemi.ui.scroll.bottom"
---| "zemi.ui.scroll.line_up"
---| "zemi.ui.scroll.line_down"
---| "zemi.ui.scroll.sticky_up"
---| "zemi.ui.scroll.sticky_down"
---| "zemi.model.pick"
---| "zemi.composer.submit"
---| "zemi.composer.insert"
---| "zemi.composer.history_prev"
---| "zemi.composer.history_next"
---| "zemi.composer.paste"
---| "zemi.composer.edit"

---@class zemi.keymap
zemi.keymap = {}

--- Bind a key or a key sequence. The right-hand side is a function, a
--- command name with or without its slash ("review", "/review", so what
--- `get` hands back rebinds as is), or a host action ("zemi.ui.scroll.up").
--- Keys may be sequences that start with a non-printable key: "ctrl+x s",
--- "<leader>gs" (`<leader>` is the `keymap.leader` setting, ctrl+x by
--- default). A key the terminal never reports ("ctrl-q", "control+q") or
--- zemi keeps (ctrl+c) raises rather than binding something nothing can
--- press, and so does a sequence that starts with a printable key, since the
--- composer takes those. Setting the same key again rebinds it, whoever
--- bound it first: the last registration wins, and a key that starts a longer
--- sequence shadows it. The composer's own editing keys (ctrl+a, ctrl+e,
--- ctrl+w, home, end) bind like any other, and the composer then no longer
--- gets them.
---
--- With `opts.list` the key moves in every list, popup and prompt, and stays
--- what it was elsewhere: `zemi.keymap.set("ctrl+j", "down", { list = true })`
--- (ctrl+j still scrolls the chat). While a line types, an alias to
--- home or end keeps the key's own meaning, since the line edits with
--- those; a ctrl+u alias still fires, and in a prompt or filter it kills
--- the line. enter, esc, tab, shift+tab, space and backspace raise.
---
--- Host actions: zemi.composer.{submit,history_prev,history_next,paste,edit},
--- zemi.composer.insert <text>,
--- zemi.ui.help, zemi.model.pick,
--- zemi.ui.scroll.{up,down,top,bottom,line_up,line_down,sticky_up,sticky_down}.
---
--- ```lua
--- zemi.keymap.set("<leader>r", "/review", { desc = "review the diff" })
--- zemi.keymap.set("<leader>k", "zemi.ui.scroll.up")
--- ```
---@param key string
---@param action zemi.HostAction|string|fun()
---@param opts? zemi.KeymapOpts
function zemi.keymap.set(key, action, opts) end

--- Unbind a key whatever bound it, but not another plugin's. A key
--- nothing bound is `false`, not an error, so a reload unbinds idempotently.
---@param key string
---@param opts? { list?: boolean }
---@return boolean removed
function zemi.keymap.del(key, opts) end

--- The binding a key runs, or its list alias with `opts.list`.
---@param key string
---@param opts? { list?: boolean }
---@return zemi.Keymap|nil
function zemi.keymap.get(key, opts) end

---@return zemi.Keymap[]
function zemi.keymap.list() end

-- zemi.command

--- What a command handler receives: the raw argument string, with attached
--- text behind what was typed, and the typed words.
---@class zemi.CommandArgs
---@field args string
---@field list string[]

---@alias zemi.NArgs
---| ""   # none (the default)
---| "?"  # at most one
---| "+"  # one or more
---| "*"  # any

---@class zemi.CommandOpts
---@field desc? string Shown in /help and the slash palette.
---@field nargs? zemi.NArgs Arguments the command takes; a line with the wrong count is refused and the handler never runs.
---@field complete? fun(lead: string, line: string): string[]|zemi.CompletionItem[] Runs off the key path; return every candidate: the palette matches the rows against `lead` with the one fuzzy matcher and ranks them best first, and it is not called past the last argument `nargs` takes. An error lands in /errors.

---@class zemi.Command
---@field name string
---@field desc string
---@field nargs string
---@field origin zemi.Origin

---@class zemi.command
zemi.command = {}

--- Register a slash command. The name is typed without the slash.
---
--- ```lua
--- zemi.command.add("greet", function(a) zemi.notify(a.args) end, {
--- 	desc = "say hello",
--- 	nargs = "?",
--- 	complete = function() return { "world", "zemi" } end,
--- })
--- ```
---@param name string
---@param fn fun(args: zemi.CommandArgs)
---@param opts? zemi.CommandOpts
function zemi.command.add(name, fn, opts) end

---@param name string
---@return boolean removed
function zemi.command.del(name) end

--- Run a registered command as if it were typed.
---@param name string
---@vararg string Each argument is one word of the typed line, joined with spaces.
---@return true|nil ok
---@return string|nil err Not a command, bad arguments, or a Lua command's own error; a Go command's failure is shown by zemi, not returned.
function zemi.command.run(name, ...) end

---@return zemi.Command[]
function zemi.command.list() end

-- zemi.event

---@alias zemi.EventName
---| "app.ready"
---| "app.quit"
---| "config.reloaded"
---| "agent.start"
---| "agent.step"
---| "agent.finish"
---| "agent.error"
---| "agent.assemble"
---| "agent.subagent.start"
---| "agent.subagent.finish"
---| "agent.text_delta"
---| "agent.reasoning_delta"
---| "tool.pre"
---| "tool.post"
---| "tool.denied"
---| "permission.request"
---| "permission.resolved"
---| "ui.resize"
---| "composer.change"
---| "composer.submit"
---| "job.exit"
---| "session.reset"
---| "lsp.request"

--- The payload every handler receives, plus the event's own fields.
---@class zemi.Event
---@field name string
---@field from string "host" or the origin that emitted it.

-- NOTE: the payload classes are kept by hand from the EmitAsync and
-- EmitVetoable calls in internal/; only the event names are pinned by a test.

---@class zemi.ev.Text: zemi.Event
---@field text string agent.start: the message; the deltas: the new text.

---@class zemi.ev.AgentError: zemi.Event
---@field error string

---@class zemi.ev.AgentFinish: zemi.Event
---@field session string
---@field model string
---@field usage zemi.Usage This turn's own model calls; subagents' calls are only in `zemi.agent.usage()`.

--- One model call of a turn, a subagent's included.
---@class zemi.ev.AgentStep: zemi.Event
---@field session string
---@field model string
---@field step integer 1-based within the send.
---@field ms integer From the call's start to its end.
---@field first_token_ms integer To the first text, reasoning or tool argument; 0 when none came.
---@field tools integer Tool calls it made.
---@field usage zemi.StepUsage

--- One model call's usage, with the cache fields a turn tally leaves out.
---@class zemi.StepUsage
---@field ["in"] integer
---@field out integer
---@field cache_read integer
---@field cache_write integer
---@field cost number

--- Fired when the main agent's prompt is built (a model, trust or tool
--- change rebuilds it), not per send, and once per subagent run.
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
---@field changed string[] Absolute paths of the files the call changed: the file a write or edit wrote, and what a shell command changed in its repository (ignored files and other tools are not tracked). Commands running at the same time each list the other's changes too.

---@class zemi.ev.ToolDenied: zemi.Event
---@field tool string
---@field subject string
---@field arg string
---@field by string Who decided, such as "user" or "event".
---@field reason string

---@class zemi.ev.PermissionRequest: zemi.Event
---@field subject string The action's subject: "tool.shell" for a shell command, "fs.read", "fs.write", "lua.http.<method>".
---@field arg string The path, url or command the subject is about.
---@field reason? string
---@field agent? string The subagent asking, such as "explore agent"; "" for the main agent.

---@class zemi.ev.PermissionResolved: zemi.Event
---@field subject string
---@field arg string
---@field decision "allow"|"deny"
---@field by string

---@class zemi.ev.Resize: zemi.Event
---@field width integer
---@field height integer

---@class zemi.ev.ComposerChange: zemi.Event
---@field text string
---@field cursor integer

---@class zemi.ev.ComposerSubmit: zemi.Event
---@field text string

---@class zemi.ev.JobExit: zemi.Event
---@field id integer
---@field cmd string
---@field code integer

---@class zemi.ev.LspRequest: zemi.Event
---@field server string
---@field path string
---@field kind "diagnostics"

--- What a vetoable handler returns to veto: `decision = "deny"` with a
--- reason. Any other return lets the event through; on `permission.request`
--- a `zemi.PermVeto` may also hand the request to the ask path, and on
--- `composer.submit` a `zemi.SubmitVeto` may rewrite the text instead.
---@class zemi.Veto
---@field decision "deny"
---@field reason? string

--- What a `permission.request` handler returns: "deny" refuses the request,
--- "ask" runs zemi's ask path (the overlay) for it, nil lets it run.
---@class zemi.PermVeto: zemi.Veto
---@field decision "deny"|"ask"

--- What a `composer.submit` handler returns: "deny" refuses the send,
--- "rewrite" sends `text` in the place of what was typed, nil sends as is.
--- The handler has 50 ms, so it cannot ask the user first.
---@class zemi.SubmitVeto: zemi.Veto
---@field decision "deny"|"rewrite"
---@field text? string

---@class zemi.EventOpts
---@field id? string Name the handler so `zemi.event.off` can drop it.
---@field once? boolean Drop the handler after it fires.

---@class zemi.event
zemi.event = {}

--- Subscribe to an event. Vetoable events (`permission.request`,
--- `app.quit`, `composer.submit`, `tool.pre`) give the handler 50 ms and
--- then take their logged default; return `{ decision = "deny", reason = "..." }`
--- to veto one (`tool.pre` also takes "halt" and "rewrite", see zemi.ToolOverride),
--- and on `permission.request` `{ decision = "ask" }` to run zemi's ask path.
--- A `tool.post` handler runs as a task and may return `{ output = "..." }`
--- to replace what the model gets.
--- A plugin needs `perm` in its `uses` to subscribe to `permission.request`.
---
--- ```lua
--- zemi.event.on("composer.submit", function(ev)
--- 	if ev.text:find("rm %-rf") then return { decision = "deny", reason = "not from the composer" } end
--- end)
--- ```
---@param name zemi.EventName|string A catalogued name or `user.*`; anything else raises.
---@param fn fun(ev: zemi.Event): any
---@param opts? zemi.EventOpts
---@return string id
---@overload fun(name: "agent.start"|"agent.text_delta"|"agent.reasoning_delta", fn: (fun(ev: zemi.ev.Text): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.finish", fn: (fun(ev: zemi.ev.AgentFinish): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.error", fn: (fun(ev: zemi.ev.AgentError): any), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.assemble", fn: (fun(ev: zemi.ev.Assemble): string|{ title?: string, text: string }|nil), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.subagent.start"|"agent.subagent.finish", fn: (fun(ev: zemi.ev.Subagent): any), opts?: zemi.EventOpts): string
---@overload fun(name: "tool.pre", fn: (fun(ev: zemi.ToolPreEvent): zemi.ToolVeto?), opts?: zemi.EventOpts): string
---@overload fun(name: "agent.step", fn: (fun(ev: zemi.ev.AgentStep): any), opts?: zemi.EventOpts): string
---@overload fun(name: "tool.post", fn: (fun(ev: zemi.ev.ToolPost): zemi.ToolPostResult?), opts?: zemi.EventOpts): string
---@overload fun(name: "tool.denied", fn: (fun(ev: zemi.ev.ToolDenied): any), opts?: zemi.EventOpts): string
---@overload fun(name: "permission.request", fn: (fun(ev: zemi.ev.PermissionRequest): zemi.PermVeto?), opts?: zemi.EventOpts): string
---@overload fun(name: "permission.resolved", fn: (fun(ev: zemi.ev.PermissionResolved): any), opts?: zemi.EventOpts): string
---@overload fun(name: "app.quit", fn: (fun(ev: zemi.Event): zemi.Veto?), opts?: zemi.EventOpts): string
---@overload fun(name: "ui.resize", fn: (fun(ev: zemi.ev.Resize): any), opts?: zemi.EventOpts): string
---@overload fun(name: "composer.change", fn: (fun(ev: zemi.ev.ComposerChange): any), opts?: zemi.EventOpts): string
---@overload fun(name: "composer.submit", fn: (fun(ev: zemi.ev.ComposerSubmit): zemi.SubmitVeto?), opts?: zemi.EventOpts): string
---@overload fun(name: "job.exit", fn: (fun(ev: zemi.ev.JobExit): any), opts?: zemi.EventOpts): string
---@overload fun(name: "lsp.request", fn: (fun(ev: zemi.ev.LspRequest): any), opts?: zemi.EventOpts): string
function zemi.event.on(name, fn, opts) end

---@param id string
---@return boolean removed
function zemi.event.off(id) end

--- Emit a `user.*` event, the only names Lua may emit from any origin.
--- Handlers get a copy made the way JSON copies: every key arrives as a
--- string (`{ [7] = "x" }` reads as `ev["7"]`). A sequence payload raises,
--- since it carries no keys; send a table with string keys.
---@param name string
---@param payload? table
function zemi.event.emit(name, payload) end

---@class zemi.EventLogOpts
---@field name? string Only this event.
---@field limit? integer At most this many.

---@class zemi.EventLogEntry: zemi.Event
---@field at string "15:04:05".
---@field ts number Unix seconds.

--- Recent events newest first, each the table its handlers saw (a vetoable
--- event nobody listens to is counted but never logged), the last 500.
--- The streaming deltas are counted in stats only; a `tool.post` entry's
--- `output` is cut to 4096 runes.
---@param opts? zemi.EventLogOpts
---@return zemi.EventLogEntry[]
function zemi.event.list(opts) end

--- Emissions per event name since zemi started; diff two snapshots for a
--- session or a turn.
---@return table<string, integer>
function zemi.event.stats() end

-- zemi.ui

---@alias zemi.TodoStatus
---| "pending"
---| "in_progress"
---| "done"

---@class zemi.Todo
---@field content string
---@field status zemi.TodoStatus
---@field active_form string

---@class zemi.Usage
---@field ["in"] integer
---@field out integer
---@field total integer
---@field cost number
---@field priced? boolean The session tally only: the conversation's model declares prices, which is what a cost widget shows.

---@class zemi.ContextUsage
---@field tokens integer
---@field window integer
---@field pct number

---@alias zemi.RunStatus
---| "running"
---| "queued"
---| "done"
---| "warn"
---| "failed"
---| "cancelled"

---@alias zemi.TurnEnd
---| "ok"
---| "error"
---| "cancelled"

---@alias zemi.Waiting
---| "approval"  # a permission ask
---| "plan"      # a plan to approve
---| "answer"    # a question

--- One overlay row. Enter on an item with `detail` or `fields` opens its
--- view (never on_select); otherwise it fills, runs or selects the item, and
--- does nothing when none applies. What acts on a row is its own `keys`, on
--- the row from the list and named in the footer of its view: a row has one
--- view, never a second one of buttons. `run` is honored only for user
--- config.
---@class zemi.OverlayItem
---@field text string the label: a filter word matches it fuzzily, or as a substring when the text holds a space (prose)
---@field id? string the row's own data: what a callback matches on, and the row the list reopens at
---@field desc? string dim column after the text; the filter finds words in it
---@field preview? string shown whole under the list while this row is selected, in a pane as tall as the longest preview (4 rows at most), so moving never shifts a row; the filter matches it too
---@field tag? string right-aligned tag
---@field on? boolean draws the on/off switch in the tag's place
---@field locked? boolean draws the switch on with nothing to switch
---@field danger? boolean cannot be undone: drawn in Danger, and the first enter only arms it (never the open of a row with a detail view); the footer then says to press it again, and any other key disarms
---@field tail? boolean a text too wide for its column is cut from the left, keeping its end: a path's name
---@field mark? zemi.MarkKind a state mark ahead of the text and of the detail view's `status` field, as the chat draws it (`zemi.ui.marks`); a running one spins
---@field section? string a change of section starts with a header row; the filter matches it too
---@field detail? string body of the detail view
---@field fields? zemi.OverlayField[] the detail view's facts, set as a table above detail
---@field keys? zemi.OverlayItemKey[] what acts on the row: a character each, run on it from the list, and named in the footer of its view
---@field fill? string put in the composer (ignored while the overlay has an on_select, which takes enter instead)
---@field run? string a command line such as "mcp restart gitlab" (ignored while the overlay has an on_select)
---@field copy? string the row's value: y copies it in place of the row's text, and enter copies it, keeping the list open, when the row does nothing else (no on_select, run, fill or detail); the filter matches it too
---@field verb? zemi.OverlayVerb enter's action in the hint where the default would say another: "open" (a screen of its own), "pick" (a value), "run" (a command), "toggle", "fill", "copy" or "stop"; space passes the row by in a toggle list, and an "open" row's run leaves the list under the screen it opens
---@field anchor? integer The transcript row this one stands for, counted as `ctx.transcript` is (1-based); a list with `follow` set scrolls the chat here when the row is selected.

--- What enter does on a row, in one word for the footer hint.
---@alias zemi.OverlayVerb
---| "open" a screen of its own, over the list
---| "pick" a value
---| "run" a command
---| "toggle" a toggle list's row
---| "fill" the composer
---| "copy" the row's value
---| "go" a following list's row: enter lands there, closing the menu
---| "stop" the screen's own thing stops: a running loop or goal
---| "paste" a key into the masked input the row opens

--- One of a row's keys: a character that acts on the row from the list.
---@class zemi.OverlayItemKey
---@field key string one character; j, k, /, y and Y are the list's and raise
---@field desc? string named beside the key in the footer, a lowercase word or phrase like every hint ("open the view", "copy")
---@field id? string hands the key to the overlay's on_action, which takes it over fill and run
---@field fill? string put in the composer, closing the list
---@field run? string a command line, run on the row; with `verb = "open"` the screen it opens sits over the list, which stays, and with `verb = "go"` it takes the chat, closing even a `reload` list
---@field danger? boolean cannot be undone: the first press only arms it, and the footer says to press it again
---@field verb? zemi.OverlayVerb "open" for a run that opens a screen

--- A detail view's fact: its name in the dim column, its value beside it. A
--- "status" value colors the status words every screen shares, an "error" one
--- draws in Danger, and an empty value leaves the row out.
--- `fields = { { "status", "ready" }, { "error", err or "" } }`
---@class zemi.OverlayField
---@field [1] string name
---@field [2] string value

---@class zemi.OverlayKey
---@field desc? string named next to the key in the footer
---@field fn fun(item: any) Gets the selected item, nil on an empty list.

---@alias zemi.OverlayAnchor
---| "bottom-left"
---| "bottom-right"

---@class zemi.OverlaySpec An unknown field raises.
---@field id string
---@field title? string
---@field body? string|zemi.Line[] text above the items, or styled lines (text and hl only); with no items a read-only view
---@field markdown? boolean render a string `body` and every item's `detail` as markdown
---@field empty? string the line shown when there are no items, under the body when both are set
---@field items? zemi.OverlayItem[]|any[]
---@field render_item? fun(item: any): string|zemi.Line A line's segments keep their `hl` (text, hl only); unstyled ones take the row's.
---@field selected? integer 1-based item selected when it opens.
---@field anchor? zemi.OverlayAnchor In line with the composer frame's edge, by the statusline pill that opened it; nil centers it.
---@field follow? boolean Moving the selection scrolls the chat to the row the selected item names by its `anchor`, centers that message when the whole of it fits, and bands its rows while the list is open; the list opens on the message a jump last placed the view at, while the view still sits where it left it, or else the one over the view's middle (the last message when the view shows the end of the transcript). `enter` on a row with no level under it closes the menu keeping that place (`go`), and `esc` puts the chat back where it was.
---@field on_select? fun(item: any) Takes enter for every item, so the items' own `fill` and `run` no longer act (a `reload` list runs both).
---@field on_action? fun(item: any, key: zemi.OverlayItemKey) Runs for a row key with an `id`; the list closes like on on_select, a `reload` list rebuilds.
---@field keys? table<string, zemi.OverlayKey> One character each (j, k, /, y and Y are the list's and raise, and this wins over a row's own keys): a second verb on the selected item, which closes or rebuilds the list like on_action.
---@field on_close? fun()
---@field reload? fun(): any[] Makes a toggle list: enter and space (while no filter types) run on_select (or the row's `run`) and keep the overlay open, then the items this returns replace the list, rendered again, keeping selection and filter. Enter on a row with `fill` runs on_select, fills the composer and closes the list. It runs again when an overlay opened over the list closes. Both run off the key path; an error, an item field of the wrong type included, keeps the old items and shows in /errors.

---@class zemi.SelectOpts
---@field prompt? string
---@field format_item? fun(item: any): string|zemi.Line

---@class zemi.InputOpts
---@field prompt? string
---@field default? string
---@field placeholder? string Shown dim in the empty line, what submitting it would mean.

---@class zemi.ui
zemi.ui = {}

--- A size along the parent box's direction: whole cells, "N%" of the box's
--- cells after separators, "fill" (a share of what is left, by `weight`) or
--- "content" (the rows a region, bar or composer draws; in a col only).
---@alias zemi.Size integer|string

--- Set the terminal tab's title (escapes stripped, whitespace collapsed);
--- nil gives it back to zemi, which names it after the session. A reload
--- that does not set it again gives it back too.
--- zemi puts its turn mark (a spinner, `?`, `✓` or `✗`) ahead of the text.
---@param text? string
function zemi.ui.title(text) end

--- Render every widget again on the next frame. Keymaps, clicks, one-shot
--- timers, job, http and agent callbacks and event handlers redraw
--- on their own when they return; a repeating timer or a widget fed by
--- other state calls this. Raises inside render.
function zemi.ui.redraw() end

---@alias zemi.ScrollAction
---| "up"         # a page, with a one-row overlap
---| "down"
---| "top"
---| "bottom"
---| "line_up"
---| "line_down"
---| "sticky_up"   # to the header (a run of sticky rows) above the view
---| "sticky_down" # to the next header below it, else the tail

--- Scroll the chat, as the zemi.ui.scroll.* host actions do: for a click or
--- a handler. A "top" or "bottom" issued before the rows arrive applies after
--- the next layout, and "top" holds until the chat scrolls again.
---@param action zemi.ScrollAction
function zemi.ui.scroll(action) end

---@class zemi.ui.overlay
zemi.ui.overlay = {}

--- Opening the id of an open overlay replaces it in place, keeping selection
--- and filter, when the caller may close it (see close); otherwise the open
--- is refused and lands in /errors. An item or action field of the wrong type
--- raises (a list item's `id` is its own data).
---
--- ```lua
--- zemi.ui.overlay.open({
--- 	id = "snippets",
--- 	title = "Snippets",
--- 	items = { { text = "review", desc = "review the diff", fill = "/review " } },
--- })
--- ```
---@param spec zemi.OverlaySpec
function zemi.ui.overlay.open(spec) end

--- Close an overlay and run its on_close. Closes nothing unless user
--- config, builtin code or the origin that opened it calls it.
---@param id string
function zemi.ui.overlay.close(id) end

--- A picker in the shape of `vim.ui.select`. on_choice gets the item on
--- enter, nil on cancel. Items take the zemi.OverlayItem keys too. A task
--- can wait on the answer with the handle instead.
---
--- ```lua
--- zemi.ui.select({ "low", "medium", "high" }, { prompt = "Effort" }, function(choice)
--- 	if choice then zemi.config.set("agent.effort", choice) end
--- end)
--- ```
---@param items any[]
---@param opts? zemi.SelectOpts
---@param on_choice fun(item: any|nil)
---@return zemi.UIHandle
function zemi.ui.select(items, opts, on_choice) end

--- A one-line prompt in the shape of `vim.ui.input`: on_confirm gets the
--- text on enter, which its footer names "send", nil on esc. A task can
--- wait on the answer with the handle instead.
---
--- ```lua
--- zemi.ui.input({ prompt = "Branch name" }, function(text)
--- 	if text then zemi.notify(text) end
--- end)
--- ```
---@param opts? zemi.InputOpts
---@param on_confirm fun(text: string|nil)
---@return zemi.UIHandle
function zemi.ui.input(opts, on_confirm) end

--- What `zemi.ui.select` and `zemi.ui.input` return: `wait()` suspends the
--- task until the user answers (the item, or the text; nil and no error on
--- cancel), `cancel()` closes the overlay, which answers nil. The callback
--- fires with the same answer either way. Raises outside a task and under
--- pcall, like `zemi.HTTPHandle.wait`.
---@class zemi.UIHandle
---@field wait fun(self: zemi.UIHandle): any|nil, string|nil
---@field cancel fun(self: zemi.UIHandle)

---@class zemi.EditOpts
---@field line? integer 1-based line to start at
---@field title? string working-row label while the editor is open (default: the file name)

--- Open a file in the user's editor (editor.command, $VISUAL, $EDITOR)
--- fullscreen over zemi, and call on_done when it exits: on_done(),
--- or on_done(err) when it failed or exited non-zero. A relative path
--- resolves against the project dir. Never blocks the interpreter.
---
--- ```lua
--- zemi.ui.edit(zemi.fs.data_dir("notes") .. "/todo.md", { line = 1 }, function(err)
--- 	if err then zemi.notify.error(err) end
--- end)
--- ```
---@param path string
---@param opts? zemi.EditOpts
---@param on_done fun(err: string|nil)
function zemi.ui.edit(path, opts, on_done) end

--- Cells `text` draws in, measured the way the renderer does: a wide glyph
--- (CJK, emoji) is two, a combining mark zero. Use it to clip, pad and
--- align, never `#text`.
---@param text string
---@return integer
function zemi.ui.width(text) end

--- The state marks the chat's rows and the host's lists share, one glyph
--- (one cell) and theme group each.
---@class zemi.ui.marks
---@field spin zemi.Mark running: draw it with `effect = "spin"` so the UI steps it
---@field ok zemi.Mark finished
---@field warn zemi.Mark finished, but something it ran failed
---@field err zemi.Mark failed
---@field stop zemi.Mark stopped by the user
---@field bg zemi.Mark going on in the background
---@field wait zemi.Mark waiting: queued, paused or on the user
---@field thought zemi.Mark reasoning
---@field note zemi.Mark a notice
zemi.ui.marks = {}

---@class zemi.Mark
---@field glyph string
---@field hl string

---@alias zemi.MarkKind
---| "spin"
---| "ok"
---| "warn"
---| "err"
---| "stop"
---| "bg"
---| "wait"
---| "thought"
---| "note"

--- The drawn screen as plain text, one line per row, styling and trailing
--- padding dropped. This is how zemi reads its own UI instead of asking what
--- the user sees.
---@return string|nil screen
---@return string|nil err
function zemi.ui.screenshot() end

--- Open the /help overlay.
function zemi.ui.help() end

-- zemi.notify and zemi.log

---@class zemi.NotifySpec
---@field title? string
---@field body? string
---@field level? zemi.Level

--- A toast. Callable: `zemi.notify({ title = "Plugin installed", body = "foo v1.2.0" })`.
--- The title says what happened, in sentence case with no period: "<Kind>
--- <past participle>" with the name in the body, "Could not <verb> <object>"
--- with the cause in the body, "Nothing to <verb> [yet]" with a reason only
--- when it is not obvious. A lone title or body sits inside the box. Live
--- toasts of one level and title share a section: the title once, a row per body.
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
---@field level zemi.Level
---@field title string
---@field body string

--- Recent notifications from zemi.notify and the host, newest first (the
--- last 100).
---@return zemi.NotifyEntry[]
function zemi.notify.list() end

--- The run's log file, one record per line, a field written as `ctx.key=`.
--- Callable: `zemi.log("message", { key = value })`.
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
--- same list `/logs` shows.
---@return zemi.LogEntry[]
function zemi.log.list() end

-- zemi.provider and zemi.model

---@class zemi.ModelCapabilities
---@field vision? boolean False for a model that refuses image input; read then returns a text error and @image attachments are omitted with a note. Assumed on where the table omits it.
---@field audio? boolean True for a model that takes audio parts, as an MCP result's audio; assumed off where the table omits it.

---@class zemi.ModelPricing
---@field input? number USD per million prompt tokens.
---@field output? number USD per million completion tokens.
---@field cache_write? number USD per million tokens written to the prompt cache.
---@field cache_read? number USD per million tokens read from it.

---@class zemi.ModelSpec
---@field id string
---@field display? string
---@field context_window? integer Without it there is no context percent in /usage and a subagent trims its history only after the provider refuses it.
---@field pricing? zemi.ModelPricing
---@field max_output? integer Reply token cap; unset, anthropic models get 32000, openai_compat ones 65536, others the provider default.
---@field capabilities? zemi.ModelCapabilities The model's own capabilities, the same shape the bundled catalog carries.
---@field api? string The wire format the model speaks, `"completions"`, `"responses"` or `"messages"`, when it differs from its provider's own - one provider can serve formats side by side.
---@field max_attachments? integer The @ references one message may carry.
---@field decisions? boolean A model that answers classification through its provider's own protocol; only the `system_one` kind takes it, and no chat turn runs on one.
---@field rate_limit? integer|zemi.ProviderRateLimit The model's own quota, for a gateway that limits per model alias; that model alone in its own bucket, the provider's (unset here) taken when absent.

---@class zemi.ProviderSpec
---@field id string
---@field kind zemi.ProviderKind
---@field api_key? string Read it with `zemi.env.get`; leave it empty and paste a key in /keys instead - zemi stores that in the OS keychain. Never write a key into a file.
---@field base_url? string Required for openai_compat, refused for openrouter; overrides the default endpoint for the other kinds.
---@field headers? table<string, string> Sent with every request; as secret as the key.
---@field body? table<string, any> Extra request-body fields, merged verbatim into every request (service_tier, a gateway's routing block). Not sent by google.
---@field object_mode? zemi.ProviderObjectMode Pins how this provider answers an object call (`zemi.model.ask` with a schema, `zemi.classify.ask`). Only the openai, openai_compat, google, openrouter and vercel kinds take it: anthropic always picks its own.
---@field rate_limit? integer|zemi.ProviderRateLimit Requests per minute this provider allows, or the full quota as a table ({ requests = 40, completion_tokens = 52000, window = 60 }): every call to it paces through one budget, the conversation waiting its turn beside the summary builder's, so a low-rate-limit provider serves instead of refusing. Leave it unset to run unpaced; an explicit 0 is refused. A model entry may carry its own rate_limit, that model alone in its own bucket.
---@field models zemi.ModelSpec[]

---@class zemi.ProviderRateLimit
---@field requests? integer Calls per window.
---@field completion_tokens? integer Reply tokens per window, charged as the reply streams in; a reply over the budget holds the next calls until it refills.
---@field window? integer The window in seconds; 60 when unset.

---@alias zemi.ProviderKind
---| "anthropic"
---| "openai"         # gpt-4 and later, over /responses
---| "openai_compat"  # any chat-completions endpoint; base_url required
---| "google"
---| "openrouter"
---| "vercel"
---| "system_one"     # a classifier endpoint (`zemi.classify.ask`), never a chat model

--- What the provider is made to do with a schema. A model that answers an
--- object call with a sentence is pinned to `tool` or `text`.
---@alias zemi.ProviderObjectMode
---| "auto" # the kind's own ladder: JSON mode, then a forced tool call, then the prompt
---| "json" # the provider's native JSON mode where it has one
---| "tool" # a forced tool call whose arguments are the object
---| "text" # the schema written into the prompt, the reply parsed as JSON

---@class zemi.ProviderInfo
---@field id string
---@field kind zemi.ProviderKind
---@field base_url string
---@field builtin boolean From the builtin catalog; false once `zemi.provider.add` replaced it.

---@alias zemi.ModelTarget
---| "default"  # the conversation, and the root every role follows
---| "agents"   # subagents
---| "compact"  # the summary tree's builder
---| string     # a name zemi.model.add registered

---@class zemi.ModelInfo
---@field id string "provider/model".
---@field display string
---@field has_key boolean
---@field roles zemi.ModelTarget[] The model targets that run on this model. "default" is models.default.

---@class zemi.provider
zemi.provider = {}

--- Register a provider and its models. An id a builtin
--- provider has replaces it whole (a gateway in front of it keeps its
--- base_url); the builtin ones are on while their key is exported. An unknown
--- field on the spec or on a model entry raises.
---
--- ```lua
--- zemi.provider.add({
--- 	id = "gw",
--- 	kind = "openai_compat",
--- 	base_url = "https://gw.example/v1",
--- 	api_key = zemi.env.get("GW_API_KEY"),
--- 	models = { { id = "deepseek-chat", context_window = 128000 } },
--- })
--- ```
---@param spec zemi.ProviderSpec
function zemi.provider.add(spec) end

--- Remove a provider, builtin or added; an id nothing registered is a
--- no-op, so init.lua works on a machine where that key is not exported.
--- `providers.builtin = false` drops every builtin one.
---@param id string
---@return boolean removed
function zemi.provider.del(id) end

---@return zemi.ProviderInfo[]
function zemi.provider.list() end

---@class zemi.model
zemi.model = {}

---@return zemi.ModelInfo[]
function zemi.model.list() end

---@class zemi.ModelTargetSpec
---@field desc string What `/model roles` shows for the slot.
---@field model? zemi.ModelTarget|string The model the slot runs until someone picks: a builtin role name (resolved on every read) or a `"provider/model"` id; "" follows models.default.
---@field classify? boolean true for a slot `zemi.classify.ask` runs on: `/model <slot>` then offers the decisions models beside the conversation's.

--- Register a model slot of its own, for code that runs a model on a job and
--- not a conversation: `/model roles` lists it among the roles and
--- `models.<name>` picks its model. User, builtin or plugin origin; a builtin
--- role name and `default` are reserved, and so is a name another origin
--- holds. The name is the key, so it is a lowercase word.
---
--- ```lua
--- zemi.model.add("classifier", { desc = "the permission classifier", model = "anthropic/claude-haiku-4-5" })
--- local verdict = zemi.model.ask({ model = "classifier", prompt = cmd }):wait()
--- ```
---@param name string
---@param spec zemi.ModelTargetSpec
function zemi.model.add(name, spec) end

--- Drop a slot `zemi.model.add` registered, with `models.<name>` and the pick
--- kept for it. User, builtin or plugin origin; false for a name nothing registered.
---@param name string
---@return boolean removed
function zemi.model.del(name) end

--- Switch the model the conversation runs on, models.default. Raises for a
--- provider nobody registered; an
--- id the provider does not list is taken and warned about. A call made after
--- boot is kept for later starts, like
--- `zemi.config.set`. An explicitly named target is plain config:
--- `zemi.config.set("models.compact", id)`.
---@param id string
function zemi.model.set(id) end

---@class zemi.ModelAsk
---@field prompt string
---@field system? string
---@field model? zemi.ModelTarget|string A target name or a "provider/model" id; "default" (models.default) by default.
---@field max_output? integer The reply's token cap, 1024 by default.
---@field schema? table The shape the reply must have, a JSON schema in the same fields a tool's schema has: `{ type = "object", properties = {...}, required = {...} }` for an object, or a bare `{ type = "string" }`, `{ type = "integer", minimum = 0, maximum = 9 }`, `{ type = "boolean" }` or `{ type = "array", items = ... }`. `enum` pins the values, `description` is read by the model. Any other field and any other type raises. With it the call returns a decoded value instead of text (see `zemi.ModelHandle.wait`).
---@field on_done? fun(value: any, err_or_raw: string?) Runs as a task when the reply is in: the reply's text alone without a `schema`, else the decoded value with the raw reply beside it, or nil and the error when the call failed.

---@class zemi.ModelHandle
---@field wait fun(self: zemi.ModelHandle): any, string|nil Suspends the task until the reply, 10 minutes at most; raises outside a task and under pcall, like `zemi.HTTPHandle.wait`. Without a `schema`: the text, or nil and the error. With one: the decoded value (a table for an object, a number, string or boolean for a scalar) and the raw reply, or nil and the error, whose text carries the reply when it did not fit the schema.
---@field cancel fun(self: zemi.ModelHandle)

--- One tool-less model call, run off the interpreter and billed to the live
--- session; a reload cancels it. A plugin's own call settles where its
--- manifest declares `model` (one it does not declare is refused, not asked).
---
--- ```lua
--- local title, err = zemi.model.ask({ model = "default", prompt = "Name this diff:\n" .. diff }):wait()
--- ```
---
--- With a `schema` the provider is left to make the model fill it - its
--- native JSON mode, a forced tool call, or the schema in the prompt - and
--- zemi hands back the decoded value, so nothing parses the reply:
---
--- ```lua
--- local v, raw = zemi.model.ask({
--- 	prompt = "Is this command safe to run?\n" .. cmd,
--- 	schema = { type = "object", properties = { safe = { type = "boolean" } }, required = { "safe" } },
--- }):wait()
--- ```
---@param spec zemi.ModelAsk
---@return zemi.ModelHandle
function zemi.model.ask(spec) end

--- Open a model picker and return at once: with no target `/model`, else that
--- target's list from `/model roles`. Choosing a model runs `/model <id>`, or
--- `/model <target> <id>` for another target. Raises for an unknown target.
---@param role? zemi.ModelTarget
function zemi.model.pick(role) end

-- zemi.classify

---@class zemi.classify
zemi.classify = {}

--- How a call was answered. `protocol` is a decisions model (a `system_one`
--- provider), `logprobs` a chat model whose provider returned token
--- probabilities, `schema` a chat model that returned neither.
---@alias zemi.ClassifyStrategy
---| "protocol" # the provider judged the state itself; the answers carry probabilities
---| "logprobs" # the answer was read off the next-token probabilities of a one-label prompt
---| "schema"   # the reply was decoded against a schema; no confidence comes with it

---@alias zemi.ClassifierType
---| "bool"   # value and probability
---| "choice" # choice, probabilities and confidence
---| "score"  # score and confidence

---@class zemi.ClassifierQuestion
---@field type zemi.ClassifierType
---@field instructions string One line saying what to judge about the state.
---@field criteria table A bool's `{ ["true"] = "...", ["false"] = "..." }` (what each answer means), a choice's `{ label = "what it means", ... }`, or a score's list of levels, lowest first.

---@class zemi.ClassifierAnswer
---@field value? boolean A bool question's answer.
---@field probability? number How likely `true` was, where a probability answered: absent from a `schema` answer, so a caller that needs certainty checks it.
---@field choice? string A choice question's answer: one of its labels. The labels are read in sorted order, which is the order the logprobs strategy's letters stand for.
---@field probabilities? table<string, number> Per-label odds, where a probability answered.
---@field score? number A score question's level, 0-based, fractional where it is a probability-weighted one.
---@field confidence? number How one-sided the answer was, 1 for a unanimous one and 0 for a tie; absent where nothing was answered with probabilities.

---@class zemi.ClassifyResult
---@field strategy zemi.ClassifyStrategy Which strategy answered.
---@field answers table<string, zemi.ClassifierAnswer> One per question, by name.
---@field usage zemi.Usage What the call cost.

---@class zemi.ClassifySpec
---@field model? zemi.ModelTarget|string Any model target or `"provider/model"` id; the conversation's model by default.
---@field state table The data to judge, handed to the model as data. A prompt-injection attempt inside it is data too.
---@field questions table<string, zemi.ClassifierQuestion> What to ask about the state.
---@field timeout_ms? integer Milliseconds to wait, 60000 by default.
---@field on_done? fun(res: zemi.ClassifyResult|nil, err: string|nil) Runs as a task when the verdict settles, `res` as `wait` answers it. The one way to act from a plain event handler, where `wait` cannot suspend.

---@class zemi.ClassifyHandle
---@field wait fun(self: zemi.ClassifyHandle): zemi.ClassifyResult|nil, string|nil Suspends the task until the verdict, `timeout_ms` at most; raises outside a task and under pcall, like `zemi.HTTPHandle.wait`. From a plain event handler, pass `on_done` instead.
---@field cancel fun(self: zemi.ClassifyHandle)

--- Judge data with a model: every question gets a validated answer, or the
--- call is an error. Nothing here parses a sentence - the strategy that
--- answers is reported back, and a strategy that fails does not settle
--- anything for the next one.
---
--- A plugin's own call settles where its manifest declares `model`. The
--- provider's key is read in Go.
---
--- ```lua
--- local res = zemi.classify.ask({
--- 	model = "classifier",
--- 	state = { cmd = "rm -rf build", cwd = "~/src/app" },
--- 	questions = {
--- 		destructive = {
--- 			type = "bool",
--- 			instructions = "Does this command delete anything the user made?",
--- 			criteria = { ["true"] = "it deletes their work", ["false"] = "it only removes a build artifact" },
--- 		},
--- 	},
--- }):wait()
--- if res and res.answers.destructive.probability and res.answers.destructive.probability > 0.9 then
--- 	-- the model is sure
--- end
--- ```
---@param spec zemi.ClassifySpec
---@return zemi.ClassifyHandle
function zemi.classify.ask(spec) end

-- zemi.agent

--- The model the conversation runs on, as the statusline sees it. The
--- session id is `zemi.session.id()`.
---@class zemi.AgentCurrent
---@field model string

---@class zemi.AgentUsage: zemi.Usage
---@field context zemi.ContextUsage

---@class zemi.AgentDef
---@field name string
---@field description string
---@field source zemi.Origin
---@field model zemi.ModelTarget|string The file's `model:` (a target such as "agents", or a "provider/model"); "" when it omits one, which runs the "agents" role.

---@class zemi.AgentRunSpec
---@field prompt string
---@field agent? string A name from `zemi.agent.list()`, or "fork" to continue the live conversation, which needs a turn running; "general" by default.
---@field description? string Shown while it runs.

---@class zemi.RunHandle
---@field wait fun(self: zemi.RunHandle): string|nil, string|nil Suspends the task until the run ends: its report, or nil and the error. Raises outside a task and under pcall, like `zemi.HTTPHandle.wait`.
---@field cancel fun(self: zemi.RunHandle)

---@class zemi.agent
zemi.agent = {}

---@return zemi.AgentCurrent
function zemi.agent.current() end

--- Send a message. Returns at once; the turn runs off the interpreter, so
--- the error is what can be known before it starts (no model yet); during
--- a turn it joins at the next step. A plugin's own send settles where its
--- manifest declares `agent`; its text reaches the
--- model as the plugin's, never the user's words.
---@param text string
---@return true?, string?
function zemi.agent.send(text) end

--- Interrupt the running turn.
function zemi.agent.cancel() end

---@return zemi.AgentUsage
function zemi.agent.usage() end

--- The pinned system prompt and context message the last send carried, "" before the first one.
--- `/prompt` shows the same text.
---@return string
function zemi.agent.prompt() end

--- The subagents this project can run: builtin, `agents/*.md`, and trusted
--- project definitions. They also complete in the `@` popup, and a mention
--- of one attaches the hint that starts it.
---@return zemi.AgentDef[]
function zemi.agent.list() end

--- Run a subagent off the interpreter. A reload cancels it; `wait` suspends
--- until it ends, `on_done` fires the same values.
---
--- ```lua
--- zemi.agent.run({ agent = "explore", prompt = "Where is the retry logic?" }, function(text, err)
--- 	zemi.notify(text or err)
--- end)
--- ```
---@param spec zemi.AgentRunSpec
---@param on_done? fun(text: string|nil, err: string|nil)
---@return zemi.RunHandle
function zemi.agent.run(spec, on_done) end

-- zemi.workflow: run() and list() in config Lua; the rest exist only inside
-- a workflow script, which also has zemi.json, zemi.csv and zemi.yaml and nothing else of zemi.

---@class zemi.WorkflowRunSpec
---@field script? string Workflow source to run.
---@field name? string A script saved under workflows/ instead.
---@field args? any The script's zemi.workflow.args.
---@field concurrency? integer Agents at once, 1-16 (default 5).

---@class zemi.WorkflowHandle: zemi.RunHandle
---@field id string The run's id, as /workflows shows it.

---@class zemi.WorkflowAgentOpts
---@field agent? string An agent definition, or "fork" to continue the live conversation; default "general".
---@field description? string Row label; default the prompt's first line.
---@field phase? string Group for this agent; default the last phase().
---@field model? string A role or provider/model id.
---@field schema? table A JSON Schema object: the report is then a table matching it.

---@class zemi.WorkflowInfo
---@field name string
---@field description string The file's leading comment.
---@field source zemi.Origin
---@field path string

---@class zemi.workflow
---@field args any Workflow scripts only: the args the run was given.
zemi.workflow = {}

--- Saved workflows also complete in the `@` popup, and a mention of one
--- attaches the hint that runs it.
---@return zemi.WorkflowInfo[]
function zemi.workflow.list() end

--- Run a workflow in its own Lua state; a reload cancels it. Raises for a
--- name nothing saved; an untrusted project's, or a past-cap run, is nil, err.
--- ```lua
--- zemi.workflow.run({ name = "review" }, function(result, err) zemi.agent.send(result or err) end)
--- ```
---@param spec zemi.WorkflowRunSpec
---@param on_done? fun(result: string|nil, err: string|nil, log: string[]) A result that is not a string arrives as JSON.
---@return zemi.WorkflowHandle|nil handle
---@return string|nil err One in an untrusted project, or past the background cap.
function zemi.workflow.run(spec, on_done) end

--- Workflow scripts only. Runs a subagent and waits for its report.
--- ```lua
--- local plan, err = zemi.workflow.agent("Plan the change to x.go", { agent = "plan" })
--- ```
---@param prompt string Everything the agent needs: it does not see the conversation.
---@param opts? zemi.WorkflowAgentOpts
---@return string|table|nil report A table when opts.schema is set.
---@return string|nil err
function zemi.workflow.agent(prompt, opts) end

--- Workflow scripts only. Runs the functions at once.
---@param fns (fun(): any)[]
---@return any[] results In order; nil where a function failed.
---@return string[]|nil errs nil when none failed.
function zemi.workflow.parallel(fns) end

--- Workflow scripts only. Runs each item through the stages; items do not wait for each other.
--- ```lua
--- local fixed = zemi.workflow.pipeline(files, function(f) return zemi.workflow.agent("Find bugs in " .. f) end,
--- 	function(bugs, f) return zemi.workflow.agent("Fix these in " .. f .. ":\n" .. bugs) end)
--- ```
---@param items any[]
---@param ... fun(prev: any, item: any, i: integer): any
---@return any[] results In item order; nil where a stage ended the item.
---@return string[]|nil errs Where a stage returned nil, err.
function zemi.workflow.pipeline(items, ...) end

--- Workflow scripts only. Groups the agents started after it.
---@param name string
function zemi.workflow.phase(name) end

--- Workflow scripts only. A line the user sees; the result carries it.
---@param ... any
function zemi.workflow.log(...) end

-- zemi.session

---@class zemi.session
zemi.session = {}

--- The chat's session id: the anchor row, "main".
---@return string id
function zemi.session.id() end

--- The chat's todo list, as the todos tool last wrote it.
---@return zemi.Todo[]|nil todos
function zemi.session.todos() end

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
---@field items fun(lead: string, line: string): string[]|zemi.CompletionItem[] `lead` is the typed word after the trigger (or the argument under the cursor), `line` the draft up to the cursor. Runs off the key path, on every edit of the word; the popup shows the rows as returned, so match them against `lead` yourself (by meaning too: an issue number may find its title). At most 50 items across a popup's sources.

---@class zemi.composer
zemi.composer = {}

--- Register a completion source. Accepting a row puts its text after the
--- trigger, or in place of the argument, followed by a space. Raises on a
--- bad spec or an id another plugin registered.
---
--- ```lua
--- zemi.composer.add({
--- 	id = "issues",
--- 	trigger = "#",
--- 	items = function(lead) return { { text = "12", desc = "login fails on Safari" } } end,
--- })
--- ```
---@param spec zemi.CompletionSpec
function zemi.composer.add(spec) end

---@return zemi.ComposerState
function zemi.composer.get() end

---
---@param text string
---@param cursor? integer -1, the default, puts the cursor at the end.
function zemi.composer.set(text, cursor) end

---
---@param text string
function zemi.composer.insert(text) end

--- Submit the draft, as if enter was pressed.
function zemi.composer.submit() end

--- The previous sent message, like up on a one-line draft.
function zemi.composer.history_prev() end

--- The next sent message, back to an empty draft past the newest.
function zemi.composer.history_next() end

--- Paste the clipboard: an image becomes an attachment token, text is
--- inserted (ctrl+v).
function zemi.composer.paste() end

--- Open the draft in the user's editor (ctrl+g when no keymap starts with
--- it).
function zemi.composer.edit() end

-- zemi.perm

---@class zemi.PermAskSpec
---@field subject string What the ask is about, as the overlay titles it: a gate subject like "fs.write" or "net.github.com", or the plugin's own word.
---@field arg? string What the ask acts on: a path, a URL, a command.
---@field reason? string Why the plugin asks; the overlay's why row.
---@field preview? string A diff or code the overlay shows under the ask.

---@class zemi.PermAnswer
---@field decision "allow"|"deny"|"told" `told`: the user denied and told zemi what to do instead.

---@class zemi.PermAskHandle
---@field wait fun(self: zemi.PermAskHandle): zemi.PermAnswer|nil, string|nil Suspends the task until the user answers; raises outside a task, like `zemi.JobHandle.wait`.

---@class zemi.perm
zemi.perm = {}

--- Ask the user through zemi's permission overlay. Only from a task; a
--- plugin needs the manifest's `uses` naming `perm`. An ask nobody can
--- answer denies.
---
--- ```lua
--- local ans = zemi.perm.ask({ subject = "fs.write", arg = path, reason = "cache refresh" }):wait()
--- if ans.decision ~= "allow" then return nil, "refused" end
--- ```
---@param spec zemi.PermAskSpec
---@return zemi.PermAskHandle
function zemi.perm.ask(spec) end

-- zemi.tool

---@class zemi.ToolSpec An unknown field raises.
---@field name string A lowercase identifier (`^[a-z][a-z0-9_]*$`); a builtin's name is refused.
---@field description string What the model reads to decide when to call it. Required.
---@field input? table The parameters, flat: a name to its JSON Schema. A `properties` key here raises.
---@field required? string[] Which of `input`'s parameters the model must send.
---@field prompt? string A system prompt section only a run that has the tool gets. zemi adds the heading ("## Tool <name>"), so write the body only. Fixed text: what changes between turns costs the provider's prompt cache.
---@field execute fun(args: table, run: zemi.ToolRun): any, string? A string, a table whose `output` is the string, or anything else sent as JSON. `nil, err` fails the call with `err`. It runs as a task: a handle's `wait` in it suspends it and frees the interpreter.
---@field timeout_ms? integer Wall time for one call, waits included; 60000 by default.
---@field parallel? boolean Safe to run beside other tool calls.
---@field final? boolean A successful call ends the turn: the model's last act, nothing after it. A failing one behaves as always, so the model can retry.

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

---@alias zemi.ToolVerdict
---| "deny"
---| "halt"
---| "rewrite"

--- What a `tool.pre` handler returns: "deny" refuses the call, "halt" also
--- ends the turn, "rewrite" merges `input` into the call's input. nil lets
--- the call through.
---@class zemi.ToolVeto
---@field decision zemi.ToolVerdict
---@field reason? string
---@field input? table

--- What a `tool.post` handler returns to replace the output the model gets;
--- nil keeps it.
---@class zemi.ToolPostResult
---@field output string

--- One of `pre` and `post` is required.
---@class zemi.ToolOverride
---@field pre? fun(ev: zemi.ToolPreEvent): zemi.ToolVeto?
---@field post? fun(ev: zemi.ev.ToolPost): zemi.ToolPostResult? Runs after the global `tool.post` handlers, as a task.

---@class zemi.ToolInfo
---@field name string
---@field builtin boolean
---@field origin? zemi.Origin Lua tools only.
---@field mcp? string The MCP server, for one of its tools.

---@class zemi.tool
zemi.tool = {}

--- Register a tool the model can call. A call is checked against `required`
--- and the parameter types before `execute` runs, and a string parameter
--- named `about` labels the call's row. It is gated as subject
--- `tool.<name>` with the name as the argument: a plugin's tool settles
--- where its manifest declares `tool`, and any other one runs unless a
--- `permission.request` handler denies it or hands it to the ask overlay.
---
--- ```lua
--- zemi.tool.add({
--- 	name = "weather_now",
--- 	description = "The weather in a city now.",
--- 	input = { city = { type = "string" } },
--- 	required = { "city" },
--- 	execute = function(args)
--- 		local res, err = zemi.http.get("https://wttr.in/" .. args.city .. "?format=3"):wait()
--- 		if not res then return nil, err end
--- 		return res.body
--- 	end,
--- })
--- ```
---@param spec zemi.ToolSpec
function zemi.tool.add(spec) end

--- Hook one tool's calls: `pre` before it runs (a rewritten input re-enters
--- at the permission gate), `post` after, to rewrite its output. A name no
--- builtin, Lua or `mcp_` tool has is an /errors entry once boot is done.
---
--- ```lua
--- zemi.tool.override("shell", {
--- 	pre = function(ev)
--- 		if ev.input.command:find("^git push") then return { decision = "deny", reason = "push by hand" } end
--- 	end,
--- })
--- ```
---@param name string
---@param spec zemi.ToolOverride
function zemi.tool.override(name, spec) end

--- The tools a script of this run may call: zemi's own, the Lua ones
--- registered, and each ready MCP server's.
---@return zemi.ToolInfo[]
function zemi.tool.list() end

--- What `zemi.tool.call` returns: `wait()` suspends the script until the call
--- ends and gives the tool's output, or `nil` and why it failed (the text zemi's
--- tools fail with carries their output).
---@class zemi.ToolCallHandle
---@field wait fun(self: zemi.ToolCallHandle): string?, string?

--- One of this run's tools, as `zemi.tool.describe` reports it.
---@class zemi.ToolDecl
---@field name string
---@field description string
---@field input table The parameters by name, each a JSON Schema.
---@field required string[]

--- Start one of the tools of this run, as if you had called it yourself: it
--- runs through the same permission gate, `tool.pre` and `tool.post` a direct
--- call goes through, and off the interpreter, so the script keeps running.
--- Start the independent calls first and `wait()` on each. `wait()` gives the
--- tool's output, or `nil` and why it failed, like every other zemi call; a
--- tool that failed reports its output as the error. Only the lua tool's own
--- script reaches this (calling it anywhere else raises), and only while it
--- runs. What the calls return stays in the script: print or return what
--- should reach you.
---
--- ```lua
--- local a = zemi.tool.call("read", { path = "a.go" })
--- local b = zemi.tool.call("read", { path = "b.go" })
--- local out, err = a:wait()
--- print(err or out)
--- ```
---@param name string A tool of this run; `zemi.tool.list()` and `zemi.tool.describe` name them.
---@param args? table The tool's arguments.
---@return zemi.ToolCallHandle? handle Nothing when the tool is unknown or `args` is unusable, with the reason in `err`.
---@return string? err
function zemi.tool.call(name, args) end

--- The declaration of one of this run's tools, for one the prompt does not
--- show (a deferred MCP tool) or a parameter to check before calling. Only
--- the lua tool's own script reaches this, like `zemi.tool.call`.
---@param name string
---@return zemi.ToolDecl? decl Nil when this run has no such tool.
function zemi.tool.describe(name) end

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

--- Paths resolve against the project directory and are not confined: reads
--- pass the `lua.fs.read` gate and changes `lua.fs.write`, which never opens
--- the overlay. A plugin's own write settles where its manifest declares
--- `fs.write`, and a read-only run's write is denied.
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

--- `<data>/lua/<name>`, where config code keeps its files: outside the config
--- tree, so a write never reloads the config, and `zemi.ui.edit` opens it
--- through the `lua.fs.read` gate like any other path. Reads
--- settle; a write takes the `lua.fs.write` gate, which never opens the
--- overlay - a plugin's declared `fs.write` settles its own, and a
--- read-only run's write is denied. Nothing is created:
--- `write` makes the parents, and a job's
--- `cwd` needs `mkdir` first.
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

--- Match the project's files, as the glob tool lists them (no .gitignored
--- files, no symbolic links), in path order. `*` and `?` stay inside one segment,
--- `**` spans directories ("**/*.go"), `{a,b}` alternates, `[...]` is a class.
--- A bad pattern raises; a failed walk is nil, err, like the other zemi.fs calls.
---@param pattern string
---@return string[]|nil paths
---@return string|nil err
function zemi.fs.glob(pattern) end

--- Search the project's files for a Go regular expression, in path order,
--- skipping binary and over 4 MB files. `opts.path` searches under one
--- directory or file, and `opts.include` is a glob limiting which files are
--- searched, like the grep tool's; any other key raises. A bad pattern
--- raises; a failed walk is nil, err, like the other zemi.fs calls.
---@param pattern string
---@param opts? { path?: string, include?: string }
---@return zemi.GrepHit[]|nil hits
---@return string|nil err
function zemi.fs.grep(pattern, opts) end

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
---@field body string At most 1 MiB; a larger body fails the call.
---@field headers table<string, string>

--- What `on_done` gets: the response fields, or `error` when the request failed.
---@class zemi.HTTPDone
---@field url string
---@field status? integer
---@field body? string
---@field headers? table<string, string>
---@field error? string

---@class zemi.HTTPHandle
---@field wait fun(self: zemi.HTTPHandle): zemi.HTTPResponse|nil, string|nil Suspends the task until the response, 120 seconds at most; raises outside a task (a tool's execute, a command, a keymap, an overlay callback, an on_done or on_exit) and under pcall.
---@field cancel fun(self: zemi.HTTPHandle) Cancels the request; a waiting `wait` comes back with its error.

--- http and https only, no userinfo. Gated as `lua.http.<method>` with the
--- URL as the argument. Redirects are not followed: the gate approved one
--- url, so follow one by requesting the location header, gated like any call.
---@class zemi.http
zemi.http = {}

--- Send a request; `on_done` or the handle's `wait` gets the response.
---
--- ```lua
--- local res, err = zemi.http.request({
--- 	url = "https://api.example.com/items",
--- 	method = "POST",
--- 	headers = { ["content-type"] = "application/json" },
--- 	body = zemi.json.encode(item),
--- }):wait()
--- ```
---@param spec zemi.HTTPSpec
---@return zemi.HTTPHandle
function zemi.http.request(spec) end

--- A GET, with `on_done` where a handle cannot be waited on (a render, a
--- timer). Without `on_done` and without a waited handle the response is
--- dropped.
---
--- ```lua
--- zemi.http.get("https://wttr.in/Oslo?format=3", {
--- 	on_done = function(res)
--- 		if res.status == 200 then zemi.notify(res.body) end
--- 	end,
--- })
--- ```
---@param url string
---@param opts? { on_done?: fun(res: zemi.HTTPDone) }
---@return zemi.HTTPHandle
function zemi.http.get(url, opts) end

-- zemi.job

---@class zemi.JobSpec
---@field cmd string
---@field args? string[]
---@field env? table<string, string>
---@field cwd? string The project dir by default; any dir `zemi.fs` reads. A missing dir is the returned error.
---@field stdin? boolean Needed before `write`.
---@field timeout_ms? integer `zemi.job.wait` only, 60000 by default; `zemi.job.start` raises when it is set.
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
--- project directory as their working directory. User and project code need
--- `lua.job.start` to pass the gate; a plugin's own job settles where its
--- manifest declares `job`.
---@class zemi.job
zemi.job = {}

--- Start a process; `on_exit` or the handle's `wait` gets how it ended.
---
--- ```lua
--- zemi.job.start({
--- 	cmd = "git",
--- 	args = { "fetch" },
--- 	on_exit = function(res) zemi.notify("git fetch exited " .. res.code) end,
--- })
--- ```
---@param spec zemi.JobSpec
---@return zemi.JobHandle|nil handle
---@return string|nil err A process that could not start.
function zemi.job.start(spec) end

--- Start a job and suspend the task until it finishes (spec.timeout_ms,
--- default 60000, then kills it). Raises outside a task, like a handle's
--- `wait`; there use `start` with `on_exit`.
---
--- ```lua
--- local res, err = zemi.job.wait({ cmd = "go", args = { "vet", "./..." }, timeout_ms = 120000 })
--- ```
---@param spec zemi.JobSpec
---@return zemi.JobResult|nil res
---@return string|nil err
function zemi.job.wait(spec) end

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

-- zemi.state and zemi.env

--- A persisted key-value store, global to zemi. Values round-trip as JSON.
--- Each plugin gets its own key space and never sees another's: its keys are
--- stored as `plugin:<name>:<key>`. Every other origin shares the one space.
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

--- Drop a key; true whether or not there was one.
---@param key string
---@return true|nil ok
---@return string|nil err
function zemi.state.del(key) end

--- The keys set, sorted, filtered by prefix when given.
---@param prefix? string
---@return string[]|nil keys
---@return string|nil err
function zemi.state.keys(prefix) end

---@class zemi.env
zemi.env = {}

--- Read an environment variable, nil when it is not set. Config fields
--- hold values, never variable names.
---@param name string
---@return string?
function zemi.env.get(name) end

-- zemi.json

---@class zemi.json
zemi.json = {}

---@class zemi.json.EncodeOpts
---@field deterministic? boolean sort map keys so equal values encode equal (the default)
---@field indent? string pretty-print with this indentation
---@field escape_for_html? boolean escape <, > and & in strings
---@field escape_for_js? boolean escape the characters a script tag or literal breaks

---@param value any
---@param opts zemi.json.EncodeOpts?
---@return string
function zemi.json.encode(value, opts) end

---@class zemi.json.DecodeOpts
---@field allow_duplicate_names? boolean repeated object keys stop being an error

---@param s string
---@param opts zemi.json.DecodeOpts?
---@return any|nil value
---@return string|nil err
function zemi.json.decode(s, opts) end

-- zemi.yaml

---@class zemi.yaml
zemi.yaml = {}

---@class zemi.yaml.EncodeOpts
---@field indent? number spaces per level
---@field indent_sequences? boolean indent the items of a list under its key
---@field literal_multiline? boolean write multi-line strings as | blocks

---@param value any
---@param opts zemi.yaml.EncodeOpts?
---@return string
function zemi.yaml.encode(value, opts) end

---Decode yaml text; a repeated key is an error and the call takes no
---options.
---@param s string
---@return any|nil value
---@return string|nil err
function zemi.yaml.decode(s) end

-- zemi.csv

---@class zemi.csv
zemi.csv = {}

---@class zemi.csv.EncodeOpts
---@field comma? string the field separator, one character (default ",")
---@field use_crlf? boolean end rows with a carriage return and newline

---Encode rows of strings and numbers as RFC 4180: a field carrying the
---delimiter, a quote, a carriage return, a newline or a leading space is
---quoted, a quote inside doubles.
---@param rows zemi.csv.Row[]
---@param opts zemi.csv.EncodeOpts?
---@return string
function zemi.csv.encode(rows, opts) end

---@class zemi.csv.DecodeOpts
---@field comma? string the field separator, one character (default ",")
---@field comment? string a line starting with it reads as a comment
---@field trim_leading_space? boolean
---@field lazy_quotes? boolean a quote beside text reads as text (the default)

---Decode RFC 4180 text into rows of strings; a quoted field may carry
---commas, doubled quotes and newlines, and a blank line separates nothing.
---@param s string
---@param opts zemi.csv.DecodeOpts?
---@return string[][]|nil rows
---@return string|nil err
function zemi.csv.decode(s, opts) end

---@alias zemi.csv.Row string[]|number[]

-- zemi.skills

---@alias zemi.SkillSource
---| "user"
---| "builtin"
---| "plugin"
---| "project"  # always enabled
---| "recipe"   # a project's private skill, always enabled

---@class zemi.Skill
---@field name string
---@field description string
---@field source zemi.SkillSource
---@field enabled boolean
---@field path string

---@class zemi.skills
zemi.skills = {}

---@return zemi.Skill[]
function zemi.skills.list() end

--- Turn a skill on or off. (`@name` in a message attaches a skill whether it
--- is on or off.)
---@param name string
---@param on boolean
---@return true|nil ok
---@return string|nil err
function zemi.skills.enable(name, on) end

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
---@field group? string Groups the server under one name: /mcp toggles the whole group, and a name is a server or a group, never both.

---@alias zemi.ServiceStatus
---| "idle" # a language server that starts on its first file
---| "starting"
---| "ready"
---| "failed" # error says why
---| "needs sign-in" # an MCP server waiting for /mcp sign-in
---| "stopped"
---| "off" # a plugin or MCP server the user turned off

---@class zemi.MCPInfo
---@field name string
---@field status zemi.ServiceStatus "starting", "ready", "failed", "needs sign-in", "stopped" or "off".
---@field error string
---@field tools string[] The tools the model gets, after enabled_tools and disabled_tools.
---@field group string The group's name, "" for none.
---@field enabled boolean The persisted /mcp switch, not the running status.

---@class zemi.mcp
zemi.mcp = {}

--- Add an MCP server. Its tools arrive as
--- `mcp_<server>_<tool>` and are gated as subject `mcp.<server>`.
---
--- ```lua
--- zemi.mcp.add("github", {
--- 	command = "npx",
--- 	args = { "-y", "@modelcontextprotocol/server-github" },
--- 	env = { GITHUB_TOKEN = zemi.env.get("GITHUB_TOKEN") },
--- })
--- ```
---@param name string
---@param spec zemi.MCPSpec
function zemi.mcp.add(name, spec) end

--- Remove and stop a server `zemi.mcp.add` registered; false for a name
--- nothing registered.
---@param name string
---@return boolean removed
function zemi.mcp.del(name) end

---@return zemi.MCPInfo[]
function zemi.mcp.list() end

--- Flip a server's persisted switch, or a whole group's at once. A disabled
--- server keeps its /mcp row and starts when turned back on. A name that is
--- neither a server nor a group gives nil and the error.
---
--- ```lua
--- zemi.mcp.enable("work", false) -- the whole group off
--- ```
---@param name string
---@param on boolean
---@return true|nil ok
---@return string|nil err
function zemi.mcp.enable(name, on) end

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
---@field filetypes string[] The extensions the server handles, as `add` takes them.
---@field status zemi.ServiceStatus "idle", "starting", "ready", "failed" or "stopped".
---@field error string

---@class zemi.lsp
zemi.lsp = {}

--- Add a language server. It starts on the first
--- matching file. Adding a builtin's name (gopls, pyright,
--- typescript, clangd, rust-analyzer, luals) replaces it, which is how a
--- builtin gets args, env or settings; `luals` also serves the Lua of your
--- config and local plugins. A server that never comes up only means no
--- diagnostics.
---@param name string
---@param spec zemi.LSPSpec
function zemi.lsp.add(name, spec) end

--- Never start this server: a builtin you do not want, or one added above.
---@param name string
---@return boolean removed
function zemi.lsp.del(name) end

--- The servers this project can use and their state.
---@return zemi.LSPInfo[]
function zemi.lsp.list() end

-- zemi.health and zemi.errors

--- What a /health check returns; no level reads as "ok", and any other
--- level fails the check like a raise.
---@class zemi.HealthResult
---@field level? zemi.HealthLevel
---@field detail string

---@class zemi.health
zemi.health = {}

--- Add a row to /health. The check runs under the 50 ms handler deadline
--- and may return a detail string instead of a table. Adding a name again
--- replaces its row, but not another plugin's. `zemi health`
--- runs it right after boot, before work started on `app.ready` has
--- reported, so a row over such state says it is not read yet.
---
--- ```lua
--- zemi.health.add("branch", function()
--- 	return missing and { level = "warn", detail = missing } or "reading the branch"
--- end)
--- ```
---@param name string
---@param fn fun(): zemi.HealthResult|string
function zemi.health.add(name, fn) end

--- One row per /health check, built-in rows first: this is how config
--- written from Lua is verified without asking the user to run /health.
--- It runs the checks inline, with no 50 ms ceiling: keep them short.
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

-- zemi.test

--- A plugin's tests: each top-level file in its `.zemi/tests/` is a test file
--- (`.zemi/tests/*.lua`), and code they share goes in a subdirectory
--- (`.zemi/tests/lib/x.lua`, `require("./lib/x")`). Only a `zemi plugin test`
--- boot has `zemi.test`; reading it anywhere else raises. Each case runs in
--- a fresh zemi holding only the plugin, after the file's top level (where
--- init.lua would run) and `app.ready`, on a clock at 2026-01-01 00:00 UTC
--- that only `advance` moves. Drivers (run, keys, tool, render, click, emit,
--- complete, answer, advance, set) run only inside a case, never under
--- `pcall` or `raises`; one returns once zemi is quiet, and an error zemi
--- logs meanwhile (a callback, a timer) fails the case at its call. A job,
--- HTTP request, model ask, agent send or `zemi.fs` write raises unless a
--- stub answers it, naming the stub to add. An error a case catches names
--- the test copy's path; `raises` and `contains` make it relative to the
--- plugin.
---
--- ```lua
--- local test = zemi.test
--- test.stub("lua.job.start", "git rev-parse *", { stdout = "main\n" })
---
--- test("the command prints the branch git reports", function()
--- 	test.eq(test.run("branch"), true)
--- end)
--- ```
---@class zemi.test
---@overload fun(name: string, fn: fun()) Declare a case, at the file's top level.
zemi.test = {}

--- Compare deeply, functions by identity; raises naming the first
--- difference, by line for multi-line strings.
---
--- ```lua
--- test.eq(zemi.session.todos(), {})
--- ```
---@param got any
---@param want any
function zemi.test.eq(got, want) end

--- Raise when got deep-equals other; prefer eq with the exact value.
---
--- ```lua
--- test.neq(zemi.notify.list(), before)
--- ```
---@param got any
---@param other any
function zemi.test.neq(got, other) end

--- Raise unless s holds sub as plain text, the plugin's paths relative.
---
--- ```lua
--- test.contains(row.detail, 'plugin jira declares no "http"')
--- ```
---@param s string
---@param sub string
function zemi.test.contains(s, sub) end

--- Call fn and raise unless it raised an error holding sub; returns the
--- message, paths relative. A driver cannot run inside fn.
---
--- ```lua
--- test.raises(function() test.stub("lua.nope") end, "not a call a stub answers")
--- ```
---@param fn fun()
---@param sub? string
---@return string message
function zemi.test.raises(fn, sub) end

--- Run the plugin's command as typed.
---@param name string
---@param args? string
---@return true|nil ok
---@return string? err
function zemi.test.run(name, args) end

--- Run what the plugin bound a global key sequence to.
---@param seq string
---@return true|nil ok
---@return string? err
function zemi.test.keys(seq) end

--- Call the plugin's tool as the model would, allowed: the output, or nil
--- and the error the model gets, behind the tool's name ("weather_now: ...").
---
--- ```lua
--- test.eq(test.tool("weather_now", { city = "Oslo" }), "Oslo: 21C, Sunny")
--- ```
---@param name string
---@param input table
---@return string|nil output
---@return string? err
function zemi.test.tool(name, input) end

--- Deliver a host event to the plugin's handlers: `tool.post` returns the
--- output the model gets, a vetoable event its verdict - `{ decision = "deny" }`,
--- `{ decision = "ask" }`, or nil when no handler decided.
---
--- ```lua
--- test.eq(test.emit("tool.post", { tool = "shell", ok = false, output = "FAIL" }), "FAIL")
--- ```
---@param name zemi.EventName
---@param ev? table
---@return any
function zemi.test.emit(name, ev) end

--- The rows the plugin's completion offers for a draft line, cursor at its
--- end: a command's `complete` for a `/command` line, else its
--- `zemi.composer.add` source for the trigger that starts the last word.
---
--- ```lua
--- test.eq(test.complete("see #rev"), { "review" })
--- ```
---@param line string
---@return string[]
function zemi.test.complete(line) end

--- The top open overlay, or nil.
---@return { id: string, title: string, body: string, items: string[], input: boolean }?
function zemi.test.overlay() end

--- Enter on an item (its index or text) or its detail action, type an
--- input's text, or esc for nil; a row no callback takes runs its `run`, a
--- command of the plugin.
---
--- ```lua
--- assert(test.run("demo"))
--- assert(test.answer("review"))
--- test.eq(test.overlay(), nil)
--- ```
---@param choice integer|string|nil
---@param action? string
---@return true|nil ok
---@return string? err
function zemi.test.answer(choice, action) end

--- Answer the calls matching pattern ("*"): reply's fields, a function
--- returning them, or false to refuse the call the way an unanswered ask
--- does. Stub at the file's top level to answer every case, or inside a case to
--- answer only there; the newest match answers. The returned table fills
--- with the requests it answered as they arrive (`reqs[1].url`).
--- A reply's fields and defaults: http `{ status = 200, body = "", headers = {} }`,
--- job `{ code = 0, stdout = "", stderr = "" }` (stdout reaches `on_stdout`),
--- model a string or `{ text }`, classify `{ answers = {...}, strategy = ... }`,
--- send and write none (a stubbed write
--- writes); an http or model reply `{ error = "..." }` fails at the
--- transport. Any other field raises. The requests: http
--- `{ method, url, headers, body }`, job `{ cmd, args, cwd }`, model
--- `{ model, prompt, system }`, classify `{ model, questions }`, send
--- `{ text }`, write `{ path }`.
---
--- ```lua
--- local reqs = test.stub("lua.http.get", "https://wttr.in/*", { body = '{"temp":"21"}' })
--- test.stub("lua.job.start", "git push *", false)
--- test.stub("lua.model.ask", "*", function(req) return "a title for " .. req.prompt end)
--- ```
---@param subject string
---@param pattern? string
---@param reply? table|string|false|fun(req: table): (table|string|false)
---@return table[] requests
function zemi.test.stub(subject, pattern, reply) end

--- Move the clock, firing due timers in order.
---
--- ```lua
--- test.advance(30000)
--- test.eq(test.run("branch"), true)
--- ```
---@param ms number
function zemi.test.advance(ms) end

--- Change one of the plugin's settings, named by its bare key, as the user
--- does in /config, its watches firing.
---
--- ```lua
--- assert(test.set("interval", 10000))
--- ```
---@param key string
---@param value any
---@return true|nil ok
---@return string? err
function zemi.test.set(key, value) end
