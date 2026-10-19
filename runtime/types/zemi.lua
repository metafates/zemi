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
--- Error convention: misuse (a bad argument, a call an origin may not make)
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
---| "plugin"   # <data>/plugins/{installed,local}/<name>; code runs as the least trusted function on its stack
---| "project"  # <repo>/.agents

---@alias zemi.Level
---| "info"
---| "warn"
---| "error"

---@alias zemi.HealthLevel
---| "ok"
---| "warn"
---| "error"

---@alias zemi.Scope
---| "user"     # holds in every project
---| "project"  # this repository only

---@alias zemi.MemoryOrigin
---| "user"     # the user wrote it or said to keep it
---| "learned"  # the model or the reflector proposed it

--- The regions the default layout draws, plus "exit", which no layout draws: its
--- widgets print once to the shell after zemi quits, foreground colors only, and code
--- below builtin may not add one. A layout may draw any other name.
---@alias zemi.Region "header"|"chat"|"working"|"statusline.left"|"statusline.right"|"exit"|string

--- One styled run of text in a widget or overlay row. `hl` names a
--- highlight group; omit it for the default foreground.
---@class zemi.Segment
---@field text string
---@field hl? string
---@field click? string Widget rows only: the id a click passes to the widget's `on_click`.
---@field effect? "shimmer"|"spin" `shimmer`: a bright band sweeps across the text every frame, blending `hl` toward Normal. `spin`: the first glyph steps through the running spinner, `zemi.ui.marks.spin`'s. Both static with ui.animations off.
---@field copy? boolean `false` marks decoration (a gutter bar, a bullet, a border, a line number): a selection neither lights nor copies it.
---@field hover? boolean `false` marks a clickable body row (a diff line, a detail line): the pointer still shows a click acts, but hovering it restyles nothing.
---@field fade? number 0 to 1: how far the segment's colors sink toward the surface it sits on (the canvas, or a box's panel). A scrolled view fades its own edge rows; a widget sets this where it clips a block itself, as the chat does to a thinking tail rolling up.
---@field soft_wrap? boolean The line continues on the next one: a copy joins the two and drops what follows this segment on the row. Go's own wrapping sets it.
---@field anchor? integer On every row of a unit: the transcript row the unit starts at, counted as `ctx.transcript` is (1-based). A list with `follow` set scrolls the chat to the row its selected item names and bands every row sharing it.
---@field sticky? boolean|"section"|"end" On a row's first segment: rows that set it form a header. While a scrolled region's first rows come after a whole header, the header's first row is drawn over the region's first rows as chrome, in the `Pinned` group on the panel across the view's width, and a click on it scrolls back there. `true` heads a turn and starts the pinned stack anew, `"section"` heads a part of that turn and pins under it, and `"end"` marks the section's last row, where its pin ends. Example: `{ text = "> ", sticky = true }`.

--- A widget row: the segments it is drawn from, left to right.
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

--- Write a setting. User config only; raises for an unknown path, a wrong
--- kind or a value outside the key's `choices` (agent.effort, memory.autonomy).
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
--- used, or watch it, never once at load. Plugin origin only; raises past
--- load, or for a default the kind or choices refuse. Never a secret:
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

--- Drop a watch by the id `zemi.config.watch` returned.
---@param id string
---@return boolean removed
function zemi.config.unwatch(id) end

--- Every known setting with its default.
---@return zemi.SettingSchema[]
function zemi.config.schema() end

-- zemi.keymap

---@class zemi.KeymapOpts
---@field desc? string Shown in /help.
---@field focus? string A focusable layout node id: the binding works only while that node has the keys (zemi.ui.focus), and there it may start with a printable key ("j", "g g").
---@field list? boolean A list alias: while a list, a composer popup or a prompt has the keys, the key makes the move the action names ("up", "down", "pgup", "pgdown", "home", "end", "ctrl+d", "ctrl+u"), resolved without calling Lua. One non-printable key, not one the lists answer with. User or builtin origin only.

---@class zemi.Keymap
---@field key string
---@field desc string
---@field origin zemi.Origin
---@field action string "/command", a host action, or "<function>".
---@field focus? string The node it is scoped to; nil for a global binding.
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
--- raises rather than binding something nothing can press, and so does a
--- sequence that starts with a printable key without `opts.focus`, since
--- the composer takes those. Setting
--- the same key again from the same origin rebinds it; a key (or a prefix
--- or extension of a key sequence) a more trusted origin bound raises.
--- With `opts.focus` the binding is scoped to a focusable node: while it has
--- the keys its bindings come first, then the global ones, unless a more
--- trusted global binding holds the key. It may not take a key a more
--- trusted global binding holds. The composer's own editing keys (ctrl+a,
--- ctrl+e, ctrl+u, ctrl+w, home, end) bind like any other, and the composer
--- then no longer gets them.
---
--- With `opts.list` the key moves in every list, popup and prompt, and stays
--- what it was elsewhere: `zemi.keymap.set("ctrl+j", "down", { list = true })`
--- (ctrl+j still scrolls the chat). While a line types, an alias to
--- home, end or ctrl+u keeps the key's own meaning, since the line edits with
--- those. enter, esc, tab, shift+tab, space and backspace raise.
---
--- Host actions: zemi.composer.{submit,history_prev,history_next,paste,edit},
--- zemi.composer.insert <text> (these user or builtin origin only),
--- zemi.ui.help, zemi.model.pick,
--- zemi.ui.scroll.{up,down,top,bottom,line_up,line_down,sticky_up,sticky_down} [id],
--- zemi.ui.focus.set [id],
--- zemi.ui.focus.{next,prev} and zemi.agent.profile.{next,prev}
--- (user or builtin origin only).
---
--- ```lua
--- zemi.keymap.set("<leader>r", "/review", { desc = "review the diff" })
--- zemi.keymap.set("<leader>k", "zemi.ui.scroll.up sidebar")
--- zemi.keymap.set("j", "zemi.ui.scroll.line_down sidebar", { focus = "sidebar" })
--- ```
---@param key string
---@param action zemi.HostAction|string|fun()
---@param opts? zemi.KeymapOpts
function zemi.keymap.set(key, action, opts) end

--- Unbind a key the caller's origin or a less trusted one set, not another plugin's. A key
--- nothing bound is `false`, not an error, so a reload unbinds idempotently.
---@param key string
---@param opts? { focus?: string, list?: boolean }
---@return boolean removed
function zemi.keymap.del(key, opts) end

--- The binding a key runs, while `opts.focus` has the keys when given, or
--- its list alias with `opts.list`.
---@param key string
---@param opts? { focus?: string, list?: boolean }
---@return zemi.Keymap|nil
function zemi.keymap.get(key, opts) end

---@return zemi.Keymap[]
function zemi.keymap.list() end

-- zemi.command

--- What a command handler receives: the raw argument string, with piped
--- stdin attached behind what was typed, and the typed words.
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
---@param args? string
---@return true|nil ok
---@return string|nil err Not a command, bad arguments, the command's own error, or a command the caller's origin may not run (a Go command below builtin, a more trusted origin's Lua command).
function zemi.command.run(name, args) end

---@return zemi.Command[]
function zemi.command.list() end

-- zemi.event

---@alias zemi.EventName
---| "app.ready"
---| "app.quit"
---| "config.reloaded"
---| "session.new"
---| "session.open"
---| "session.close"
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
---| "memory.written"
---| "reflector.start"
---| "reflector.done"
---| "job.exit"
---| "lsp.request"

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

--- Fired when the main agent's prompt is built (a new session, a resume, a
--- compaction, a model, profile, trust or tool change), not per send, and
--- once per subagent run.
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
---@field subject string
---@field arg string
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

---@class zemi.ev.MemoryWritten: zemi.Event
---@field id string
---@field scope zemi.Scope
---@field origin zemi.MemoryOrigin
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
--- reason. Any other return lets the event through; on `permission.request`
--- a `zemi.PermVeto` may also hand the request to the ask path.
---@class zemi.Veto
---@field decision "deny"
---@field reason? string

--- What a `permission.request` handler returns: "deny" refuses the request,
--- "ask" runs zemi's ask path (the overlay) for it, nil lets it run.
---@class zemi.PermVeto: zemi.Veto
---@field decision "deny"|"ask"

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
--- `app.quit` vetoes from project code are ignored. A plugin needs `perm`
--- in its `uses` to subscribe to `permission.request`.
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
---@overload fun(name: "permission.request", fn: (fun(ev: zemi.ev.PermissionRequest): zemi.PermVeto?), opts?: zemi.EventOpts): string
---@overload fun(name: "permission.resolved", fn: (fun(ev: zemi.ev.PermissionResolved): any), opts?: zemi.EventOpts): string
---@overload fun(name: "app.quit", fn: (fun(ev: zemi.Event): zemi.Veto?), opts?: zemi.EventOpts): string
---@overload fun(name: "ui.resize", fn: (fun(ev: zemi.ev.Resize): any), opts?: zemi.EventOpts): string
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

-- zemi.ui

--- The session's goal (/goal).
---@class zemi.GoalCtx
---@field condition string What must hold.
---@field turns integer Turns it was checked after.
---@field reason string The last check, "not yet: <why>" or "blocked: <why>"; "" before the first.

--- The session's loop (/loop).
---@class zemi.LoopCtx
---@field prompt string What each run sends.
---@field every integer Seconds between runs; 0 while the model paces them.
---@field next integer When the next run fires, in unix seconds.
---@field runs integer Runs so far.

--- One message waiting for the turn's next step boundary.
---@class zemi.QueuedMessage
---@field text string What the user typed, or the head line of a subagent's report, update or plugin send.
---@field notice boolean True for a subagent's or a plugin's message, which the model reads whole and the chat shows as a notice row instead of the user's own.
---@field run string The id of the run the row names, "" for the user's own words, a plugin's or a loop's line and a workflow's report.

--- The live turn, as the chat widget sees it: while ctx.view is set, the shown
--- agent run in its place.
---@class zemi.AgentCtx
---@field active boolean True while the turn runs; while ctx.view is set, while that run does.
---@field model string
---@field text string Reply text so far.
---@field reasoning string Thinking text so far; while ctx.view is set, the shown run's open block.
---@field thinking boolean A reasoning block is open: the model is thinking right now, and the text above is that of the open block; while ctx.view is set, the shown run's.
---@field tools zemi.ToolCall[]
---@field steering zemi.QueuedMessage[] Messages waiting for the turn's next step boundary, oldest first; while ctx.view is set, the ones queued for that agent run.
---@field thought number Seconds spent reasoning.
---@field elapsed number Seconds since the turn started; while ctx.view is set, since that run did.
---@field compacting boolean History is being summarized.
---@field checking_goal boolean The goal is being checked after a turn (/goal); the agent stays active.
---@field retry? { attempt: integer, why: string, next_in: number } Set while a provider retry waits or is under way (next_in 0).

---@alias zemi.CallStatus
---| "wait"       # its arguments stream, or a serial call ahead of it runs
---| "run"
---| "ok"
---| "warn"       # it finished, but something it ran failed
---| "err"
---| "stopped"    # the user denied, dismissed or interrupted it
---| "bg"         # its work goes on in the background (a job, a background agent)
---| "cancelled"  # an error row an interrupt left

---@class zemi.ToolCall
---@field name string
---@field input string
---@field status zemi.CallStatus
---@field detail string Subagent and workflow progress.
---@field run string Agent calls: the run id (`/agents view <id>`), else "".
---@field job string A call that handed off: the background job id, else "".
---@field label string The model's description of the call; a question call's headers.
---@field elapsed number
---@field diff string
---@field output string Last lines of the result; a plan call's verdict; an answered question call's rows, each `question\tanswer\tnote`.

---@alias zemi.Role
---| "user"
---| "assistant"

---@alias zemi.RowKind
---| ""           # a plain message
---| "reasoning"  # a finished thinking block
---| "tool"
---| "shell"      # a !command and its output
---| "compact"
---| "notice"
---| "error"

--- One transcript entry.
---@class zemi.TranscriptRow
---@field role zemi.Role
---@field kind zemi.RowKind
---@field text string
---@field tool string
---@field status zemi.CallStatus|"" Tool and shell rows, and "cancelled" on an error row an interrupt left.
---@field label string
---@field detail string Tool rows: subagent and workflow progress; notice rows: a hint drawn after the text, never copied.
---@field run string Agent calls: the run id, else "".
---@field job string A call that handed off: the background job id, else "".
---@field diff string
---@field output string Last lines of a tool result; a plan call's verdict ("approved", "changes requested: ..."); an answered question call's rows, each `question\tanswer\tnote` ("not answered" when left open, the note "" when none).
---@field ["in"] integer Prompt tokens.
---@field out integer Completion tokens.
---@field thought number
---@field elapsed number
---@field ended number Unix time the turn ended, 0 when unknown.

---@class zemi.RecentSession
---@field id string
---@field title string The session's title, its marker emoji prefixed; "(untitled)" when it has none.
---@field ago string "now", "5m ago", "2d ago".

---@class zemi.RecentProject
---@field path string The project root, ~-shortened.
---@field root string The project root, absolute: what `/projects switch` takes.
---@field ago string When zemi was last used there: "now", "5m ago", "2d ago".

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

---@class zemi.ContextUsage
---@field tokens integer
---@field window integer
---@field pct number

---@alias zemi.RunStatus
---| "running"
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

--- What a widget's render function is given. Everything is a snapshot:
--- render must be pure and finish in about 10 ms: only the read-only
--- `zemi.*` calls other than `zemi.fs.*` and a handle's `wait` work inside
--- it, any other raises. `transcript`, `agent`, `todos`, `recent`, `projects`,
--- `regions` and `instructions` are built on first read, so a widget that does
--- not read them does not pay for them; kept past the render, a ctx reads
--- nil for the ones render did not read.
---@class zemi.WidgetCtx
---@field width integer Cells of the layout node drawing the widget's region.
---@field height integer Rows of that node; for a content-sized node, the most it could get.
---@field time string "15:04".
---@field version string
---@field cwd string The session's working directory, ~-shortened.
---@field home string The user's home directory, which ~ stands for in `cwd` and elsewhere.
---@field project string The repository root trust is decided for, ~-shortened.
---@field trusted boolean Whether the user trusts this project (/trust).
---@field trustable boolean Whether /trust can trust this project: false at $HOME, / and outside any.
---@field init_error string First line of the failure the user init.lua raised this boot, "" when it loaded.
---@field init_path string The user init.lua, ~-shortened.
---@field model_hint string How to get a model when `model` is "", short enough for one row: the first key to export, or init.lua.
---@field model string
---@field profile string
---@field profile_read_only boolean Whether the active agent is read-only (plans, presents for approval).
---@field effort string The reasoning level `model` runs at: the /effort pick fitted to the model, or its catalog default; "" sends none.
---@field agent_active boolean True while the live turn runs; while `view` is set, while that run does.
---@field agent zemi.AgentCtx
---@field view? { id: string, agent: string, description: string, status: zemi.RunStatus } The agent run the chat shows instead of the session; a submit messages it while it is running, and `agent` is that run's name, the model's description or the definition's when it wrote none.
---@field transcript zemi.TranscriptRow[]
---@field transcript_count integer Rows in `transcript`, without building it.
---@field transcript_epoch integer Changes when the transcript is replaced; 0 while an agent run or a printed run is shown, whose rows it does not describe.
---@field transcript_edit integer Changes when a settled row's text grew in place: every unit redraws, unlike `transcript_epoch`, whose bump also drops the rows the reader opened.
---@field view_revision integer Changes when the shown run rewrites its transcript, in place included; 0 while the session shows.
---@field todos zemi.Todo[]
---@field recent? zemi.RecentSession[] Recent sessions, only while the transcript is empty.
---@field projects? zemi.RecentProject[] The three projects zemi started in last besides this one, newest first; only while the transcript is empty and zemi runs in ~, / or outside a repository with no session here.
---@field resume string The shell command that reopens this session: from its project, or with the -C dir this run started with; "" before the first send.
---@field instructions? string[] The project's instruction files, repo-relative; empty when it has none (what /init writes), nil when untrusted or at $HOME or /. Cached: read again after a boot or reload, a trust change and a new session.
---@field usage zemi.Usage
---@field context zemi.ContextUsage
---@field show_reasoning boolean `zemi run --reasoning` prints every thought and call whole; always false in the TUI.
---@field emoji string The session's marker emoji, derived from its id; "" before the first send.
---@field regions table<string, true> The regions the layout draws this frame: `ctx.regions.working` says whether the composer's top rule is on screen. Empty in a printed run.
---@field frame integer The frame clock: wall time in 50 ms steps. While a turn runs every frame renders, so it advances by one per render; divide it for a slower cadence. Always 0 with animations off, which leaves every animation on its first frame.
---@field ended zemi.TurnEnd|"" How the turn that ended a moment ago ended, so the mascot can show it while the working row is otherwise gone; "" once that moment passed.
---@field busy? { label: string, elapsed: number, cancellable: boolean }[] Every job the user waits on that is not a turn (/btw, a plugin install, the reflector, a recap, compaction), newest first: each one's label, seconds it has run, and whether a double esc stops it (esc stops every such job). nil when none runs.
---@field pending_memory integer
---@field agents_running integer Agents of this session still running outside a workflow: the turn's own and background ones, whose reports arrive as messages.
---@field workflows_running integer Workflows of this session still running, paused ones included.
---@field jobs_running integer Background jobs (job_start, or a shell call that outlived its timeout) still running.
---@field goal? zemi.GoalCtx The session's goal while one is set (/goal).
---@field loop? zemi.LoopCtx The session's loop while one runs (/loop).
---@field quit_armed boolean A second ctrl+c quits.
---@field interrupt_armed boolean A second esc interrupts the turn.
---@field key_seq string Keys of a bound sequence typed so far, space-separated; "" when none.
---@field rows_below integer Rows under the stage's scrolled region (the chat by default) while it is scrolled up; 0 when it follows the tail.
---@field cancelling boolean An interrupt was sent and the turn has not ended yet.
---@field editing string Title of the file open in the user's editor, else "".
---@field waiting zemi.Waiting|"" The prompt holding the turn, else "".
---@field focus string Id of the layout node that has the keys, "" while the composer has them.
---@field headless boolean `zemi run` prints the frame to a shell: nothing is clicked, ctrl+c is the only key, and only the live tail redraws.
---@field printed_prev? zemi.TranscriptRow In a printed run, the last row printed above this frame: the batch's first unit seeds its block gap from it.

--- `render` returns a list of lines. A bare string or a single line is taken
--- as one line; a list of several strings raises, since it could mean one
--- line or several. It may return true as a second result to render again
--- next frame: return what is ready and do the rest then. Never change a line table after
--- returning it, one seen in the last render is converted only once: build a
--- new table for a line that changed.
---@class zemi.WidgetSpec
---@field id string
---@field region zemi.Region
---@field render fun(ctx: zemi.WidgetCtx): zemi.Line[], boolean?
---@field priority? number Within a region lower sits first: top to bottom, and in the statusline from the outer edge inward (`statusline.right` from its right edge leftward), so one above a shipped widget's priority sits just inside it. A short statusline drops the highest first.
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
---@field priority number
---@field origin zemi.Origin

---@alias zemi.Underline
---| "none"
---| "single"
---| "double"
---| "curl"
---| "dotted"
---| "dashed"

--- A highlight group. The hover groups `Hover` (clickable widget segments,
--- statusline pills) and `HoverItem` (list rows) draw
--- over the text's own style: keys left out keep it, false and "NONE" clear it.
---@class zemi.HLSpec
---@field link? string Draw as this group instead; of the other fields only bold and italic then apply, over it. Chains longer than 8 or cycles draw unstyled.
---@field fg? string A palette role ("accent", see zemi.ThemeSpec), hex ("#a2adfd"), an ANSI color name ("red", "green", ...), or "NONE".
---@field bg? string
---@field sp? string Underline color.
---@field bold? boolean
---@field italic? boolean
---@field underline? boolean|zemi.Underline true is "single", false "none".
---@field reverse? boolean
---@field shade? number A stop of the knot's ramp, 0 to 1: the audited whorl-knot curve through the theme's rule, accent and literal. Takes the place of fg.
---@field brighten? number Hover groups: mix the fg this far (-1..1) toward white, or toward black on a lighter background.
---@field replace? boolean Hover groups: draw as this group alone instead of over the text's style.

--- A theme's palette, each role #rrggbb. What it leaves out is derived,
--- and every text role is moved in lightness until it reads on bg.
---@class zemi.ThemeSpec
---@field bg string
---@field fg string
---@field ansi? string[] The terminal's 16 colors, black to bright white: red, green, yellow and blue stand in for a missing danger, ok, warn and accent.
---@field accent? string Required without ansi.
---@field ok? string Required without ansi.
---@field warn? string Required without ansi.
---@field danger? string Required without ansi.
---@field literal? string Text quoted as written; derived halfway between danger and warn.
---@field dim? string
---@field rule? string
---@field panel? string The surface of every box and the composer, kept near bg in lightness.
---@field band? string A box's selected row, kept near bg in lightness.
---@field addbg? string
---@field delbg? string

--- One overlay row. Enter on an item with `detail` or `actions` opens a
--- detail view (never on_select); otherwise it fills, runs or selects the
--- item, and does nothing when none applies. `run` is honored only for
--- user config.
---@class zemi.OverlayItem
---@field text string the label: a filter word matches it fuzzily, or as a substring when the text holds a space (prose)
---@field desc? string dim column after the text; the filter finds words in it
---@field preview? string shown whole under the list while this row is selected, in a pane as tall as the longest preview (4 rows at most), so moving never shifts a row; the filter matches it too
---@field key? string right-aligned tag
---@field on? boolean draws the on/off switch in the tag's place
---@field locked? boolean draws the switch on with nothing to switch
---@field danger? boolean cannot be undone: drawn in Danger, and the first enter only arms it (never the open of a row with a detail view); the footer then says to press it again, and any other key disarms
---@field tail? boolean a text too wide for its column is cut from the left, keeping its end: a path's name
---@field mark? zemi.MarkKind a state mark ahead of the text and of the detail view's `status` field, as the chat draws it (`zemi.ui.marks`); a running one spins
---@field section? string a change of section starts with a header row; the filter matches it too
---@field detail? string body of the detail view
---@field fields? zemi.OverlayField[] the detail view's facts, set as a table above detail
---@field actions? zemi.OverlayAction[] listed under the detail
---@field fill? string put in the composer (ignored while the overlay has an on_select, which takes enter instead)
---@field run? string a command line such as "mcp restart gitlab" (user and builtin overlays only; ignored while the overlay has an on_select)
---@field hotkey? string one character that acts like enter on this row (not while a filter types; j, k, /, y and Y are the list's and raise)
---@field copy? string the row's value: y copies it in place of the row's text, and enter copies it, keeping the list open, when the row does nothing else (no on_select, run, fill or detail); the filter matches it too
---@field verb? string enter's action in the hint where the default would say another: zemi's lists use "open" (another screen), "pick" (a value), "run" (a command), "toggle" and "fill"; space passes the row by in a toggle list, and an "open" row's run leaves the list under the screen it opens
---@field anchor? integer The transcript row this one stands for, counted as `ctx.transcript` is (1-based); a list with `follow` set scrolls the chat here when the row is selected.

--- A detail view action; with no id, fill or run it goes back.
---@class zemi.OverlayAction
---@field text string
---@field id? string hands the action to the overlay's on_action, which takes it over fill and run
---@field danger? boolean cannot be undone: drawn in Danger, and the first enter or hotkey only arms it; the footer then says to press it again, and any other key disarms
---@field hotkey? string one character that acts like enter on this action, and from the list on the selected row (a `danger` one opens the detail on it armed); with none, its position ("1", "2", ...). j, k, /, y and Y raise.
---@field detail? string enter opens it as a detail view of its own, over this one
---@field fields? zemi.OverlayField[] that view's facts, set as a table above detail
---@field fill? string
---@field run? string

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

---@class zemi.OverlaySpec
---@field id string
---@field title? string
---@field body? string|zemi.Line[] text above the items, or styled lines (text and hl only); with no items a read-only view
---@field markdown? boolean render a string `body` and every item's `detail` as markdown
---@field empty? string the line shown when there are no items, under the body when both are set
---@field items? zemi.OverlayItem[]|any[]
---@field render_item? fun(item: any): string|zemi.Line A line's segments keep their `hl` (text, hl only); unstyled ones take the row's.
---@field selected? integer 1-based item selected when it opens.
---@field anchor? zemi.OverlayAnchor In line with the composer frame's edge, by the statusline pill that opened it; nil centers it.
---@field follow? boolean Moving the selection scrolls the chat to the row the selected item names by its `anchor`, centers that message when the whole of it fits, and bands its rows while the list is open; the list opens on the item the reader is at (the one whose jump would put the chat exactly where it is), enter closes it keeping that place, and esc puts the chat back where it was.
---@field on_select? fun(item: any) Takes enter for every item, so the items' own `fill` and `run` no longer act (a `reload` list runs both).
---@field on_action? fun(item: any, action: zemi.OverlayAction) Runs for a detail action with an `id`; the list closes like on on_select, a `reload` list rebuilds.
---@field keys? table<string, zemi.OverlayKey> One character each (j, k, /, y and Y are the list's and raise; a row's hotkey wins, and this wins over a row's action keys): a second verb on the selected item, which closes or rebuilds the list like on_action.
---@field on_close? fun()
---@field reload? fun(): any[] Makes a toggle list: enter and space (while no filter types) run on_select (or the row's `run`) and keep the overlay open, then the items this returns replace the list, rendered again, keeping selection and filter. Enter on a row with `fill` runs on_select, fills the composer and closes the list. It runs again when an overlay opened over the list closes. Both run off the key path; an error, an item field of the wrong type included, keeps the old items and shows in /errors.

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
---
--- ```lua
--- zemi.ui.widget.add({
--- 	id = "clock",
--- 	region = "statusline.right",
--- 	render = function(ctx) return { { { text = ctx.time, hl = "Dim" } } } end,
--- })
--- ```
---@param spec zemi.WidgetSpec
function zemi.ui.widget.add(spec) end

--- Remove a widget the caller's origin or a less trusted one registered, not another plugin's.
---@param id string
---@return boolean removed false when no widget has that id
function zemi.ui.widget.del(id) end

---@return zemi.WidgetInfo[]
function zemi.ui.widget.list() end

--- A size along the parent box's direction: whole cells, "N%" of the box's
--- cells after separators, "fill" (a share of what is left, by `weight`) or
--- "content" (the rows a region, bar or composer draws; in a col only).
---@alias zemi.Size integer|string

---@alias zemi.LayoutDir
---| "row"  # left to right
---| "col"  # top to bottom

---@alias zemi.LayoutScroll
---| "top"
---| "bottom"  # follows the tail

---@alias zemi.FloatAnchor
---| "NW"
---| "NE"
---| "SW"
---| "SE"

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
---@field stage? boolean Prompts, overlays, toasts and completion popups go over this node (the screen when none, or when it is under 40 columns or 8 rows). At most one.

--- Children laid out left to right ("row") or top to bottom ("col").
---@class zemi.LayoutBox: zemi.LayoutBase
---@field kind "box"
---@field dir zemi.LayoutDir
---@field sep? boolean A one-cell separator between children.
---@field children zemi.LayoutNode[]

--- The widgets registered in `region`, stacked by priority. A region is drawn at most once.
---@class zemi.LayoutRegion: zemi.LayoutBase
---@field kind "region"
---@field region zemi.Region
---@field scroll? zemi.LayoutScroll Scrollable (needs an id): the wheel over it and the zemi.ui.scroll.* actions move it.
---@field focusable? boolean Can take the keys (needs an id): zemi.ui.focus, a click on it, and keymaps with `focus` set to its id.
---@field fade? integer Rows at an edge content lies past that fade toward the canvas (needs scroll); the shipped chat uses 4.

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
---@field placeholder? string Drawn dim under the cursor while the draft is empty, on one row cut to the composer; the first key typed replaces it.

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
---@field anchor? zemi.FloatAnchor Default "NW".
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

--- Scroll a layout node, or the stage's scrolled region (the chat) without
--- `id`, as the zemi.ui.scroll.* host actions do: for a click or a handler.
--- A "top" or "bottom" issued before the node's rows arrive applies after the
--- next layout, and "top" holds until the node scrolls again.
--- User or builtin origin only; raises inside render.
---@param action zemi.ScrollAction
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

--- Register a palette; every highlight group and the Go chrome draw in it
--- once `ui.theme` names it. A "<name>-light" theme draws on a light terminal
--- when the same or a more trusted origin (the same plugin) added it.
---
--- ```lua
--- zemi.ui.theme.add("mine", { bg = "#1e1e2e", fg = "#cdd6f4", accent = "#89b4fa", ok = "#a6e3a1", warn = "#f9e2af", danger = "#f38ba8" })
--- zemi.config.set("ui.theme", "mine")
--- ```
---@param name string
---@param spec zemi.ThemeSpec
function zemi.ui.theme.add(name, spec) end

--- The theme drawing now: the set one (whorl when that names none), or its
--- "-light" twin on a light terminal.
---@return string
function zemi.ui.theme.get() end

--- Every registered theme, by name.
---@return { name: string, origin: string }[]
function zemi.ui.theme.list() end

---@class zemi.ui.overlay
zemi.ui.overlay = {}

--- Opening the id of an open overlay replaces it in place, keeping selection
--- and filter, when the caller may close it (see close); otherwise it opens
--- over it. An item or action field of the wrong type raises (a list item's
--- `id` and `hl` are its own data).
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
--- enter, nil on cancel. Items take the zemi.OverlayItem keys too.
---
--- ```lua
--- zemi.ui.select({ "low", "medium", "high" }, { prompt = "Effort" }, function(choice)
--- 	if choice then zemi.config.set("agent.effort", choice) end
--- end)
--- ```
---@param items any[]
---@param opts zemi.SelectOpts
---@param on_choice fun(item: any|nil)
function zemi.ui.select(items, opts, on_choice) end

--- A one-line prompt in the shape of `vim.ui.input`: on_confirm gets the
--- text on enter, which its footer names "send", nil on esc.
---
--- ```lua
--- zemi.ui.input({ prompt = "Branch name" }, function(text)
--- 	if text then zemi.notify(text) end
--- end)
--- ```
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

---@class zemi.ui.hl
zemi.ui.hl = {}

--- Define one highlight group for every theme, replacing the shipped
--- definition: its colors name palette roles, so it follows a theme switch.
--- User or builtin origin only.
---@param group string
---@param spec zemi.HLSpec
function zemi.ui.hl.set(group, spec) end

--- How a group draws under the active theme: links followed, roles
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
--- (`Accent` keywords, `Literal` strings and numbers, `Dim` comments and
--- punctuation, `Normal` the rest), each wrapped to `width` cells when
--- given. A `lang` with no lexer comes back plain. Every segment copies, so
--- a selection copies the code.
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

---@class zemi.notify.level
---@field INFO "info"
---@field WARN "warn"
---@field ERROR "error"
zemi.notify.level = {}

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

---@class zemi.ModelSpec
---@field id string
---@field display? string
---@field context_window? integer Without it there is no context percent and no auto compaction.
---@field cost_per_1m_in? number
---@field cost_per_1m_out? number
---@field cost_per_1m_cache_write? number
---@field cost_per_1m_cache_read? number
---@field max_output? integer Reply token cap; unset, anthropic models get 32000 and other kinds the provider default.
---@field vision? boolean false for a model that refuses image input; read then returns a text error and @image attachments are omitted with a note.
---@field decisions? boolean A model that answers classification through its provider's own protocol; only the `system_one` kind takes it, and no chat turn runs on one.

---@class zemi.ProviderSpec
---@field id string
---@field kind zemi.ProviderKind
---@field api_key? string Read it with `zemi.env.get`; never write a key into a file.
---@field base_url? string Required for openai_compat, refused for openrouter; overrides the default endpoint for the other kinds.
---@field headers? table<string, string> Sent with every request; as secret as the key.
---@field body? table<string, any> Extra request-body fields, merged verbatim into every request (service_tier, a gateway's routing block). Not sent by google.
---@field object_mode? zemi.ProviderObjectMode Pins how this provider answers an object call (`zemi.model.ask` with a schema, `zemi.classify.ask`). Only the openai, openai_compat, google, openrouter and vercel kinds take it: anthropic always picks its own.
---@field models zemi.ModelSpec[]

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

---@alias zemi.ModelRole
---| "chat"     # the conversation
---| "agents"   # subagents
---| "titles"   # session titles
---| "compact"  # summaries
---| "memory"   # the reflector

---@alias zemi.ModelTarget
---| "chat"     # the conversation
---| "agents"   # subagents
---| "titles"   # session titles
---| "compact"  # summaries
---| "memory"   # the reflector
---| string     # a name zemi.model.add or zemi.agent.profile.add registered

---@class zemi.ModelInfo
---@field id string "provider/model".
---@field display string
---@field has_key boolean
---@field roles zemi.ModelTarget[] The model targets that run on this model. "chat" is models.chat, also while the active profile runs the conversation on its own model.

---@class zemi.provider
zemi.provider = {}

---@class zemi.provider.kind
---@field ANTHROPIC "anthropic"
---@field OPENAI "openai"
---@field OPENAI_COMPAT "openai_compat"
---@field GOOGLE "google"
---@field OPENROUTER "openrouter"
---@field VERCEL "vercel"
---@field SYSTEM_ONE "system_one"
zemi.provider.kind = {}

---@class zemi.provider.object_mode
---@field AUTO "auto"
---@field JSON "json"
---@field TOOL "tool"
---@field TEXT "text"
zemi.provider.object_mode = {}

--- Register a provider and its models. User config only. An id a builtin
--- provider has replaces it whole (a gateway in front of it keeps its
--- base_url); the builtin ones are on while their key is exported. An unknown
--- field on the spec or on a model entry raises.
---
--- ```lua
--- zemi.provider.add({
--- 	id = "gw",
--- 	kind = zemi.provider.kind.OPENAI_COMPAT,
--- 	base_url = "https://gw.example/v1",
--- 	api_key = zemi.env.get("GW_API_KEY"),
--- 	models = { { id = "deepseek-chat", context_window = 128000 } },
--- })
--- ```
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

---@class zemi.model.role
---@field CHAT "chat"
---@field AGENTS "agents"
---@field TITLES "titles"
---@field COMPACT "compact"
---@field MEMORY "memory"
zemi.model.role = {}

--- The levels /effort offers, for `zemi.config.set("agent.effort", ...)`.
---@class zemi.model.effort
---@field LOW "low"
---@field MEDIUM "medium"
---@field HIGH "high"
zemi.model.effort = {}

---@return zemi.ModelInfo[]
function zemi.model.list() end

---@class zemi.ModelTargetSpec
---@field desc string What `/model roles` shows for the slot.
---@field model? zemi.ModelTarget|string The model the slot runs until someone picks: a builtin role name (resolved on every read) or a `"provider/model"` id; "" follows chat.
---@field classify? boolean true for a slot `zemi.classify.ask` runs on: `/model <slot>` then offers the decisions models beside the chat ones.

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
--- kept for it. A profile is not one: nothing removes a definition. User,
--- builtin or plugin origin.
---@param name string
function zemi.model.del(name) end

--- Switch the model the conversation runs on: the active profile's slot when
--- it has one, else models.chat. Raises for a provider nobody registered; an
--- id the provider does not list is taken and warned about. User or builtin
--- origin only. A call made after boot is kept for later starts, like
--- `zemi.config.set`. An explicitly named target is plain config:
--- `zemi.config.set("models.titles", id)`.
---@param id string
function zemi.model.set(id) end

---@class zemi.ModelAsk
---@field prompt string
---@field system? string
---@field model? zemi.ModelTarget|string A target name or a "provider/model" id; "chat" (models.chat, not a profile's own model) by default.
---@field max_output? integer The reply's token cap, 1024 by default.
---@field schema? table The shape the reply must have, a JSON schema in the same fields a tool's schema has: `{ type = "object", properties = {...}, required = {...} }` for an object, or a bare `{ type = "string" }`, `{ type = "integer", minimum = 0, maximum = 9 }`, `{ type = "boolean" }` or `{ type = "array", items = ... }`. `enum` pins the values, `description` is read by the model. Any other field and any other type raises. With it the call returns a decoded value instead of text (see `zemi.ModelHandle.wait`).
---@field on_done? fun(value: any, err_or_raw: string?) Runs as a task when the reply is in: the reply's text alone without a `schema`, else the decoded value with the raw reply beside it, or nil and the error when the call failed.

---@class zemi.ModelHandle
---@field wait fun(self: zemi.ModelHandle): any, string|nil Suspends the task until the reply, 10 minutes at most; raises outside a task and under pcall, like `zemi.HTTPHandle.wait`. Without a `schema`: the text, or nil and the error. With one: the decoded value (a table for an object, a number, string or boolean for a scalar) and the raw reply, or nil and the error, whose text carries the reply when it did not fit the schema.
---@field cancel fun(self: zemi.ModelHandle)

--- One tool-less model call, run off the interpreter and billed to the live
--- session; a reload cancels it. A plugin's own call settles where its
--- manifest declares `model` (one it does not declare is refused, not asked);
--- project code raises.
---
--- ```lua
--- local title, err = zemi.model.ask({ model = "titles", prompt = "Name this diff:\n" .. diff }):wait()
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
---@field timeout? number Seconds to wait, 60 by default.

---@class zemi.ClassifyHandle
---@field wait fun(self: zemi.ClassifyHandle): zemi.ClassifyResult|nil, string|nil Suspends the task until the verdict, `timeout` at most; raises outside a task and under pcall, like `zemi.HTTPHandle.wait`.
---@field cancel fun(self: zemi.ClassifyHandle)

--- Judge data with a model: every question gets a validated answer, or the
--- call is an error. Nothing here parses a sentence - the strategy that
--- answers is reported back, and a strategy that fails does not settle
--- anything for the next one.
---
--- A plugin's own call settles where its manifest declares `model`; project code raises. The
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

---@class zemi.ProfileSpec
---@field model? zemi.ModelRole|string The model the profile's slot runs until someone picks `models.<name>`: a builtin role name (resolved on every read) or a "provider/model" id.
---@field system? string Replaces the system prompt.
---@field temperature? number
---@field tools? string[] Allowed tool names; "-name" removes one and keeps the rest, and fails the send when no tool has that name.
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
---@field source zemi.Origin
---@field model zemi.ModelTarget|string The file's `model:` (a target such as "agents", or a "provider/model"); "" when it omits one, which runs the "agents" role.

---@class zemi.AgentRunSpec
---@field prompt string
---@field agent? string A name from `zemi.agent.list()`, or "fork" to continue the live conversation, which needs a turn running; "general" by default.
---@field description? string Shown while it runs.

---@class zemi.RunHandle
---@field cancel fun(self: zemi.RunHandle)

---@class zemi.agent
zemi.agent = {}

---@class zemi.agent.profile
zemi.agent.profile = {}

--- Define a named profile (model, prompt, tool set). User, builtin or plugin
--- origin; project code may not. The `agent.profile` config key names the
--- active one, and `/profile` or ctrl+t switches it.
---@param name string
---@param spec zemi.ProfileSpec
function zemi.agent.profile.add(name, spec) end

---@return zemi.AgentCurrent
function zemi.agent.current() end

--- Send a message. Returns at once; the turn runs off the interpreter, so
--- the error is what can be known before it starts (no model yet); during
--- a turn it joins at the next step. A plugin's own send settles where its
--- manifest declares `agent`, and raises in `zemi run`; its text reaches the
--- model as the plugin's, never the user's words. Project code raises.
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
--- project definitions.
---@return zemi.AgentDef[]
function zemi.agent.list() end

--- Run a subagent off the interpreter. User or builtin origin only; a reload cancels it.
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
-- a workflow script, which also has zemi.json and nothing else of zemi.

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

---@return zemi.WorkflowInfo[]
function zemi.workflow.list() end

--- Run a workflow in its own Lua state. Config Lua, user or builtin origin
--- only; a reload cancels it.
--- ```lua
--- zemi.workflow.run({ name = "review" }, function(result, err) zemi.agent.send(result or err) end)
--- ```
---@param spec zemi.WorkflowRunSpec
---@param on_done? fun(result: string|nil, err: string|nil, log: string[]) A result that is not a string arrives as JSON.
---@return zemi.WorkflowHandle|nil handle
---@return string|nil err No such saved workflow, or one in an untrusted project.
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
--- bad spec or an id a more trusted origin registered.
---
--- ```lua
--- zemi.composer.complete({
--- 	id = "issues",
--- 	trigger = "#",
--- 	items = function(lead) return { { text = "12", desc = "login fails on Safari" } } end,
--- })
--- ```
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

-- zemi.perm

---@class zemi.PermAskSpec
---@field subject string What the ask is about, as the overlay titles it: a gate subject like "fs.write" or "net.github.com", or the plugin's own word.
---@field arg? string What the ask acts on: a path, a URL, a command.
---@field reason? string Why the plugin asks; the overlay's why row.
---@field preview? string A diff or code the overlay shows under the ask.

---@class zemi.PermAnswer
---@field answer "allow"|"deny"|"told" `told`: the user denied and told zemi what to do instead.

---@class zemi.PermAskHandle
---@field wait fun(self: zemi.PermAskHandle): zemi.PermAnswer|nil, string|nil Suspends the task until the user answers; raises outside a task, like `zemi.JobHandle.wait`.

---@class zemi.perm
zemi.perm = {}

--- Ask the user through zemi's permission overlay. Plugin origin only, and
--- only from a task; the manifest's `uses` naming `perm` is the consent. A
--- headless run answers "deny" and records the ask.
---
--- ```lua
--- local ans = zemi.perm.ask({ subject = "fs.write", arg = path, reason = "cache refresh" }):wait()
--- if ans.answer ~= "allow" then return nil, "refused" end
--- ```
---@param spec zemi.PermAskSpec
---@return zemi.PermAskHandle
function zemi.perm.ask(spec) end

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

---@alias zemi.ToolVerdict
---| "deny"
---| "halt"
---| "rewrite"

--- What a `tool.pre` handler returns: "deny" refuses the call, "halt" also
--- ends the turn, "rewrite" merges `input` into the call's input (ignored
--- for project code). nil lets the call through.
---@class zemi.ToolVeto
---@field decision zemi.ToolVerdict
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
--- script reaches this, and only while it runs. What the calls return stays in
--- the script: print or return what should reach you.
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
--- show (a deferred MCP tool) or a parameter to check before calling.
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

--- `<data>/lua/<name>`, where config code keeps its files: accepted by
--- `zemi.ui.edit` for user config only, and outside the config tree, so a
--- write never reloads the config. Reads
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
--- Raises on a bad pattern or a failed walk, unlike the other zemi.fs calls.
---@param pattern string
---@return string[] paths
function zemi.fs.glob(pattern) end

--- Search the project's files for a Go regular expression, in path order,
--- skipping binary and over 4 MB files.
--- Raises on a bad pattern or a failed walk, unlike the other zemi.fs calls.
---@param pattern string
---@return zemi.GrepHit[]
function zemi.fs.grep(pattern) end

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
---@field wait fun(self: zemi.HTTPHandle): zemi.HTTPResponse|nil, string|nil Suspends the task until the response, 120 seconds at most; raises outside a task (a tool's execute, a command, a keymap, an overlay callback, an on_done or on_exit) and under pcall.

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

--- A GET, with `on_done` where a handle cannot be waited on (a render, a timer).
---
--- ```lua
--- zemi.http.get("https://wttr.in/Oslo?format=3", {
--- 	on_done = function(res)
--- 		if res.status == 200 then zemi.notify(res.body) end
--- 	end,
--- })
--- ```
---@param url string
---@param opts? { on_done: fun(res: zemi.HTTPDone) }
---@return zemi.HTTPHandle
function zemi.http.get(url, opts) end

-- zemi.job

---@class zemi.JobSpec
---@field cmd string
---@field args? string[]
---@field env? table<string, string>
---@field cwd? string The project dir by default; any dir `zemi.fs` reads. A missing dir is the returned error.
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

--- Read an environment variable, nil when it is not set. Config fields
--- hold values, never variable names.
---@param name string
---@return string?
function zemi.env.get(name) end

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

-- zemi.memory, zemi.learn, zemi.skills

---@class zemi.Memory
---@field id string
---@field scope zemi.Scope
---@field origin zemi.MemoryOrigin
---@field text string
---@field quote string The user's own words a memory about them rests on; "" otherwise.

---@class zemi.MemoryOpts
---@field scope? zemi.Scope "project" by default; "user" holds in every project.
---@field quote? string The user's exact words from this session, at most 160 characters.
---@field replaces? string The memory this one rewrites, by its id.

---@class zemi.MemoryAddResult
---@field stored? boolean Written straight away under auto autonomy.
---@field id? string
---@field queued? boolean Waiting for review in /memory.
---@field reason? string Why it waits, "" when the autonomy setting alone holds it.
---@field duplicate? string Id of the live or pending memory it repeats; nothing was written.
---@field dropped? boolean The quote is not in the session's messages; nothing was written.

---@class zemi.memory
zemi.memory = {}

---@class zemi.memory.scope
---@field USER "user"
---@field PROJECT "project"
zemi.memory.scope = {}

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

--- Delete a memory by id. User or builtin
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

---@alias zemi.LearnAction
---| "accept"
---| "reject"

--- Decide one proposal by id. User or builtin origin only.
---@param id string
---@param action zemi.LearnAction
---@return true|nil ok
---@return string|nil err
function zemi.learn.decide(id, action) end

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

--- Enable a disabled skill. User or builtin origin only.
---@param name string
---@return true|nil ok
---@return string|nil err
function zemi.skills.enable(name) end

---@param name string
---@return true|nil ok
---@return string|nil err
function zemi.skills.disable(name) end

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

---@alias zemi.ServiceStatus
---| "idle" # a language server that starts on its first file
---| "starting"
---| "ready"
---| "failed" # error says why
---| "needs sign-in" # an MCP server waiting for /mcp sign-in
---| "stopped"
---| "off" # a plugin turned off

---@class zemi.MCPInfo
---@field name string
---@field status zemi.ServiceStatus "starting", "ready", "failed", "needs sign-in" or "stopped".
---@field error string
---@field tools string[] The tools the model gets, after enabled_tools and disabled_tools.

---@class zemi.mcp
zemi.mcp = {}

--- Add an MCP server. User config only. Its tools arrive as
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
---@field status zemi.ServiceStatus "idle", "starting", "ready", "failed" or "stopped".
---@field error string

---@class zemi.lsp
zemi.lsp = {}

--- Add a language server. User config only; it starts on the first
--- matching file. Adding a builtin's name (gopls, pyright,
--- typescript, clangd, rust-analyzer, luals) replaces it, which is how a
--- builtin gets args, env or settings; `luals` also serves the Lua of your
--- config and local plugins. A server that never comes up only means no
--- diagnostics.
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
--- replaces its row; a name a more trusted origin holds raises. `zemi health`
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

--- A plugin's tests: each top-level file in its `tests/` is a test file
--- (`tests/*.lua`), and code they share goes in a subdirectory
--- (`tests/lib/x.lua`, `require("./lib/x")`). Only a `zemi plugins test`
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
--- test("the widget shows the branch git reports", function()
--- 	test.eq(test.render("demo.branch"), { "main" })
--- end)
--- ```
---@class zemi.test
---@overload fun(name: string, fn: fun()) Declare a case, at the file's top level.
zemi.test = {}

--- Compare deeply, functions by identity; raises naming the first
--- difference, by line for multi-line strings.
---
--- ```lua
--- test.eq(test.render("demo.branch"), { "main" })
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

--- Draw the plugin's widget: its lines as text, then as segments; ctx's
--- fields override width 80, height 1 and the rest.
---
--- ```lua
--- local text, lines = test.render("demo.branch", { width = 40 })
--- test.eq(text, { "main" })
--- test.eq(lines[1][1].hl, "Dim")
--- ```
---@param id string
---@param ctx? table
---@return string[] text
---@return zemi.Line[] lines
function zemi.test.render(id, ctx) end

--- Run the widget's on_click for a segment's click id.
---@param id string
---@param click string
---@return true|nil ok
---@return string? err
function zemi.test.click(id, click) end

--- Deliver a host event to the plugin's handlers: `tool.post` returns the
--- output the model gets, a vetoable event its zemi.Veto or nil.
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
--- `zemi.composer.complete` source for the trigger that starts the last word.
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
--- model a string or `{ text }`, classify `{ answers = {...}, strategy = ... }`
--- (the whole result `zemi.classify.ask` hands back), send and write none (a
--- stubbed write
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
--- test.eq(test.render("demo.branch"), { "fix-login" })
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
