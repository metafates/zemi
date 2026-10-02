# Subagents and profiles

## Subagents (`agents/<name>.md`)

A subagent is a model run the main agent starts for a self-contained task. It gets a fresh context: zemi's lines saying it is a subagent whose final message is its whole report, the file's body, the tool rules, the project instructions, the skills index when it has the `skill` tool, the `agent.assemble` sections, the environment (working directory, scratchpad), the memory digest, and the task the main agent writes. It never sees the conversation. The body is the job alone: zemi already says the rest.

```markdown
---
description: Reviews the uncommitted change for bugs, missing tests and broken conventions; use after finishing a change, before calling it done
tools: read, grep, glob, shell, lsp
read_only: true
---
You review the uncommitted change in this repository.

1. Run `git status` and `git diff HEAD`, then read each changed file around the change and its callers.
2. Check that the change does what the task says, errors are handled, each new branch has a test, and nothing else changed.

Report a one-line verdict (ready, or fix first), then each finding as `path:line: problem -> fix`, most severe first, then what you could not check.
```

- The file name is the agent's name: lowercase letters, digits, `-`. A `name` field is ignored. The builtin agents are `explore`, `general`, `web` and `plan`; a user file with one of those names replaces it.
- `description` (required) is how the main agent picks the agent: it reads every description, cut at 200 characters, on every request. Say what it does and when to use it, in one sentence specific enough that no other agent matches. "Use after X" or "use for any Y" gets it used without being named.
- `tools` is an allowlist (`read, grep, glob`), or `-name` entries that remove from the full set (`-fetch, -web_search`), so later tools still arrive; omitted, everything but `agent`, `workflow`, `todos`, `question`, `plan` and `lua`, which no subagent gets. The full set holds `update` too, which sends the caller a line while the run goes on: name it in the allowlist or the agent cannot use it. Names are the lowercase tool names you call yourself (`config.md` lists them; MCP tools are `mcp_<server>_<tool>`). An unknown tool name fails the agent.
- `read_only: true` denies file writes and runs its commands with the project unwritable, whatever its tools and the mode, like `explore`; the agent is told so, and its write and edit still work in the scratchpad. Use it for any agent that should only look, even one that runs commands; a build or a test run usually writes inside the project (build dirs, caches) and fails under it.
- `model` is a role (`chat`, `agents`, `titles`, `compact`, `memory`: the model that role runs on) or a `provider/model`; omitted or `inherit`, `models.agents`.
- The body is the job, in under 40 lines: its one job in a line; the steps, with this project's real commands and paths; what to check; the report's shape; what it must not do.
- `/agents` lists every agent; its first row, New agent, fills `/agents new`, and `/agents open <name>` edits a file agent.

New agent (`/agents new` sends you here, or the user asked for one):

1. Unless the request already says, ask in one `question` call: what the agent does and when the main agent should hand it work; whether it only looks (`read_only`) or changes files; and where it lives: this repository (`.agents/agents/`, shared through git, loaded while the project is trusted) or every project (the user config's `agents/`). Recommend the repository for an agent about this code base and every project otherwise; outside a git repository offer only every project, and in a project the user has not trusted say that a repository agent loads only after `/trust`. Without the `question` tool (`zemi run`) take the recommended answers and say which.
2. The `agent` tool's description lists every agent zemi has. If one already does the job, say which and offer to use or change it instead. Pick a name nothing there uses.
3. Read what the agent will work on (the instruction files, the build and test commands, the directories it will search), so its steps name real commands and paths.
4. Write the file in one write: the fewest tools that do the job, `read_only` unless it must change files or run builds and tests, no `model` unless the user asked for one. A user config file asks for approval, which shows the user the file.
5. Check it loaded with one Lua tool script: `zemi.agent.list()` lists it with its description, and `zemi.errors.list()` has no `agents` entry for it.
6. Tell the user the path and that from their next message "use the <name> agent to ..." runs it, or that the main agent picks it on its own when a task matches its description. Do not run it yourself unless they ask.

## Profiles (`init.lua`)

A profile sets the main conversation's model, prompt and tools; `/profile <name>`, ctrl+t, `--profile` or `zemi.config.set("agent.profile", name)` picks one. `tools` works as in an agent file, except that an allowed name no tool has is only an /errors entry (a `-name` one still fails the send):

```lua
-- no workflows, Lua tool, background jobs or LSP queries: fewer schemas on every request
zemi.agent.profile.add("lean", { tools = { "-workflow", "-lua", "-job_start", "-lsp" } })
```

Also `model`, `temperature`, `system` (replaces the base prompt; zemi's own sections stay), `context = { files = { ... } }` (files pinned into the prompt) and `read_only = true` (a planning agent like the shipped `planner`). Without the file tools there is no read-before-write check, no rewind checkpoint and no diagnostics after an edit; without `skill` no skill loads.
