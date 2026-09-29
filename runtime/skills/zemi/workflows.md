# Workflows

Run a workflow inline: pass the script to the `workflow` tool, whose description is the API. Nothing needs saving first. The script runs in its own sandbox with Lua's string, table and math, `zemi.json` and `zemi.workflow`, nothing else of zemi; a bare `agent` or `print` raises naming the `zemi.workflow` spelling.

- `concurrency` (1-16, default 5) is how many agents run at once; 100 start at most per run. `background` runs it detached, and its result arrives as a message.
- `run` with `action` acts on a run of this session: `status` lists its agents by phase with their ids, `pause` lets running agents finish and starts no more, `resume`, `stop`, `concurrency` with a number changes it while it runs. The agent tool's `send_to` messages one of its agents.
- Every run keeps its script and a journal in the session's scratchpad as `workflow-<id>.lua` and `.jsonl`; the result names the script. To change a run, edit that file (or pass `script` with it) and pass `run` with action `rerun`: agent calls identical to its finished ones (same agent, model, schema and prompt) answer at once from the journal, in order, even when the files they read changed since.
- `agent(prompt)` runs `general`; pass `{ agent = "explore" }` for a read-only look.
- `agent(prompt, { schema = s })` asks for a JSON object matching `s` (`type = "object"` with `properties`, and `required` naming only those) and returns it as a table; a reply that does not match is asked for again twice, then the call fails. Required fields and types are checked, not every keyword (`enum`, bounds).
- `agent` and `parallel` cannot run inside `pcall`, `table.sort` or `string.gsub` callbacks.
- The user watches and steers runs in `/workflows`: the runs (running, then this session's earlier ones) and the saved workflows; enter on a run shows its agents by phase, where `p` pauses or resumes, `+` and `-` change how many run at once, `x` stops it, `r` reruns an ended one, `o` and `s` open or save its script, and an agent's detail views its transcript, restarts or stops it. `/workflows run <name> [args]` and `/review [ref]` start a saved one without a model call; its result arrives as a report.

## Saved workflows (`workflows/<name>.lua`)

Save one only when the user wants it by name: copy the run's script with the write tool to `workflows/<name>.lua` in the user config directory or the repository's `.agents/` (commands cannot write `.agents`). The file's leading `--` lines are its description, and the tool's `name` runs it with `args`.

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
