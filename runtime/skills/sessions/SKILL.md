---
name: sessions
description: "Use when the user asks about something said in an earlier conversation, or wants to find, read or resume a past session: zemi stores every conversation per project and can search it."
---

# Past sessions

zemi keeps one SQLite store per project, `<state>/projects/<hash of the trust root>/zemi.db` (`ZEMI_STATE_HOME`, default `~/.local/state/zemi`). Every top-level conversation is a session with an id, a title and its messages. A session belongs to the project it ran in: its tool calls, checkpoints and scratchpad are there, so it resumes there.

## Find

- `zemi sessions -a` lists every project's sessions, grouped by project, newest first; `--json` adds `project`.
- `zemi sessions search <words>` finds sessions by what was said in them, across every project: every word must appear in a prompt or a reply, case-insensitive, anywhere. It prints the session, its hit count and the newest matching line (`title` and no line when only the title matched; `--json` adds `hits` and `match`), and exits 1 when nothing matches. It searches text only, never tool output; a common word returns a lot, so narrow the search with more words.
- Titles match fuzzily, so a half-remembered one lands; messages match on whole words only.
- Search returns lines, not conversations: search, then export the session that looks right, then answer from that.

## Read

- `zemi sessions export <id>` prints one session as markdown; `--full` adds reasoning, tool calls and subagent runs.
- Another project's session reads through that project: `zemi -C <project root> sessions export <id>`, with the path the group header `search` printed.
- Quote what the export says and name the id, so the user can resume it.

## Resume

`zemi -C <project root> -r <id>` resumes a session (`-r`, `--resume`). In the TUI the same search is `/sessions search <words>`: picking a result resumes it, restarting zemi in that project when it was another one.

## Rules

- Never read or query the `.db` files yourself: the schema is internal, zemi refuses a store another version wrote, and the commands are the supported path.
- A session's scratchpad and approved plan live beside its store; `/projects delete` removes a project's data.
