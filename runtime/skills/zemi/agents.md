# Subagents

## Subagents (`agents/<name>.md`)

A subagent is a model run the main agent starts for a self-contained task. It gets a fresh context: zemi's lines saying it is a subagent whose final message is its whole report, the file's body, the tool rules, the project instructions, the skills index when it has the `skill` tool, the `agent.assemble` sections, the environment (working directory, scratchpad), the brain section, and the task the main agent writes. It never sees the conversation (a fork does, `## Forks`). The body is the job alone: zemi already says the rest.

```markdown
---
description: Reviews the uncommitted change for bugs, missing tests and broken conventions; use after finishing a change, before calling it done
tools: read, grep, glob, shell
read_only: true
---
You review the uncommitted change in this repository.

1. Run `git status` and `git diff HEAD`, then read each changed file around the change and its callers.
2. Check that the change does what the task says, errors are handled, each new branch has a test, and nothing else changed.

Report a one-line verdict (ready, or fix first), then each finding as `path:line: problem -> fix`, most severe first, then what you could not check.
```

- The file name is the agent's name: lowercase letters, digits, `-`. A `name` field is ignored. The builtin agents are `explore`, `general` and `plan`; a user file with one of those names replaces it. `fork` is a builtin run, not a definition: `agents/fork.md` is ignored (`## Forks`).
- `description` (required) is how the main agent picks the agent: it reads every description, cut at 200 characters, on every request. Say what it does and when to use it, in one sentence specific enough that no other agent matches. "Use after X" or "use for any Y" gets it used without being named.
- `tools` is an allowlist (`read, grep, glob`), or `-name` entries that remove from the full set (`-glob`), so later tools still arrive; omitted, everything but `agent`, `workflow`, `todos`, `question`, `plan` and `lua`, which no subagent gets. `update` is outside the allowlist and every agent definition has it: it sends the caller a line while the run goes on. Names are the lowercase tool names you call yourself (`config.md` lists them; MCP tools are `mcp_<server>_<tool>`). An unknown tool name fails the agent.
- `read_only: true` denies every action - a write, a command, a Lua tool - whatever its tools, like `explore`; the agent is told so, and its write and edit still work in the scratchpad. Use it for any agent that should only look; a build or a test run usually writes inside the project (build dirs, caches) and is denied under it.
- `model` is a target (`default`, `chat`, `agents`, `titles`, `compact`, `memory`, a name `zemi.model.add` registered, or a `provider/model`); omitted or `inherit`, `models.agents`. An id that resolves to nothing fails the run, not the file: `provider ... is not registered`.
- The body is the job, in under 40 lines: its one job in a line; the steps, with this project's real commands and paths; what to check; the report's shape; what it must not do.
- `/agents` lists every agent; its first row, New agent, fills `/agents new`, and `/agents open <name>` edits a file agent.

New agent (`/agents new` sends you here, or the user asked for one):

1. Unless the request already says, ask in one `question` call: what the agent does and when the main agent should hand it work, whether it only looks (`read_only`) or changes files, and where it lives: `.agents/agents/` in this repository or `agents/` in the user config (`SKILL.md` has the placement rule).
2. The `agent` tool's description lists every agent zemi has. If one already does the job, say which and offer to use or change it instead. Pick a name nothing there uses.
3. Read what the agent will work on (the instruction files, the build and test commands, the directories it will search), so its steps name real commands and paths.
4. Write the file in one write: the fewest tools that do the job, `read_only` unless it must change files or run builds and tests, no `model` unless the user asked for one. A user config file asks for approval, which shows the user the file.
5. Check it loaded with one Lua tool script: `zemi.agent.list()` lists it with its description, and `zemi.errors.list()` has no `agents` entry for it.
6. Tell the user the path and that from their next message "use the <name> agent to ..." runs it, or that the main agent picks it on its own when a task matches its description. Do not run it yourself unless they ask.

## Forks

`agent = "fork"` continues the conversation that called it, in parallel, on the caller's model, prompt and tools, so it shares the caller's prompt cache and needs no briefing: give it the task alone. It is not a definition (there is no `agents/fork.md`). Only its report comes back, like a subagent's; it cannot fork again, and `question`, `plan` and `todos` refuse in it.

