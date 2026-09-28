# Workflows (`workflows/<name>.lua`)

A Lua script that coordinates subagents, run by `/workflow <name> [args]` or by the `workflow` tool's `name`. Its leading `--` lines are its description. It runs in its own sandbox with only `agent`, `parallel`, `phase`, `log` (or `print`), `json`, `args` and Lua's string, table and math.

```lua
-- Summarize this branch for a PR description.
-- args: the base branch (default main)
local base = type(args) == "string" and args ~= "" and args or "main"
phase("collect")
local diff, err = agent(
	"Run `git log --oneline " .. base .. "..HEAD` and `git diff " .. base .. "...HEAD --stat`, then summarize each change in one line with its files."
)
if not diff then return "failed: " .. err end
phase("write")
return agent("Write a PR description (motivation, changes, how to test) from:\n" .. diff, { agent = "explore" })
```

- `agent(prompt, { agent, description, model })` returns the report or `nil, err`; the agent defaults to `general`. The prompt must stand alone: agents do not see the conversation.
- `parallel({ fn, ... })` returns `results, errs`; 5 agents run at once, 100 at most per run.
- `phase(name)` labels progress, `log(...)` adds a line to the result, `json.decode` tolerates prose around the JSON.
- `agent` and `parallel` cannot run inside `pcall`, `table.sort` or `string.gsub` callbacks.
