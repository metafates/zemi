---
name: zemi-customize
description: Extend or restyle zemi without editing its source - workflows, subagents, prompt commands, splash, statusline, chat, theme, keys. Load before creating any.
---

# Customizing zemi

zemi is changed through config, never by editing its Go or runtime source. Everything
below is a file you write with your file tools; show the user what you wrote and where.

| Scope | Directory | Loads |
| --- | --- | --- |
| every project | the user config directory (the system prompt names it; usually `~/.config/zemi`) | at once for workflows and agents, when the files change for Lua and commands |
| this repository | `<repo>/.zemi/` | only in a project the user trusts: a prompt at the first start there, or `/trust` |

Always use the exact directories from the system prompt: `ZEMI_CONFIG_HOME` can move the
user directory, and writing to a guessed `~/.config/zemi` changes the wrong config.

Use the user's directory for personal habits, `.zemi/` for things the team should share
through git. When unsure, ask which one.

## Workflows (`workflows/<name>.lua`)

A Lua script that coordinates subagents, run with `/workflow <name> [args]` or by the
model through the workflow tool with `name`. The leading `--` lines are its description.
It runs in a sandbox, not the config interpreter: only `agent`, `parallel`, `phase`,
`log`, `json`, `args` and Lua's string, table and math libraries.

```lua
-- Summarize what changed on this branch for a PR description.
-- args: the base branch (default main)
local base = type(args) == "string" and args ~= "" and args or "main"
phase("collect")
local diff, err = agent("Run `git log --oneline " .. base .. "..HEAD` and `git diff " .. base .. "...HEAD --stat`, then summarize each change in one line with its files.", { agent = "general" })
if not diff then return "failed: " .. err end
phase("write")
return agent("Write a PR description (motivation, changes, how to test) from:\n" .. diff, { agent = "explore" })
```

- `agent(prompt, { agent = "explore" | "general" | custom, description, model })` returns the
  report or `nil, err`. Prompts must be self-contained; agents do not see the conversation.
- `parallel({ fn, ... })` returns `results, errs`; up to 5 agents run at once, 100 per run.
- `phase(name)` labels progress, `log(...)` adds a line to the result, `json.encode/decode`
  carry structured data between agents (decode tolerates prose around the JSON).
- Do not call `agent` or `parallel` inside `pcall`, `table.sort` or `string.gsub` callbacks.
- `return agent(...)` as the last statement of a `parallel` branch is a tail call and loses
  the result: assign it, then return the variable.
- Workflow files are read when they run: no reload needed.
- Keep one the user wants again in `~/.config/zemi/workflows/<name>.lua` (every project) or
  `<project>/.zemi/workflows/<name>.lua` (shared through git, run only once the user
  trusts the project). It then runs as `/workflow <name>`, or by name from the workflow tool.

## Subagents (`agents/<name>.md`)

```markdown
---
description: Reviews Go code for concurrency bugs; use for any change touching goroutines
tools: read, grep, glob
model: inherit
---
You review Go code for data races, deadlocks and leaked goroutines. Cite file:line.
```

`description` is required; `name` defaults to the file name (lowercase letters, digits,
`-`); `model:` takes a tier name (`small`, `main`, `smart`) or a configured `provider/model`;
`model: inherit` or no model uses the main tier. The `agent` tool's `model`
field and a workflow's `agent(prompt, { model = ... })` override it per run. `tools` limits what it may use
(omit for everything but agent, workflow, todos, question, plan and lua);
a `-name` entry removes one instead, so `tools: -bash` keeps the rest, later releases
included. A name nothing answers to fails the agent. The model sees the description
when choosing an agent, so say when to use it. Read at use, no reload needed. `/agents`
lists them. Builtin `explore` has `read, glob, grep, lsp, fetch, web_search, skill`; builtin `web` has `web_search, fetch, read, grep, glob` and runs on the small tier; builtin `general` has everything a subagent may have. The model's `agent` tool also
takes `background: true`: the report arrives as a later message, and a call with `send_to`
set to a run's id sends its `prompt` to a running agent at its next step, or runs a finished
one again on its own history and answers. `/agents` also lists the
session's runs (running, background, workflow ones): enter, or a click on an agent's row in
the chat, shows that run's transcript in the chat, a message submitted there goes to that
agent while it runs, and esc goes back. Background jobs
(`job_start`, bash past its timeout) are listed there too, with a Stop action.

## A lean tool set (`/profile <name>`, `init.lua`, or `tools:` in an agent file)

Profiles are defined with `zemi.agent.profile`; a lean one (the default tool set minus
the schemas a coding turn rarely calls) looks like:

```lua
zemi.agent.profile("lean", { tools = { "-workflow", "-lua", "-job_start", "-lsp" } })
-- or an allowlist, strong models only: tools = { "bash", "skill" }
```

The default set minus `workflow`, `lua`, `job_start` and `lsp` is about 40% of the builtin
schema text (1.8k tokens a request). It gives up workflow scripts, reading or changing
zemi's live state, starting a server or watcher in the background (bash still hands a
command past its timeout to `job_read` and `job_kill`), and language server queries;
diagnostics after an edit stay. Apply it with `/profile lean`, `--profile lean`, or
`zemi.config.set("agent.profile", "lean")` in `init.lua`.

The prompt drops what names a missing tool (the skills index without `skill`, the
delegation section without `agent`, a read-only agent replies in text without `plan`). A bash-only
set pays with a strong model that is good at the shell, on shell-centric work; a weaker
model edits less accurately through sed and heredocs than through `edit`. Without the file
tools there is no check that a file was read and unchanged before a write, no rewind
checkpoint, no diagnostics after an edit, and a package's own AGENTS.md no longer rides the
first read. Without `skill` no skill loads. bash output is cut at 10000 characters, `read`
returns 50000.

## Prompt commands (`commands/<name>.md`)

```markdown
---
description: Write a failing test for a bug
argument-hint: <file> <symptom>
---
Read $1 and add a failing test showing: $ARGUMENTS. Do not fix the bug.
```

`$ARGUMENTS` is everything after the name, `$1`..`$9` single words. Commands load when
the file is saved (zemi reloads config on change).

## Permission modes (`init.lua`, user config only)

`zemi.permission.mode.add(name, spec)` adds a mode to `/mode`: `base` (auto, yolo), `description`, `prompt`, static `rules`, and `decide`, which answers one ask
that the rules left open. `decide` runs off the UI with a 60s ceiling, so it may ask a model
of its own; it holds the interpreter while it runs, so the rest of zemi waits on it.

```lua
zemi.permission.mode.add("judged", {
  base = "auto", description = "a model of my own answers command asks",
  decide = function(req)
    if req.subject ~= "tool.bash" then return nil end -- everything else: ask me
    local res = zemi.http.request({
      method = "POST", url = "http://localhost:11434/api/generate",
      body = zemi.json.encode({ model = "llama3", stream = false,
        prompt = "Reply allow or deny. Command: " .. req.arg }),
    }):wait()
    if res and (zemi.json.decode(res.body).response or ""):match("^allow") then return "allow" end
  end,
})
```

Returning anything but `"allow"` or `"deny"` (nil, an error, a timeout) leaves the request
to the user whatever the base settles, and `"allow"` never covers `sandbox: false`,
`network: true`, an ask a rule asked for or a write to `.git`, `.zemi` or zemi's own files,
which always ask, as they do for the mode's `rules`. zemi ships no such mode: `auto` settles reads, project
edits and build, test and read-only commands by fixed rules and sends everything else to
the user. A `decide` that answers nothing is how a user gets every ask:

```lua
zemi.permission.mode.add("ask", {
  base = "auto", description = "every ask reaches me", decide = function() end,
})
```

## UI (`init.lua`, or modules under `lua/`)

Everything on screen except the composer and the Go-drawn prompts is a Lua widget.
Registering a widget with a builtin id replaces it; a new id adds one. Builtin ids:
`splash`, `chat`, `working` (the composer frame's whole top rule, `ctx.width` cells: the mascot in its left corner, its eyes showing the turn state, and the turn status at its right end), `statusline.quit`,
`statusline.mode`, `statusline.cwd`, `statusline.reflect`, `statusline.agents`, `statusline.view`,
`statusline.learn`, `statusline.keys`, `statusline.scroll`, and on the right `statusline.profile`,
`statusline.model`, `statusline.context`, `statusline.percent`, `statusline.cost`. zemi puts a dim
` · ` between the statusline widgets that draw, so a widget returns its text alone. Regions: `header`,
`chat`, `working`, `statusline.left`, `statusline.right`, or any name a layout draws. zemi reloads config on its own when these files
change; `/reload` forces it.

Where things go is a layout tree, `defaultui/layout.lua` by default. `zemi.ui.layout.set`
in `init.lua` replaces it whole (the types file has every field): boxes (`dir = "row"` or
`"col"`, `sep`) hold a `region`, the one `composer` (with `frame = true` it draws in a
square frame whose `top`, `left` and `right` rules hold regions, which is where the shipped
layout puts the mascot and the statusline), a statusline `bar`
and `spacer`s, each sized in cells, `"N%"`, `"fill"` (with `weight`) or `"content"`, with
`min`, `max`, `hidden` and `collapse`. `stage = true` marks where prompts, overlays and
toasts go. `floats` put a region over the tree. A scrolled region takes keys by id:

```lua
-- <user config>/init.lua: statusline on top, a todo sidebar left of the chat
zemi.ui.widget.add({
  id = "user.todos", region = "sidebar",
  render = function(ctx)
    local rows = {}
    for _, td in ipairs(ctx.todos) do rows[#rows + 1] = { { text = td.content, hl = "Comment" } } end
    return #rows > 0 and rows or { { { text = "no todos" } } }
  end,
})
local layout = {
  root = { kind = "box", dir = "col", children = {
    { kind = "bar", left = "statusline.left", right = "statusline.right" },
    { kind = "box", dir = "row", sep = true, children = {
      { kind = "region", id = "sidebar", region = "sidebar", size = 30, min = 20, collapse = true, scroll = "top" },
      { kind = "box", dir = "col", stage = true, min = 40, children = {
        { kind = "region", region = "header" },
        { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
        { kind = "composer", frame = true, top = "working" },
      } },
    } },
  } },
}
zemi.ui.layout.set(layout)
zemi.keymap.set("<leader>b", function() -- toggle the sidebar
  local side = layout.root.children[2].children[1]
  side.hidden = not side.hidden
  zemi.ui.layout.set(layout)
end, { desc = "toggle the sidebar" })
zemi.keymap.set("alt+j", "zemi.ui.scroll.down sidebar", { desc = "scroll the sidebar" })
```

A region with an id and `focusable = true` can take the keys, which makes a panel you drive
from the keyboard: `zemi.ui.focus.set(id)` (or the `zemi.ui.focus.set <id>` host action, or a
click on it) gives it the keys, `zemi.ui.focus.next`/`zemi.ui.focus.prev` cycle through the
composer and the drawn focusable nodes, and esc hands the keys back to the composer. Only
user config and shipped code may move the focus, as with the layout itself. Keymaps
with `{ focus = id }` work only there and may be plain letters; the render ctx says who has
the keys in `ctx.focus`:

```lua
local sel = 1
zemi.keymap.set("<leader>t", "zemi.ui.focus.set sidebar", { desc = "focus the todo sidebar" })
zemi.keymap.set("j", function() sel = sel + 1 end, { focus = "sidebar" })
zemi.keymap.set("k", function() sel = math.max(1, sel - 1) end, { focus = "sidebar" })
-- and add focusable = true to the sidebar node above; its render marks row `sel`
-- when ctx.focus == "sidebar"
```

Check the result with `zemi.ui.screenshot()` and `zemi.ui.layout.rects()`.

The shipped UI is Lua too: `types/zemi/defaultui/` under the data directory (the system
prompt names the exact path; rewritten on every start) holds the theme (`theme.lua`), the
widgets (`statusline.lua`, `working.lua`, `chat.lua`, `splash.lua`, sharing `text.lua` and
the mascot in `snail.lua`), the default keymaps (`keymaps.lua`) and the layout
(`layout.lua`), each typed with LuaLS annotations. Read it before changing one: copy
the widget you want to alter into `init.lua` under the same id and change what you
need, or `require("zemi.defaultui")` to build on its splash.
Try a change live with the Lua tool first (it asks once and lasts until the next reload), screenshot, then
write it to config.

```lua
-- <user config>/init.lua
-- Add a line to the default splash instead of rewriting it: defaultui returns
-- its splash builder.
local default = require("zemi.defaultui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    if ctx.transcript_count > 0 or ctx.agent_active then return { { { text = "" } } } end
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[#row].text == "@zemi" then -- the wordmark row
        table.insert(rows, i + 1, { row[1], { text = "hack the planet", hl = "Title" } })
        break
      end
    end
    return rows
  end,
})

-- A clock. ctx.time is already "15:04", and a widget only redraws when
-- something else does, so one that changes on its own drives the redraw.
zemi.ui.widget.add({
  id = "statusline.clock", region = "statusline.right", priority = 1,
  render = function(ctx) return { { { text = ctx.time, hl = "Comment" } } } end,
})
zemi.timer.every(30000, zemi.ui.redraw)

-- Slow work belongs in a job whose callback sets a local; the widget only reads
-- it. A local dies with a reload and the timer fills it again; zemi.state is
-- persisted and shared by every project, so a branch kept there shows up in the
-- wrong repository. A job started from config needs its own allow rule.
zemi.permission.rule.add({
  subject = "lua.job.start", pattern = "git rev-parse *", decision = "allow",
  match = "git rev-parse --abbrev-ref HEAD",
})
local branch = ""
zemi.ui.widget.add({
  id = "statusline.branch", region = "statusline.right", priority = 2,
  render = function(ctx) return { { { text = branch, hl = "Comment" } } } end,
})
zemi.timer.every(30000, function()
  zemi.job.start({ cmd = "git", args = { "rev-parse", "--abbrev-ref", "HEAD" },
    on_exit = function(res)
      branch = res.stdout:gsub("%s+$", "")
      zemi.ui.redraw()
    end })
end)

-- Change a few colors: override just those groups. theme.add below
-- builds a complete theme, and groups it leaves out lose their colors.
zemi.ui.hl.set("ModePlan", { fg = "yellow", bold = true })
zemi.ui.hl.set("ModeAuto", { fg = "#c4a7e7", bold = true })

-- A whole theme (define every group the UI uses, or copy the default's).
-- Add a "dusk-light" beside it and a light terminal picks that one.
zemi.ui.theme.add("dusk", {
  palette = { bg = "#1e1e2e", fg = "#cdd6f4", muted = "#6c7086", accent = "#f5c2e7", ok = "#a6e3a1", warn = "#f9e2af", err = "#f38ba8", panel = "#313244" },
  hl = { Normal = { fg = "fg" }, Comment = { fg = "muted" }, Title = { fg = "accent", bold = true } },
})
zemi.ui.theme.set("dusk")

zemi.keymap.set("<leader>r", "review", { desc = "review changes" })
```

Rules for widgets: `render(ctx)` returns lines of segments `{ text, hl }` (`{}` for no
lines; `effect = "shimmer"` animates a segment like the working label; `copy = false`
marks decoration a selection skips, `soft_wrap = true` a line that continues on the next
row), must be pure
(registering, `config.set`, `state.set`, `fs` writes, jobs, http and `agent.send` raise
during render) and fast (about 10 ms): do slow work in `zemi.job` or `zemi.timer`
callbacks and read the result from a local, as the branch widget above does.
A line table it returned is never changed afterwards: zemi converts a line it saw in the
last render only once, so a widget may keep finished lines and return them again for free,
and builds a new table for a line that changed.
A render with more work than that returns what is ready and `true` as a second result;
zemi then renders that widget again next frame. The shipped chat fills a long session in
that way, latest messages first, 30 ms a render, which is the most a widget should take.
A widget redraws only when something else does; one that changes on its own calls
`zemi.ui.redraw` from a `zemi.timer.every` callback. The theme groups the UI
uses are the `hl` table in `defaultui/theme.lua`. To make a segment clickable give it
`click = "some-id"` and the widget `on_click = function(ev) ... end` (ev.id is that id);
the shipped statusline widgets run `zemi.command.run(ev.id)`, so a replacement statusline
keeps its clicks by doing the same. A replacement chat widget keeps its clicks by calling
`zemi.ui.reasoning.toggle()` for a reasoning line, `zemi.command.run("agents", "view " .. t.run)`
for a tool row with a `run` id, and by expanding or folding the output of any other tool
row (the shipped one keeps an `expanded` table keyed by the row's `tool:<idx>` click id).
Full API: the zemi-lua skill.

## After writing

Run a quick check where possible: `zemi.health.list()` and `zemi.errors.list()` through the
Lua tool read the same rows `/health` and `/errors` show, without asking the user. Then tell
the user the path and how to use or undo it. Writing to the config directory asks the
user in every permission mode but yolo, so say what you are about to write before the ask
appears.
