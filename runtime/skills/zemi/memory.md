# How zemi learns

## Memories

Memories reach you as the `## Memory` section of the `# Brain` section in zemi's context message, after the system prompt: this project's, then every project's, newest first, each line with its id. The section is pinned at session start, so a new memory shows from the next session or compaction. Asked what you remember, read that section; never invent a memory.

- The `memory` tool's description has the writing rules; each scope holds 3 KB.
- When a scope is full, a stored line can move into a brain file: `memory {promote: id, path: "name.md", description: "...", read_if: "..."}`. The body is the line's text unchanged; the file's catalog line injects in every project the line did (a user-partition promotion) or in trusted projects (private). Under `ask` every promotion queues for review; under `auto` each applies at once, a line the user wrote and one in the user brain included. `memory {query: "words"}` searches both scopes, live and ledger lines, by text or id - the way to recall what the budget cut (its answer names the ids); a hit re-enters with `replaces`. A write a scope cannot fit even after cutting every learned line stores in the ledger trail alone: the answer says so, query recalls it, promote graduates it.
- `memory.autonomy` decides the queue, not the checks: `"ask"` queues every memory, promotion and brain file for review, `"auto"` (the default) applies each at once. Either way a near-duplicate is refused and a learned line's quote must be in the typed part of the session's user rows. Forgetting a memory the user saved asks (`memory.write`), whatever the setting.
- The reflector reads a session when it closes (`/new`, `/resume`), once it has had no turn for ten minutes in an open zemi, or at the next start once it has sat idle that long, if it has two new user messages and a tool call, or a failed tool call. One zemi reads a session's rows once: a start reads one idle session and a close skips a session that has gained no message since that start looked at it. It proposes memories under the same rules, never skills, and retirements of memories the session proved wrong, which apply at once under `auto` and queue under `ask` (a line the user saved is never retired). `/reflect` runs it now.
- The user reviews in `/memory`, where a row's keys act at once and `u` undoes; `/memory <note>` saves a note as the user's own, `/memory forget <id>` opens it there to confirm, and `/memory forget <id> --yes` drops it at once. The statusline counts the memories waiting for review.

## Recipes

A recipe (the environment's `Recipes:` line) is a procedure for this repository only, on this machine. A fact is a memory; a procedure for every project is a user skill (`skills.md`). Secret-shaped text there is redacted after a write. The user deletes one with `/skills delete <name>`. A recipe is for you: `user-invocable` does not apply to it.

## From Lua

```lua
zemi.memory.add(text, { scope = "project", quote = "...", replaces = id })
zemi.memory.list(true) -- true includes superseded memories
zemi.memory.del(id)
for _, p in ipairs(zemi.learn.list()) do
	print(p.id, p.forget, p.text)
end
zemi.learn.decide(id, "accept") -- or "reject"
zemi.skills.list()
zemi.skills.enable(name)
zemi.skills.disable(name)
```

A runtime failure returns `nil, err`; misuse raises. Events: `memory.written`, `reflector.start`, `reflector.done`.
