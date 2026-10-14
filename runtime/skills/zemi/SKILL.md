---
name: zemi
description: "Load only to change or extend zemi, the tool you run in (settings, models, permissions, keys, UI, Lua, plugins, subagents, commands, skills, workflows, memory, errors), or before parallel work in git worktrees. Never for work on the user's code."
---

# zemi

This file is what every zemi task needs. The rest is in files beside it: read only the one your task names, at the skill directory above joined with its name (`<skill directory>/config.md`), since a bare name resolves against the project.

| Task | Read |
| --- | --- |
| settings, providers and models, permission modes, MCP and language servers, trust, the command line, what a slash command or key does | `config.md` |
| Lua in init.lua or the lua tool: the API, origins, events, what may wait | `lua.md` |
| keymaps, widgets, the statusline, themes and colors, layout | `ui.md`, then `lua.md` for API |
| a plugin: new, changed, tested, published | `plugins.md` (and `ui.md` for a widget or key), which says when `lua.md` is needed |
| a subagent or a profile | `agents.md` |
| a skill or a prompt command | `skills.md` |
| a workflow: controlling a run, rerunning, saving or making one | `workflows.md` |
| memories, the reflector, recipes | `memory.md` |
| zemi misbehaves: an error, a silent model, config that does not load | `debug.md` |
| several branches at once in git worktrees | `worktrees.md` |

## Where things go

- Settings, keymaps, profiles, permission modes, colors, layout and changes to the shipped widgets: the user's `init.lua`.
- A feature with logic of its own (a tool, a new widget, a command and its handlers): a local plugin, which the user can share, update and turn off on its own (`plugins.md`).
- Agents, prompt commands, skills and saved workflows: files in the user config directory (every project) or in the repository's `.agents/` (shared through git, loaded once the user trusts the project). Writing one, ask where it lives in one `question` call: recommend the repository for one about this code base and every project otherwise; outside a git repository offer only every project, and in a project the user has not trusted say that a repository file loads only after `/trust`. Without the `question` tool (`zemi run`) take the recommended answers and say which.

The prompt names the exact user config, project config and local plugins directories and the types file (the whole Lua API with its docs, rewritten every start). Also: `~/.agents/skills/` (skills other agents read too) and `<state>/` (logs, databases, trust decisions, and per project its recipes). `ZEMI_CONFIG_HOME`, `ZEMI_DATA_HOME`, `ZEMI_STATE_HOME` and `ZEMI_CACHE_HOME` move the roots (default `~/.config/zemi`, `~/.local/share/zemi`, `~/.local/state/zemi`, `~/.cache/zemi`).

## Rules for every change

- Ask before you edit the user's init.lua, and say which lines you add or change. A plugin, agent, prompt command, skill or workflow never needs an init.lua edit to load.
- Never write an API key into a file: config reads it with `zemi.env.get("NAME")`.
- zemi reloads within a second of a config file change once no turn runs, and the Lua tool reloads before a script whenever a config or plugin file changed, so a script always sees what is on disk and its result starts with what failed to load. A reload drops what earlier scripts registered.
- The system prompt is pinned per session: the listed skills, agents and workflows, a profile's text and `agent.assemble` sections change after `/new` or `/compact`. A new agent or workflow still loads by name at once, a new skill after the next reload, and an edited AGENTS.md reaches the model with the next message.
- For the Lua API, grep the types file in the shell, never read it whole: `grep -n '^function zemi\.'` lists every member with its parameters, `grep -n -B20 'function zemi.config.add('` one member with the rules and `---@return` above it, and `grep -n -A20 '@class zemi.WidgetSpec'` (or `@alias zemi.SettingKind`) one type with its fields.

## Check a change

For a config change (a plugin is checked by its tests instead, `plugins.md`): read in one Lua tool script, which reloads the changed config first and needs no approval in a trusted project: `zemi.errors.list()` (what `/errors` shows, newest first), `zemi.health.list()` (the `/health` rows), `zemi.ui.screenshot()` (the screen as text) and `zemi.ui.layout.rects()` (the rect of each layout node with an id, keyed by id; widgets are not nodes, the screenshot shows where they draw). An init.lua change may be tried live with the Lua tool first (one approval, gone at the next reload). Then tell the user the path and how to undo it.
