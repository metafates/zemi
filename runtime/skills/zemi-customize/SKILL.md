---
name: zemi-customize
description: Load before adding to zemi or restyling it - plugins, workflows, subagents, prompt commands, profiles, permission modes, keymaps, themes, widgets, layout.
---

# Customizing zemi

Settings, keymaps, profiles, permission modes, colors, layout and changes to the shipped widgets go in the user's init.lua. A feature with logic of its own (a tool, a new widget, a command and the handlers behind them) is a local plugin (Plugins, below), which the user can share, update and turn off on its own. Workflows, agents and prompt commands are files in the user config directory (every project) or in `<repo>/.agents/` (shared through git, loaded once the user trusts the project); ask which when unsure. Workflows and agents are read when used, Lua and commands reload when their file changes. Ask before you edit the user's init.lua and say which lines you will add or change; a plugin never needs such an edit to load and work.

## Workflows (`workflows/<name>.lua`)

A Lua script that coordinates subagents, run by `/workflow <name> [args]` or by the `workflow` tool's `name`. Its leading `--` lines are its description. It runs in its own sandbox with only `agent`, `parallel`, `phase`, `log` (or `print`), `json`, `args` and Lua's string, table and math.

```lua
-- Summarize this branch for a PR description.
-- args: the base branch (default main)
local base = type(args) == "string" and args ~= "" and args or "main"
phase("collect")
local diff, err = agent("Run `git log --oneline " .. base .. "..HEAD` and `git diff " .. base .. "...HEAD --stat`, then summarize each change in one line with its files.")
if not diff then return "failed: " .. err end
phase("write")
return agent("Write a PR description (motivation, changes, how to test) from:\n" .. diff, { agent = "explore" })
```

- `agent(prompt, { agent, description, model })` returns the report or `nil, err`; the agent defaults to `general`. The prompt must stand alone: agents do not see the conversation.
- `parallel({ fn, ... })` returns `results, errs`; 5 agents run at once, 100 at most per run.
- `phase(name)` labels progress, `log(...)` adds a line to the result, `json.decode` tolerates prose around the JSON.
- `agent` and `parallel` cannot run inside `pcall`, `table.sort` or `string.gsub` callbacks.

## Subagents (`agents/<name>.md`)

```markdown
---
description: Reviews Go code for concurrency bugs; use for any change touching goroutines
tools: read, grep, glob
model: anthropic/claude-haiku-4-5
---
You review Go code for data races, deadlocks and leaked goroutines. Cite file:line.
```

`description` is required and is what the model reads when it picks an agent, so say when to use it. `name` defaults to the file name (lowercase letters, digits, `-`). `model` is a role (`chat`, `agents`, `titles`, `compact`, `memory`: the model that role runs on) or a `provider/model`; omitted or `inherit`, `models.agents`. `tools` is an allowlist, or `-name` entries that remove from the full set, so later tools still arrive; omitted, everything but `agent`, `workflow`, `todos`, `question`, `plan` and `lua`. An unknown tool name fails the agent. The builtin agents are `explore`, `general`, `web` and `plan`; a user file with one of those names replaces it.

## Profiles (`init.lua`)

A profile sets the main conversation's model, prompt and tools; `/profile <name>`, ctrl+t, `--profile` or `zemi.config.set("agent.profile", name)` picks one. `tools` works as in an agent file:

```lua
-- no workflows, Lua tool, background jobs or LSP queries: fewer schemas on every request
zemi.agent.profile.add("lean", { tools = { "-workflow", "-lua", "-job_start", "-lsp" } })
```

Also `model`, `temperature`, `system` (replaces the base prompt; zemi's own sections stay), `context = { files = { ... } }` (files pinned into the prompt) and `read_only = true` (a planning agent like the shipped `planner`). Without the file tools there is no read-before-write check, no rewind checkpoint and no diagnostics after an edit; without `skill` no skill loads.

## Prompt commands (`commands/<name>.md`)

```markdown
---
description: Write a failing test for a bug
argument-hint: <file> <symptom>
---
Read $1 and add a failing test showing: $ARGUMENTS. Do not fix the bug.
```

`/<name> args` sends the body with `$ARGUMENTS` (everything after the name) and `$1`..`$9` (single words) filled in. The body goes behind the typed line, so an `@file` in it attaches nothing; the model reads the file itself.

## Permission modes (`init.lua`, user config only)

`zemi.permission.mode.add(name, { base = "auto" or "yolo", description, prompt, rules, decide })` adds a mode to `/mode` or replaces one of that name, `zemi.permission.mode.del(name)` takes one out, and `permissions.mode` is the start mode. A mode acts like its base; `rules` (`{ "tool.bash", "go test*", "allow" }`) answer what would otherwise need the user's approval, `prompt` joins the system prompt while it is on, and `decide(req)` answers a request the rules left open. `decide` returns `"allow"` or `"deny"`; anything else (nil, an error, its 60 s ceiling) sends the request to the user even where the base would have allowed it. A `wait` in it frees the interpreter. Neither `rules` nor `decide` can allow `sandbox: false`, `network: true`, a request an `ask` rule covers, or a write to `.git`, the Lua in `.agents` or zemi's own files.

```lua
-- every request reaches the user
zemi.permission.mode.add("ask", { base = "auto", description = "I answer everything", decide = function() end })

-- a local model approves commands; zemi.http needs the user's allow rule, localhost included
zemi.permission.rule.add({ subject = "lua.http.post", pattern = "http://localhost:11434/*",
  decision = "allow", match = "http://localhost:11434/api/generate" })
zemi.permission.mode.add("judged", {
  base = "auto", description = "a local model approves commands",
  decide = function(req)
    if req.subject ~= "tool.bash" then return end
    local res = zemi.http.request({ method = "POST", url = "http://localhost:11434/api/generate",
      body = zemi.json.encode({ model = "llama3", stream = false, prompt = "Reply allow or deny. Command: " .. req.arg }) }):wait()
    local out = res and zemi.json.decode(res.body)
    if out and (out.response or ""):match("^allow") then return "allow" end
  end,
})
```

## Keymaps and commands

```lua
zemi.keymap.set("<leader>r", "review", { desc = "review changes" })   -- a command name or a host action
zemi.keymap.set("ctrl+t", function() zemi.agent.send("run the tests") end, { desc = "run tests" })
zemi.keymap.del("ctrl+y")                                               -- shipped keys unbind too
zemi.command.add("standup", function(a) zemi.agent.send("summarize yesterday's commits " .. a.args) end,
  { desc = "draft a standup", nargs = "*" })
```

A key is a non-printable key or a sequence that starts with one (`"ctrl+x s"`; `<leader>` is `keymap.leader`, ctrl+x). Binding a taken key rebinds it: ctrl+t above replaces the shipped next profile key. The shipped keys are in `keymaps.lua`. Host actions: `zemi.ui.help`, `zemi.model.pick`, `zemi.ui.reasoning.toggle`, `zemi.ui.scroll.up`, `zemi.ui.scroll.down`, `zemi.ui.scroll.top`, `zemi.ui.scroll.bottom`, `zemi.ui.scroll.line_up` and `zemi.ui.scroll.line_down` (each takes a node id after a space), `zemi.ui.focus.set <id>`, `zemi.ui.focus.next`, `zemi.ui.focus.prev`, `zemi.permission.mode.next`, `zemi.permission.mode.prev`, `zemi.agent.profile.next`, `zemi.agent.profile.prev`, and the `zemi.composer.*` ones zemi-lua lists.

## UI

Everything on screen but the composer and the Go-drawn prompts is a Lua widget. The shipped UI is readable source in the tui directory the system prompt names: `theme.lua`, `layout.lua`, `keymaps.lua`, and the widgets in `statusline.lua`, `working.lua`, `chat.lua` and `splash.lua`. Read the one you change. A widget registered with a shipped id replaces it; a new id adds one.

Shipped ids: `splash` and `chat` (region `chat`); `working` (the composer frame's top rule: the mascot and the turn status); in `statusline.left` `statusline.mode`, `statusline.quit`, `statusline.cwd`, `statusline.agents`, `statusline.view`, `statusline.learn`, `statusline.keys`, `statusline.scroll`; in `statusline.right` `statusline.profile`, `statusline.model`, `statusline.context`, `statusline.percent`, `statusline.cost`. zemi draws a separator between the statusline widgets that draw, so each returns its text alone. Each side draws from its outer edge by priority and a short row drops the highest first, so a lasting widget takes a low priority and one that comes and goes a high one, where it moves nothing.

Rules for `render(ctx)`:

- It returns lines of segments `{ text, hl }`, `{}` for none. A segment may add `click = "id"` (the widget's `on_click(ev)` gets it as `ev.id`), `effect = "shimmer"`, `copy = false` (decoration a selection skips) and `soft_wrap = true` (the line continues on the next row).
- It is pure and fast: only read-only calls (`config.get`, `state.get`, `ui.width`, `session.messages`, ...) work in it, and not `zemi.fs` or a handle's `wait`; anything else raises, and it has about 10 ms. Slow work runs in a `zemi.job` or `zemi.timer` callback that sets a local the render reads; job, http and one-shot timer callbacks redraw when they return.
- A widget redraws only when something else does. One that changes on its own calls `zemi.ui.redraw` from `zemi.timer.every`.
- Returning the same line table again is free; never change one after returning it. With more work than fits, return what is ready and `true` as a second result, and zemi renders it again next frame (the chat fills a long session this way, 30 ms a render at most).
- Colors are theme groups: `Accent`, `Dim`, `Rule`, `Title`, `Selected`, `Button`, `Danger`, `Ok`, and the rest in `theme.lua`.
- A replacement statusline widget keeps its click with `on_click = function(ev) zemi.command.run(ev.id) end`. A replacement chat keeps reasoning toggles (`zemi.ui.reasoning.toggle()`), agent rows (`zemi.command.run("agents", "view " .. row.run)`) and tool output folding (keyed by the row's `tool:<idx>` click id).

```lua
-- a line under the default splash's tagline
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    if ctx.transcript_count > 0 or ctx.agent_active then return {} end
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[#row].text == "@zemi" then
        local indent = 0
        for j = 2, #row - 1 do indent = indent + zemi.ui.width(row[j].text) end
        table.insert(rows, i + 2, { row[1], { text = string.rep(" ", indent), copy = false }, { text = "hack the planet", hl = "Title" } })
        break
      end
    end
    return rows
  end,
})

-- the git branch: a job fills a local, the widget reads it
zemi.permission.rule.add({ subject = "lua.job.start", pattern = "git rev-parse *", decision = "allow",
  match = "git rev-parse --abbrev-ref HEAD" })
local branch = ""
zemi.ui.widget.add({
  id = "statusline.branch", region = "statusline.right", priority = 50,
  render = function() return { { { text = branch, hl = "Dim" } } } end,
})
local function poll()
  zemi.job.start({ cmd = "git", args = { "rev-parse", "--abbrev-ref", "HEAD" },
    on_exit = function(res) branch = res.stdout:gsub("%s+$", "") end })
end
poll()
zemi.timer.every(30000, poll)
```

Keep such state in a local, not `zemi.state`: that store is shared by every project, so a branch kept there shows in the wrong repository.

Colors: `zemi.ui.hl.set("Accent", { fg = "#c4a7e7" })` overrides one group, and groups that link to it follow. `zemi.ui.theme.add(name, { palette, hl })` defines a whole theme and a group it leaves out has no color, so copy the builder in `theme.lua` and change its palette. The shipped theme is `whorl` (`whorl-light` on a light terminal). A `<name>-light` theme beside it is used on a light terminal. `zemi.ui.theme.set(name)` or `ui.theme` picks one.

Layout: `zemi.ui.layout.set({ root, floats })` in init.lua replaces `layout.lua` whole (the types file has every field). A `box` (`dir = "row"` or `"col"`, `sep`) holds `region`s, the one `composer` (with `frame = true` its square frame's `top`, `left` and `right` rules hold regions: the shipped layout keeps the mascot and the statusline there), a statusline `bar` and `spacer`s. Each is sized in cells, `"N%"`, `"fill"` (with `weight`) or `"content"`, with `min`, `max`, `hidden` and `collapse`. `stage = true` marks where prompts, overlays and toasts go; `floats` draw a region over the tree.

```lua
-- statusline on top, a todo sidebar left of the chat
zemi.ui.widget.add({
  id = "user.todos", region = "sidebar",
  render = function(ctx)
    local rows = {}
    for _, td in ipairs(ctx.todos) do rows[#rows + 1] = { { text = td.content, hl = "Dim" } } end
    return #rows > 0 and rows or { { { text = "no todos" } } }
  end,
})
local layout = { root = { kind = "box", dir = "col", children = {
  { kind = "bar", left = "statusline.left", right = "statusline.right" },
  { kind = "box", dir = "row", sep = true, children = {
    { kind = "region", id = "sidebar", region = "sidebar", size = 30, min = 20, collapse = true, scroll = "top" },
    { kind = "box", dir = "col", stage = true, min = 40, children = {
      { kind = "region", region = "header" },
      { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
      { kind = "composer", frame = true, top = "working" },
    } },
  } },
} } }
zemi.ui.layout.set(layout)
zemi.keymap.set("<leader>b", function()
  local side = layout.root.children[2].children[1]
  side.hidden = not side.hidden
  zemi.ui.layout.set(layout)
end, { desc = "toggle the sidebar" })
zemi.keymap.set("alt+j", "zemi.ui.scroll.down sidebar", { desc = "scroll the sidebar" })
```

A region with an id and `focusable = true` can take the keys: `zemi.ui.focus.set(id)`, the `zemi.ui.focus.set <id>` host action or a click gives them to it, `zemi.ui.focus.next` and `zemi.ui.focus.prev` cycle, and esc hands them back to the composer. Keymaps with `{ focus = id }` work only there and may be plain letters; `ctx.focus` names the node that has the keys.

## Plugins (`<data>/plugins/local/<name>/`)

A plugin is a directory shaped like the config dir, in the local plugins directory the system prompt names. At startup zemi runs every enabled plugin's `init.lua`, sorted by name, before the user's init.lua:

```
<name>/                  the plugin's name: lowercase letters, digits, "_", "-", starting with a letter or digit
  init.lua               runs as the `plugin` origin; line 1 "-- <what it does>" is its /plugins description
  lua/<name>/init.lua    require("<name>") from the plugin and the user's init.lua
  skills/  agents/  commands/  workflows/    as in the config dir; the user's and zemi's own win a
                         name, and a skill's zemi-skill.lua does not run
```

There is no manifest: the version is the checked-out `vX.Y.Z` tag, the source the `origin` remote. `installed/<name>/` holds the clones the user manages with `/plugins` (or `zemi plugins` in a shell: install from a git URL, check and update, turn on and off, delete). You never write there; to change one, the user deletes it and you clone it into `local/`. A local plugin named like an installed one, or a symlink in `local/`, does not load.

New plugin (the user asked for one, or `/plugins new` sent you here):

1. Load zemi-lua. Unless the request already says, ask with the `question` tool what the plugin should do and where it shows up: a command, a key, a statusline widget, a tool the model calls, an event handler or a theme. Ask whether anything that runs on its own (a timer, an event handler that acts, a network call, a message to the agent) should be on from the start.
2. Before writing code, check whether zemi already does it. With the Lua tool, read `zemi.config.schema()`, `zemi.command.list()`, `zemi.keymap.list()`, `zemi.ui.widget.list()` and `zemi.tool.list()`; also read the shipped UI source and the plugins directory. If a setting, a shipped command, a prompt command (`commands/<name>.md`) or a workflow does the job, say which, offer that instead, and write a plugin only if the user still wants one.
3. Pick a name nothing registers yet and say in one message what you will write: `local/<name>/init.lua`, `lua/<name>/init.lua` and a `README.md` naming its commands, keys, `setup` options and every rule it needs.
4. Write it to work with no line in the user's init.lua (below). Then run `zemi.config.reload()`, check `zemi.errors.list()` and the plugin's `/plugins` row, and try what it registered (`zemi.command.run`, `zemi.ui.screenshot()`).
5. Tell the user what it registered, that `/plugins` turns it off, and which README lines they may add to init.lua for more. Do not add them yourself.

`init.lua` registers what the plugin is for, with defaults, so it works the moment it loads with no line in the user's init.lua: its commands, its widgets (rendering `{}` until they have something to say), its tool, and keys only where `zemi.keymap.get(key)` is nil. What the user did not ask for stays off until `setup(opts)` turns it on: timers and polling beyond what its own widgets need, event handlers that act (sending to the agent, vetoing a submit or a tool call), network calls, a key someone else holds. Keep `setup` safe to call again, and start gated calls on `app.ready`: the user's allow rules are added after the plugin's init.lua runs.

`init.lua`:

```lua
-- the git branch in the statusline
require("branch")
```

`lua/branch/init.lua`:

```lua
local M = { opts = { interval = 30000 } }
local branch = ""

function M.setup(opts)
  for k, v in pairs(opts or {}) do M.opts[k] = v end
end

local missing -- the rule line while zemi.job is not allowed yet

local function poll()
  local ok, err = pcall(zemi.job.start, { cmd = "git", args = { "rev-parse", "--abbrev-ref", "HEAD" },
    on_exit = function(res) branch = res.code == 0 and res.stdout:gsub("%s+$", "") or "" end })
  missing = not ok and err or nil
end

zemi.health.add("branch", function()
  return missing and { level = "warn", detail = missing } or "reading the branch"
end)

zemi.ui.widget.add({ id = "branch", region = "statusline.right", priority = 50,
  render = function() return branch == "" and {} or { { { text = branch, hl = "Dim" } } } end })

zemi.event.on("app.ready", function()
  poll()
  zemi.timer.every(M.opts.interval, poll)
end)

return M
```

The README lists what the user may add to their init.lua: the rule a gated call needs and the `setup` options. You never add them yourself unless the user asks:

```lua
zemi.permission.rule.add({ subject = "lua.job.start", pattern = "git rev-parse *", decision = "allow",
  match = "git rev-parse --abbrev-ref HEAD", principal = "plugin:branch" })
require("branch").setup({ interval = 10000 })
```

Identity: a plugin's code runs as the plugin wherever it is called from, its `setup()` in the user's init.lua included. A command, keymap, widget, theme, tool, completion source or health row another plugin registered first (by name) is refused, with both named in `/errors`, and a user or builtin one always wins, so prefix ids with the plugin name and bind only keys nothing else uses. Its `zemi.state` keys are its own (stored under `plugin:<name>:`) and go when it is deleted or installed again. Only it answers `require("<name>...")`, and another plugin gets its modules read-only.

Permissions: a job, an HTTP request, a `zemi.fs` write, a model call (`zemi.model.ask`) or a message to the agent (`zemi.agent.send`) raises until the user's init.lua allows it; the error carries the rule line to paste, with `principal = "plugin:<name>"` so it covers that plugin alone. A plugin tool asks the user on every call until the user allows `tool.<name>`. List every rule the plugin needs in its README. A missing rule is a state the plugin reports, not a crash: call the gated API under `pcall`, draw nothing while it fails, and put the rule line in a `zemi.health.add` row, so the plugin loads clean and `/health` says what to paste. Show the user that line and ask before adding it to their init.lua. Prefer a design that needs no rule: a tool the model calls asks the user itself, and a widget can read the render `ctx` instead of running a job. A plugin cannot edit the composer, change settings, the theme, the layout, the model or the mode, add allow rules, or read secret-shaped env vars: a key comes through `setup({ key = zemi.env.get("NAME") })` in the user's init.lua.

What a plugin cannot do at all: register providers, MCP servers or language servers (they run unconfined or hold keys, so the README gives the user the `zemi.provider.add` or `zemi.mcp.add` line to paste), rewrite the conversation or the provider request, draw its own transcript rows or a panel, change the tool set per turn (a profile does it once), or replace a builtin tool (`zemi.tool.override` adds `pre` and `post` around one). Each would break the prompt cache, the conversation or the UI rules below.

Style, so a plugin looks like zemi: colors are theme groups (`Accent`, `Dim`, `Ok`, `Warn`, `Danger`, ...), never hex, so the user's theme applies. No state change adds or removes a row: status goes in a statusline widget that renders `{}` when there is nothing to say (with a high priority, since it comes and goes) or in a toast (`zemi.notify`), whose title says what happened like zemi's own: "Plugin installed" with the name in the body, "Could not reach the server" with the cause in the body. A tool's call row is zemi's own, labeled by a string `about` parameter, with `run:progress(text)` for the running line. Slow work goes in a `wait` inside a task or a job's `on_exit`, never in a render (zemi-lua has the API).

Developing one:

- Reads under `local/` need no approval; every write there asks the user, with no always answer and no mode but yolo settling it, since plugin code runs in every project. Say in one message what you will write.
- zemi reloads within a second of an edit once no turn runs; `zemi.config.reload()` from the Lua tool applies it at once. A load error is in `zemi.errors.list()` (`plugin <name>: ...`), and the plugin's `/plugins` row reads `failed` with the error in its detail.
- A sandboxed command cannot see the plugins directory, so running its tests or git there needs `sandbox: false`, which asks each time outside yolo.
- Moving a feature out of the config dir is the one plugin task that edits init.lua: say which lines leave it and which stay, and wait for the user's yes. Then put its registrations in `local/<name>/init.lua` and its modules in `lua/<name>/`, leave `require("<name>").setup(...)` in the user's init.lua, and keep there what only user config may do (replacing a shipped widget, `hl.set`, layout, settings). Its allow rules keep working (a rule with no `principal` covers every caller); add the principal to narrow them. Its old `zemi.state` keys stay behind.

Publishing: `git init`, commit, push to a host the user picks, then tag `v0.1.0` and push the tag. An install checks out the newest `vX.Y.Z` tag and an update stays in its major version (one installed from a branch moves to the first tag ahead of it), so a breaking change bumps the major. Users install with `/plugins install <git url>` or `zemi plugins install <git url>`.

## After writing

Try a change live with the Lua tool first (it needs the user's approval once and lasts until the next reload), then write it where it belongs: the plugin's own files, or init.lua only for a config change the user asked for. Check with the Lua tool: `zemi.health.list()` and `zemi.errors.list()` hold what `/health` and `/errors` show, `zemi.ui.screenshot()` the screen and `zemi.ui.layout.rects()` where each node landed. Then tell the user the path and how to undo it.
