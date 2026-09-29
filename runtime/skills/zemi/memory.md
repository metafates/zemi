# How zemi learns

## Memories

Memories reach you as the `# Memory` section of the system prompt: this project's, then every project's, newest first, each line with its id. The section is pinned at session start, so a new memory shows from the next session or compaction. Asked what you remember, read that section; never invent a memory.

- The `memory` tool's description has the writing rules; each scope holds 3 KB.
- Under `memory.autonomy = "auto"` (the default) a memory stores at once unless it is instruction-shaped, replaces one the user saved, comes from an untrusted project, or comes from a session that used `fetch`, `web_search`, the `lua` tool or a tool zemi does not ship (MCP, `zemi.tool`), directly or through a subagent. Those wait for review; `ask` queues every one. Forgetting a memory the user saved needs the user's approval.
- The reflector reads a session when it closes (`/new`, `/resume`), once it has had no turn for ten minutes in an open zemi, or at the next start once it has sat idle that long, if it has two new user messages and a tool call, or a failed tool call. It proposes memories under the same rules, never skills, and retirements of memories the session proved wrong, which always wait for review. `/reflect` runs it now.
- The user reviews in `/memory`, whose footer names its keys; `/memory <note>` saves a note as the user's own, `/memory forget <id>` opens it there to confirm, and `/memory forget <id> --yes` drops it at once. The statusline counts the memories waiting for review.

## Recipes

A recipe (the environment's `Recipes:` line) is a procedure for this repository only, on this machine. A fact is a memory; a procedure for every project is a user skill (`skills.md`). A write there needs the user's approval in every mode but yolo, and secret-shaped text is redacted after it. The user deletes one with `/skills delete <name>`. A recipe is for you: `user-invocable` and `disable-model-invocation` do not apply to it.

## From Lua

```lua
zemi.memory.add(text, { scope = zemi.memory.scope.PROJECT, quote = "...", replaces = id })
zemi.memory.list(true) -- true includes superseded memories
zemi.memory.del(id) -- user config only
for _, p in ipairs(zemi.learn.list()) do
	print(p.id, p.forget, p.text)
end
zemi.learn.decide(id, "accept") -- or "reject"; user config only
zemi.skills.list()
zemi.skills.enable(name)
zemi.skills.disable(name)
```

A runtime failure returns `nil, err`; misuse raises. Events: `memory.written`, `reflector.start`, `reflector.done`.
