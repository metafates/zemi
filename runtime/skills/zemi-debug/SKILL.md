---
name: zemi-debug
description: Diagnose zemi itself - crashes, errors, a silent model, missing providers, broken or unloaded config, no learning. Load when zemi misbehaves or fails.
---

# Debugging zemi

## First look

1. Ask the user to run `/health`. It checks the model and its API key, the stores,
   `permissions.lua`, git, ripgrep, the reflector, pending reviews, skills, recent errors,
   language servers and the Lua runtime, plus any rows config added with `zemi.health.register`.
2. `/errors` lists recent errors: boot and reload failures, widget, keymap and
   event-handler errors, and recovered panics. The red `! errors` statusline badge means
   there are unseen ones. Enter opens one in full, with "Ask zemi to explain and fix
   this". In every list `/` filters, `y` copies the selected row and `Y` copies the whole
   list, so the user can paste an error back to you.
3. Read the same entries yourself with the Lua tool instead of asking the user what the
   screen says:

   ```lua
   return zemi.errors.list()   -- newest first: { index, at, source, message }
   ```

   `zemi.config.reload()` applies config you just wrote, the same as `/reload`.

4. For anything about what is on the screen - a widget drawing wrong, a theme, an overlay
   in the way, text wrapping badly, a statusline that lost a segment - look instead of
   asking:

   ```lua
   return zemi.ui.screenshot()   -- the drawn screen as plain text, nil, err before the first frame
   ```

   It is the frame the user is looking at, at their real terminal width, with styling and
   padding dropped. Save one before a change and one after and diff them; that is the whole
   loop for UI work: edit config, `zemi.config.reload()`, screenshot, compare. Colors and
   attributes do not survive it, so for a highlight question read the theme with
   `zemi.ui.theme.get()` instead.

5. Read the log of the run (reading the log directory asks: MCP and LSP server output is
   logged as written and may hold tokens). One file per start:

   ```sh
   ls -t ~/.local/state/zemi/logs/ | head -1
   grep -n 'ERROR\|WARN\|panic' ~/.local/state/zemi/logs/<file>.log
   ```

   Each line is `time LEVEL source message key=value...`. Sources worth grepping:
   `agent`, `reflector`, `memory`, `skills`, `trust`, `events`, `panic`, `ui`, `boot`,
   `config-repo`, `mcp`, `lsp`, `rewind`, `tools`, `commands`, `workflows`.
   A recovered panic logs its full stack under source `panic`.

## Common failures

| Symptom | Cause and fix |
| --- | --- |
| "no model configured" | no provider or `agent.model`: see zemi-config (`zemi.provider.register`) |
| 401 / unauthorized | the provider's `api_key` is empty or wrong: the variable read with `zemi.env.get` is not exported in the shell that started zemi |
| "Retrying (attempt N)" then an error | network, VPN or `base_url`; the log has each retry's provider error |
| boot error "permissions.lua refused" | the file was edited by hand; restore it from the git history of `~/.config/zemi` or delete it (permanent rules are lost) |
| `/model` shows "no models registered" but a provider is in init.lua | `init.lua` failed before or at `zemi.provider.register` (for example the removed `api_key_env` field); the statusline shows `boot failed`, `/errors` has the message |
| statusline `boot failed` | init.lua, a plugin or project config raised during startup; `/errors` or the log (source `boot`) has the error |
| statusline `stuck: /reload` | a Lua handler wedged the interpreter; `/reload` rebuilds it; find the handler in `/errors` or the log |
| "does not take images; going on without them" | the provider refused image input; set `images = false` on the model entry, or pick a multimodal model for screenshots |
| context percent missing, no auto compaction | the model has no `context_window`; add it to the model entry or set `agent.context_window` |
| project `.zemi/` config, AGENTS.md or language servers ignored; auto mode asks for every build, edit and git command | the project is not trusted (`/health` trust row); `/trust` asks again. Decisions live in `~/.local/state/zemi/trust.json` by repository root; a linked worktree uses its main checkout's |
| auto mode asks for builds, tests and git in a trusted project ("not settled: no sandbox") | no sandbox backend (`/health` sandbox row: `sandbox-exec` missing, zemi itself already inside a sandbox, or no Landlock); without one only reading commands settle |
| "operation not permitted" or "could not resolve host" from a command auto mode ran | the sandbox blocked it (writes outside the project, scratchpad and temp dirs, `.git`, network; on Linux any project write and localhost too); the agent retries with `sandbox: false`, which asks |
| reflector never proposes | `/health` reflector row; it runs on `/new`, `/resume` and the next start, only for sessions with new messages; `/reflect` runs it now |
| learned skill not used | drafted skills start disabled: `/skills`, enter toggles |

## Stores

SQLite, safe to inspect read-only (`sqlite3 -readonly <file>`):

- `~/.local/state/zemi/zemi.db` (global): user-scope memories and their proposals,
  skill enabled state (`skills`), permanent-rule commits (`config_revisions`), `kv`.
- `~/.local/state/zemi/projects/<hash>/zemi.db` (per project): `sessions`, `messages`,
  project memories and proposals, `todos`, `tool_calls`, `checkpoints` (file contents
  `/rewind` restores).

Useful tables: `memory` (text, kind, origin, superseded_by), `proposals` (pending review,
JSON payload), `sessions` (title, prompt_tokens, completion_tokens, cost,
summary_message_id), `messages` (role `tail` rows are copies of messages a compaction kept).

## Reproduce without touching the user's setup

```sh
ZEMI_CONFIG_HOME=$(mktemp -d) ZEMI_DATA_HOME=$(mktemp -d) ZEMI_STATE_HOME=$(mktemp -d) zemi
```

Starts with empty config and stores. Copy only the pieces needed to reproduce.

## Reporting a bug

Collect the `/errors` line, the matching log lines (with the `panic` stack if any), the
`/health` output and the steps. Never paste API keys or `.env` contents, and check the log
lines for tokens first: server output is logged as written.
