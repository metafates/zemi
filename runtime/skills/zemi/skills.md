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

- `name` is the directory's name: lowercase letters, digits, `-`.
- `description` is the trigger: when to load it, not a summary, at most 512 characters. Every request carries it.
- `user-invocable: true` also makes it the command `/<name> [request]`, which sends the skill with the request; `argument-hint: <pr number>` shows after it in the palette, and `$ARGUMENTS` in the body becomes the request. When `/<name>` is already zemi's own command, or one from the skill's own place or a more trusted one (the user's config over a plugin over the repository), that command stays, the skill is no command, and `/errors` says so.
- `disable-model-invocation: true` keeps it out of the model's index, so only the user runs it; it is then a command unless it also sets `user-invocable: false`.
- Material the steps need only sometimes goes in files beside SKILL.md, named in the body; the skill tool hands the model the skill's directory, where it reads one when needed, so SKILL.md stays short.
- User skills go in `<config>/skills/`, then `~/.agents/skills/` (shared with other agents; a name in both loads from the config dir), on by default; the user toggles them in `/skills`, which takes the command along. A `zemi-skill.lua` beside a `<config>/skills/` skill's SKILL.md runs at startup, after init.lua, while the skill is on; one in `~/.agents/skills/` never runs.
- A trusted project's `.agents/skills/` skills are always on and run no Lua; a user skill of the same name wins. They load at startup, on `/reload` and on `/trust`.
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
