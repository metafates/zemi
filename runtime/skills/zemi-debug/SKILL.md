---
name: zemi-debug
description: Diagnose zemi itself - crashes, errors, a silent model, missing providers, broken or unloaded config, no learning. Load when zemi misbehaves or fails.
---

# Debugging zemi

## First look

1. Ask the user to run `/health`. It checks the model and its API key, the stores,
   `permissions.lua`, git, the reflector, pending reviews, skills, recent errors, language
   servers and the Lua runtime.
2. `/errors` lists recent widget, keymap and event-handler errors and recovered panics;
   the red `! errors` statusline badge means there are unseen ones. Enter opens an error
   in full, with "Ask zemi to explain and fix this". In every list `/` filters.
3. Read the log of the run. One file per start, newest last:

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
| "no models registered" | no provider: see zemi-config (`zemi.provider.register`) |
| 401 / unauthorized | the provider's `api_key` is empty or wrong: the variable read with `zemi.env.get` is not exported in the shell that started zemi |
| "Retrying (attempt N)" then an error | network, VPN or `base_url`; the log has each retry's provider error |
| boot error "permissions.lua refused" | the file was edited by hand; restore it from the git history of `~/.config/zemi` or delete it (permanent rules are lost) |
| "no models registered" but a provider is in init.lua | `init.lua` failed before or at `zemi.provider.register` (for example the removed `api_key_env` field); the statusline shows `boot failed`, `/errors` has the message |
| statusline `boot failed` | init.lua, a plugin or project config raised during startup; `/errors` or the log (source `boot`) has the error |
| statusline `stuck: /reload` | a Lua handler wedged the interpreter; `/reload` rebuilds it; find the handler in `/errors` or the log |
| context percent missing, no auto compaction | the model has no `context_window`; add it to the model entry or set `agent.context_window` |
| project `.zemi/init.lua` ignored | trust refused; `~/.local/state/zemi/project_trust.json` maps the dir to a hash, a `!` prefix means denied; remove the entry to be asked again |
| reflector never proposes | `/health` reflector row; it runs on `/new`, `/resume` and the next start, only for sessions with new messages; `/reflect` runs it now |
| learned skill not used | drafted skills start disabled: `/skills`, enter toggles |

## Stores

SQLite, safe to inspect read-only (`sqlite3 -readonly <file>`):

- `~/.local/state/zemi/zemi.db` (global): user-scope memories and their proposals,
  skill enabled state (`skills`), config revisions (`config_revisions`), `kv`.
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
`/health` output and the steps. Never paste API keys or `.env` contents; the logs do not
contain keys.
