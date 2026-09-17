---
name: zemi-lua
description: Reference for zemi's Lua API (zemi.* namespaces, events, widgets, overlays, tools) and its rules. Load before writing or fixing init.lua, plugins, skill hooks.
---

# zemi Lua API

zemi runs Lua 5.1 (gopher-lua) in a sandbox. There is no `io`, `os.execute` or
`require` of arbitrary C modules; filesystem, network and processes go through
`zemi.fs`, `zemi.http` and `zemi.job`, each behind the permission engine. The `zemi.*`
tables are frozen: you cannot add or replace members.

## Origins

Every chunk runs with an origin from where it lives: `user` (`~/.config/zemi`) >
`builtin` (shipped) > `plugin` (`site/pack`) > `project` (`<project>/.zemi`). A
registration with the same id from a lower origin is refused; the same origin replaces
its own, and a binding may not be a prefix or an extension of a more trusted key. Only
`user` may register providers, add allow rules or decide learning proposals. Below
`builtin`, code may not call `zemi.model.set` or `zemi.config.reload`, run a more trusted
origin's command with `zemi.command.run`, or close, focus or `set` a window another origin
opened. Project and learned handlers cannot deny `app.quit` (a plugin denial holds every
time). Lua calls to gated APIs never open a prompt: an "ask" decision raises
`permission ask required` unless an allow rule exists.

## Modules

`require("a.b")` loads `lua/a/b.lua` (or `lua/a/b/init.lua`) from the config dir or a
plugin; project code also searches `.zemi/lua/` first. `require("./x")` and
`require("../x")` resolve next to the calling file and must stay in the same origin's
tree. Builtin modules load once, and plugin, project and learned code get them read-only;
any other file loads once, except that code below the file's origin gets its own instance
(both names share an instance). `/reload`
loads fresh.

## Rules that keep zemi responsive

- Keymap handlers, event handlers and widget `render` must finish in about 10 ms.
- `render` must be pure: no `zemi.*` mutations (they raise during render).
- Long work goes to `zemi.job`, `zemi.http` or `zemi.timer` with callbacks.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err` (`fs.read`,
  `fs.write`, `state.put`, `json.decode`, `session.new`, `session.open`, `job.start`,
  `workflow.run`, and the `memory`, `learn` and `skills` calls).
- A wedged handler turns the runtime "hot"; `/reload` rebuilds it. Errors show in `/errors`.

## Running Lua from the agent

The `Lua` tool runs a chunk in the live runtime as `user` origin: the agent can read and
change zemi's state through the same API, including toggling skills, widening permissions,
adding MCP servers and reading secret-shaped environment names. `print` output and return
values (as JSON) come back. A script that only reads (`zemi.config.get`, `zemi.session.messages`,
`zemi.errors.list`, `zemi.ui.screenshot`, the `list` calls, `zemi.json`, `zemi.log`, ...) runs at
once in a trusted project. Anything else asks the user once for the whole script: write every
call out by name (`zemi.job.start(...)`, `w:send(...)`), since a call reached through a name
built from strings, `_G` or `load` asks too, and one the scan misses raises. Plan mode and a
project that is not trusted ask for every script. Callbacks a script registers (keymaps,
timers, event handlers) keep only the read-only calls once the run ends, so put lasting
behavior in `init.lua`.
It holds the interpreter for at most 10s. Keep reusable scripts in the session scratchpad
(its path is in the Environment section; files there are read and written without asking)
and pass `path`. Globals a chunk sets stay in that chunk.

Check a widget, theme or layout change by reading the screen back with
`zemi.ui.screenshot()` after `zemi.config.reload()` (zemi-debug has the loop).

## Type annotations

zemi writes LuaLS annotations for this whole API to `~/.local/share/zemi/types/zemi.lua`
on every start, and a `~/.config/zemi/.luarc.json` pointing at them the first time. Read
that file when you need an exact signature, field name or return shape - tests fail when it
drifts from the surface Go registers. Each event's payload is a `zemi.ev.*` class that
`zemi.event.on` picks by name, and the globals the sandbox strips (`setfenv`, `rawset`,
`dofile`, ...) are marked deprecated, so the editor flags them. Do not edit it: the
next start overwrites it. Next to it, `types/zemi/defaultui/` is the shipped default UI
(theme, widgets, keymaps, one module each) and `types/zemi/util.lua` the helpers: real source, readable
without asking, the reference for how a widget is written.

## Namespaces

| Namespace | Members |
| --- | --- |
| `zemi.api` | `version` (API version string) |
| `zemi.config` | `get`, `set` (user config only), `watch`, `schema`, `reload()` (same as `/reload`) |
| `zemi.errors` | `list()` -> what `/errors` shows, newest first |
| `zemi.keymap` | `set(key, fn_or_action, {desc, focus})` (again to rebind), `del(key, {focus})`, `get(key, {focus})`, `list()`; keys may be sequences starting with a non-printable key: `"ctrl+x s"`, `"<leader>gs"` (`keymap.leader`, default ctrl+g); `focus = id` scopes a binding to a focusable layout node, where it may be a plain letter |
| `zemi.command` | `add(name, fn, { desc, nargs, complete })`, `del(name)`, `run(name [, args])`, `list` |
| `zemi.event` | `on(name, fn, { id, once })` -> id, `off(id)` -> bool, `emit("user.*", payload)`, `log({ name, limit })`, `stats()` |
| `zemi.ui` | `widget.register({ id, region, priority, render, wrap, on_click })/list` (a segment `{ text, hl, click = "id", copy = false, soft_wrap = true }` is clickable when the widget has `on_click(ev)`; ev is `{ id, button, clicks, mods, x, y }` and the handler runs under the widget's origin), `widget.del(id)`, `theme.register/set/get`, `hl(group, spec)` (override single groups: `{ fg, bg, sp, bold, italic, reverse, underline = true or "double"/"curl"/"dotted"/"dashed" }`, `{ link = "Group" }` draws as another; `Hover` and `HoverItem` draw over the hovered text's style, keys left out keep it, false or "NONE" clear it, plus `brighten = 0.25` and `replace = true`), `hl_get(group)` -> the resolved spec or nil, `overlay.open({ id, title, body, empty, items, render_item, selected, anchor, on_select, on_close, reload })/close` (`reload = function() return items end` makes a toggle list: enter and space run on_select, keep it open and show the new items) (items may be `{ text, desc, key, section, detail, actions, fill, run }`, see the example; `render_item` returns a string or a line of `{ text, hl }` segments; `hotkey` is one character that acts like enter on its row outside a filter; `selected` is the 1-based item selected on open; `anchor = "bottom"` opens it by the statusline), `select`, `input({ prompt, default }, on_confirm(text or nil))`, `edit(path, { line, title }, on_done(mode or nil, err))` (the user's editor on a project file, in a focused float; mode is "window"), `markdown`, `screenshot()` -> the drawn screen as plain text (editor windows blank), or nil, err, `help`, `title(text or nil)` (the terminal tab's title, user config only), `redraw()` (keymaps, clicks, one-shot timers, job/http callbacks and event handlers redraw when they return; a `timer.every` callback calls it), `reasoning.toggle()` (ctrl+o), `layout.set({ root, floats })` (user config only; see zemi-customize), `layout.get()` -> the table last set or nil, `layout.rects()` -> `{ [id] = { x, y, width, height } }` for nodes and floats with an id plus `stage` and `composer`, `focus.set(id or nil)`/`get()`/`next()`/`prev()` (the keys go to a `focusable` node until esc; `ctx.focus` names it) |
| `zemi.notify` | callable `zemi.notify({ title, body, level })` or `zemi.notify(body [, title])`, plus `info`, `warn`, `error`, `list()` |
| `zemi.log` | callable, plus `debug`, `info`, `warn`, `error` |
| `zemi.agent` | `send`, `cancel`, `current`, `profile(name, { model, system, temperature, tools, context = { files } })`, `set_default(name)`, `usage`, `list`, `run({ prompt, agent, description }, on_done(text, err))` -> `{ cancel }` (`send`, `profile`, `set_default` and `run` from user config only) |
| `zemi.workflow` | `list()` -> `{ name, description, source, path }`, `run({ script = src } or { name = saved, args = v }, on_done(result, err, log))` -> `{ cancel }` or nil, err (user config only) |
| `zemi.provider`, `zemi.model` | `register`, `list`, `kind.ANTHROPIC` / `kind.OPENAI` / `kind.OPENAI_COMPAT`; `list`, `set`, `pick` |
| `zemi.session` | `id`, `title`, `set_title`, `messages`, `list({ limit })`, `new()`, `open(id)`, `todos()`, `rewind(id)` -> files restored or nil, err (`new`, `open`, `rewind` from user config only) |
| `zemi.composer` | `get()` -> `{ text, cursor }`, `set(text [, cursor])`, `insert(text)`, `submit()`, `history_prev()`, `history_next()`, `paste()` (clipboard image or text), `edit()` (the draft in the user's editor); edits from user config only. `complete({ id, trigger = "#" or command = "name", items = function(lead, line) return { "12", { text = "40", desc = "..." } } end })` adds a completion popup for a trigger character (an "@" source goes ahead of the files) or a command's arguments; items runs off the key path on every edit, so filter by `lead`. Keymap strings for the same: `zemi.composer.submit`, `zemi.composer.insert <text>`, `zemi.composer.history_prev`, `zemi.composer.history_next`, `zemi.composer.paste`, `zemi.composer.edit` |
| `zemi.json` | `encode(v)`, `decode(s)` -> v or nil, err |
| `zemi.health` | `register(name, fn)`; `fn` returns `{ level = "ok"/"warn"/"error", detail = "..." }` or a detail string, and shows as a row in `/health`. It runs under the 50 ms handler deadline |
| `zemi.lsp` | `add(name, { command, args, env, filetypes, settings, init_options })`, `disable(name)` (both user config only), `list()` -> `{ name, command, filetypes, status, error }` (status: available, starting, running, failed, exited) |
| `zemi.mcp` | `add(name, { command, args, env, url, headers, enabled_tools, disabled_tools, timeout })` (user config only), `list()` -> `{ name, status, error, source, tools }` (status: starting, ready, needs sign-in, failed, stopped) |
| `zemi.memory`, `zemi.learn`, `zemi.skills` | see the zemi-learning skill |
| `zemi.permission` | `rule({ subject, pattern, decision, match, not_match, not_match_example })` (any origin may add deny and ask rules; allow rules need user config), `rules()`, `mode.add(name, spec)`, `mode.remove(name)`, `mode.list()` (modes: user config only; see zemi-config) |
| `zemi.tool` | `register({ name, description, input, execute, timeout_ms, parallel })`, `override(name, { pre = fn })` (not from project or learned code), `list` |
| `zemi.fs` | `read`, `write`, `list`, `stat`, `exists`, `mkdir`, `remove`, `glob` (`**` spans directories), `grep` |
| `zemi.http` | `request({ method, url, headers, body, timeout_ms, on_done })` -> handle with `wait()`; `get(url, { on_done })`. http/https only, no userinfo, and loopback, RFC1918 and metadata addresses are refused at connect time, per redirect hop; an explicit user allow rule for the host is the localhost opt-in (project and learned code never get it) |
| `zemi.win` | `open({ cmd, kind = "split"/"vsplit"/"float", title, cols, rows, focus, hidden, on_exit(info) })` -> `{ id, keys(notation), mouse({ x, y, kind, button, mod }), send(text), text(), wait_text(want, ms), info(), focus(), set({ kind, hidden, frac, cols, rows, x, y, title }), close() }` (a program in a real terminal drawn inside zemi; needs `lua.job.start` allowed; from user config its OSC 52 clipboard writes reach the host; windows survive reloads, so init.lua opens one only when `list()` has none with its title), `get(id)`, `list()` (editor windows only for user config), `close(id)`, `focus(id)` |
| `zemi.job` | `start({ cmd, args, env, stdin, on_stdout(lines), on_stderr(lines), on_exit(result) })` -> `{ id, wait(ms), kill(), write(s), close_stdin() }` (`write` needs `stdin = true`); `out(spec)` runs to completion. Both `zemi.job` and `zemi.win` need `lua.job.start` allowed for the whole command line, matched like `tool.bash` (normalized segments, so `go test *` covers `{ cmd = "go", args = { "test", "./..." } }`) |
| `zemi.timer` | `set(ms, fn)` (0 ms = next tick), `every(ms, fn)` -> `{ stop }` |
| `zemi.state`, `zemi.env` | `put`, `get` (persisted kv); `get` (filtered environment) |

`require("zemi.util")` adds `split`, `trim`, `startswith`, `endswith`, `keys`, `map`,
`filter`, `contains`, `extend`, `get`, `validate`, `inspect`.

## Examples

```lua
-- Command with argument completion; handler gets { args = "raw", list = { ... } }
zemi.command.add("greet", function(a)
  zemi.notify({ title = "hello", body = a.list[1] or "world" })
end, { desc = "say hi", nargs = "?", complete = function(lead, line) return { "alice", "bob" } end })

-- Picker
zemi.ui.select({ "patch", "minor" }, { prompt = "Bump" }, function(choice)
  if choice then zemi.agent.send("bump the " .. choice .. " version") end
end)

-- A list whose items open a detail view: enter shows `detail` with `actions` under
-- it; an action fills the composer or runs a command (`run` only from user config),
-- one with neither goes back. esc goes back, y copies the text.
zemi.ui.overlay.open({
  id = "deploys", title = "Deploys", empty = "Nothing deployed yet.",
  items = {
    { text = "api", desc = "failed · 2m ago", section = "Failed", detail = "exit 1: migration timed out",
      actions = { { text = "Retry", run = "deploy api" }, { text = "Cancel" } } },
    { text = "web", desc = "live · 1h ago", section = "Live", fill = "/deploy web " },
  },
})

-- Widget
zemi.ui.widget.register({
  id = "statusline.turns", region = "statusline.right", priority = 5,
  render = function(ctx) return { { { text = ctx.transcript_count .. " msgs", hl = "Comment" } } } end,
})

-- Background work
zemi.job.start({ cmd = "go", args = { "test", "./..." },
  on_stdout = function(lines) zemi.log.info(table.concat(lines, "\n")) end,
  on_exit = function(res) zemi.notify({ title = "go test", body = res.code == 0 and "pass" or "fail" }) end,
})
local t = zemi.timer.every(60000, function() zemi.log.info("tick") end)   -- t:stop()

-- A tool the model can call
zemi.tool.register({
  name = "ticket", description = "look up a ticket title by id",
  input = { type = "object", properties = { id = { type = "string" } }, required = { "id" } },
  execute = function(args) return "ticket " .. args.id end,
})

-- Subagents and workflows run off the interpreter; callbacks fire when done
zemi.agent.run({ agent = "explore", prompt = "Where is the retry logic?" }, function(text, err)
  zemi.notify({ title = "explore", body = text or err })
end)
zemi.command.add("review", function()
  zemi.workflow.run({ name = "review" }, function(result, err) zemi.agent.send(result or err) end)
end, { desc = "run workflows/review.lua and hand the result to the agent" })
```

Subagent definitions are markdown files in `agents/` (frontmatter `description`, optional
`name`, `tools` list or comma string, `model`; the body is the system prompt). Workflow
scripts (`workflows/<name>.lua`) run in a separate sandbox, not this interpreter: they see
only `agent(prompt, { agent, description })` -> text or nil, err, `parallel({ fn, ... })` ->
results, errs, `phase(name)`, `log(...)` (alias `print`), `json.encode/decode`, `args`, and
Lua's string, table and math libraries. `agent` and `parallel` cannot run inside `pcall`.

Widget `ctx` carries `width`, `height`, `time`, `version`, `cwd`, `project` (the trust
root, `~`-shortened), `trusted`, `model`, `profile`,
`effort`, `permission_mode`, `permission_mode_base`, `agent_active`, `transcript`,
`agent` (live turn: `active`, `model`, `text`, `reasoning`, `tools`, `steering`,
`thought`, `elapsed`, `reports` (background reports among `steering`), `compacting`, `retry`
(`attempt`, `why`, `next_in`, only while a retry waits)), `todos` (`content`, `status`, `active_form`), `view` (the agent run
shown instead of the session: `id`, `agent`, `description`, `status`), `recent` (sessions,
only while the transcript is empty), `usage`, `context`, `show_reasoning`, `pulse`,
`reflecting`, `pending_memory`, `agents_running`, `quit_armed`, `key_seq` (a pending key
sequence, "" when none), `rows_below` (transcript rows under a scrolled-up chat),
`cancelling`, `editing` (the file open in the editor, else ""), `waiting` ("approval" or
"answer" while a prompt holds the turn, else ""), `transcript_epoch`
(changes when the transcript is replaced, 0 while an agent run is shown: rows are worth
caching only when it is stable). Transcript rows carry
`role`, `kind` ("", reasoning, tool, compact, error), `text`, `tool`, `status`, `label`,
`detail` (subagent and workflow progress), `diff`, `output` (the last lines of a tool
result), `in`, `out`, `thought`, `elapsed`.
Regions: `header`, `chat`, `working` (above the composer), `statusline.left`,
`statusline.right`. Segments are `{ text = "...", hl = "Group" }`, plus `click = "id"`,
`effect = "shimmer"` (Go animates the text every frame, blending `hl` toward Normal),
`copy = false` (decoration: a selection neither lights nor copies it) and
`soft_wrap = true` (the line continues on the next row, so a copy joins them).

## Events

`app.ready`, `session.new`, `session.open`, `session.close`, `agent.start`,
`agent.finish`, `agent.error`, `agent.subagent.start`, `agent.subagent.finish`,
`agent.assemble` (return a string or `{ title, text }` to add a system prompt section;
user, builtin and plugin handlers only), `composer.change`, `tool.denied` (tool, subject, arg, by,
reason), `app.quit` (vetoable), `agent.text_delta`, `agent.reasoning_delta`, `tool.pre`
(vetoable: deny, halt, rewrite), `tool.post`, `permission.request`, `permission.resolved`,
`composer.submit` (vetoable), `ui.resize`, `ui.theme`, `overlay.open`, `overlay.close`,
`memory.written`, `skill.learned`, `config.reloaded` (after /reload or a config file
change), `reflector.start`, `reflector.done`, `job.exit`, `win.open`, `win.exit`, `win.close` (windows
opened with `zemi.win` or the window tool), `lsp.request` (server, path, kind
`diagnostics` after a write or `tool`), and your own `user.*` names. A vetoable handler vetoes
by returning `{ decision = "deny", reason = "..." }` (`tool.pre` also takes `"halt"`, and
`"rewrite"` with `input`); any other return lets the event through.

Every event is recorded whether or not anything listens: `zemi.event.log({ name, limit })`
returns the last 500 newest first, each the table a handler would have seen plus `at` and
`ts`; `zemi.event.stats()` counts emissions per name since start (the streaming deltas are
counted but not logged). Diff two `stats()` snapshots for a session or a turn, for example
`stats()["lsp.request"]` in a widget. `zemi.notify.list()` is the last 100 notifications
newest first (`at`, `ts`, `level`, `title`, `body`).
