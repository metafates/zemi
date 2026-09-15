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
proposals. Lua calls to gated APIs never open a prompt: an "ask" decision fails with
`nil, "permission ask required"` unless an allow rule exists.

## Rules that keep zemi responsive

- Keymap handlers, event handlers and widget `render` must finish in about 10 ms.
- `render` must be pure: no `zemi.*` mutations (they raise during render).
- Long work goes to `zemi.job`, `zemi.http` or `zemi.timer` with callbacks.
- Misuse raises; a runtime failure of a synchronous call returns `nil, err`.
- A wedged handler turns the runtime "hot"; `/Reload` rebuilds it. Errors show in `/errors`.

## Namespaces

| Namespace | Members |
| --- | --- |
| `zemi.config` | `get`, `set`, `watch`, `schema` |
| `zemi.keymap` | `bind(mode, keys, fn_or_action, opts)`, `del`, `list` |
| `zemi.command` | `add(name, fn, { desc, nargs, complete })`, `list` |
| `zemi.event` | `on(name, fn, { id, coalesce })`, `emit("user.*", payload)` |
| `zemi.ui` | `widget.register/list`, `theme.register/set/get`, `overlay.open/close`, `select`, `markdown`, `help` |
| `zemi.notify` | callable `zemi.notify({ title, body })`, plus `info`, `warn`, `error` |
| `zemi.log` | `debug`, `info`, `warn`, `error` |
| `zemi.agent` | `send`, `cancel`, `current`, `profile`, `set_default`, `usage` |
| `zemi.provider`, `zemi.model` | `register`, `list`; `list`, `set`, `pick` |
| `zemi.session` | `id`, `title`, `set_title`, `messages` |
| `zemi.memory`, `zemi.learn`, `zemi.skills` | see the zemi-learning skill |
| `zemi.permission` | `rule({ subject, pattern, decision, match, not_match })`, `rules` |
| `zemi.tool` | `register({ name, description, input, execute })`, `override`, `list` |
| `zemi.fs`, `zemi.http`, `zemi.job`, `zemi.timer` | `read`, `write`, `glob`, `grep`; `get`; `start`, `out`; `set`, `every` |
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
zemi.event.on("job.exit", function(ev)        -- ev.cmd, ev.code
  zemi.notify({ title = ev.cmd, body = ev.code == 0 and "pass" or "fail" })
end)
zemi.job.start({ cmd = "go", args = { "test", "./..." } })
local t = zemi.timer.every(60000, function() zemi.log.info("tick") end)   -- t:stop()

-- A tool the model can call
zemi.tool.register({
  name = "ticket", description = "look up a ticket title by id",
  input = { type = "object", properties = { id = { type = "string" } }, required = { "id" } },
  execute = function(args) return "ticket " .. args.id end,
})
```

Widget `ctx` carries `width`, `cwd`, `model`, `transcript`, `agent` (live turn: `text`,
`tools`, `steering`), `usage`, `context`, `show_reasoning`, `pulse`, `reflecting`,
`pending_memory`, `pending_config`. Regions: `header`, `chat`, `statusline.left`,
`statusline.right`. Segments are `{ text = "...", hl = "Group" }`.

## Events

`app.ready`, `session.new`, `session.open`, `session.close`, `agent.start`,
`agent.finish`, `agent.error`, `agent.text_delta`, `agent.reasoning_delta`, `tool.pre`
(vetoable: deny, halt, rewrite), `tool.post`, `permission.request`, `permission.resolved`,
`composer.submit` (vetoable), `ui.resize`, `ui.theme`, `overlay.open`, `overlay.close`,
`memory.written`, `skill.learned`, `config.proposed`, `config.applied`,
`reflector.start`, `reflector.done`, `job.exit`, and your own `user.*` names.
