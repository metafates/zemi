---
name: zemi
description: "Load before any task about zemi itself: its settings, models, permissions, keys, UI, Lua, plugins, subagents, commands, skills, workflows, memory or errors, and before parallel work in git worktrees."
---

# zemi

This file is what every zemi task needs. The rest is in files beside it: read only the one your task names, at the skill directory above joined with its name (`<skill directory>/config.md`), since a bare name resolves against the project.

| Task | Read |
| --- | --- |
| settings, providers and models, permission rules and modes, the sandbox, MCP and language servers, trust, the command line, what a slash command or key does | `config.md` |
| Lua in init.lua or the lua tool: the API, origins, events, what may wait | `lua.md` |
| keymaps, widgets, the statusline, themes and colors, layout | `ui.md`, then `lua.md` for API |
| a plugin: new, changed, tested, published | `plugins.md`, then `lua.md` for API |
| a subagent or a profile | `agents.md` |
| a skill or a prompt command | `skills.md` |
| a workflow script | `workflows.md` |
| memories, the reflector, recipes | `memory.md` |
| zemi misbehaves: an error, a silent model, config that does not load | `debug.md` |
| several branches at once in git worktrees | `worktrees.md` |

## Where things go

- Settings, keymaps, profiles, permission modes, colors, layout and changes to the shipped widgets: the user's `init.lua`.
- A feature with logic of its own (a tool, a new widget, a command and its handlers): a local plugin, which the user can share, update and turn off on its own (`plugins.md`).
- Agents, prompt commands, skills and workflows: files in the user config directory (every project) or in the repository's `.agents/` (shared through git, loaded once the user trusts the project). Ask which when unsure.

The system prompt names the exact user config, project config and local plugins directories. Also: `~/.agents/skills/` (skills other agents read too), `<data>/types/zemi.lua` (the whole Lua API with its docs, rewritten every start) and `<state>/` (logs, databases, trust decisions, and per project its `permissions` file and recipes). `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME` and `ZEMI_CACHE_HOME` move the roots (default `~/.config/zemi`, `~/.local/share/zemi`, `~/.local/state/zemi`, `~/.cache/zemi`).

## Rules for every change

- Ask before you edit the user's init.lua, and say which lines you add or change. A plugin, agent, prompt command, skill or workflow never needs an init.lua edit to load.
- Never write an API key into a file: config reads it with `zemi.env.get("NAME")`.
- zemi reloads within a second of a config file change, but only once no turn runs, so not while you work; `zemi.config.reload()` from the Lua tool applies it like `/reload` as soon as the script returns, after one approval.
- The system prompt is pinned per session: the listed skills, agents and workflows, a profile's text and `agent.assemble` sections change after `/new` or `/compact`. A new agent or workflow still loads by name at once, a new skill after `zemi.config.reload()`, and an edited AGENTS.md reaches the model with the next message.
- For the Lua API, grep the types file, never read it whole: `^function zemi\.` lists every member with its parameters, and `function zemi.<namespace>.` finds one namespace, whose doc comments above each hit hold its rules. The shipped Lua UI is readable source in the directory the system prompt names.

## Check a change

Run `zemi.config.reload()` alone, then read in a second Lua tool script, which needs no approval in a trusted project: `zemi.errors.list()` (what `/errors` shows, newest first), `zemi.health.list()` (the `/health` rows), `zemi.ui.screenshot()` (the screen as text) and `zemi.ui.layout.rects()` (where each layout node landed). An init.lua change may be tried live with the Lua tool first (one approval, gone at the next reload). A plugin also gets its tests (`plugins.md`). Then tell the user the path and how to undo it.
