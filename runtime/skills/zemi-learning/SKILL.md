---
name: zemi-learning
description: How zemi learns - memories, reflector, skill drafts, rule patches. Load when asked what zemi remembers, to remember or forget, or to write a skill.
---

# How zemi learns

## The loop

1. **Capture.** `/memory <note>` (or `/remember <note>`) stores a project fact immediately. You (the agent)
   can call `memory_add` with a kind (`fact`, `gotcha`, `preference`, `convention`,
   `decision`) and one specific fact (up to 500 characters); under the default autonomy it
   is queued for the user's review. `memory_search` finds earlier memories. `memory_update`
   rewrites one and `memory_forget` deletes one, by the 8-character id the digest and search
   show; the change asks the user unless autonomy is `auto` and the memory is `learned`.
2. **Reflect.** When a session closes (`/new`, `/resume`, or the next start) the
   reflector reads it and proposes memories, permission rule patches and skill drafts.
   `/reflect` runs it on demand.
3. **Review.** Proposals persist until decided; the statusline shows how many wait.
   - `/memory`: one line per memory; the selected one (j/k, the wheel or a click) opens
     in place with its whole text, kind, scope, origin and tags. `a` (or `y`) accepts a proposal, `d` (or `x`, backspace) rejects it or
     forgets a stored memory, `u` undoes the last mark; enter, esc or `q` closes the screen
     and applies the marks. `/memory forget <id>`, `/memory supersede <old> <new>`.
   - `/config`: `y` or `a` accepts a rule patch, `n` or `x` rejects, esc leaves the rest
     queued. `/config revisions`, `/config revert <id>`.
   - `/skills`: drafted skills start disabled; enter toggles, or `/skills enable|disable <name>`.
   - Autonomy (`memory.autonomy`, `config.autonomy`) is host-owned. Under `auto`, memories
     and session-scope deny or ask rule patches apply without review.
4. **Use.** Accepted memories reach the model as a digest in the system prompt, pinned at
   session start (new memories apply from the next session or compaction). Enabled skills
   appear as a name + description index; the full body loads with the `skill` tool.

Everything learned is scrubbed for secrets and labeled `user` or `learned`.

## Answering "what do you remember?"

Use `memory_search` with a few keywords, or ask the user to open `/memory` to see and
curate everything. Do not invent memories that are not in the digest or search results.

## Writing a skill

A skill is a directory with a `SKILL.md`:

```markdown
---
name: run-integration-tests
description: when asked to run the local integration tests
---

1. `docker compose up -d db`
2. `go test -tags integration ./...`
3. If the db is not ready, wait for `pg_isready` before retrying.
```

- Put user skills in `~/.config/zemi/skills/<name>/SKILL.md` (enabled by default).
- `name` must match the directory: lowercase letters, digits and `-`.
- `description` says when to use it (at most 512 characters, but the index cuts at 160 and
  drafts from the reflector must fit in 160).
- An optional `zemi-skill.lua` next to it runs when the skill is enabled.
- Skills zemi drafts itself live in `~/.local/share/zemi/skills/` and start disabled.

## From Lua

```lua
zemi.memory.search("deploy", { scope = "all", limit = 5 })  -- scope: all, user, project
zemi.memory.add({ kind = "gotcha", text = "...", scope = "project" })  -- queued under ask
zemi.memory.list(true)                   -- true includes superseded memories
zemi.memory.supersede(old_id, new_id); zemi.memory.forget(id)  -- user config only
for _, p in ipairs(zemi.learn.proposals()) do print(p.id, p.text) end
zemi.learn.decide(id, "accept")          -- or "reject"; user config only
zemi.skills.list(); zemi.skills.enable("name"); zemi.skills.disable("name")
zemi.event.on("memory.written", function(ev) zemi.log.info(ev.text) end)
```

Events: `memory.written`, `skill.learned`, `config.proposed`, `config.applied`,
`reflector.start`, `reflector.done`.
