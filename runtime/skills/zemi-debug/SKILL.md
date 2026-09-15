---
name: zemi-debug
description: Diagnose problems with zemi itself - crashes, errors, a model that does not answer, missing providers, broken config, project config not loading, learning that does not happen. Load when zemi misbehaves or the user asks why something in zemi failed.
---

# Debugging zemi

## First look

1. Ask the user to run `/health`. It checks the model and its API key, the stores,
   `permissions.lua`, git, the reflector, pending reviews, skills and the Lua runtime.
2. `/errors` lists recent widget, keymap and event-handler errors and recovered panics;
   the red `! errors` statusline badge means there are unseen ones.
3. Read the log of the run. One file per start, newest last:

   ```sh
   ls -t ~/.local/state/zemi/logs/ | head -1
   grep -n 'ERROR\|WARN\|panic' ~/.local/state/zemi/logs/<file>.log
   ```

   Each line is `time LEVEL source message key=value...`. Sources worth grepping:
   `agent`, `reflector`, `memory`, `skills`, `trust`, `events`, `panic`, `ui`.
   A recovered panic logs its full stack under source `panic`.

## Common failures

| Symptom | Cause and fix |
| --- | --- |
| "no models registered" | no provider: see zemi-config (`zemi.provider.register` or crush.json) |
| 401 / unauthorized | the `api_key_env` variable is not exported in the shell that started zemi |
| "Retrying (attempt N)" then an error | network, VPN or `base_url`; the log has each retry's provider error |
| boot error "permissions.lua refused" | the file was edited by hand; restore it from the git history of `~/.config/zemi` or delete it (permanent rules are lost) |
| statusline "HOT /reload" | a Lua handler wedged the interpreter; `/reload` rebuilds it; find the handler in `/errors` or the log |
| project `.zemi/init.lua` ignored | trust refused; `~/.local/state/zemi/project_trust.json` maps the dir to a hash, a `!` prefix means denied; remove the entry to be asked again |
| reflector never proposes | `/health` reflector row; it runs on `/new`, `/resume` and the next start, only for sessions with new messages; `/reflect` runs it now; `reflector.every_n_sessions = 0` disables it |
| learned skill not used | drafted skills start disabled: `/skills`, enter toggles |

## Stores

SQLite, safe to inspect read-only (`sqlite3 -readonly <file>`):

- `~/.local/state/zemi/zemi.db` (global): user-scope memories and their proposals,
  skill enabled state (`skills`), config revisions (`config_revisions`), `kv`.
- `~/.local/state/zemi/projects/<hash>/zemi.db` (per project): `sessions`, `messages`,
  project memories and proposals, `todos`, `tool_calls`.

Useful tables: `memory` (text, kind, origin, superseded_by), `proposals` (pending review,
JSON payload), `sessions` (title, tokens, cost, summary_message_id).

## Reproduce without touching the user's setup

```sh
ZEMI_CONFIG_HOME=$(mktemp -d) ZEMI_DATA_HOME=$(mktemp -d) ZEMI_STATE_HOME=$(mktemp -d) zemi
```

Starts with empty config and stores. Copy only the pieces needed to reproduce.

## Reporting a bug

Collect the `/errors` line, the matching log lines (with the `panic` stack if any), the
`/health` output and the steps. Never paste API keys or `.env` contents; the logs do not
contain keys.
