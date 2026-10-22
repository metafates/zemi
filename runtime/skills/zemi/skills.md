# Skills and prompt commands

## Skills (`skills/<name>/SKILL.md`)

A skill is a procedure: the model loads it when its description matches the task, and the user can run it. Frontmatter, then the steps:

```markdown
---
name: run-integration-tests
description: Load before running the integration tests, or when they fail on a missing database.
---
1. `docker compose up -d db`
2. Wait for `pg_isready`, then run `go test -tags integration ./...`.
```

- `name` is the directory's name: a lowercase letter or digit, then lowercase letters, digits or `-`, up to 64. It may be left out; the directory's name is it. A written name that does not match the directory is an error.
- `description` is the trigger: when to load it, not a summary. It is cut to 512 runes when the skill is parsed, with a warning, and every request carries its first 240, then `...`. It may be left out; the body's first non-empty line is it.
- `user-invocable: true` also makes it the command `/<name> [request]`, which sends the skill with the request; `argument-hint: <pr number>` shows after it in the palette, and `$ARGUMENTS` in the body becomes the request. The palette shows its description beside zemi's own commands, so lead it with a verb ("Run the integration tests; load before..."). When `/<name>` is already zemi's own command, or one from the skill's own place, that command stays, the skill is no command, and `/errors` says so; a command from anywhere else is replaced.
- Claude's `disable-model-invocation` is ignored: the model may load every skill. `user-invocable: false` keeps a skill off the command palette.
- Material the steps need only sometimes goes in files beside SKILL.md, named in the body; the skill tool hands the model the skill's directory, where it reads one when needed, so SKILL.md stays short.
- User skills go in `<config>/skills/`, then `~/.agents/skills/` (shared with other agents; a name in both loads from the config dir), on by default; the user toggles them in `/skills`, which takes the command along. A `zemi-skill.lua` beside a `<config>/skills/` skill's SKILL.md runs at startup, after init.lua, while the skill is on; one in `~/.agents/skills/` never runs.
- A trusted project's `.agents/skills/` skills are always on and run no Lua; a user skill of the same name wins. They load at startup, with every reload (a change under `.agents` starts one) and on `/trust`.
- `/skills use <name> [request]` sends the skill with a request, one that is off or no command too; the chat shows the command line, the model gets the body.

## Prompt commands (`commands/<name>.md`)

```markdown
---
description: Write a failing test for a bug
argument-hint: <file> <symptom>
---
Read $1 and add a failing test showing: $ARGUMENTS. Do not fix the bug.
```

`/<name> args` sends the body with `$ARGUMENTS` (everything after the name) and `$1`..`$9` (single words) filled in. The body goes behind the typed line, so an `@file` in it attaches nothing; the model reads the file itself.

A prompt command is the smaller tool: one prompt, never loaded by the model on its own. Write a skill when the model should find it too, or when it needs files beside it.
