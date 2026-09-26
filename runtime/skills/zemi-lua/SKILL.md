---
name: zemi-lua
description: "Load before writing Lua for zemi (init.lua, a plugin, a skill hook) or running the lua tool: the zemi.* API, origins, events and handler rules."
---

# zemi Lua API

zemi runs Lua 5.1 (gopher-lua) in a sandbox. There is no `io` and no C modules, and `os`
keeps only `time`, `date` and `clock`; filesystem, network and processes go through
`zemi.fs`, `zemi.http` and `zemi.job`, each behind the permission engine. The `zemi.*`
tables are frozen: you cannot add or replace members, and reading a member a namespace
does not have raises with the list of the ones it does.

## Origins

Every chunk runs with an origin from where it lives: `user` (`~/.config/zemi`) >
`builtin` (shipped) > `plugin` (`<data>/plugins/installed|local/<name>`) > `project`
(`<repo>/.agents`). A call runs as the least trusted code on its stack, so a plugin's
`setup()` that init.lua calls runs as the plugin, not as user config. A registration with
the same id from a lower origin is refused; the same origin replaces its own, except that
one plugin can neither replace nor remove another's, and a binding may not be a prefix or an extension of
a more trusted key. Only
`user` may register providers, add allow rules or decide learning proposals. Below
`builtin`, code may not call `zemi.model.set` or `zemi.config.reload`, or run a more trusted
origin's command with `zemi.command.run`, and a project handler cannot deny `app.quit`. Lua calls to gated APIs (`zemi.fs` writes, `zemi.http`, `zemi.job`, and a
plugin's `zemi.agent.send` and `zemi.model.ask`) never open a prompt: an "ask" decision
raises `permission ask required` with the `zemi.permission.rule.add` line that allows it,
which only the user's init.lua may add (show it to the user and ask before adding it); a plugin's line carries `principal = "plugin:<name>"`,
so the rule allows that plugin alone.
No permission mode allows such a call, yolo included.
In plugin and project code, `setmetatable` takes only that code's own Lua functions as
handlers (a `zemi.*` member, a builtin helper or another plugin's function raises) and reads them when it runs, so set
every handler before the call.
zemi-customize lists what a plugin cannot do.

## Modules

`require("a.b")` loads `lua/a/b.lua` (or `lua/a/b/init.lua`) from the config dir, else
from the plugin named `a` (`a/lua/a/`), which its own code searches first; project code
also searches `.agents/lua/` first. A plugin answers only names under its own, so it cannot
shadow another's modules. `require("./x")` and `require("../x")` resolve next to the calling
file and must stay in the same origin's tree, and plugin code in its own plugin. Each file
loads once (both names share an instance), except that code below the file's origin gets its
own instance, one per plugin. Builtin modules for plugin and project code, and one plugin's
modules for another plugin, come read-only, so keep a secret in a local, not in the module
table. `/reload` loads fresh.

## Rules that keep zemi responsive

- Keymap and event handlers finish in about 10 ms; long work goes to zemi.job, zemi.http or zemi.timer callbacks, or to a `wait` in a task (below). Widget render has stricter rules, in zemi-customize.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err` (`fs.read`,
  `fs.write`, `state.get`, `state.set`, `json.decode`, `session.new`, `session.resume`,
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
  what only this run needs in a local. Each plugin and project code get their own space
  (stored under `plugin:<name>:` or `project:`) and never see user keys; user config sees
  them all.
- Nothing stores a value that can never work: a color that is not `#rrggbb`, a palette
  key or an ANSI color name, a rule subject outside `tool. fs. net. lua. mcp. memory.`, a printable key
  with no `focus`, a key sequence containing ctrl+c (cancel and quit), a `zemi.config.watch` path outside the schema, and a tool spec with no
  `description` or with a wrapped `input` all raise. A vetoable handler that returns
  anything but nil or `{ decision = ... }` denies (`tool.pre`, `permission.request`).

## Waiting

A handle's `wait()` (`zemi.http`, `zemi.job` and `zemi.model.ask` handles) and
`zemi.job.wait` suspend the task that calls them: the interpreter serves keys, renders and
other callbacks until the result is there, and the task goes on after the call. A task is a
Lua tool's `execute`, a Lua command, a keymap function, an overlay callback (`on_select`,
`on_action`, `on_close`, `keys`, `reload`), an `on_done` or `on_exit` callback, a
`tool.post` handler, a permission mode's `decide`, and a lua tool script. Anywhere else
`wait` raises: in a widget render, any other event handler, a timer callback, `on_stdout`, a
completion source or a health check use the callback form (`on_done`, `on_exit`) instead. It
also raises under `pcall` and inside a library callback such as a `table.sort` comparator or
a `gsub` function, and in a coroutine you made: call it outside them and check its
`nil, err`. `return job:wait()` is fine. A keymap or command that waits lets the next key
run, so two presses can interleave, and a reload ends every waiting task at once.

## The lua tool

Its description covers what comes back, the 10 s ceiling between waits and which calls need the user's approval. A script runs as `user` origin, so an approved one can do what init.lua can, secret-shaped `zemi.env` names included. A call reached through a name built from strings, `_G` or `load` needs approval too, and one the scan misses raises; in an untrusted project or a read-only agent every script needs approval. After the run, callbacks the script registered keep only the read-only calls and its globals are gone, so lasting behavior belongs in init.lua. Check a widget, theme or layout change with `zemi.ui.screenshot()` after `zemi.config.reload()` (zemi-debug has the loop).

## Type annotations

zemi writes LuaLS annotations for this whole API to `~/.local/share/zemi/types/zemi.lua`
on every start, and a `.luarc.json` pointing at them in `~/.config/zemi` and each local plugin, rewritten when zemi
changes it. Read
that file when you need an exact signature, field name or return shape - tests fail when it
drifts from the surface Go registers. Each event's payload is a `zemi.ev.*` class that
`zemi.event.on` picks by name, and the globals the sandbox strips (`setfenv`, `rawset`,
`dofile`, ...) are marked deprecated, so the editor flags them. Do not edit it: the
next start overwrites it. `types/zemi/util.lua` beside it is the `zemi.util` source.

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
| `zemi.keymap` | `set(key, fn_or_action, {desc, focus})` (again to rebind), `del(key, {focus})`, `get(key, {focus})`, `list()`; a printable key with no `focus` raises, since those reach the composer; keys may be sequences starting with a non-printable key: `"ctrl+x s"`, `"<leader>gs"` (`keymap.leader`, default ctrl+x); `focus = id` scopes a binding to a focusable layout node, where it may be a plain letter. An action string is a command name (`"usage"`, `"/usage"`, what `get` hands back) or a host action; `zemi.HostAction` in the types file lists every host action. Taking a key the composer owns (`ctrl+a/e/k/u/w`, `home`, `end`) works and leaves the composer without it |
| `zemi.command` | `add(name, fn, { desc, nargs, complete })` (`complete(lead, line)` returns the same rows as `zemi.composer.complete`: strings or `{ text, desc }`, and is not called past the last argument `nargs` takes), `del(name)`, `run(name [, args])`, `list`; `nargs` is `""` none (the default), `"?"` at most one, `"+"` one or more, `"*"` any, and a line with the wrong count is refused with a toast instead of running the handler |
| `zemi.event` | `on(name, fn, { id, once })` -> id, `off(id)` -> bool, `emit("user.*", payload)` (handlers get a JSON-style copy: a table that is not a sequence arrives with string keys), `log({ name, limit })`, `stats()` |
| `zemi.ui` | `widget.add({ id, region, priority, render, wrap, on_click })/list` (`render` returns a list of lines, each a list of segments; a bare string and a single line are wrapped for you, a list of several strings raises) (a segment `{ text, hl, click = "id", copy = false, hover = false, soft_wrap = true }` is clickable when the widget has `on_click(ev)`; ev is `{ id, button, clicks, mods, x, y }` and the handler runs under the widget's origin), `widget.del(id)`, `theme.add/set/get/list` (`set` is user config only; `list` is `{ name, origin }` rows), `hl.set(group, spec)` (user config only; override single groups: `{ fg, bg, sp, bold, italic, reverse, underline = true or "double"/"curl"/"dotted"/"dashed" }`, `{ link = "Group" }` draws as another (a `bold` or `italic` beside it applies over it); `Hover` and `HoverItem` draw over the hovered text's style, keys left out keep it, and a segment with `hover = false` takes neither, false or "NONE" clear it, plus `brighten = 0.25` and `replace = true`; a color is `#rrggbb`, a palette key or an ANSI color name (`"red"`, `"green"`, ...) the terminal decides, anything else raises), `hl.get(group)` -> the resolved spec or nil, `overlay.open({ id, title, body, markdown, empty, items, render_item, selected, anchor, on_select, on_action, keys, on_close, reload })/close` (`reload = function() return items end` makes a toggle list: enter and space run on_select, keep it open and show the new items, and it rebuilds when an overlay opened over it closes) (opening the id of an open overlay replaces it in place, keeping selection and filter) (`body` is a string or a list of lines of `{ text, hl }` segments; `markdown = true` renders a string body and every `detail` as markdown) (items may be `{ text, desc, key, on, locked, section, detail, actions, fill, run, copy, verb }`, see the example; `key` is a right-aligned tag, `on = true` or `false` draws the on/off switch in its place and `locked = true` one that is on and cannot switch; `copy` is the row's value: `y` copies it in place of the row's text, and enter copies it, keeping the list open, when the row does nothing else; a change of `section` starts a header row, and the filter matches the section and `copy` too; `verb` names enter's action in the hint (`"open"`), space passes that row by in a `reload` list, and an `"open"` row's `run` leaves the list under the screen it opens; `render_item` returns a string or a line of `{ text, hl }` segments; `on_select` takes enter for every item, so an item's `fill` and `run` are ignored while it is set (a `reload` list runs both); `hotkey` is one character that acts like enter on its row outside a filter; an action `{ text, id, hl }` with an `id` runs `on_action(item, action)`; `keys = { e = { desc = "edit", fn = function(item) end } }` are one-character verbs on the selected item, named in the footer (`j`, `k`, `/`, `y` and `Y` are the list's own and raise); both close the list like `on_select`, or rebuild a `reload` list; `selected` is the 1-based item selected on open; `anchor = "bottom-left"` or `"bottom-right"` opens it by the statusline, in line with that edge of the composer frame), `select`, `input({ prompt, default }, on_confirm(text or nil))`, `edit(path, { line, title }, on_done(err))` (the user's editor on a project file), `markdown`, `wrap(text, width)` -> plain text as rows of segments, a wrapped row ending in `soft_wrap`, `highlight(text, lang, width or nil)` -> code as rows in the code-block groups (`lang` a chroma lexer name like "bash"; plain rows when none), `width(text)` -> its cells (a wide glyph is two; use it for clipping and padding), `screenshot()` -> the drawn screen as plain text, or nil, err, `help`, `title(text or nil)` (the terminal tab's title, user config only), `redraw()` (keymaps, clicks, one-shot timers, job/http callbacks and event handlers redraw when they return; a `timer.every` callback calls it), `reasoning.toggle()` (ctrl+o), `scroll(action, id or nil)` (a `zemi.ui.scroll.*` action for a click: `"bottom"`, `"line_up"`, ...), `layout.set({ root, floats })` (user config only; see zemi-customize), `layout.get()` -> the table last set or nil, `layout.rects()` -> `{ [id] = { x, y, width, height } }` for nodes and floats with an id plus `stage` and `composer`, `focus.set(id or nil)`/`get()`/`next()`/`prev()` (user config only; the keys go to a `focusable` node until esc; `ctx.focus` names it) |
| `zemi.notify` | callable `zemi.notify({ title, body, level })` or `zemi.notify(body [, title])`, plus `info`, `warn`, `error`, `list()`, and `level.INFO` / `level.WARN` / `level.ERROR` |
| `zemi.log` | callable, plus `debug`, `info`, `warn`, `error`, `list()` (the last 100 print and log lines, newest first, what `/messages` shows) |
| `zemi.agent` | `send(text)` -> true, or nil, err (what can be known before the turn starts), `cancel`, `current`, `profile.add(name, { model, system, temperature, tools, context = { files }, read_only })`, `usage`, `prompt()` (the system prompt of the last send, "" before it), `list`, `run({ prompt, agent, description }, on_done(text, err))` -> `{ cancel }` (`profile.add` and `run` from user config only; `send` raises for project code, and from a plugin it needs a `lua.agent.send` rule for it, reaches the model as the plugin's text, never the user's, raises in a print run (-p), and during a turn joins at the next step, so an `agent.finish` handler can keep a goal going; the `agent.profile` config key picks the active profile; a `read_only` profile is a planning agent) |
| `zemi.workflow` | `list()` -> `{ name, description, source, path }`, `run({ script = src } or { name = saved, args = v }, on_done(result, err, log))` -> `{ cancel }` or nil, err (user config only) |
| `zemi.provider`, `zemi.model` | `add({ id, kind, api_key, base_url, headers, body, models })` (`base_url` is required for `kind.OPENAI_COMPAT` and refused for `kind.OPENROUTER`; `body` holds extra request-body fields, which `kind.OPENAI` and `kind.GOOGLE` do not send), `del(id)`, `list`, `kind.ANTHROPIC` / `kind.OPENAI` / `kind.OPENAI_COMPAT` / `kind.GOOGLE` / `kind.OPENROUTER` / `kind.VERCEL`; `zemi.model`: `list()` -> `{ id, display, has_key, roles }` (`roles`: the role names that run on that model, `chat` meaning `models.chat` even while a profile runs its own model), `set(id)` (writes `models.chat`), `role.CHAT` / `role.AGENTS` / `role.TITLES` / `role.COMPACT` / `role.MEMORY`, `effort.LOW` / `effort.MEDIUM` / `effort.HIGH` (for `agent.effort`), `pick(role)` (with no role opens `/model`, else that role's list from `/model roles`; an unknown role raises), `ask({ prompt, system, model, max_output, on_done(text, err) })` -> `{ wait(), cancel() }` (one tool-less call on a role's model or an id, `chat` by default, 1024 tokens by default, billed to the session; a plugin needs a `lua.model.ask` rule for it, project code raises) |
| `zemi.session` | `id`, `title`, `rename(title)`, `messages()` -> `{ id, role, text, json }` rows (`text` is the text parts joined; `json` is the provider wire shape), `list({ limit })`, `new()`, `resume(id)`, `todos()`, `rewind(id)` -> files restored or nil, err (`new`, `resume`, `rewind` from user config only) |
| `zemi.composer` | `get()` -> `{ text, cursor }`, `set(text [, cursor])`, `insert(text)`, `submit()`, `history_prev()`, `history_next()`, `paste()` (clipboard image or text), `edit()` (the draft in the user's editor); edits from user config only. `complete({ id, trigger = "#" or command = "name", items = function(lead, line) return { "12", { text = "40", desc = "..." } } end })` adds a completion popup for a trigger character (an "@" source goes ahead of the files) or a command's arguments; items runs off the key path on every edit, so filter by `lead`. Keymap strings for the same: `zemi.composer.submit`, `zemi.composer.insert <text>`, `zemi.composer.history_prev`, `zemi.composer.history_next`, `zemi.composer.paste`, `zemi.composer.edit` |
| `zemi.json` | `encode(v)`, `decode(s)` -> v or nil, err |
| `zemi.health` | `add(name, fn)`; `fn` returns `{ level = "ok"/"warn"/"error", detail = "..." }` or a detail string, and shows as a row in `/health`; any other level fails the row like a raise. It runs under the 50 ms handler deadline, and `--health` runs it right after boot, before a job started on `app.ready` has reported, so a row over async state says "not read yet" until it has. `list()` -> the same rows (`level`, `name`, `detail`), which is how config written from Lua is checked without the user |
| `zemi.lsp` | `add(name, { command, args, env, filetypes, settings, init_options })`, `disable(name)` (both user config only), `list()` -> `{ name, command, filetypes, status, error }` (status: idle until its first file, starting, ready, failed, stopped) |
| `zemi.mcp` | `add(name, { command, args, env, url, headers, enabled_tools, disabled_tools, timeout })` (user config only), `list()` -> `{ name, status, error, tools }` (status: starting, ready, failed, needs sign-in, stopped) |
| `zemi.memory`, `zemi.learn`, `zemi.skills` | see the zemi-learning skill |
| `zemi.permission` | `decision.ALLOW` / `decision.ASK` / `decision.DENY`, `rule.add({ subject, pattern, decision, match, not_match, not_match_example, principal })` (any origin may add deny and ask rules; allow rules and `principal = "plugin:<name>"`, which scopes a rule to one plugin's Lua calls, need user config; the subject starts with `tool. fs. net. lua. mcp. memory.`; `match` is required and is an argument the pattern must catch, checked here, and `not_match` needs `not_match_example` too), `rule.list()`, `mode.add(name, spec)`, `mode.del(name)`, `mode.set(name)`, `mode.list()` (add and set: user config only; see zemi-customize). Allow rules for a project also come from its `permissions` file in zemi's state dir, read when the project is trusted |
| `zemi.tool` | `add({ name, description, input, required, execute, prompt, timeout_ms, parallel })` (`prompt` is a system prompt section that ships only while the tool is in the run's tool set, so guidance on using the tool goes there and not in an `agent.assemble` handler; keep it fixed text; `input` maps a parameter name to its JSON Schema, flat; `required` lists parameter names; a missing `description` or an `input` holding `properties` raises; the call is checked against `required` and the types before `execute` runs, which fails with `return nil, err`: a raise works too but puts file and line in front of the message; a string parameter named `about` is the row's label; `execute` is a task, so a `wait` in it frees the interpreter, and `timeout_ms` (default 60000) bounds the call in wall time, waits included; the model's call is gated as subject `tool.<name>` with the name as the argument, and no mode allows it, so every call needs the user's approval until user config has `{ subject = "tool.<name>", pattern = "*", decision = zemi.permission.decision.ALLOW, match = "<name>" }`, which a plugin cannot add), `execute(args, run)` gets `run:progress(text)`, one line under the call's row; `override(name, { pre, post })` (not from project code; `post` runs after the global `tool.post` handlers), `list` |
| `zemi.fs` | `read`, `write`, `list`, `stat`, `exists`, `mkdir`, `remove`, `glob` (`**` spans directories), `grep`, `data_dir(name)` -> `<data>/lua/<name>` (user config only); writes reach the project dir (the launch dir, `ctx.cwd`, not `zemi.project.root()`: the same dir a sandboxed command may write) and a data dir only, each needs a `lua.fs.write` allow rule for the path, and `zemi.ui.edit` opens both, plus the whole repository in a trusted project; `data_dir` creates nothing, so `mkdir` it before it is a job's `cwd`; keep state files in a data dir, since a write under `~/.config/zemi` reloads the config |
| `zemi.http` | `request({ method, url, headers, body, timeout_ms, on_done })` -> handle with `wait()`; `get(url, { on_done })`. http/https only, no userinfo, and loopback, RFC1918 and metadata addresses are refused at connect time, per redirect hop. Gated as `lua.http.<method>` with the URL as the pattern (`net.<host>` is the model's `fetch` only), redirects included; a user allow rule that matches a localhost URL is the localhost opt-in (project code never gets it) |
| `zemi.job` | `start({ cmd, args, env, cwd, sandbox, stdin, on_stdout(lines), on_stderr(lines), on_exit(result) })` -> `{ id, wait(ms), kill(), write(s), close_stdin() }` (`write` needs `stdin = true`; `cwd` is the project dir unless given, any dir `zemi.fs` reads; `sandbox = true` confines the program like a bash command auto mode runs on its own and raises when no sandbox can, which is what a job running model-written code unattended wants; `on_exit` gets `{ id, stdout, stderr, code, error }` after the last output callback, also after `kill()` (code -1), and `stdout`/`stderr` hold the first 1 MiB whether or not `on_stdout` is set); `zemi.job.wait(spec)` (`timeout_ms`, default 60000, then it kills the command) runs one to completion, suspending the task (see Waiting). `zemi.job` needs `lua.job.start` allowed for the whole command line, matched like `tool.bash` (normalized segments, so `go test *` covers `{ cmd = "go", args = { "test", "./..." } }`) |
| `zemi.timer` | `after(ms, fn)` (0 ms = next tick), `every(ms, fn)` -> `{ stop }` |
| `zemi.state`, `zemi.env` | `set`, `get`, `del`, `keys(prefix)` (persisted kv; a key nobody set is nil with no error); `get(name)` (filtered environment; nil when the variable is not set, so `zemi.env.get("K") or error("set K")` works) |

`require("zemi.util")` adds `split`, `trim`, `keys`, `get`, `validate` and `inspect`.

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
    { text = "api", desc = "failed, 2m ago", section = "Failed", detail = "exit 1: migration timed out",
      actions = { { text = "Retry", run = "deploy api" }, { text = "Drop", id = "drop", hl = "Danger" } } },
    { text = "web", desc = "live, 1h ago", section = "Live", fill = "/deploy web " },
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

-- Guidance ships with the tool; state lives in a data dir (the rule is the user's to add)
zemi.permission.rule.add({ subject = "lua.fs.write", pattern = zemi.fs.data_dir("tickets") .. "/*",
  decision = zemi.permission.decision.ALLOW, match = zemi.fs.data_dir("tickets") .. "/seen.json" })
zemi.tool.add({
  name = "ticket_seen", description = "mark a ticket as seen",
  prompt = "Call ticket_seen after you read a ticket, once per ticket.",
  input = { id = { type = "string" }, about = { type = "string" } }, required = { "id" },
  execute = function(args)
    local ok, err = zemi.fs.write(zemi.fs.data_dir("tickets") .. "/seen.json", zemi.json.encode({ args.id }))
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

Agent files and workflow scripts are not config Lua: zemi-customize has both.

Widget `ctx` carries `width`, `height`, `time` (`"15:04"`), `version`, `cwd`, `project` (the trust
root, `~`-shortened), `trusted`, `model`, `profile`,
`effort`, `permission_mode`, `permission_mode_base`, `profile_read_only`, `model_hint` (what to do when no model is set), `init_error` and `init_path` (a failed init.lua), `instructions_missing` (a trusted project with no AGENTS.md or CLAUDE.md, where the splash offers `/init`), `focus`, `agent_active`, `transcript`,
`transcript_count` (its rows, without building it),
`agent` (live turn: `active`, `model`, `text`, `reasoning`, `tools`, `steering`,
`thought`, `elapsed`, `reports` (background reports among `steering`), `compacting`, `retry`
(`attempt`, `why`, `next_in`, only while a retry waits or is under way, `next_in` 0 then)), `todos` (`content`, `status`, `active_form`), `view` (the agent run
shown instead of the session: `id`, `agent`, `description`, `status`), `recent` (sessions,
only while the transcript is empty), `usage`, `context`, `show_reasoning`, `frame` (the 50ms animation tick, 0 with animations off), `ended` (`"ok"`, `"error"` or `"cancelled"` for a moment after a turn ends, else `""`),
`busy` (the newest job the user waits on that is not a turn, like /btw or a plugin install: `label`, `elapsed` seconds, `cancellable` by a double esc; nil when none), `pending_memory`, `agents_running` (this session's background agents; their reports arrive as messages), `jobs_running` (background jobs), `quit_armed`, `interrupt_armed` (a second esc interrupts), `key_seq` (a pending key
sequence, "" when none), `rows_below` (transcript rows under a scrolled-up chat),
`cancelling`, `editing` (the file open in the editor, else ""), `waiting` ("approval", "plan"
or "answer" while a prompt holds the turn, else ""), `transcript_epoch`
(changes when the transcript is replaced, 0 while an agent run is shown: rows are worth
caching only when it is stable). Transcript rows carry
`role`, `kind` ("", reasoning, tool, shell, compact, notice, error), `text`, `tool`, `status`, `label`,
`detail` (subagent and workflow progress), `diff`, `output` (the last lines of a tool
result), `in`, `out`, `thought`, `elapsed`.

## Events

`app.ready`, `session.new`, `session.open`, `session.close`, `agent.start`,
`agent.step` (after each model call, a subagent's too: session, model, `step`, `ms`,
`first_token_ms`, `tools`, `usage` `{ ["in"], out, cache_read, cache_write, cost }`),
`agent.finish` (session, model, `usage`: this turn's `{ ["in"], out, total, cost }`),
`agent.error`, `agent.subagent.start`, `agent.subagent.finish`,
`agent.assemble` (return a string or `{ title, text }` to add a system prompt section;
user, builtin and plugin handlers only), `composer.change`, `tool.denied` (tool, subject, arg, by,
reason), `app.quit` (vetoable), `agent.text_delta`, `agent.reasoning_delta`, `tool.pre`
(vetoable: deny, halt, rewrite), `tool.post` (tool, input, ok, `output` in full, `ms`,
`changed`: the absolute paths a file tool or a bash command changed; it runs as a task, and
returning `{ output = "..." }` replaces what the model gets, which the next handler sees;
a project handler's return is ignored), `permission.request`,
`permission.resolved`,
`composer.submit` (vetoable), `ui.resize`, `ui.theme`,
`memory.written`, `config.reloaded` (after /reload or a config file
change), `reflector.start`, `reflector.done`, `job.exit`, `lsp.request` (server, path, kind
`diagnostics` after a write or `tool`), and your own `user.*` names. Overlays have no events:
a plugin watches its own overlay through its `on_close` callback. A vetoable handler vetoes
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
