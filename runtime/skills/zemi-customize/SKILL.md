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
| this repository | `<repo>/.zemi/` | after the user trusts the project config: a prompt at start or on `/reload` |

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
`-`); `model: inherit` or no model uses the agent model. `tools` limits what it may use
(omit for everything but agent, workflow, todos, question, plan). The model sees the description
when choosing an agent, so say when to use it. Read at use, no reload needed. `/agents`
lists them. Builtin `explore` has `read, glob, grep, fetch, sourcegraph, memory_search,
skill`; builtin `general` has everything a subagent may have. The model's `agent` tool also
takes `background: true`: the report arrives as a later message. `/agents` also lists the
session's runs (running, background, workflow ones): enter, or a click on an agent's row in
the chat, shows that run's transcript in the chat, and esc goes back.

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
`splash`, `chat`, `statusline.mode`, `statusline.cwd`, `statusline.reflect`,
`statusline.agents`, `statusline.learn`, `statusline.info`. Regions: `header`, `chat`,
`statusline.left`, `statusline.right`. zemi reloads config on its own when these files
change; `/reload` forces it.

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

Rules for widgets: `render(ctx)` returns lines of segments `{ text, hl }`, must be pure
(no `zemi.*` mutations) and fast (about 10 ms): do slow work in `zemi.job` or
`zemi.timer` callbacks and read the result from `zemi.state`. Theme groups used by the
default UI: `Normal`, `Comment`, `Title`, `UserMsg`, `AgentToolOk`, `AgentToolErr`,
`AgentToolRun`, `AgentToolName`, `AgentReason`, `Thinking`, `ThinkingBar`, `ThinkingMark`,
`DiffAdd`, `DiffDel`, `DiffCtx` (each with a `*Num` group for its line numbers), `Logo1`..`Logo6`, `SplashKey`, `StatusUsage`, `StatusUsageWarn`,
`StatusUsageErr`, `Errors`, `Mode*`, `Prompt*`, `ToastInfo`, `ToastWarn`, `ToastErr`,
`ToastTitle`, `Markdown*`.
Full API: the zemi-lua skill.

## After writing

Run a quick check where possible (`/health` for config errors, `/errors` for Lua errors
after `/reload`) and tell the user the path and how to use or undo it.
