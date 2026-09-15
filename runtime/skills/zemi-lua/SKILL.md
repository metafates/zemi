---
name: zemi-lua
description: Reference for zemi's Lua API (zemi.* namespaces, events, widgets, overlays, tools) and the rules plugin and config code must follow. Load before writing or fixing Lua for zemi - init.lua, plugins, skill hooks.
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
its own. Only `user` may register providers, add allow rules or decide learning
proposals. Lua calls to gated APIs never open a prompt: an "ask" decision raises
`permission ask required` unless an allow rule exists.

## Modules

`require("a.b")` loads `lua/a/b.lua` (or `lua/a/b/init.lua`) from the config dir or a
plugin; project code also searches `.zemi/lua/` first. `require("./x")` and
`require("../x")` resolve next to the calling file and must stay in the same origin's
tree. Each file loads once per runtime (both names share it); `/reload` loads fresh.

## Rules that keep zemi responsive

- Keymap handlers, event handlers and widget `render` must finish in about 10 ms.
- `render` must be pure: no `zemi.*` mutations (they raise during render).
- Long work goes to `zemi.job`, `zemi.http` or `zemi.timer` with callbacks.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err` (`fs.read`,
  `fs.write`, `state.put`, `json.decode`, `session.new`, `session.open`).
- A wedged handler turns the runtime "hot"; `/reload` rebuilds it. Errors show in `/errors`.

## Running Lua from the agent

The `lua` tool runs a chunk in the live runtime as `user` origin, so the agent can read or
change zemi's state with the same API: `print` output and return values (as JSON) come
back. Each run asks (`tool.lua`, script shown) unless a rule or mode allows it. It holds
the interpreter for at most 10s. Keep reusable scripts in the session scratchpad (its path is
in the Environment section; files there are read and written without asking) and pass `path`.
Globals a chunk sets stay in that chunk.

## Namespaces

| Namespace | Members |
| --- | --- |
| `zemi.api` | `version` (API version string, currently "0.12.0") |
| `zemi.config` | `get`, `set`, `watch`, `schema` |
| `zemi.keymap` | `set(key, fn_or_action, {desc})` (again to rebind), `del(key)`, `get(key)`, `list()`; keys may be sequences starting with a non-printable key: `"ctrl+x s"`, `"<leader>gs"` (`keymap.leader`, default ctrl+g) |
| `zemi.command` | `add(name, fn, { desc, nargs, complete })`, `del(name)`, `run(name [, args])`, `list` |
| `zemi.event` | `on(name, fn, { id, coalesce, once })` -> id, `off(id)` -> bool, `emit("user.*", payload)` |
| `zemi.ui` | `widget.register({ id, region, priority, render, wrap, keys })/list`, `theme.register/set/get`, `hl(group, spec)` (override single groups), `overlay.open({ id, title, items, render_item, on_select, on_close })/close`, `select`, `input({ prompt, default }, on_confirm(text or nil))`, `markdown`, `help`, `invalidate()` (redraw) |
| `zemi.notify` | callable `zemi.notify({ title, body })`, plus `info`, `warn`, `error` |
| `zemi.log` | callable, plus `debug`, `info`, `warn`, `error` |
| `zemi.agent` | `send`, `cancel`, `current`, `profile(name, { model, system, temperature, tools, context = { files } })`, `set_default(name)`, `usage`, `list`, `run({ prompt, agent, description }, on_done(text, err))` -> `{ cancel }` (`run` from user config only) |
| `zemi.workflow` | `list`, `run({ script = src } or { name = saved, args = v }, on_done(result, err, log))` -> `{ cancel }` (user config only) |
| `zemi.provider`, `zemi.model` | `register`, `list`, `kind.ANTHROPIC` / `kind.OPENAI` / `kind.OPENAI_COMPAT`; `list`, `set`, `pick` |
| `zemi.session` | `id`, `title`, `set_title`, `messages`, `list({ limit })`, `new()`, `open(id)`, `todos()`, `rewind(id)` -> files restored or nil, err |
| `zemi.composer` | `get()` -> `{ text, cursor }`, `set(text [, cursor])`, `insert(text)`, `submit()` |
| `zemi.json` | `encode(v)`, `decode(s)` -> v or nil, err |
| `zemi.lsp` | `add(name, { command, args, env, filetypes })` (user config only), `list()` -> `{ name, command, status, error }` (status: starting, running, failed, exited) |
| `zemi.mcp` | `add(name, { command, args, env, url, headers, enabled_tools, disabled_tools, timeout })` (user config only), `list()` -> `{ name, status, error, source, tools }` (status: starting, ready, needs sign-in, failed, stopped) |
| `zemi.memory`, `zemi.learn`, `zemi.skills` | see the zemi-learning skill |
| `zemi.permission` | `rule({ subject, pattern, decision, match, not_match, not_match_example, scope })` (any origin may add deny and ask rules; allow and permanent rules need user config), `rules()`, `mode.add(name, spec)`, `mode.remove(name)`, `mode.list()` (modes: user config only; see zemi-config) |
| `zemi.tool` | `register({ name, description, input, execute, timeout_ms, parallel })`, `override(name, { pre = fn })` (not from project or learned code), `list` |
| `zemi.fs` | `read`, `write`, `list`, `stat`, `exists`, `mkdir`, `remove`, `glob` (`**` spans directories), `grep` |
| `zemi.http` | `request({ method, url, headers, body, timeout_ms, on_done })` -> handle with `wait()`; `get(url, { on_done })` |
| `zemi.job` | `start({ cmd, args, env, stdin, on_stdout(lines), on_stderr(lines), on_exit(result) })` -> `{ id, wait(ms), kill(), write(s), close_stdin() }` (`write` needs `stdin = true`); `out(spec)` runs to completion |
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

-- Widget
zemi.ui.widget.register({
  id = "statusline.turns", region = "statusline.right", priority = 5,
  render = function(ctx) return { { { text = #ctx.transcript .. " msgs", hl = "Comment" } } } end,
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

Widget `ctx` carries `width`, `height`, `time`, `version`, `cwd`, `model`, `profile`,
`effort`, `permission_mode`, `permission_mode_base`, `agent_active`, `transcript`,
`agent` (live turn: `active`, `model`, `text`, `reasoning`, `tools`, `steering`,
`thought`, `elapsed`), `todos` (`content`, `status`, `active_form`), `recent` (sessions,
only while the transcript is empty), `usage`, `context`, `show_reasoning`, `pulse`,
`reflecting`, `pending_memory`, `pending_config`, `agents_running`. Transcript rows carry
`role`, `kind` ("", reasoning, tool, compact, error), `text`, `tool`, `status`, `label`,
`detail` (subagent and workflow progress), `diff`, `in`, `out`, `thought`, `elapsed`.
Regions: `header`, `chat`, `statusline.left`, `statusline.right`. Segments are
`{ text = "...", hl = "Group" }`.

## Events

`app.ready`, `session.new`, `session.open`, `session.close`, `agent.start`,
`agent.finish`, `agent.error`, `agent.subagent.start`, `agent.subagent.finish`,
`agent.assemble` (return a string or `{ title, text }` to add a system prompt section;
user, builtin and plugin handlers only), `composer.change`, `tool.denied` (tool, subject, arg, by,
reason), `app.quit` (vetoable), `agent.text_delta`, `agent.reasoning_delta`, `tool.pre`
(vetoable: deny, halt, rewrite), `tool.post`, `permission.request`, `permission.resolved`,
`composer.submit` (vetoable), `ui.resize`, `ui.theme`, `overlay.open`, `overlay.close`,
`memory.written`, `skill.learned`, `config.proposed`, `config.applied`,
`reflector.start`, `reflector.done`, `job.exit`, and your own `user.*` names.
