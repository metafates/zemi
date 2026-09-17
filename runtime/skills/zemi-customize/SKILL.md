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

- `agent(prompt, { agent = "explore" | "general" | custom, description })` returns the report
  or `nil, err`. Prompts must be self-contained; agents do not see the conversation.
- `parallel({ fn, ... })` returns `results, errs`; up to 5 agents run at once, 100 per run.
- Do not call `agent` or `parallel` inside `pcall`, `table.sort` or `string.gsub` callbacks.
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
`-`); `model: provider/model` pins a configured model, `model: inherit` or no model uses
`agent.subagent_model` when set and the session model otherwise. The `agent` tool's `model`
field and a workflow's `agent(prompt, { model = ... })` override it per run. `tools` limits what it may use
(omit for everything but agent, workflow, todos, question, plan, open_in_editor and Lua). The model sees the description
when choosing an agent, so say when to use it. Read at use, no reload needed. `/agents`
lists them. Builtin `explore` has `read, glob, grep, lsp, fetch, web_search, memory_search,
skill`; builtin `web` has `web_search, fetch, read, grep, glob` and runs on `agent.small_model`
when set; builtin `general` has everything a subagent may have. The model's `agent` tool also
takes `background: true`: the report arrives as a later message, and a call with `send_to`
set to the run's id sends its `prompt` to the running agent at its next step. `/agents` also lists the
session's runs (running, background, workflow ones): enter, or a click on an agent's row in
the chat, shows that run's transcript in the chat, a message submitted there goes to that
agent while it runs, and esc goes back. Background jobs
(`job_start`, bash past its timeout) are listed there too, with a Stop action.

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

## UI (`init.lua`, or modules under `lua/`)

Everything on screen except the composer and the Go-drawn prompts is a Lua widget.
Registering a widget with a builtin id replaces it; a new id adds one. Builtin ids:
`splash`, `chat`, `working` (the activity row above the composer), `statusline.quit`,
`statusline.mode`, `statusline.cwd`, `statusline.reflect`, `statusline.agents`, `statusline.view`,
`statusline.learn`, `statusline.keys`, `statusline.scroll`, `statusline.info`. Regions: `header`,
`chat`, `working`, `statusline.left`, `statusline.right`, or any name a layout draws. zemi reloads config on its own when these files
change; `/reload` forces it.

Where things go is a layout tree, `defaultui/layout.lua` by default. `zemi.ui.layout.set`
in `init.lua` replaces it whole (the types file has every field): boxes (`dir = "row"` or
`"col"`, `sep`) hold a `region`, the one `composer`, a statusline `bar`, window `slot`s
and `spacer`s, each sized in cells, `"N%"`, `"fill"` (with `weight`) or `"content"`, with
`min`, `max`, `hidden` and `collapse`. `stage = true` marks where prompts, overlays and
toasts go. `floats` put a region over the tree. A scrolled region takes keys by id:

```lua
-- <user config>/init.lua: statusline on top, a todo sidebar left of the chat
zemi.ui.widget.register({
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
        { kind = "windows", slot = "split" },
        { kind = "region", region = "working" },
        { kind = "composer" },
      } },
      { kind = "windows", slot = "vsplit" },
    } },
  } },
}
zemi.ui.layout.set(layout)
zemi.keymap.set("<leader>b", function() -- toggle the sidebar
  local side = layout.root.children[2].children[1]
  side.hidden = not side.hidden
  zemi.ui.layout.set(layout)
end, { desc = "toggle the sidebar" })
zemi.keymap.set("ctrl+j", "zemi.ui.scroll.down sidebar", { desc = "scroll the sidebar" })
```

Keep a `windows` slot of each kind, or windows opened by the model float over the stage.
Check the result with `zemi.ui.screenshot()` and `zemi.ui.layout.rects()`.

The shipped UI is Lua too: `types/zemi/defaultui/` under the data directory (the system
prompt names the exact path; rewritten on every start) holds the theme (`theme.lua`), the
widgets (`statusline.lua`, `working.lua`, `chat.lua`, `splash.lua`) and the default keymaps
(`init.lua`). Read it before changing one: copy the widget you want to alter into
`init.lua` under the same id and change what you need, or `require("zemi.defaultui")` to
build on its splash.
Try a change live with the Lua tool first (it asks once and lasts until the next reload), screenshot, then
write it to config.

```lua
-- <user config>/init.lua
-- Add a line to the default splash instead of rewriting it: defaultui returns
-- its splash builder.
local default = require("zemi.defaultui")
zemi.ui.widget.register({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    if #ctx.transcript > 0 or ctx.agent_active then return { { { text = "" } } } end
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[2] and row[2].hl == "Logo6" then -- after the last logo row
        table.insert(rows, i + 1, { row[1], { text = "hack the planet", hl = "Title" } })
        break
      end
    end
    return rows
  end,
})

zemi.ui.widget.register({
  id = "statusline.branch", region = "statusline.right", priority = 1,
  render = function(ctx) return { { { text = zemi.state.get("branch") or "", hl = "Comment" } } } end,
})

-- Change a few colors: override just those groups. theme.register below
-- builds a complete theme, and groups it leaves out lose their colors.
zemi.ui.hl("ModePlan", { fg = "bg", bg = "warn", bold = true })
zemi.ui.hl("ModeAuto", { fg = "bg", bg = "#c4a7e7", bold = true })

-- A whole theme (define every group the UI uses, or copy the default's)
zemi.ui.theme.register("dusk", {
  palette = { bg = "#1e1e2e", fg = "#cdd6f4", muted = "#6c7086", accent = "#f5c2e7", ok = "#a6e3a1", warn = "#f9e2af", err = "#f38ba8", panel = "#313244" },
  hl = { Normal = { fg = "fg" }, Comment = { fg = "muted" }, Title = { fg = "accent", bold = true } },
})
zemi.ui.theme.set("dusk")

zemi.keymap.set("<leader>r", "review", { desc = "review changes" })
```

Rules for widgets: `render(ctx)` returns lines of segments `{ text, hl }` (`{}` for no
lines; `effect = "shimmer"` animates a segment like the working label), must be pure
(no `zemi.*` mutations) and fast (about 10 ms): do slow work in `zemi.job` or
`zemi.timer` callbacks and read the result from `zemi.state`. The theme groups the UI
uses are the `hl` table in `defaultui/theme.lua`. To make a segment clickable give it
`click = "some-id"` and the widget `on_click = function(ev) ... end` (ev.id is that id);
the shipped statusline widgets run `zemi.command.run(ev.id)`, so a replacement statusline
keeps its clicks by doing the same. A replacement chat widget keeps its clicks by calling
`zemi.ui.reasoning.toggle()` for a reasoning line and `zemi.command.run("agents", "view " .. t.run)`
for a tool row with a `run` id.
Full API: the zemi-lua skill.

## After writing

Run a quick check where possible (`/health` for config errors, `/errors` for Lua errors
after `/reload`) and tell the user the path and how to use or undo it.
