---
name: zemi-learning
description: How zemi learns - memories, the reflector, skills and recipes. Load when asked what zemi remembers, to remember or forget, or to write a skill.
---

# How zemi learns

## The loop

1. **Capture.** `/memory <note>` stores a project fact immediately (`s` in `/memory` makes it apply everywhere). You (the agent)
   call `memory` with one specific fact (up to 300 characters) and, for anything about the
   user, `quote`: their exact words from this session (a quote that is not in the session
   drops the memory; a user-scope memory without one waits for review). Under the default
   autonomy it stores at once when policy-clean; the answer names the id, or the id of a
   near-duplicate to pass as `replaces` instead. `replaces=<id>` (the 8-character id the
   `# Memory` section shows) rewrites that memory; empty text with `replaces` drops it: at
   once for a `learned` memory under `auto`, proposed for review otherwise, and one the user
   saved asks. Each scope holds 3 KB of lines; a write past that queues for review and
   names the oldest ids to replace, and the review refuses to accept into a full scope.
2. **Reflect.** When a session closes (`/new`, `/resume`), and at startup for sessions
   idle ten minutes, the reflector reads what was added since it last read the session,
   once it holds two user messages and a tool call or one failed tool call. It sees the
   failed tool calls, the instruction files, every memory with its budget use and the
   recipes, and proposes memories only, under the same rules as the `memory` tool (a memory
   about the user carries the user's words; a quote not in the session drops it), plus
   retirements of memories the session showed are no longer true (they wait for review as
   `forget:` rows). It drafts no skills. `/reflect` runs it on demand, floor or not.
3. **Review.** Proposals persist until decided; the statusline shows how many wait.
   - `/memory`: every memory whole, as plain text, with its quote, scope, origin and age
     under it and each scope's budget use in the Remembered title; `›` marks the selected one (j/k, the wheel or a click). `a` (or `y`) accepts a proposal, `d` (or `x`, backspace) rejects it or
     forgets a stored memory, `s` moves it between this project and everywhere, `e` opens
     it in the user's editor (the saved text becomes the user's own memory, an edited
     suggestion is stored, and the screen comes back on that row), `u` undoes
     the last mark; esc or `q` closes the screen and applies the marks. `/memory forget <id>`.
   - `/skills`: space toggles a skill, enter shows one with its full
     body and its actions (Enable or Disable, Use now, Open in the editor),
     or `/skills enable|disable|open <name>`.
     `/skills use <name> [request]` sends the skill's body with the request when the model
     did not load it on its own; a disabled skill works too (keep a rarely used one off the
     index and name it on the day it applies).
   - Autonomy (the default `memory.autonomy = "auto"`, or set `ask` to review every one)
     stores proposed memories without review, except instruction-shaped text, retirements,
     replacements of the user's own memories, and anything from an untrusted project or a
     session that used `fetch`, `web_search`, the `lua` tool or a tool zemi does not ship,
     MCP and `zemi.tool` ones included (itself or through a subagent).
4. **Use.** Accepted memories reach the model as the `# Memory` section of the system
   prompt, pinned at session start (new memories apply from the next session or
   compaction): every live memory, this project then every project, newest first, each
   scope cut at its 3 KB budget; each line shows its id, origin and age in days from two
   days on. Enabled skills appear as a name + description index; the full body loads with
   the `skill` tool.

Everything learned is scrubbed for secrets and labeled `user` or `learned`.

## Answering "what do you remember?"

Read the `# Memory` section of your prompt, or ask the user to open `/memory` to see and
curate everything. Do not invent memories that are not there.

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
- Secret-shaped text in any file there is redacted after the write, and the result names
  the lines: name the env var or use a placeholder like `$TOKEN`. A run of 32+ hex or 40+
  base64 characters (a digest) and the value after `token=` or `authorization:` are cut too.
- Fix a stale step in place with `edit` and date the correction; link another recipe as
  `[[name]]`.
- The user deletes one with Delete (`x`) in its `/skills` detail view or
  `/skills delete <name>`; no other skill can be deleted there.

## From Lua

```lua
zemi.memory.add("...", { scope = "project", quote = "...", replaces = id })
                                         -- queued under ask, origin user from config
zemi.memory.list(true)                   -- true includes superseded memories
zemi.memory.forget(id)                   -- user config only
for _, p in ipairs(zemi.learn.proposals()) do print(p.id, p.forget, p.text) end
zemi.learn.decide(id, "accept")          -- or "reject"; user config only
zemi.skills.list(); zemi.skills.enable("name"); zemi.skills.disable("name")
zemi.event.on("memory.written", function(ev) zemi.log.info(ev.text) end)
```

These return `nil, err` when the call fails at runtime (no such memory, proposal or skill;
a store error) and raise only on misuse (a bad argument, a scope outside the enum, an
origin that may not make the call).

Events: `memory.written`, `reflector.start`, `reflector.done`.
