---
name: zemi-learning
description: How zemi learns - memories, reflector, skill drafts. Load when asked what zemi remembers, to remember or forget, or to write a skill.
---

# How zemi learns

## The loop

1. **Capture.** `/memory <note>` stores a project fact immediately (`s` in `/memory` makes it apply everywhere). You (the agent)
   can call `memory_add` with a kind (`fact`, `gotcha`, `preference`, `convention`,
   `decision`) and one specific fact (up to 500 characters); under the default autonomy it
   is queued for the user's review. `memory_search` finds earlier memories. `memory_update`
   rewrites one and `memory_forget` deletes one, by the 8-character id the digest and search
   show; the change asks the user unless autonomy is `auto` (and would store at once, see
   below) and the memory is `learned`.
2. **Reflect.** When a session closes (`/new`, `/resume`, or the next start) the
   reflector reads it and proposes memories and skill drafts, and retirements of memories
   the session showed are no longer true (they wait for review as `forget:` rows).
   `/reflect` runs it on demand.
3. **Review.** Proposals persist until decided; the statusline shows how many wait.
   - `/memory`: every memory whole, as plain text, with its kind, scope, origin, age and
     tags under it; `›` marks the selected one (j/k, the wheel or a click). `a` (or `y`) accepts a proposal, `d` (or `x`, backspace) rejects it or
     forgets a stored memory, `s` moves it between this project and everywhere, `e` opens
     it in the user's editor (the saved text becomes the user's own memory, an edited
     suggestion is stored, and the screen comes back on that row), `u` undoes
     the last mark; esc or `q` closes the screen and applies the marks. `/memory forget <id>`, `/memory supersede <old> <new>`.
   - `/skills`: drafted skills start disabled; space toggles, enter shows one with its full
     body and its actions (Enable or Disable, Use now, Open in the editor),
     or `/skills enable|disable|open <name>`.
     `/skills use <name> [request]` sends the skill's body with the request when the model
     did not load it on its own; a disabled skill works too (keep a rarely used one off the
     index and name it on the day it applies), except an unreviewed learned draft.
   - Autonomy (`memory.autonomy = "auto"` in user config) stores proposed memories without
     review, except instruction-shaped text, retirements, replacements of the user's own
     memories, and anything from an untrusted project or a session that used `fetch`,
     `web_search`, the `Lua` tool or a tool zemi does not ship, MCP and `zemi.tool` ones
     included (itself or through a subagent).
4. **Use.** Accepted memories reach the model as a digest in the system prompt, pinned at
   session start (new memories apply from the next session or compaction). The digest
   (`memory.digest_lines`, 12 by default, 0 off) favors memories about the session's first
   message, the user's own notes, and memories written or searched for recently; each line
   shows its age in days from two days on, and so does `memory_search`. Enabled skills
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
- `description` says when to use it, in at most 512 characters; every prompt carries it
  whole, so keep it short.
- An optional `zemi-skill.lua` next to it runs when the skill is enabled.
- Skills zemi drafts itself live in `~/.local/share/zemi/skills/` and start disabled.
- A project the user trusts adds the skills in `.zemi/skills/`, `.agents/skills/` and
  `.claude/skills/` at the repository root: always on, no Lua hook, and a user skill of the
  same name wins. zemi reads them at startup, on a reload and on `/trust`.

## Recipes

A recipe is a skill private to one trusted repository on this machine: an exact procedure
that was costly to find (a local run rig, a release, a data fix), too long for a memory and
not for the repository. A fact is a memory; a procedure for every project is a user skill.

- Write `<Recipes>/<name>/SKILL.md` with `write`, in the format above (no Lua hook); the
  `Recipes:` line of the environment names the directory. No rule and neither auto nor plan
  settles the write; yolo still allows.
- It loads with the `skill` tool at once and is listed under Skills from the next session.
- Secret-shaped text is redacted after the write, and the result says so: name the env var
  or use a placeholder like `$TOKEN`. Long hex and base64 runs (digests) are cut too.
- Fix a stale step in place with `edit` and date the correction; link another recipe as
  `[[name]]`.

## From Lua

```lua
zemi.memory.search("deploy", { scope = "all", limit = 5 })  -- scope: all, user, project
zemi.memory.add({ kind = "gotcha", text = "...", scope = "project" })  -- queued under ask,
                                                                      -- origin user from config
zemi.memory.list(true)                   -- true includes superseded memories
zemi.memory.supersede(old_id, new_id); zemi.memory.forget(id)  -- user config only
for _, p in ipairs(zemi.learn.proposals()) do print(p.id, p.forget, p.text) end
zemi.learn.decide(id, "accept")          -- or "reject"; user config only
zemi.skills.list(); zemi.skills.enable("name"); zemi.skills.disable("name")
zemi.event.on("memory.written", function(ev) zemi.log.info(ev.text) end)
```

These return `nil, err` when the call fails at runtime (no such memory, proposal or skill;
a store error) and raise only on misuse (a bad argument, a kind or scope outside the enum,
an origin that may not make the call).

Events: `memory.written`, `skill.learned`, `reflector.start`, `reflector.done`.
