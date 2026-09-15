---
name: zemi-learning
description: How zemi learns from the user - memories, the reflector, skill drafts, permission rule patches - and how to review, fix or write them. Load when the user asks what zemi remembers, wants it to remember or forget something, or wants to write a skill.
---

# How zemi learns

## The loop

1. **Capture.** `/remember <text>` stores a project fact immediately. You (the agent)
   can call `memory_add` with a kind (`fact`, `gotcha`, `preference`, `convention`,
   `decision`) and one short sentence; under the default autonomy it is queued for the
   user's review. `memory_search` finds earlier memories.
2. **Reflect.** When a session closes (`/new`, `/resume`, or the next start) the
   reflector reads it and proposes memories, permission rule patches and skill drafts.
   `/reflect` runs it on demand.
3. **Review.** Proposals persist until decided; the statusline shows how many wait.
   - `/memory`: `a` accepts a proposal, `d` rejects it or deletes a stored memory,
     enter applies, esc changes nothing. `/memory forget <id>`, `/memory supersede <old> <new>`.
   - `/config`: `y` accepts a rule patch, `n` rejects. `/config revisions`, `/config revert <id>`.
   - `/skills`: drafted skills start disabled; enter toggles.
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
- `description` says when to use it (at most 512 characters; the index shows 160).
- An optional `zemi-skill.lua` next to it runs when the skill is enabled.
- Skills zemi drafts itself live in `~/.local/share/zemi/skills/` and start disabled.

## From Lua

```lua
zemi.memory.search("deploy", { scope = "all", limit = 5 })
zemi.memory.add({ kind = "gotcha", text = "...", scope = "project" })  -- queued under ask
for _, p in ipairs(zemi.learn.proposals()) do print(p.id, p.text) end
zemi.learn.decide(id, "accept")          -- or "reject"; user config only
zemi.skills.list(); zemi.skills.enable("name"); zemi.skills.disable("name")
zemi.event.on("memory.written", function(ev) zemi.log.info(ev.text) end)
```

Events: `memory.written`, `skill.learned`, `config.proposed`, `config.applied`,
`reflector.start`, `reflector.done`.
