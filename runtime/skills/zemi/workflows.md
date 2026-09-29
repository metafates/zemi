# Workflows

A workflow is a Lua script that coordinates subagents. Pass it inline to the `workflow` tool; a saved one lives in `workflows/<name>.lua` (here or in `.agents/`), whose leading `--` lines are its description, and runs by `/workflow <name> [args]` or the tool's `name`. It runs in its own sandbox with Lua's string, table and math, `zemi.json` and `zemi.workflow`, nothing else of zemi.

```lua
-- Summarize this branch for a PR description.
-- args: the base branch (default main)
local wf = zemi.workflow
local base = type(wf.args) == "string" and wf.args ~= "" and wf.args or "main"
wf.phase("collect")
local diff, err = wf.agent(
	"Run `git log --oneline " .. base .. "..HEAD` and `git diff " .. base .. "...HEAD --stat`, then summarize each change in one line with its files."
)
if not diff then return "failed: " .. err end
wf.phase("write")
return wf.agent("Write a PR description (motivation, changes, how to test) from:\n" .. diff, { agent = "explore" })
```

- `agent(prompt, { agent, description, phase, model })` returns the report or `nil, err`; the agent defaults to `general`. The prompt must stand alone: agents do not see the conversation.
- `parallel({ fn, ... })` returns `results, errs`; 5 agents run at once, 100 at most per run.
- `pipeline(items, stage, ...)` runs each item through the stages, `stage(prev, item, i)`, items independent; a stage's `nil` ends that item, its `nil, err` lands in `errs`.
- `phase(name)` groups the agents after it, `log(...)` adds a line the user sees and the result carries, `args` is the run's args.
- `agent` and `parallel` cannot run inside `pcall`, `table.sort` or `string.gsub` callbacks.
