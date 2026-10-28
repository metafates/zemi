# Skills and prompt commands

## Skills (`skills/<name>/SKILL.md`)

A skill is a procedure: the model loads it when its description matches the task, and the user attaches it with `@name`. Frontmatter, then the steps:

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
- Claude's `disable-model-invocation` and `argument-hint` are ignored: the model may load every skill, and binds the request from the typed message. A `user-invocable` key is accepted and ignored.
- Material the steps need only sometimes goes in files beside SKILL.md, named in the body; the skill tool hands the model the skill's directory, where it reads one when needed, so SKILL.md stays short.
- User skills go in `<config>/skills/`, then `~/.agents/skills/` (shared with other agents; a name in both loads from the config dir), on by default; the user toggles them in `/skills`. A `zemi-skill.lua` beside a `<config>/skills/` skill's SKILL.md runs at startup, after init.lua, while the skill is on; one in `~/.agents/skills/` never runs.
- A trusted project's `.agents/skills/` skills are always on and run no Lua; a user skill of the same name wins. They load at startup, with every reload (a change under `.agents` starts one) and on `/trust`.
- `@name` in a message attaches the skill, one that is off too: type `@` in the composer to pick from the popup - files and skills, under their headers - go on writing your prompt, and the model gets the body behind the message as typed; it binds the request from your message. An `@word` that names no skill and no file is left as written, and a repeated `@name` attaches once.

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
