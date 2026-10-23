# Workflows

Run a workflow inline: pass the script to the `workflow` tool, whose description is the API. Nothing needs saving first. The script runs in its own sandbox with Lua's string, table and math, `zemi.json` and `zemi.workflow`, nothing else of zemi; a bare `agent` or `print` raises naming the `zemi.workflow` spelling.

- `concurrency` (1-16, default 5) is how many agents run at once; 100 start at most per run. `background` runs it detached, and its result arrives as a message. A call without it waits for the run, unless the user sends a message while it runs: the run then hands off and its result arrives as a message too. Waiting on one already under way is `run` with action `wait`: it returns when the run ends, the turn is interrupted, or the user types, and the report still arrives as a message rather than in the call. Everything still going (a run, an agent, a background job, a compaction) draws in one pending row above the todos in the chat, so the user can see what is left.
- `run` with `action` acts on a run of this session: `status` lists its agents by phase with their ids, `pause` lets running agents finish and starts no more, `resume`, `stop`, `concurrency` with a number changes it while it runs. `action` `list` alone names this session's runs, newest first, the ones the user started with `/workflows` or `/review` among them, with the id each acts on. The agent tool's `send_to` messages one of its agents; `wait = true` beside `send_to` blocks for that run the same way `wait` does, and `stop = true` kills it instead, its report never arriving.
- Every run keeps its script and a journal in the session's scratchpad as `workflow-<id>.lua` and `.jsonl`; the result names the script. To change a run, edit that file (or pass `script` with it) and pass `run` with action `rerun`: agent calls identical to its finished ones (same agent, model, schema and prompt) answer at once from the journal, in order, even when the files they read changed since.
- `wf.agent(prompt)` runs `general`; pass `{ agent = "explore" }` for a read-only look.
- `wf.agent(prompt, { schema = s })` asks for a JSON object matching `s` (`type = "object"` with `properties`, and `required` naming only those) and returns it as a table; a reply that does not match is asked for again twice, then the call fails. Required fields and types are checked, not every keyword (`enum`, bounds).
- `wf.agent` and `wf.parallel` cannot run inside `pcall`, `table.sort` or `string.gsub` callbacks.
- The user watches and steers runs in `/workflows`: the runs (running, then this session's earlier ones) and the saved workflows, which `new [what it should do]` has you write (see New workflow below); enter on a run shows its agents by phase, where `p` pauses or resumes, `+` and `-` change how many run at once, `x` stops it, `r` reruns an ended one, `o` opens its script and `s` saves one you wrote, and an agent's detail views its transcript, restarts or stops it. `/workflows run <name> [args]` and `/review [ref]` start a saved one without a model call; its result arrives as a report.

## Saved workflows (`workflows/<name>.lua`)

Save one only when the user wants it by name: copy the run's script with the write tool to `workflows/<name>.lua` in the user config directory or the repository's `.agents/`. The file's leading `--` lines are its description, and the tool's `name` runs it with `args`.

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

New workflow (`/workflows new` sends you here, or the user asked for one):

1. Unless the request already says, ask in one `question` call: what the workflow should do, what it needs from the user as `args` and what to do when it is missing, and where it lives: `.agents/workflows/` in this repository or `workflows/` in the user config (`SKILL.md` has the placement rule).
2. The `workflow` tool's description lists every saved workflow, `review`, `research` and `hunt` among them. If one already does the job, say which and offer to run or change it instead. Pick a name nothing there uses.
3. Write it in one write: the leading `--` lines are the description (what it does, and what `args` is), a `wf.phase` per stage, agents that do not depend on each other in one `wf.parallel` (one table of functions), one agent gathering what the others judge, and a `return` naming what went wrong when a step it needs fails. An angle that fails is a `wf.log`, not a lost run: the run ends warn (an amber `!`), not done.
4. Check it loaded with one Lua tool script: `zemi.workflow.list()` lists it with its description, and `zemi.errors.list()` has no `workflows` entry for it.
5. Tell the user the path and that `/workflows run <name> [args]` runs it. Do not run it yourself unless they ask: every agent it starts is a model call on their bill.
