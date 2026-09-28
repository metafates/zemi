# Debugging zemi

## First look

1. Read the `/health` rows yourself with the Lua tool:

   ```lua
   return zemi.health.list() -- { level, name, detail } per check
   ```

   `/health` shows the same rows to the user, problems first.
2. `/errors` lists recent errors: boot and reload failures, widget, keymap, click, command and event-handler errors, and recovered panics; a command's bad argument is a warning toast only. The red `errors /errors` statusline badge means there are unseen ones. Enter opens one in full, with [a] Ask zemi to fix it (and [r] Restart for an MCP or language server). In every list `/` filters (enter keeps the filter, esc clears it), `y` copies the selected row (in a detail view, the whole text) and `Y` copies the whole list, so the user can paste an error back to you.
3. Read the same entries yourself with the Lua tool instead of asking the user what the screen says:

   ```lua
   return zemi.errors.list() -- newest first: { index, at, source, message }
   ```

   For a question about what the model was told - a profile's `system`, an `agent.assemble` section, an instruction file the budget cut, the memory digest - read the prompt itself:

   ```lua
   return zemi.agent.prompt() -- the pinned system prompt, "" before the first send
   ```

   `/prompt` shows the same text; `/usage` only measures it. The prompt is pinned for the session, so it may lag the files on disk: an edit to a skill, a profile or an `agent.assemble` handler shows after `/new` or `/compact`, and a hand edit to an instruction file rides the next message instead.
4. For anything about what is on the screen - a widget drawing wrong, a theme, an overlay in the way, text wrapping badly, a statusline that lost a segment - look instead of asking:

   ```lua
   return zemi.ui.screenshot() -- the drawn screen as plain text, nil, err before the first frame
   ```

   It is the frame the user is looking at, at their real terminal width, with styling and padding dropped (a plugin's widget is checked by `test.render` in its tests instead, `plugins.md`). Save one before a change and one after and diff them. Colors and attributes do not survive it, so for a highlight question read the group with `zemi.ui.hl.get(group)` instead. For a layout question, `zemi.ui.layout.rects()` says where each node with an id, the stage and the composer landed, and `/health` names widgets whose region the layout does not draw (`exit` aside: it prints on quit).
5. A `print()` or `zemi.log` line from config code (a keymap, a widget render, a timer, an event handler) is in `/messages` and `zemi.log.list()`, the last 100, newest first. Headless (`zemi run`, `zemi health`) it goes only to the log file, as `print: ...` lines.
6. Read the log of the run (reading the log directory needs the user's approval: MCP and LSP server output is logged as written and may hold tokens). One file per start:

   ```sh
   ls -t ~/.local/state/zemi/logs/ | head -1
   grep -n 'ERROR\|WARN\|panic' ~/.local/state/zemi/logs/<file>.log
   ```

   Each line is `time LEVEL source message key=value...`. Sources worth grepping: `agent`, `reflector`, `memory`, `skills`, `trust`, `events`, `panic`, `ui`, `boot`, `mcp`, `lsp`, `rewind`, `tools`, `commands`, `workflows`. A recovered panic logs its full stack under source `panic`.

## Common failures

| Symptom | Cause and fix |
| --- | --- |
| "no model yet", or `/model` shows "No models yet" | no key exported (zemi reads keys at start: export, then start zemi again) and no provider in init.lua, or init.lua failed before `zemi.provider.add` (statusline `boot failed`, `/errors` has why); `/model` lists the builtin providers with their variables, `config.md` has `zemi.provider.add` |
| 401 / unauthorized | the provider's key is empty or wrong: the exported builtin key, or the variable read with `zemi.env.get`, is not right in the shell that started zemi |
| "Retrying (attempt N)" then an error | network, VPN or `base_url`; the log has each retry's provider error |
| statusline `boot failed` | init.lua, a plugin or project config raised during startup; `/errors` or the log (source `boot`) has the error |
| statusline `stuck /reload` | a Lua handler wedged the interpreter; `/reload` rebuilds it; find the handler in `/errors` or the log |
| "does not take images; going on without them" | the provider refused image input; set `vision = false` on the model entry, or pick a multimodal model for screenshots |
| context percent missing, no auto compaction | the model has no `context_window` (`/health` context row); add it to the model entry or set `agent.context_window` |
| project `.agents/` config, AGENTS.md or language servers ignored; auto mode needs approval for every build, edit and git command | the project is not trusted (`/health` trust row); `/trust` shows the prompt again. Decisions live in `~/.local/state/zemi/trust.json` by repository root; a linked worktree uses its main checkout's |
| auto mode needs approval for every command that is not a plain read in a trusted project ("no sandbox") | no sandbox backend (`/health` sandbox row: `sandbox-exec` missing, zemi itself already inside a sandbox, or no Landlock); without one only reading commands run on their own |
| "operation not permitted" or "could not resolve host" from a command auto mode ran | the sandbox blocked it (writes outside the project and the scratchpad, which is every model command's `$TMPDIR` (`/tmp` is not writable), `.git`, network, reads of zemi's own dirs; on Linux with `landlock + seccomp` any project write and localhost too); the agent retries with `network: true` (sandbox kept, sockets added) or, as a last resort, `sandbox: false`; both need the user's approval |
| "connection refused" to a local server from a command auto mode ran, on Linux | `landlock + namespaces` gives the sandbox a localhost of its own, so servers already running on the machine are unreachable; retry with `network: true`, which shares zemi's own network |
| "a sandboxed command created ...; project trust is off until /trust" | a sandboxed command made `.agents` or `.git` where there was none (only the Linux namespace sandbox lets it); zemi stops loading project config until `/trust` is answered. Look at what it created first |
| reflector never proposes | `/health` reflector row; it runs on `/new`, `/resume` and at start for sessions idle ten minutes, once one has two new user messages and a tool call or a failed call; `/reflect` runs it now |
| `module "x" not found on runtimepath` from a plugin | the module is not at `<plugin x>/lua/x.lua` or `lua/x/init.lua` (a plugin answers only requires under its own name, so plugin `zemi-branch` cannot answer `require("branch")`), or plugin x is disabled or failed to load; plugins are listed once per boot, and a new one reloads within a second |
| `module "..." at ... is outside plugin <name>` | a relative require or a symlink left the plugin's own directory, or reached a disabled plugin |
| `plugin code running with no file of its plugin on the stack has no principal` | user code called a function a plugin handed out that is not the plugin's own file (a user module it required, a `load` chunk); call it through the plugin's own code |
| `zemi run "..."` from a script sits 3 seconds before it starts | stdin is an open pipe nobody writes to, and zemi waits that long for piped input; add `</dev/null` |
| one plugin fails at boot | the others still load; `/errors` has the message (`plugin <name>: ...`) and the `/health` plugins row counts the failed ones. A local plugin named like an installed one, or a symlinked plugin dir, never loads |
| a plugin registers nothing, with no error | it has no `init.lua` at `<data>/plugins/installed/<name>/` or `local/<name>/` (a library-only plugin has just `lua/`), or its name is in `<data>/plugins/disabled` |
| `... is registered by plugin x, not y` | two plugins want one command, key, widget, theme, event handler id, tool, completion source or health row, or y tried to remove x's; the first by name keeps it |

## Stores

SQLite, safe to inspect read-only (`sqlite3 -readonly <file>`):

- `~/.local/state/zemi/zemi.db` (global): user-scope memories and their proposals, skill enabled state (`skills`), `kv`.
- `~/.local/state/zemi/projects/<hash of the repository root>/zemi.db` (per project, shared by its subdirectories and worktrees): `sessions`, `messages`, project memories and proposals, `todos`, `tool_calls`, `checkpoints` (file contents `/rewind` restores). The same directory holds the project's `permissions` file and `skills/` (recipes).

Useful tables: `memory` (text, quote, origin, superseded_by), `proposals` (pending review, JSON payload), `sessions` (title, prompt_tokens, completion_tokens, cost), `messages` (role `tail` rows are copies of messages a compaction kept; resume starts at the last summary row).

## Reproduce without touching the user's setup

```sh
ZEMI_CONFIG_HOME=$(mktemp -d) ZEMI_DATA_HOME=$(mktemp -d) ZEMI_STATE_HOME=$(mktemp -d) ZEMI_CACHE_HOME=$(mktemp -d) zemi
```

Starts with empty config and stores. Copy only the pieces needed to reproduce.

## Reporting a bug

Collect the `/errors` line, the matching log lines (with the `panic` stack if any), the `/health` output and the steps. Never paste API keys or `.env` contents, and check the log lines for tokens first: server output is logged as written.
