# Subagents and profiles

## Subagents (`agents/<name>.md`)

A subagent is a model run the main agent starts for a self-contained task. It gets a fresh context: zemi's lines saying it is a subagent whose final message is its whole report, the file's body, the tool rules, the project instructions, the skills index when it has the `skill` tool, the `agent.assemble` sections, the environment (working directory, scratchpad), the memory digest, and the task the main agent writes. It never sees the conversation. The body is the job alone: zemi already says the rest.

```markdown
---
description: Reviews Go code for concurrency bugs; use for any change touching goroutines
tools: read, grep, glob
model: anthropic/claude-haiku-4-5
---
You review Go code for data races, deadlocks and leaked goroutines. Cite file:line.
```

- The file name is the agent's name: lowercase letters, digits, `-`. A `name` field is ignored. The builtin agents are `explore`, `general`, `web` and `plan`; a user file with one of those names replaces it.
- `description` is required and is what the model reads when it picks an agent, so say when to use it.
- `model` is a role (`chat`, `agents`, `titles`, `compact`, `memory`: the model that role runs on) or a `provider/model`; omitted or `inherit`, `models.agents`.
- `tools` is an allowlist, or `-name` entries that remove from the full set, so later tools still arrive; omitted, everything but `agent`, `workflow`, `todos`, `question`, `plan` and `lua`, which no subagent gets. `config.md` lists the tool names. An unknown tool name fails the agent.

## Profiles (`init.lua`)

A profile sets the main conversation's model, prompt and tools; `/profile <name>`, ctrl+t, `--profile` or `zemi.config.set("agent.profile", name)` picks one. `tools` works as in an agent file:

```lua
-- no workflows, Lua tool, background jobs or LSP queries: fewer schemas on every request
zemi.agent.profile.add("lean", { tools = { "-workflow", "-lua", "-job_start", "-lsp" } })
```

Also `model`, `temperature`, `system` (replaces the base prompt; zemi's own sections stay), `context = { files = { ... } }` (files pinned into the prompt) and `read_only = true` (a planning agent like the shipped `planner`). Without the file tools there is no read-before-write check, no rewind checkpoint and no diagnostics after an edit; without `skill` no skill loads.
