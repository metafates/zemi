# Past sessions

Every conversation is a session of the project it ran in, with an id, a title and its messages. It belongs to its project: it resumes there, and its tool calls and checkpoints are there.

## Find

- `zemi sessions -a` lists every project's sessions, grouped by project, newest first.
- `zemi sessions search <words>` finds sessions by what was said in them, across every project: every word must appear in a prompt or a reply, case-insensitive, anywhere. It prints the session, its hit count and the newest matching line, and exits 1 when nothing matches. It searches text only, never tool output; a common word returns a lot, so narrow it with more words.
- Titles match fuzzily, so a half-remembered one lands; messages match on whole words only. Search returns lines, not conversations: search, then export the session that looks right, then answer from that. In the TUI the same search is `/sessions search <words>`, and picking a result resumes it.

## Read

- `zemi sessions export <id>` prints one session as markdown; `--full` adds reasoning, tool calls and subagent runs.
- Another project's session reads through that project: `zemi -C <project root> sessions export <id>`, with the path the group header `search` printed.
- Quote what the export says and name the id, so the user can resume it.

## Resume

`zemi -C <project root> -r <id>` resumes a session (`-r`, `--resume`).

## The store behind them

For what the commands do not expose - totals, a session's tool calls, config picks - read the store through `zemi db`: `db.md` has the grammar, the schema and the read-only guarantee. Do not open the `.db` with another tool.
