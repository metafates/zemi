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

---@alias zemi.Origin
---| "user"     # ~/.config/zemi
---| "builtin"  # shipped with zemi
---| "plugin"   # site/pack
---| "project"  # <project>/.zemi
---| "learned"  # model-written, lowest trust

---@alias zemi.Level "debug"|"info"|"warn"|"error"
---@alias zemi.Scope "user"|"project"
---@alias zemi.Region "header"|"chat"|"statusline.left"|"statusline.right"

--- One styled run of text in a widget or overlay row. `hl` names a
--- highlight group; omit it for the default foreground.
---@class zemi.Segment
---@field text string
---@field hl? string

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
---@field writable boolean False for host-owned knobs `zemi.config.set` refuses.

---@class zemi.config
zemi.config = {}

--- Read a setting, or nil when it is not set and has no default.
---@param path string
---@return any
function zemi.config.get(path) end

--- Write a setting. User config only; raises for an unknown path, a wrong
--- kind, or a host-owned knob.
---@param path string
---@param value any
function zemi.config.set(path, value) end

--- Call fn whenever a setting changes.
---@param path string
---@param fn fun(new: any, old: any)
---@return integer watch_id
function zemi.config.watch(path, fn) end

--- Every known setting with its default.
---@return zemi.SettingSchema[]
function zemi.config.schema() end

--- Replay the boot steps, so config just written takes effect. Same as
--- /reload. Refused while config is still loading.
---@return true|nil ok
---@return string|nil err
function zemi.config.reload() end

-- ---------------------------------------------------------------------------
-- zemi.keymap

---@class zemi.KeymapOpts
---@field desc? string Shown in /help.

---@class zemi.Keymap
---@field key string
---@field desc string
---@field origin zemi.Origin
---@field rhs string A command, a host action, or "function".

---@class zemi.keymap
zemi.keymap = {}

--- Bind a key or a key sequence. The right-hand side is a function, a
--- command name, or a host action ("zemi.ui.scroll.up"). Keys may be
--- sequences that start with a non-printable key: "ctrl+x s", "<leader>gs"
--- (`<leader>` is the `keymap.leader` setting, ctrl+g by default). Setting
--- the same key again rebinds it.
---@param key string
---@param action string|fun()
---@param opts? zemi.KeymapOpts
function zemi.keymap.set(key, action, opts) end

---@param key string
---@return boolean removed
function zemi.keymap.del(key) end

---@param key string
---@return zemi.Keymap|nil
function zemi.keymap.get(key) end

---@return zemi.Keymap[]
function zemi.keymap.list() end

-- ---------------------------------------------------------------------------
-- zemi.command

--- What a command handler receives: the raw argument string and it split
--- into words.
---@class zemi.CommandArgs
---@field args string
---@field list string[]

---@class zemi.CommandOpts
---@field desc? string Shown in /help and the slash palette.
---@field nargs? "0"|"?"|"*"|"+" How many arguments the command takes.
---@field complete? fun(lead: string, line: string): string[]

---@class zemi.Command
---@field name string
---@field desc string
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
function zemi.command.run(name, args) end

---@return zemi.Command[]
function zemi.command.list() end

-- ---------------------------------------------------------------------------
-- zemi.event

--- The payload every handler receives, plus the event's own fields.
---@class zemi.Event
---@field name string
---@field from string "host" or the origin that emitted it.

---@class zemi.EventOpts
---@field id? string Name the handler so `zemi.event.off` can drop it.
---@field coalesce? boolean Collapse bursts into the last one.
---@field once? boolean Drop the handler after it fires.

---@class zemi.event
zemi.event = {}

--- Subscribe to an event. Vetoable events (`permission.request`,
--- `app.quit`, `composer.submit`, `tool.pre`) give the handler 50 ms and
--- then take their logged default; return `{ deny = true, reason = "..." }`
--- to veto one.
---@param name string
---@param fn fun(ev: zemi.Event): any
---@param opts? zemi.EventOpts
---@return string id
function zemi.event.on(name, fn, opts) end

---@param id string
---@return boolean removed
function zemi.event.off(id) end

--- Emit an event. Only `user.*` names and catalogued events are emittable,
--- and project code may emit `user.*` only.
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
---@field steering string[] Messages queued mid-turn.
---@field thought number Seconds spent reasoning.
---@field elapsed number Seconds since the turn started.

---@class zemi.ToolCall
---@field name string
---@field input string
---@field status string "running", "ok" or "err".
---@field detail string Subagent and workflow progress.
---@field label string The model's description of the call.
---@field elapsed number
---@field diff string
---@field output string

--- One transcript entry.
---@class zemi.TranscriptRow
---@field role string "user" or "assistant".
---@field kind string "", "reasoning", "tool", "compact" or "error".
---@field text string
---@field tool string
---@field status string
---@field label string
---@field detail string
---@field diff string
---@field output string Last lines of a tool result.
---@field ["in"] integer Prompt tokens.
---@field out integer Completion tokens.
---@field thought number
---@field elapsed number

---@class zemi.Todo
---@field content string
---@field status string "pending", "in_progress" or "completed".
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
--- render must be pure and finish in about 10 ms.
---@class zemi.WidgetCtx
---@field width integer
---@field height integer Rows the chat region has.
---@field time string "15:04".
---@field version string
---@field cwd string
---@field model string
---@field profile string
---@field effort string
---@field permission_mode string
---@field permission_mode_base string
---@field agent_active boolean
---@field agent zemi.AgentCtx
---@field transcript zemi.TranscriptRow[]
---@field transcript_epoch integer Changes when the transcript is replaced; 0 while an agent run is shown, so rows are worth caching only when it is stable.
---@field todos zemi.Todo[]
---@field recent table[] Recent sessions, only while the transcript is empty.
---@field usage zemi.Usage
---@field context zemi.ContextUsage
---@field show_reasoning boolean
---@field pulse boolean The shared 600 ms blink phase; always false with animations off.
---@field reflecting boolean
---@field pending_memory integer
---@field agents_running integer

---@class zemi.WidgetSpec
---@field id string
---@field region zemi.Region
---@field render fun(ctx: zemi.WidgetCtx): zemi.Line[]
---@field priority? integer Higher draws first within a region.
---@field wrap? boolean Word-wrap instead of truncating.
---@field keys? fun(key: string): boolean Handle a key before the composer; return true when handled.

---@class zemi.WidgetInfo
---@field id string
---@field region string
---@field priority integer
---@field origin zemi.Origin

---@class zemi.HLSpec
---@field fg? string Hex ("#7aa2f7") or a palette name.
---@field bg? string
---@field bold? boolean
---@field italic? boolean
---@field underline? boolean
---@field reverse? boolean

---@class zemi.ThemeSpec
---@field palette? table<string, string>
---@field hl? table<string, zemi.HLSpec>

---@class zemi.OverlaySpec
---@field id string
---@field title? string
---@field items? any[]
---@field render_item? fun(item: any): string
---@field on_select? fun(item: any)
---@field on_close? fun()

---@class zemi.SelectOpts
---@field prompt? string
---@field format_item? fun(item: any): string

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
function zemi.ui.widget.register(spec) end

---@return zemi.WidgetInfo[]
function zemi.ui.widget.list() end

---@class zemi.ui.theme
zemi.ui.theme = {}

---@param name string
---@param spec zemi.ThemeSpec
function zemi.ui.theme.register(name, spec) end

---@param name string
function zemi.ui.theme.set(name) end

---@return string
function zemi.ui.theme.get() end

---@class zemi.ui.overlay
zemi.ui.overlay = {}

---@param spec zemi.OverlaySpec
function zemi.ui.overlay.open(spec) end

---@param id string
---@return boolean closed
function zemi.ui.overlay.close(id) end

--- A picker in the shape of `vim.ui.select`. on_choice gets the item on
--- enter, nil on cancel.
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
---@field title? string popup title and working-row label (default: the file name)

--- Open a project file in the user's editor (editor.command, $VISUAL, $EDITOR)
--- and call on_done when it closes: on_done(mode) with "fullscreen", "popup"
--- or "split", or on_done(nil, err). Inside tmux the editor opens over zemi
--- (editor.window); otherwise zemi hands over the terminal until it exits.
--- Never blocks the interpreter. Raises for a path outside the project.
---@param path string
---@param opts? zemi.EditOpts
---@param on_done fun(mode: string|nil, err: string|nil)
function zemi.ui.edit(path, opts, on_done) end

--- Change one highlight group on top of the active theme, and any theme
--- set later. `zemi.ui.theme.register` builds a whole theme instead.
---@param group string
---@param spec zemi.HLSpec
function zemi.ui.hl(group, spec) end

--- Render markdown into widget rows of at most `width` cells.
---@param text string
---@param width integer
---@return zemi.Line[]
function zemi.ui.markdown(text, width) end

--- The drawn screen as plain text, one line per row, styling and trailing
--- padding dropped. This is how zemi reads its own UI instead of asking
--- what the user sees.
---@return string|nil screen
---@return string|nil err
function zemi.ui.screenshot() end

--- Open the /help overlay.
function zemi.ui.help() end

-- ---------------------------------------------------------------------------
-- zemi.win: programs running in windows over the chat

---@class zemi.win
zemi.win = {}

---@class zemi.WinSpec
---@field cmd string Run with sh -c in the project directory.
---@field kind? "split"|"vsplit"|"float" Below the chat (default), beside it, or over it.
---@field title? string Default: the command; the program's own title replaces it.
---@field cols? integer Float only: requested width.
---@field rows? integer Float only: requested height.
---@field focus? boolean Give the user's keys to the window at once.
---@field hidden? boolean Start off screen (running); show with set({ hidden = false }).
---@field on_exit? fun(info: zemi.WinInfo) Runs when the program exits.

---@class zemi.WinLayout
---@field kind? "split"|"vsplit"|"float"
---@field hidden? boolean
---@field frac? number A split or vsplit's share of the screen, 0.1 to 0.9.
---@field cols? integer A float's width.
---@field rows? integer A float's height.
---@field x? integer A float's left column, -1 centers.
---@field y? integer A float's top row, -1 centers.
---@field title? string

---@class zemi.WinInfo
---@field id integer
---@field title string
---@field kind string
---@field cmd string
---@field owner "model"|"lua"
---@field cols integer
---@field rows integer
---@field running boolean
---@field hidden boolean
---@field frac? number
---@field x? integer
---@field y? integer
---@field code? integer Exit code once it exited.
---@field cursor { x: integer, y: integer, visible: boolean }

---@class zemi.Win
---@field id integer
local Win = {}
--- Type text into the program (control characters included).
---@param text string
---@return true|nil, string|nil
function Win:send(text) end
--- Press keys in neovim notation: plain characters type themselves,
--- <esc> <cr> <tab> <bs> <up> <f1> name keys, <c-x> <a-x> <s-tab> add
--- modifiers, <lt> is a literal <. Raises nothing; returns nil, err for a
--- name it does not know.
---@param keys string
---@return true|nil, string|nil
function Win:keys(keys) end
--- A mouse event at 0-based cell x, y: kind click (default: press and
--- release), press, release, move or wheel; button left (default), middle,
--- right, wheel_up, wheel_down; mod such as "ctrl" or "shift+alt".
---@param spec { x: integer, y: integer, kind?: string, button?: string, mod?: string }
---@return true|nil, string|nil
function Win:mouse(spec) end
--- The screen as plain text.
---@return string
function Win:text() end
--- Block until the screen shows want (default 5000 ms). Blocks the
--- interpreter: keep it short, prefer zemi.timer for long waits.
---@param want string
---@param timeout_ms? integer
---@return boolean shown, string text
function Win:wait_text(want, timeout_ms) end
---@return zemi.WinInfo
function Win:info() end
--- Give the user's keys to this window.
function Win:focus() end
--- Change where or how the window shows, or hide and show it; the program
--- keeps running. Like nvim_win_set_config for a terminal buffer.
---@param layout zemi.WinLayout
---@return true|nil, string|nil
function Win:set(layout) end
--- End the program (if still running) and remove the window.
function Win:close() end

--- Open a window: a real terminal (TERM=xterm-256color) drawn inside zemi
--- that keeps running while zemi works. Needs `lua.job.start` allowed for
--- the command. Returns nil, err when it cannot start.
---@param spec zemi.WinSpec
---@return zemi.Win|nil, string|nil
function zemi.win.open(spec) end

---@param id integer
---@return zemi.Win|nil
function zemi.win.get(id) end

---@return zemi.WinInfo[]
function zemi.win.list() end

---@param id integer
---@return boolean
function zemi.win.close(id) end

--- Focus a window by id; 0 or nothing gives the keys back to zemi.
---@param id? integer
function zemi.win.focus(id) end

-- ---------------------------------------------------------------------------
-- zemi.notify and zemi.log

---@class zemi.NotifySpec
---@field title? string
---@field body? string
---@field level? zemi.Level

--- A toast. Callable: `zemi.notify({ title = "done", body = "..." })`.
---@class zemi.notify
---@overload fun(spec: zemi.NotifySpec)
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
---@field max_output_tokens? integer
---@field images? boolean false for a model that refuses image input; read then returns a text error and @image attachments are omitted with a note.

---@class zemi.ProviderSpec
---@field id string
---@field kind string One of `zemi.provider.kind`.
---@field api_key? string Read it with `zemi.env.get`; never write a key into a file.
---@field base_url? string Required for openai_compat.
---@field models zemi.ModelSpec[]

---@class zemi.ProviderKind
---@field ANTHROPIC "anthropic"
---@field OPENAI "openai"
---@field OPENAI_COMPAT "openai_compat"

---@class zemi.ProviderInfo
---@field id string
---@field kind string
---@field base_url string

---@class zemi.ModelInfo
---@field id string "provider/model".
---@field display string
---@field has_key boolean

---@class zemi.provider
---@field kind zemi.ProviderKind
zemi.provider = {}

--- Register a provider and its models. User config only; zemi ships none.
---@param spec zemi.ProviderSpec
function zemi.provider.register(spec) end

---@return zemi.ProviderInfo[]
function zemi.provider.list() end

---@class zemi.model
zemi.model = {}

---@return zemi.ModelInfo[]
function zemi.model.list() end

--- Switch the model. Raises for an unknown id.
---@param id string
function zemi.model.set(id) end

--- Open the model picker and switch to the chosen model.
---@return string|nil id nil when cancelled.
function zemi.model.pick() end

-- ---------------------------------------------------------------------------
-- zemi.agent

---@class zemi.ProfileSpec
---@field model? string
---@field system? string Replaces the system prompt.
---@field temperature? number
---@field tools? string[] Allowed tool names.
---@field context? { files: string[] } Files pinned into the prompt.

---@class zemi.AgentCurrent
---@field id string Session id.
---@field model string
---@field profile string

---@class zemi.AgentUsage
---@field ["in"] integer
---@field out integer
---@field total integer
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
---@field cancel fun()

---@class zemi.agent
zemi.agent = {}

--- Define a named profile (model, prompt, tool set). User or project
--- config only.
---@param name string
---@param spec zemi.ProfileSpec
function zemi.agent.profile(name, spec) end

---@param name string
function zemi.agent.set_default(name) end

---@return zemi.AgentCurrent
function zemi.agent.current() end

--- Send a message. Returns at once; the turn runs off the interpreter.
---@param text string
function zemi.agent.send(text) end

--- Interrupt the running turn.
function zemi.agent.cancel() end

---@return zemi.AgentUsage
function zemi.agent.usage() end

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

---@class zemi.workflow
zemi.workflow = {}

---@return string[] names
function zemi.workflow.list() end

--- Run a workflow in its own Lua state. User config only.
---@param spec zemi.WorkflowRunSpec
---@param on_done fun(result: any|nil, err: string|nil, log: string)
---@return zemi.RunHandle
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
---@field json string The fantasy message parts, as stored.

---@class zemi.session
zemi.session = {}

---@return string id
function zemi.session.id() end

---@return string title
function zemi.session.title() end

---@param title string
function zemi.session.set_title(title) end

---@return zemi.StoredMessage[]
function zemi.session.messages() end

---@param opts? { limit: integer }
---@return zemi.SessionInfo[]
function zemi.session.list(opts) end

---@return true|nil ok
---@return string|nil err
function zemi.session.new() end

---@param id string
---@return true|nil ok
---@return string|nil err
function zemi.session.open(id) end

---@return zemi.Todo[]
function zemi.session.todos() end

--- Restore the files a turn changed and cut the conversation back to the
--- message with this id.
---@param message_id string
---@return integer|nil restored
---@return string|nil err
function zemi.session.rewind(message_id) end

-- ---------------------------------------------------------------------------
-- zemi.composer

---@class zemi.ComposerState
---@field text string
---@field cursor integer 0-based character offset.

---@class zemi.composer
zemi.composer = {}

---@return zemi.ComposerState
function zemi.composer.get() end

---@param text string
---@param cursor? integer -1, the default, puts the cursor at the end.
function zemi.composer.set(text, cursor) end

---@param text string
function zemi.composer.insert(text) end

--- Submit the draft, as if enter was pressed.
function zemi.composer.submit() end

-- ---------------------------------------------------------------------------
-- zemi.permission

---@alias zemi.Decision "allow"|"ask"|"deny"

---@class zemi.RuleSpec
---@field subject string "tool.bash", "fs.read", "fs.write", "net.<host>", "lua.<ns.call>", "mcp.<server>".
---@field pattern string Glob over the subject's argument.
---@field decision zemi.Decision Allow and permanent rules need user config.
---@field match? string An argument the rule must match; checked at registration.
---@field not_match? string A glob that falls through and forces an ask.
---@field not_match_example? string An argument `not_match` must catch.
---@field scope? "session"|"permanent"

---@class zemi.Rule
---@field subject string
---@field pattern string
---@field decision zemi.Decision
---@field not_match string
---@field scope string

---@class zemi.PermModeSpec
---@field base? string The mode this one starts from.
---@field description? string
---@field prompt? string A system prompt section while the mode is active.

---@class zemi.PermMode
---@field name string
---@field base string
---@field description string
---@field active boolean

---@class zemi.permission
zemi.permission = {}

--- Add a permission rule. Any origin may add deny and ask rules; allow and
--- permanent rules need user config. Ordering is deny > ask > allow, then
--- fewest wildcards, then longest literal prefix.
---@param spec zemi.RuleSpec
function zemi.permission.rule(spec) end

---@return zemi.Rule[]
function zemi.permission.rules() end

---@class zemi.permission.mode
zemi.permission.mode = {}

--- Define a permission mode. User config only.
---@param name string
---@param spec zemi.PermModeSpec
function zemi.permission.mode.add(name, spec) end

---@param name string
function zemi.permission.mode.remove(name) end

---@return zemi.PermMode[]
function zemi.permission.mode.list() end

-- ---------------------------------------------------------------------------
-- zemi.tool

---@class zemi.ToolSpec
---@field name string
---@field description string What the model reads to decide when to call it.
---@field input table A JSON Schema object with `type`, `properties` and `required`.
---@field execute fun(args: table): string
---@field timeout_ms? integer
---@field parallel? boolean Safe to run beside other tool calls.

---@class zemi.ToolPreEvent
---@field tool string
---@field input table

---@class zemi.ToolOverride
---@field pre fun(ev: zemi.ToolPreEvent): table|nil Return `{ decision = "deny"|"halt", reason = ... }` or `{ input = ... }` to rewrite. Not available to project or learned code.

---@class zemi.ToolInfo
---@field name string
---@field description string
---@field origin string

---@class zemi.tool
zemi.tool = {}

--- Register a tool the model can call.
---@param spec zemi.ToolSpec
function zemi.tool.register(spec) end

--- Hook a tool call before it runs. A rewritten input re-enters at the
--- permission gate.
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
--- `**` spans directories ("**/*.go").
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
---@field timeout_ms? integer
---@field on_done? fun(res: zemi.HTTPResponse|nil, err: string|nil)

---@class zemi.HTTPResponse
---@field status integer
---@field body string
---@field headers table<string, string>

---@class zemi.HTTPHandle
---@field wait fun(self: zemi.HTTPHandle, ms?: integer): zemi.HTTPResponse|nil, string|nil

--- http and https only, no userinfo. Loopback, RFC1918 and metadata
--- addresses are refused at connect time, on every redirect hop; an
--- explicit user allow rule for the host is the localhost opt-in.
---@class zemi.http
zemi.http = {}

---@param spec zemi.HTTPSpec
---@return zemi.HTTPHandle
function zemi.http.request(spec) end

---@param url string
---@param opts? { on_done: fun(res: zemi.HTTPResponse|nil, err: string|nil) }
---@return zemi.HTTPHandle
function zemi.http.get(url, opts) end

-- ---------------------------------------------------------------------------
-- zemi.job

---@class zemi.JobSpec
---@field cmd string
---@field args? string[]
---@field env? table<string, string>
---@field stdin? boolean Needed before `write`.
---@field timeout_ms? integer `out` only; 60000 by default.
---@field on_stdout? fun(lines: string[])
---@field on_stderr? fun(lines: string[])
---@field on_exit? fun(res: zemi.JobResult)

---@class zemi.JobResult
---@field id integer
---@field stdout string
---@field stderr string
---@field code integer
---@field error string

---@class zemi.JobHandle
---@field id integer
---@field wait fun(self: zemi.JobHandle, ms?: integer): zemi.JobResult|nil, string|nil
---@field kill fun(self: zemi.JobHandle)
---@field write fun(self: zemi.JobHandle, s: string)
---@field close_stdin fun(self: zemi.JobHandle)

--- Child processes run in their own session and process group, with the
--- project directory as their working directory.
---@class zemi.job
zemi.job = {}

---@param spec zemi.JobSpec
---@return zemi.JobHandle
function zemi.job.start(spec) end

--- Start a job and wait for it.
---@param spec zemi.JobSpec
---@return zemi.JobResult|nil res
---@return string|nil err
function zemi.job.out(spec) end

-- ---------------------------------------------------------------------------
-- zemi.timer

---@class zemi.TimerHandle
---@field stop fun(self: zemi.TimerHandle)

---@class zemi.timer
zemi.timer = {}

--- Run fn once after ms. `set(0, fn)` runs it on the next queue turn.
---@param ms integer
---@param fn fun()
---@return zemi.TimerHandle
function zemi.timer.set(ms, fn) end

--- Run fn every ms until stopped.
---@param ms integer
---@param fn fun()
---@return zemi.TimerHandle
function zemi.timer.every(ms, fn) end

-- ---------------------------------------------------------------------------
-- zemi.state and zemi.env

--- A persisted key-value store, global to zemi. Values round-trip as JSON.
---@class zemi.state
zemi.state = {}

---@param key string
---@return any
function zemi.state.get(key) end

---@param key string
---@param value any
---@return true|nil ok
---@return string|nil err
function zemi.state.put(key, value) end

---@class zemi.env
zemi.env = {}

--- Read an environment variable. Secret-shaped names resolve for user and
--- builtin origin only; config fields hold values, never variable names.
---@param name string
---@return string
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

---@alias zemi.MemoryKind "fact"|"gotcha"|"preference"|"convention"|"decision"

---@class zemi.Memory
---@field id string
---@field scope zemi.Scope
---@field kind zemi.MemoryKind
---@field origin "user"|"learned"
---@field text string
---@field score number
---@field tags string[]

---@class zemi.MemorySpec
---@field text string At most 500 characters.
---@field kind zemi.MemoryKind
---@field scope? zemi.Scope "project" by default.
---@field tags? string[]

---@class zemi.MemoryAddResult
---@field stored? boolean Written straight away under auto autonomy.
---@field queued? boolean Waiting for review in /memory.
---@field id? string

---@class zemi.memory
zemi.memory = {}

--- Best-first search. Every hit bumps the memory's hit count.
---@param query string
---@param opts? { scope: "all"|zemi.Scope, limit: integer }
---@return zemi.Memory[]
function zemi.memory.search(query, opts) end

--- Propose a memory. It goes through intake and the autonomy gate, so it
--- is stored or queued for review, never written blindly.
---@param spec zemi.MemorySpec
---@return zemi.MemoryAddResult
function zemi.memory.add(spec) end

---@param include_superseded? boolean
---@return zemi.Memory[]
function zemi.memory.list(include_superseded) end

--- Mark `old` as replaced by `by`. Both ids may be 8-character prefixes.
--- User or builtin origin only.
---@param old string
---@param by string
function zemi.memory.supersede(old, by) end

--- User or builtin origin only.
---@param id string
function zemi.memory.forget(id) end

---@class zemi.Proposal
---@field id string
---@field kind zemi.MemoryKind
---@field scope zemi.Scope
---@field origin string
---@field text string
---@field tags string[]

---@class zemi.learn
zemi.learn = {}

--- Memory proposals awaiting review, the rows /memory shows.
---@return zemi.Proposal[]
function zemi.learn.proposals() end

--- Decide one proposal by id. User config only.
---@param id string
---@param action "accept"|"reject"
function zemi.learn.decide(id, action) end

---@class zemi.Skill
---@field name string
---@field description string
---@field source "authored"|"builtin"|"learned"
---@field enabled boolean
---@field path string

---@class zemi.skills
zemi.skills = {}

---@return zemi.Skill[]
function zemi.skills.list() end

--- A skill is inert until it is enabled. User or builtin origin only.
---@param name string
function zemi.skills.enable(name) end

---@param name string
function zemi.skills.disable(name) end

-- ---------------------------------------------------------------------------
-- zemi.mcp and zemi.lsp

---@class zemi.MCPSpec
---@field command? string A stdio server.
---@field args? string[]
---@field env? table<string, string>
---@field url? string A streamable HTTP server instead.
---@field headers? table<string, string>
---@field enabled_tools? string[]
---@field disabled_tools? string[]
---@field timeout? integer Seconds.

---@class zemi.MCPInfo
---@field name string
---@field status string "starting", "ready", "needs sign-in", "failed" or "stopped".
---@field error string
---@field source string
---@field tools string[]

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
---@field status string "available" (starts on its first file), "starting", "running", "failed" or "exited".
---@field error string

---@class zemi.lsp
zemi.lsp = {}

--- Add a language server. User config only; it starts on the first
--- matching file. Adding a builtin's name (gopls, pyright, rust-analyzer,
--- typescript, clangd) replaces it, which is how a builtin gets args, env
--- or settings. A server that never comes up only means no diagnostics.
---@param name string
---@param spec zemi.LSPSpec
function zemi.lsp.add(name, spec) end

--- Never start this server: a builtin you do not want, or one added above.
--- User config only.
---@param name string
function zemi.lsp.disable(name) end

--- The servers this project can use and their state.
---@return zemi.LSPInfo[]
function zemi.lsp.list() end

-- ---------------------------------------------------------------------------
-- zemi.health and zemi.errors

---@class zemi.HealthResult
---@field level zemi.Level
---@field detail string

---@class zemi.health
zemi.health = {}

--- Add a row to /health. The check runs under the 50 ms handler deadline
--- and may return a detail string instead of a table.
---@param name string
---@param fn fun(): zemi.HealthResult|string
function zemi.health.register(name, fn) end

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
