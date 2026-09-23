---
name: zemi-lua
description: Reference for zemi's Lua API (zemi.* namespaces, events, widgets, overlays, tools) and its rules. Load before writing or fixing init.lua, plugins, skill hooks.
---

# zemi Lua API

zemi runs Lua 5.1 (gopher-lua) in a sandbox. There is no `io` and no C modules, and `os`
keeps only `time`, `date` and `clock`; filesystem, network and processes go through
`zemi.fs`, `zemi.http` and `zemi.job`, each behind the permission engine. The `zemi.*`
tables are frozen: you cannot add or replace members, and reading a member a namespace
does not have raises with the list of the ones it does.

## Origins

Every chunk runs with an origin from where it lives: `user` (`~/.config/zemi`) >
`builtin` (shipped) > `plugin` (`site/pack`) > `project` (`<project>/.zemi`). A
registration with the same id from a lower origin is refused; the same origin replaces
its own, and a binding may not be a prefix or an extension of a more trusted key. Only
`user` may register providers, add allow rules or decide learning proposals. Below
`builtin`, code may not call `zemi.model.set` or `zemi.config.reload`, run a more trusted
origin's command with `zemi.command.run`. Project handlers cannot deny `app.quit` (a plugin denial holds every
time). Lua calls to gated APIs (`zemi.fs` writes, `zemi.http`, `zemi.job`)
never open a prompt: an "ask" decision raises `permission ask required` with the
`zemi.permission.rule.add` line that allows it, which only the user's init.lua may add.
No permission mode settles such a call, yolo included.
zemi-config lists what a plugin cannot do.

## Modules

`require("a.b")` loads `lua/a/b.lua` (or `lua/a/b/init.lua`) from the config dir or a
plugin's own directory (`start/<name>/lua/`); project code also searches `.zemi/lua/`
first. `require("./x")` and
`require("../x")` resolve next to the calling file and must stay in the same origin's
tree. Builtin modules load once, and plugin and project code get them read-only;
any other file loads once, except that code below the file's origin gets its own instance
(both names share an instance). `/reload`
loads fresh.

## Rules that keep zemi responsive

- Keymap handlers, event handlers and widget `render` must finish in about 10 ms.
- `render` must be pure: registering anything, `config.set`, `state.set`/`del`, the `zemi.fs`
  writes, `job.start`, `http.request` and `agent.send` raise during render.
- A widget redraws only when something else does. A clock or any other widget with its own
  clock drives it: `zemi.timer.every(1000, zemi.ui.redraw)` (`ctx.time` is already `"15:04"`).
- Long work goes to `zemi.job`, `zemi.http` or `zemi.timer` with callbacks.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err` (`fs.read`,
  `fs.write`, `state.get`, `state.set`, `json.decode`, `session.new`, `session.open`,
  `job.start`, `workflow.run`, and the `memory`, `learn` and `skills` calls). A jail
  escape or a bad pattern is misuse, so `fs.read` outside the jail raises: wrap it in
  `pcall` when the path may be outside.
- Removal reports a boolean and never raises for "not there" (`keymap.del`, `event.off`,
  `widget.del`, `config.unwatch`), so a reloaded config unbinds idempotently.
- A wedged handler turns the runtime "hot"; `/reload` rebuilds it. Errors show in `/errors`.
- Once a `pcall` catches an error, closures made before it stop sharing locals with the
  function that made them (gopher-lua): a later `n = n + 1` inside one is lost outside it.
  Share state through a table (`local got = {}`), or return values out of the `pcall`.
- Module locals die on a reload. `zemi.state` is persisted and shared by every project and
  running zemi, so put the project in the key (`"branch:" .. zemi.project.root()`) and keep
  what only this run needs in a local. Plugin and project code each get their own space
  (stored under `plugin:` or `project:`) and never see user keys; user config sees them all.
- Nothing stores a value that can never work: a color that is not `#rrggbb`, a palette
  key or an ANSI color name, a rule subject outside `tool. fs. net. lua. mcp. memory.`, a printable key
  with no `focus`, a `zemi.config.watch` path outside the schema, and a tool spec with no
  `description` or with a wrapped `input` all raise. A vetoable handler that returns
  anything but nil or `{ decision = ... }` denies (`tool.pre`, `permission.request`).

## Running Lua from the agent

The `lua` tool runs a chunk in the live runtime as `user` origin: the agent can read and
change zemi's state through the same API, including toggling skills, widening permissions,
adding MCP servers and reading secret-shaped environment names. `print` output and return
values (as JSON) come back. A script that only reads (`zemi.config.get`, `zemi.session.messages`,
`zemi.errors.list`, `zemi.ui.screenshot`, the `list` calls, the `zemi.fs` readers, `zemi.json`,
`zemi.log`, a field such as `zemi.api.version`, `require("zemi.util")`, ...) runs at
once in a trusted project. Anything else asks the user once for the whole script: write every
call out by name (`zemi.job.start(...)`, `w:send(...)`), since a call reached through a name
built from strings, `_G` or `load` asks too, and one the scan misses raises. A read-only agent (the planner) and a
project that is not trusted ask for every script. Callbacks a script registers (keymaps,
timers, event handlers) keep only the read-only calls once the run ends, so put lasting
behavior in `init.lua`.
It holds the interpreter for at most 10s. Keep reusable scripts in the session scratchpad
(its path is in the Environment section; files there are read and written without asking)
and pass `path` (a `.lua` file, read under the same rules as the read tool). Globals a chunk sets stay in that chunk.

Check a widget, theme or layout change by reading the screen back with
`zemi.ui.screenshot()` after `zemi.config.reload()` (zemi-debug has the loop).

## Type annotations

zemi writes LuaLS annotations for this whole API to `~/.local/share/zemi/types/zemi.lua`
on every start, and a `~/.config/zemi/.luarc.json` pointing at them, rewritten when zemi
changes it. Read
that file when you need an exact signature, field name or return shape - tests fail when it
drifts from the surface Go registers. Each event's payload is a `zemi.ev.*` class that
`zemi.event.on` picks by name, and the globals the sandbox strips (`setfenv`, `rawset`,
`dofile`, ...) are marked deprecated, so the editor flags them. Do not edit it: the
next start overwrites it. Next to it, `types/zemi/defaultui/` is the shipped default UI
(theme, widgets, keymaps, one module each) and `types/zemi/util.lua` the helpers: real source, readable
without asking, the reference for how a widget is written.

## Namespaces

The verbs are regular: `add` registers (`command`, `tool`, `provider`, `health`, `mcp`,
`lsp`, `ui.widget`, `ui.theme`, `permission.rule`, `permission.mode`), `set` writes a
value (`config`, `state`, `keymap`, `ui.hl`, `ui.focus`, `ui.layout`, `ui.theme`), `get`
and `list` read back, and `del` removes where anything can be removed (`command`, `keymap`, `state`,
`ui.widget`, `permission.mode`), returning whether it did; a tool, provider or health row goes with the
reload that no longer adds it. `event.on` and `config.watch` hand back the id their `off` and `unwatch` take.

| Namespace | Members |
| --- | --- |
| `zemi.api` | `version` (API version string) |
| `zemi.config` | `get`, `set` (user config only), `watch(path, fn)` -> id (a path outside the schema raises), `unwatch(id)` -> bool, `schema`, `reload()` (same as `/reload`) |
| `zemi.errors` | `list()` -> what `/errors` shows, newest first |
| `zemi.project` | `trusted()` -> the user's trust decision (check it before a job runs the project's code), `root()` -> the repository root, the launch dir outside a repository |
| `zemi.keymap` | `set(key, fn_or_action, {desc, focus})` (again to rebind), `del(key, {focus})`, `get(key, {focus})`, `list()`; a printable key with no `focus` raises, since those reach the composer; keys may be sequences starting with a non-printable key: `"ctrl+x s"`, `"<leader>gs"` (`keymap.leader`, default ctrl+g); `focus = id` scopes a binding to a focusable layout node, where it may be a plain letter. An action string is a command name (`"usage"`, `"/usage"`, what `get` hands back) or a host action; `zemi.HostAction` in the types file lists every host action. Taking a key the composer owns (`ctrl+a/e/k/u/w`, `home`, `end`) works and leaves the composer without it |
| `zemi.command` | `add(name, fn, { desc, nargs, complete })` (`complete(lead, line)` returns the same rows as `zemi.composer.complete`: strings or `{ text, desc }`), `del(name)`, `run(name [, args])`, `list`; `nargs` is `""` none (the default), `"?"` at most one, `"+"` one or more, `"*"` any, and a line with the wrong count is refused with a toast instead of running the handler |
| `zemi.event` | `on(name, fn, { id, once })` -> id, `off(id)` -> bool, `emit("user.*", payload)`, `log({ name, limit })`, `stats()` |
| `zemi.ui` | `widget.add({ id, region, priority, render, wrap, on_click })/list` (`render` returns a list of lines, each a list of segments; a bare string and a single line are wrapped for you, a list of several strings raises) (a segment `{ text, hl, click = "id", copy = false, hover = false, soft_wrap = true }` is clickable when the widget has `on_click(ev)`; ev is `{ id, button, clicks, mods, x, y }` and the handler runs under the widget's origin), `widget.del(id)`, `theme.add/set/get/list` (`set` is user config only; `list` is `{ name, origin }` rows), `hl.set(group, spec)` (user config only; override single groups: `{ fg, bg, sp, bold, italic, reverse, underline = true or "double"/"curl"/"dotted"/"dashed" }`, `{ link = "Group" }` draws as another; `Hover` and `HoverItem` draw over the hovered text's style, keys left out keep it, and a segment with `hover = false` takes neither, false or "NONE" clear it, plus `brighten = 0.25` and `replace = true`; a color is `#rrggbb`, a palette key or an ANSI color name (`"red"`, `"green"`, ...) the terminal decides, anything else raises), `hl.get(group)` -> the resolved spec or nil, `overlay.open({ id, title, body, markdown, empty, items, render_item, selected, anchor, on_select, on_action, keys, on_close, reload })/close` (`reload = function() return items end` makes a toggle list: enter and space run on_select, keep it open and show the new items) (`body` is a string or a list of lines of `{ text, hl }` segments; `markdown = true` renders a string body and every `detail` as markdown) (items may be `{ text, desc, key, section, detail, actions, fill, run, copy }`, see the example; `copy` makes enter copy that text and keep the list open; `render_item` returns a string or a line of `{ text, hl }` segments; `on_select` takes enter for every item, so an item's `fill` and `run` are ignored while it is set (a `reload` list runs both); `hotkey` is one character that acts like enter on its row outside a filter; an action `{ text, id, hl }` with an `id` runs `on_action(item, action)`; `keys = { e = { desc = "edit", fn = function(item) end } }` are one-character verbs on the selected item, named in the footer (`j`, `k`, `/`, `y` and `Y` are the list's own and raise); both close the list like `on_select`, or rebuild a `reload` list; `selected` is the 1-based item selected on open; `anchor = "bottom"` opens it by the statusline), `select`, `input({ prompt, default }, on_confirm(text or nil))`, `edit(path, { line, title }, on_done(err))` (the user's editor on a project file), `markdown`, `wrap(text, width)` -> plain text as rows of segments, a wrapped row ending in `soft_wrap`, `width(text)` -> its cells (a wide glyph is two; use it for clipping and padding), `screenshot()` -> the drawn screen as plain text, or nil, err, `help`, `title(text or nil)` (the terminal tab's title, user config only), `redraw()` (keymaps, clicks, one-shot timers, job/http callbacks and event handlers redraw when they return; a `timer.every` callback calls it), `reasoning.toggle()` (ctrl+o), `layout.set({ root, floats })` (user config only; see zemi-customize), `layout.get()` -> the table last set or nil, `layout.rects()` -> `{ [id] = { x, y, width, height } }` for nodes and floats with an id plus `stage` and `composer`, `focus.set(id or nil)`/`get()`/`next()`/`prev()` (user config only; the keys go to a `focusable` node until esc; `ctx.focus` names it) |
| `zemi.notify` | callable `zemi.notify({ title, body, level })` or `zemi.notify(body [, title])`, plus `info`, `warn`, `error`, `list()` |
| `zemi.log` | callable, plus `debug`, `info`, `warn`, `error`, `list()` (the last 100 print and log lines, newest first, what `/messages` shows) |
| `zemi.agent` | `send(text)` -> true, or nil, err (what can be known before the turn starts), `cancel`, `current`, `profile(name, { model, system, temperature, tools, context = { files }, read_only })`, `usage`, `prompt()` (the system prompt of the last send, "" before it), `list`, `run({ prompt, agent, description }, on_done(text, err))` -> `{ cancel }` (`send`, `profile` and `run` from user config only; the `agent.profile` config key picks the active profile; a `read_only` profile is a planning agent) |
| `zemi.workflow` | `list()` -> `{ name, description, source, path }`, `run({ script = src } or { name = saved, args = v }, on_done(result, err, log))` -> `{ cancel }` or nil, err (user config only) |
| `zemi.provider`, `zemi.model` | `add({ id, kind, api_key, base_url, headers, body, models })` (`base_url` is required for `kind.OPENAI_COMPAT`; `body` holds extra request-body fields and only `kind.ANTHROPIC` and `kind.OPENAI_COMPAT` send it), `list`, `kind.ANTHROPIC` / `kind.OPENAI` / `kind.OPENAI_COMPAT`; `list`, `set`, `pick` |
| `zemi.session` | `id`, `title`, `set_title`, `messages()` -> `{ id, role, text, json }` rows (`text` is the text parts joined; `json` is the provider wire shape), `list({ limit })`, `new()`, `open(id)`, `todos()`, `rewind(id)` -> files restored or nil, err (`new`, `open`, `rewind` from user config only) |
| `zemi.composer` | `get()` -> `{ text, cursor }`, `set(text [, cursor])`, `insert(text)`, `submit()`, `history_prev()`, `history_next()`, `paste()` (clipboard image or text), `edit()` (the draft in the user's editor); edits from user config only. `complete({ id, trigger = "#" or command = "name", items = function(lead, line) return { "12", { text = "40", desc = "..." } } end })` adds a completion popup for a trigger character (an "@" source goes ahead of the files) or a command's arguments; items runs off the key path on every edit, so filter by `lead`. Keymap strings for the same: `zemi.composer.submit`, `zemi.composer.insert <text>`, `zemi.composer.history_prev`, `zemi.composer.history_next`, `zemi.composer.paste`, `zemi.composer.edit` |
| `zemi.json` | `encode(v)`, `decode(s)` -> v or nil, err |
| `zemi.health` | `add(name, fn)`; `fn` returns `{ level = "ok"/"warn"/"error", detail = "..." }` or a detail string, and shows as a row in `/health`. It runs under the 50 ms handler deadline, and `--health` runs it right after boot, before a job started on `app.ready` has reported, so a row over async state says "not read yet" until it has. `list()` -> the same rows (`level`, `name`, `detail`), which is how config written from Lua is checked without the user |
| `zemi.lsp` | `add(name, { command, args, env, filetypes, settings, init_options })`, `disable(name)` (both user config only), `list()` -> `{ name, command, filetypes, status, error }` (status: available, starting, running, failed, exited) |
| `zemi.mcp` | `add(name, { command, args, env, url, headers, enabled_tools, disabled_tools, timeout })` (user config only), `list()` -> `{ name, status, error, source, tools }` (status: starting, ready, needs sign-in, failed, stopped) |
| `zemi.memory`, `zemi.learn`, `zemi.skills` | see the zemi-learning skill |
| `zemi.permission` | `rule.add({ subject, pattern, decision, match, not_match, not_match_example })` (any origin may add deny and ask rules; allow rules need user config; the subject starts with `tool. fs. net. lua. mcp. memory.`; `match` is required and is an argument the pattern must catch, checked here, and `not_match` needs `not_match_example` too), `rule.list()`, `mode.add(name, spec)`, `mode.del(name)`, `mode.list()` (modes: user config only; see zemi-config). Allow rules for a project also come from its `permissions` file in zemi's state dir, read when the project is trusted |
| `zemi.tool` | `add({ name, description, input, required, execute, prompt, timeout_ms, parallel })` (`prompt` is a system prompt section that ships only while the tool is in the run's tool set, so guidance on using the tool goes there and not in an `agent.assemble` handler; keep it fixed text; `input` maps a parameter name to its JSON Schema, flat; `required` lists parameter names; a missing `description` or an `input` holding `properties` raises; the call is checked against `required` and the types before `execute` runs, which fails with `return nil, err`: a raise works too but puts file and line in front of the message; a string parameter named `about` is the row's label; `execute` holds the interpreter until it returns, so keep a `wait` in it short; the model's call is gated as subject `tool.<name>` with the name as the argument, and no mode settles it, so it asks each time until user config has `{ subject = "tool.<name>", pattern = "*", decision = "allow", match = "<name>" }`, which a plugin cannot add), `override(name, { pre = fn })` (not from project code), `list` |
| `zemi.fs` | `read`, `write`, `list`, `stat`, `exists`, `mkdir`, `remove`, `glob` (`**` spans directories), `grep`, `datadir(name)` -> `<data>/lua/<name>` (user config only); writes reach the project dir (the launch dir, `ctx.cwd`, not `zemi.project.root()`: the same dir a sandboxed command may write) and a datadir only, each needs a `lua.fs.write` allow rule for the path, and `zemi.ui.edit` opens both, plus the whole repository in a trusted project; `datadir` creates nothing, so `mkdir` it before it is a job's `cwd`; keep state files in a datadir, since a write under `~/.config/zemi` reloads the config |
| `zemi.http` | `request({ method, url, headers, body, timeout_ms, on_done })` -> handle with `wait()`; `get(url, { on_done })`. http/https only, no userinfo, and loopback, RFC1918 and metadata addresses are refused at connect time, per redirect hop. Gated as `lua.http.<method>` with the URL as the pattern (`net.<host>` is the model's `fetch` only), redirects included; a user allow rule that matches a localhost URL is the localhost opt-in (project code never gets it) |
| `zemi.job` | `start({ cmd, args, env, cwd, sandbox, stdin, on_stdout(lines), on_stderr(lines), on_exit(result) })` -> `{ id, wait(ms), kill(), write(s), close_stdin() }` (`write` needs `stdin = true`; `cwd` is the project dir unless given, any dir `zemi.fs` reads; `sandbox = true` confines the program like a bash command auto mode settled and raises when no sandbox can, which is what a job running model-written code unattended wants; `on_exit` gets `{ id, stdout, stderr, code, error }` after the last output callback, also after `kill()` (code -1), and `stdout`/`stderr` hold the first 1 MiB whether or not `on_stdout` is set); `wait(spec)` runs it to completion and blocks the interpreter while it does, so keep it to tools and short commands. `zemi.job` needs `lua.job.start` allowed for the whole command line, matched like `tool.bash` (normalized segments, so `go test *` covers `{ cmd = "go", args = { "test", "./..." } }`) |
| `zemi.timer` | `after(ms, fn)` (0 ms = next tick), `every(ms, fn)` -> `{ stop }` |
| `zemi.state`, `zemi.env` | `set`, `get`, `del`, `keys(prefix)` (persisted kv; a key nobody set is nil with no error); `get(name)` (filtered environment; nil when the variable is not set, so `zemi.env.get("K") or error("set K")` works) |

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
-- it; an action fills the composer, runs a command (`run` only from user config) or,
-- with an `id`, calls on_action; one with none goes back. esc goes back, y copies the text.
-- The item a callback gets is a copy: match on an id field, never on the table. A callback
-- closes the list unless it has `reload`; to stay open without one, open it again from the callback.
zemi.ui.overlay.open({
  id = "deploys", title = "Deploys", empty = "Nothing deployed yet.",
  items = {
    { text = "api", desc = "failed · 2m ago", section = "Failed", detail = "exit 1: migration timed out",
      actions = { { text = "Retry", run = "deploy api" }, { text = "Drop", id = "drop", hl = "Danger" } } },
    { text = "web", desc = "live · 1h ago", section = "Live", fill = "/deploy web " },
  },
  on_action = function(item, action) zemi.notify({ body = action.id .. " " .. item.text }) end,
  keys = { l = { desc = "logs", fn = function(item) zemi.agent.send("show the logs of " .. item.text) end } },
})

-- Widget
zemi.ui.widget.add({
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
zemi.tool.add({
  name = "ticket", description = "look up a ticket title by id",
  input = { id = { type = "string" } },   -- the parameters, flat
  required = { "id" },
  execute = function(args)
    if not args.id:match("^%u+-%d+$") then return nil, "not a ticket id: " .. args.id end
    return "ticket " .. args.id
  end,
})

-- Guidance ships with the tool; state lives in a datadir (the rule is the user's to add)
zemi.permission.rule.add({ subject = "lua.fs.write", pattern = zemi.fs.datadir("tickets") .. "/*",
  decision = "allow", match = zemi.fs.datadir("tickets") .. "/seen.json" })
zemi.tool.add({
  name = "ticket_seen", description = "mark a ticket as seen",
  prompt = "Call ticket_seen after you read a ticket, once per ticket.",
  input = { id = { type = "string" }, about = { type = "string" } }, required = { "id" },
  execute = function(args)
    local ok, err = zemi.fs.write(zemi.fs.datadir("tickets") .. "/seen.json", zemi.json.encode({ args.id }))
    if not ok then return nil, err end
    return "seen " .. args.id
  end,
})

-- Run the project's tests after the model changes Go files: only in a trusted project,
-- confined, from the repository root.
zemi.event.on("tool.post", function(ev)
  if not zemi.project.trusted() or not table.concat(ev.changed, " "):find("%.go") then return end
  zemi.job.start({ cmd = "go", args = { "test", "./..." }, cwd = zemi.project.root(), sandbox = true,
    on_exit = function(res) zemi.log.info("go test: " .. res.stdout .. res.stderr) end })
end)

-- Spend per turn
zemi.event.on("agent.finish", function(ev) zemi.log.info(ev.model .. " $" .. ev.usage.cost) end)

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

Widget `ctx` carries `width`, `height`, `time` (`"15:04"`), `version`, `cwd`, `project` (the trust
root, `~`-shortened), `trusted`, `model`, `profile`,
`effort`, `permission_mode`, `permission_mode_base`, `agent_active`, `transcript`,
`transcript_count` (its rows, without building it),
`agent` (live turn: `active`, `model`, `text`, `reasoning`, `tools`, `steering`,
`thought`, `elapsed`, `reports` (background reports among `steering`), `compacting`, `retry`
(`attempt`, `why`, `next_in`, only while a retry waits)), `todos` (`content`, `status`, `active_form`), `view` (the agent run
shown instead of the session: `id`, `agent`, `description`, `status`), `recent` (sessions,
only while the transcript is empty), `usage`, `context`, `show_reasoning`, `frame` (the 50ms animation tick, 0 with animations off), `done` (a turn ended a moment ago),
`reflecting`, `pending_memory`, `agents_running` (this session's background agents; their reports arrive as messages), `jobs_running` (background jobs), `quit_armed`, `key_seq` (a pending key
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
`agent.finish` (session, model, `usage`: this turn's `{ ["in"], out, total, cost }`),
`agent.error`, `agent.subagent.start`, `agent.subagent.finish`,
`agent.assemble` (return a string or `{ title, text }` to add a system prompt section;
user, builtin and plugin handlers only), `composer.change`, `tool.denied` (tool, subject, arg, by,
reason), `app.quit` (vetoable), `agent.text_delta`, `agent.reasoning_delta`, `tool.pre`
(vetoable: deny, halt, rewrite), `tool.post` (tool, input, ok, output cut to 4096 characters,
`changed`: the absolute paths a file tool or a bash command changed), `permission.request`,
`permission.resolved`,
`composer.submit` (vetoable), `ui.resize`, `ui.theme`, `overlay.open`, `overlay.close`,
`memory.written`, `config.reloaded` (after /reload or a config file
change), `reflector.start`, `reflector.done`, `job.exit`, `lsp.request` (server, path, kind
`diagnostics` after a write or `tool`), and your own `user.*` names. A vetoable handler vetoes
by returning `{ decision = "deny", reason = "..." }` (`tool.pre` also takes `"halt"`, and
`"rewrite"` with `input`); any other return lets the event through. A handler has 50 ms, so
it cannot wait for an answer. A denied `composer.submit` keeps the draft, so to confirm a
send: deny, ask, and let the same text through once.

```lua
local pass
zemi.event.on("composer.submit", function(ev)
  if pass == ev.text or not ev.text:find("prod") then pass = nil return end
  zemi.ui.select({ "send", "cancel" }, { prompt = "This mentions prod" }, function(choice)
    if choice ~= "send" then return end
    pass = ev.text
    zemi.composer.submit()   -- user config only; from a plugin, tell the user to press enter
  end)
  return { decision = "deny", reason = "confirm first" }
end)
```

Every event is recorded whether or not anything listens: `zemi.event.log({ name, limit })`
returns the last 500 newest first, each the table a handler would have seen plus `at` and
`ts`; `zemi.event.stats()` counts emissions per name since start (the streaming deltas are
counted but not logged). Diff two `stats()` snapshots for a session or a turn, for example
`stats()["lsp.request"]` in a widget. `zemi.notify.list()` is the last 100 notifications
newest first (`at`, `ts`, `level`, `title`, `body`).
