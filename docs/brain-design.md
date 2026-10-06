# Brain: disk-native memory for zemi (design)

Companion to `docs/brain-design-brief.md`, which frames the problem and the requirement. This
document is normative for implementation; `PLAN.md` carries the staged order and gates. Every
file:line citation was verified against this repository on 2026-10-05; re-verify at implementation
time.

## 1. Verdict

Replace the SQLite `memory` table and its digest with a partitioned Markdown brain on disk, entirely
inside zemi's own state: a private partition keyed by TrustRoot next to today's project DB, and a
user partition under the data home. No repo-side directory exists - nothing of the brain is ever
created in a repository, so no byte of it can be written by a pull; committed team knowledge stays
in the repo's own files (the AGENTS.md chain, docs). The brain carries knowledge and maps, and no
standing-instruction tier: standing instructions stay where they are (the user's ConfigHome
AGENTS.md, the repo's AGENTS.md/CLAUDE.md chain); outside yolo the only agent-authored standing
instruction that passes without the user's approval goes through the review queue.

One-liner memories survive as lines in a reserved `memories.md` per state-dir partition, so intake,
quote verification, autonomy, review, reflector, undo, rewind and fork deltas all keep working with
their storage swapped. A scope past its 3 KiB budget cuts its oldest learned lines to the audit
ledger and never auto-cuts a user-attested line: an apply whose cut set would include an origin=user
line queues instead (D1).

The ledger is the SQLite zemi already runs: an append-only `memory_ops` table in each partition's
own DB holds quote, provenance, supersession, prior text, prior file bytes and a per-line text
hash. It is audit trail, not a second store - nothing reads it to build the prompt - and its insert
precedes the file replace under the same flock. Nothing compacts; the pre-migration `memory` table
freezes as the superseded trail and is never dropped.

The memory tool and the reflector's gated apply (D8) are the only writers of state-dir brain paths
that settle without asking; the memory tool's file mode is shape-gated like a one-liner and never
auto-overwrites a file the session did not write. Retrieval gets teeth: a brain file may declare
`paths:` globs, and the first read - or write - of a matching path earns a one-line zemi pointer
through the hook nested AGENTS.md already uses.

Constraint check: no data-loss path (lock + ledger-first writes + atomic replaces + prior bytes on
every file-mode replace + reconcile-on-open migration that never resurrects a hand-deleted line + a
frozen table no stage drops + rewind that locks before truncating or restoring); no prompt-injection
regression (untrusted, the injected memory lines are byte-equal to today's digest and the rest of
the render is user-owned state-dir content, D3; injected surfaces pass one-liner intake;
user-partition file writes ask in every mode, yolo aside); no unbounded growth (per-section caps, a
terminating backstop, render-time cuts, a bounded and file-safety-checked catalog scan); no cache
hostility (context message only, hash-pinned, built at the build points only (D4); pointers ride tool
results outside the pinned prefix); no second CLI. Net: the store's write path and the digest
renderer are deleted, the read path stays as the frozen-table reader, and one new `internal/brain`
package is added. The win is the capability - codebase maps, long-form knowledge, host-enforced lazy
reads, one store for both memory kinds - not a line-count reduction.

## 2. Decisions

### D1. One storage mechanism: files are the store; one-liners become `memories.md` lines

The Markdown partitions are the memory store. One-liner memories are lines in a reserved freeform
file `memories.md` in each state-dir partition (private project + user). Line shape:
`- [id origin ISO-date] text`; the date is the line's last write - created for a new line; an edit
refreshes it and re-prepends the line (newest-first by last write, the digest's own updated_at
order, store/memory.go:94) - so the rendered age tracks the line, not its first save (today's digest
ages from UpdatedAt, internal/learn/digest.go:60-68), and migration's
age preservation and undo's age-preserving restore hold. The renderer emits `- [id origin Nd] text`,
byte-identical to today's digest line (internal/learn/digest.go:51). Ids are preserved verbatim.

The ledger is zemi's own SQLite: an append-only ops table (`memory_ops`: op, id, ts, quote,
provenance_session_id, prior text, prior file bytes for file-mode writes, text hash) created lazily
with CREATE TABLE IF NOT EXISTS in the DB the scope already uses (project scope in the project DB,
user scope in the global DB), inserted before the file replace; "the ledger" below means this table
plus the frozen `memory` table, its pre-migration half (section 10). INSERT-only and never
compacted, its import entries are permanent tombstones - the marker that keeps a hand-deleted line
from re-importing (section 8). The pre-migration `memory` table freezes and no stage drops it
(section 10).

Budget as promotion: when an apply pushes a scope past its 3 KiB budget (brain.ScopeBudget, charged
at internal/hostapi/memory.go:1032-1051), the oldest learned lines are cut from `memories.md` - not
lost: each cut line's text, quote and provenance live on in its ledger entries, `/memory` lists cut
ids in a collapsed group, and the tool result names them, replacing today's oldest-ids hint
(memory.go:1044-1050). An overflow whose cut set would include any origin=user line queues the write
instead (memory.go:1020-1021; DESIGN.md:2932-2934 forbids silently dropping a user-attested memory).
GetMemory, forget, supersede, `locateMemory` (hostapi/memory.go:790-802), the overlay and the
reflector's dedup scan `memories.md` plus the ledger. Supersede removes the old line from
`memories.md`; its text, quote and the superseding id trail in the supersede ledger entry: the hot
file holds live lines only, and the trail stays resolvable by id.

Concurrency: cutMu (rewind/reflector, rewind.go:171-177) is outer, the partition flock inner, in
every writer. SQLite serialized writers (the INSERT at internal/store/memory.go:75); a Markdown file
read-modify-write does not, and two windows on one project are a designed topology
(internal/hostapi/projects.go:286-290 refuses a delete only while another zemi holds the store;
internal/config/dirs.go:307-308 documents the same hazard for trust.json). Every write to a
partition's stores - memory-tool apply (text and file mode), reflector apply, `/memory`
move/edit/undo/forget, supersede, budget cut, migration, rewind's `forgetLearned` and its checkpoint
restore of brain files, the AfterWrite scrub of `memories.md` (D5) - holds an exclusive flock
(syscall.Flock, LOCK_EX) on `<brain>/memories.lock` for the whole operation and replaces the file
with WriteFileAtomic (the trust.json precedent, internal/config/dirs.go:314). The ledger insert goes
first, both inside the lock: a crash between the two leaves the D6 divergence finding - an entry
whose id is missing from the file - never a live line whose provenance does not exist, so rewind can
always forget what it can see. Every removal appends its op entry under the same lock - `/memory`
forget, undo's removals and rewind's `forgetLearned` write `forget` per removed id (D7) - so the
absence finding (D6) always has an explainer. A blocked
writer waits up to 2s, then fails with "another zemi is writing the memory store" (the
ClaimForDelete UX, projects.go:288-290). A cross-partition move acquires in fixed order, user then
private, and inserts ledger entries in both DBs (move-out, move-in) so replay reconstructs the moved
line's provenance; a lost update is the D6 absence finding, visible, not silent.

Rejected: SQLite tier-0 + files tier-1 - two sources of truth, two trust stories, two budgets; the
ledger adoption is not that: nothing reads the ledger or the frozen table to build the prompt, the
file is the only injected, budgeted, flocked thing. Rejected: drop one-liners entirely - it orphans
the quote-verified user-scope policy and instruction-shape routing, and every id-addressable surface
(`replaces`, `/memory forget`, reflector supersedes) loses its key. Rejected: a JSONL journal file -
a hand-rolled ledger paying for torn-tail semantics, a compaction ceiling and an append-vs-rename
race analysis that SQLite gives for free in the DB zemi opens anyway.

### D2. Partition roots and repo impact

- Private project partition: `~/.local/state/zemi/projects/<hash-of-TrustRoot>/brain/` - the same
  hash key as today's project DB (the recipes precedent, internal/hostapi/skills.go:198-204),
  sibling to that DB. zemi already maps linked worktrees to the main checkout
  (internal/config/dirs.go:317-331), so one brain per repo survives across worktrees, and the brain
  sits where a pull cannot write.
- User partition: `~/.local/share/zemi/brain/user/`. Rejected `~/.operator/user/` (the
  operator-memory default): two harnesses with different policy layers writing one directory means
  neither's guarantees hold - operator-memory injects unscrubbed and unbudgeted. The format stays
  compatible, so an opt-in `brain.userRoot` override or symlink can come later (open question 1).
- No shared partition: the owner wants the brain fully machine-local. There is no repo-side
  directory, no `.operator-shared/`, and no committed brain content - the trust question "what may
  a pull write into the prompt" has an empty answer by construction.

Hard line 10.8 (DESIGN.md:3097) is renegotiated explicitly, with the permission machinery named and
nothing loosened: "never writes outside its homes - the memory store, the brain partitions (private
via the memory tool's file mode or an approved file-tool write, user via the file tools only when
the user approves), the skills dir". Concretely:

- `readJails()` (internal/hostapi/io_lua.go:98-109) gains the private brain root for trusted
  projects, the same trust-gated pattern RepoRoot uses today (io_lua.go:103-107), and the user brain
  root unconditionally, the ConfigHome pattern (io_lua.go:101): the user partition is user-owned
  state a pull cannot write (D3), so its lazy reads settle in every project and the catalog it
  injects is readable.
- `writeJails()` is untouched (ProjectDir-only, io_lua.go:111-113). No brain root enters it: a
  file-tool write to any state-dir brain path keeps today's protected ask - the zemiDirs case in
  `protectedWhy` (permmode.go:522-523), never settled by rules or decide (ask.go:76-77; base yolo
  settles every surviving ask unconditionally, ask.go:114-115, DESIGN.md:2419 - the shared
  protected-path ceiling, DESIGN.md:2714), no "always" offered (DESIGN.md:2936-2938) - and the ask's
  outside-text note (ask.go:172-176) extends from the recipes dir to the brain roots. Outside yolo,
  a session that read fetched or web text cannot silently write standing instructions anywhere: no
  brain file's body injects, injected frontmatter passes the injected-surface intake (D5), and a
  state-dir write needs the user's ask.
- `protectedWhy` (permmode.go:502-526) gains exactly one case, before the zemiDirs case: a dedicated
  reason for the reserved `memories.md` under the state-dir brain roots, private and user. It adds a
  message, not a settlement - the write stays protected.
- The sandbox is untouched: the brain roots sit inside the zemiDirs the policy already Protects and
  Hides (internal/hostapi/sandbox.go:107,128-133), so a settled shell command can neither write nor
  read the corpus. File-tool reads are gated by readJails (the permission layer, not the sandbox);
  the loop says "read", never "grep", for the brain.

Everything else in 10.8 stands.

### D3. Trust

One rule per surface, no protocol:

- State-dir `## Memory` lines (private + user) inject in every project exactly as the digest does
  today: `digestFor` renders all live rows regardless of trust (hostapi/memory.go:826-831), and the
  10.4 gate governs writes, not injection (DESIGN.md:3098-3100).
- The user partition's catalog injects in every project too: it is user-owned state-dir content a
  pull cannot write, the same trust class zemi already injects untrusted - the user's ConfigHome
  AGENTS.md, collected before the trust check and injected in every project ("The user's own, for
  every project", prompt.go:262, 338-345) - and gating it would make the user brain strictly less
  available than its own memory lines. The private partition's maps inject in trusted projects only:
  its write path asks the user every time outside the memory tool (D2) and untrusted sessions'
  proposals queue, so trusted-only injection costs nothing the user partition does not already cover
  untrusted. Rejected: tracked-and-clean gating - it flaps the context hash (cache-hostile) and buys
  no threat-model delta.
- The catalog scan meets AGENTS.md's file-safety bar: the scan applies `instructionFile`'s checks -
  regular file, not secret-shaped, inside the partition's jail (prompt.go:485-488) - to every
  cataloged file, so a symlink planted in a brain dir cannot feed a target outside the partition (or
  a secret-shaped file) into the catalog, and the render strips control characters from the rendered
  path as from description/read_if (prompt.go:605), so a filename can neither break the
  one-line-per-file format nor carry multi-line text.
- `<brain-warning>` lines are zemi-generated findings, never brain file content, and they leak no
  repository free text: in an untrusted project the private partition is not linted at all and no
  finding names a repo path - the same posture that cuts the environment before "Recent commits"
  (internal/hostapi/prompt.go:543-548). Findings render single-line with control characters stripped
  (the topLevel precedent, prompt.go:580-583). A golden test pins the untrusted render (section 4):
  no byte from the private partition's catalog, map files or findings - the digest-equivalent memory
  lines and the loop's root naming are the exceptions - no repo path in any warning; the user
  partition is the only one linted untrusted. The `## Memory` lines are byte-equal to today's digest
  lines (digest.go:51) under the same scope headings when the store is healthy - the age computed
  from the line's day-granular last-write date, D1's own semantics, so a row written in the last two
  days can read a day older than the digest's timestamp math; the digest's
  `# Memory` H1 (digest.go:18) is not duplicated - `## Memory` replaces it.

Writes from untrusted or outside-text sessions always queue and inject only on the user's accept -
today's semantics.

### D4. Injection economics and cache

All brain content rides the context message, never the system prompt (the system prompt must stay
per-user stable, internal/hostapi/prompt.go:279-282); built once at session open - new session at
agent.go:1711, resume at session.go:105-118 - rebuilt at compaction (compact.go:374-385) and at a
mid-session trust flip (SetProjectTrusted, host.go:604, which a revoke's next send would otherwise
re-pin stale, D3), hash-pinned, never per send. `contextKey` (internal/hostapi/agent.go:506-512) hashes the brain
render where it hashed the digest; the stored role-"context" row is written on the next send
(agent.go:590) and replays its bytes while the key matches, and the pin/drop machinery around it is
untouched. A mid-session brain edit changes nothing until the next build. (The brain does not join
`promptParts.files`: that map feeds the system-prompt pin, not the context row, prompt.go:431-434.)
Forks re-send the caller's prefix under the caller's cache key and keep per-fork `reflectSeen`
deltas (export.go:355-359) - the brain render is just the new digest payload, so the fork-delta
property survives. Path-triggered pointers are tool-result text appended after a read or write -
outside the pinned prefix, no cache impact (D10).

Budgets: per-section caps (section 3) plus a 32 KiB backstop on the whole `# Brain` section. The
backstop cut order terminates: catalogs to zero; what can remain
(loop text 1 KiB, warnings 1 KiB, memory lines 6 KiB worst case, 3 KiB per scope) always fits.
Rejected: a single global pool with no per-section caps (one fat catalog starves the rest). Every
cut marker names a discoverable location, so a dropped entry is never undiscoverable.

Enforcement: memory lines are budget-enforced at write time (ScopeBudget 3 KiB per scope at intake,
internal/hostapi/memory.go:1032-1051, by cutting learned lines to the ledger, D1) and again at
render time - a hand-edited or corrupted `memories.md` is cut at the same budget with a marker
naming the file, and a lint finding names the partition: the renderer cannot cut the store, but it
never injects past the cap, so the backstop invariant holds for any store state. Maps cannot be
meaningfully refused at write time - the cap is a property of an aggregate of files - so they are
enforced at render time with cut markers, the contract instruction files already have
(prompt.go:289-311); a hard write-time refusal would make a legitimate file unwriteable.

Resume recomputes the render - the digest's own designed trade: session.go:105 calls `digestFor()`,
session.go:118 stores it, and contextKey exists "so a memory dropped since is not sent again"
(agent.go:506-508). The 32 KiB backstop bounds any rebuild; within a session the context message
stays byte-stable while the brain is unchanged.

### D5. Tool surface

The `memory` tool stays the only model-facing writer of `memories.md` that settles without asking -
the reflector applies through the same gate (D8), and `/memory` keys, migration, rewind and the
AfterWrite scrub are the user's and zemi's own hands (D1, D6) - and its file mode is the only
brain-file write path that settles without asking; its intake policy is intact and now covers
everything the render injects.

- Text mode (unchanged shape): text, scope, quote, replaces -> a `memories.md` line + ledger entry,
  under the D1 lock, all intake rows as today.
- File mode (new): `path` (must resolve inside the private partition's brain dir) + `content`.
  Reserved names refuse: `memories.md` with "the memory tool owns this file". Freeform files apply
  as below. Creation applies; an overwrite of a file this session did not write - by file mode or
  by an approved file-tool write - queues like row 15 (the checkpoint hook never sees these writes,
  agent.go:2665-2667, so the prior version must not be silently destroyed); every later write by the
  same session applies when the file still matches the write set - the gate consults the session's
  write set, populated by file-mode applies and by AfterWrite on approved file-tool writes, each
  entry recording the file's hash at apply time and cleared on rewind; an apply whose file's current
  hash differs queues like row 15 (a foreign edit happened, the staleness rule the file tools apply
  through Seen) - and the op=write entry
  carries the replaced file's prior bytes (capped at the 32 KiB content cap, truncation flagged) for
  rewind to restore (D7, section 7); a flagged restore prefers the approved file-tool write's
  checkpoint when one exists, else restores the truncated bytes with a note.
- Injected-surface intake: the derived catalog injects `description` and `read_if` verbatim, so
  those fields get what a one-liner gets - control characters and newlines stripped, a 200-char cap
  per field, and the recall-biased instruction-shape check (memory.go:1013, DESIGN.md:2910-2914);
  shaped or oversized frontmatter queues the write like a one-liner (rows 14/15). File bodies are
  lazy-read and exempt - a body is not a claim. Content is capped at the 32 KiB backstop; the secret
  scan and scrub run on content as on one-liner text (a secret refuses and names the env var,
  intake.go:44-46); missing frontmatter applies and is named by the lint note. Apply replaces the
  file (WriteFileAtomic) under the D1 lock and settles under `memory.autonomy` exactly as a learned
  one-liner does - `memoryAuto` (trust + local-only, internal/hostapi/memory.go:1077-1079) and the
  same 20-per-session auto budget (`autoApplyBudgetLeft`, memory.go:1017-1018) - everything else
  queues as a pending proposal whose accept writes the file.
- User partition: file mode refuses with "user-brain files are written with the file tools; those
  writes ask" - the user brain asks in every mode, yolo aside (DESIGN.md:2936-2938).

File-tool writes (write, edit, LSP rename (internal/tools/lsp_tool.go:45-46)) to state-dir brain
paths never settle in any mode (yolo aside, D2) and ask with the outside-text note; once approved
they are scrubbed post-write. Lua `zemi.fs.write` (io_lua.go:519-525) is rejected earlier, at jail
resolution, and is never asked (row 16 and below).

The reserved file asks, and what the user approves gets scrubbed: `protectedWhy`'s dedicated case
(permmode.go:502-526) gives `memories.md` under the state-dir brain roots the reason "the memory
tool owns this file" - a protected ask in every mode (yolo aside, D2), which no allow rule or decide
settles (ask.go:76-77; the protected-write skip is tested at agent_test.go:1897) and which the user
may approve (row 17: AfterWrite lints it, and a malformed line is dropped from the render) or deny.
The file tool runs no intake, so the approved write gets the recipe treatment (DESIGN.md:2940-2944):
the AfterWrite scrub below is its secret check, `brain.Lint`'s instruction-shape finding its shape
check, and the ask's reason appends the file's open lint findings. Lua needs no case:
`zemi.fs.write` arrives as subject "lua.fs.write" (io_lua.go:484-495) and dies at jail resolution
(D2; row 16). Residual ceiling, stated the
way DESIGN 10.4 states its own: a command the user lets run unconfined can write anything on the
machine; the gate is the permission layer, not a physical one.

The AfterWrite hook (internal/tools/tools.go:284-287, wired at agent.go:2613-2619 - it "may rewrite
the file") extends to brain paths. On freeform files it runs `learn.ScrubShapes` + `brain.Lint`, its
rewrite taken under the partition flock, its note appended to the tool result. On `memories.md` it
takes the D1 lock, re-reads after acquiring it, and runs the same scrub - an unlocked
read-modify-write would write stale bytes over a concurrent window's locked apply - appending an
edit ledger entry per changed line (prior text and the new hash, D6) so the hash finding stays
honest. It redacts, never drops: instruction-shaped lines are a detection-only finding, not a
rewrite. No `brain` tool: it would re-implement write/edit with path-jail tweaks. No helper CLI:
init, lint, check and repair become native host behavior; zemi gains no mandatory npm CLI.

Near-dup stays memories-only in both modes, instruction-shape runs on frontmatter fields in file
mode.

### D6. Maintenance and lint

One lint function, `brain.Lint(dir) []Finding`, seven findings: a brain file with missing/empty
`description` or `read_if` frontmatter - until fixed the catalog renders a one-line self-repair note
in its place (operator-memory preamble.ts:200-205) and the file triggers nothing - or an invalid
`paths:` glob, which leaves the file visible in the catalog but untriggered; a malformed
`memories.md` line (dropped from the render, never injected); a ledger add or import entry whose id
is missing from `memories.md` with no later entry explaining the absence (supersede, forget, budget
cut, move-out - every removal writes its entry, D1) - divergence: a lost update, a crash between
ledger and file, or a hand edit, and the finding names the benign case; a `memories.md` line whose
text hash matches no ledger entry recorded for its id (an unjournaled rewrite of a live line - a
hand edit is the benign case and the finding says so); a partition's injectable render over its cap
- a `memories.md` whose lines exceed ScopeBudget at render (the render cuts; the marker names
`memories.md`, D4) or a catalog scan past its file ceiling (the marker names the partition, section
3), the finding naming the partition; a `memories.md` line that is instruction-shaped
(learn.InstructionShaped, learn/instruction.go:28) - detection only, the line still injects flagged,
because silently dropping content the user approved is the review queue's job, not the linter's; a
cataloged file whose description or read_if exceeds the 200-char render cap (a hand edit; the render
truncates in place and the finding names the file). Four homes for findings: session-start
`<brain-warning>` lines (1 KiB cap), the AfterWrite note on a brain write, a `/health` row, the loop
text's standing instruction; drift is repaired by the model or the user.

zemi never rewrites brain knowledge files - freeform content reaches disk only through the gated
write paths (memory tool file mode, approved file-tool writes). `memories.md` is rewritten only by
the ops D1 lists, all under the D1 lock - the post-write scrub of approved writes included (D5):
skipping it as "zemi never writes" would skip a security step. No `zemi brain check` subcommand:
`/health` plus injected warnings cover
repair; revisit only if they prove insufficient (open question 2).

### D7. Provenance and audit

The `memories.md` line carries id, origin, and its last-write date inline; the renderer computes `Nd`
at injection so stored lines never go stale. Quote, provenance_session_id, replaces, prior text,
timestamps and a sha256 of the text live in the ledger (D1): the ops table for everything written
after the swap, the frozen `memory` table for the rows that predate it - the schema that already
carries every field (store/memory.go:35-48). One `op` field: add, supersede (prior text and the
superseding id), forget (`/memory` forget, undo's removals, rewind's `forgetLearned`), edit (prior
text; `/memory` edit and the AfterWrite scrub write it), write (file-mode path, the replaced file's
prior bytes capped at the 32 KiB content cap with a truncation flag, and an existed flag - the
rewind.go:45 precedent - since a creation has no prior bytes to restore; the bytes the checkpoint hook never
sees, D5), move-out,
move-in, archive (budget cut, carrying the acting session id), import. The text hash covers
`memories.md` line ops only; the scrub's edit entries carry the new hash, so D6's hash finding stays
honest. The ledger powers rewind's
`forgetLearned` (internal/hostapi/rewind.go:206-233 needs provenance plus created_at ranges, which
the line format does not carry) as a SELECT plus a file rewrite - the shape `DeleteLearnedBetween`
nearly is today (store/memory.go:117-123) - quote display in the overlay, `/memory` list details,
and migration's age preservation; `/memory` undo itself is closure-based (host.go:435-440) and needs
no ledger reads: the hash finding fires only when no entry recorded for the id matches the line's
current hash, so undo's age-preserving restore matches the original add entry and stays quiet (D6);
a removal writes its forget entry like every removal (D1). A cross-scope move inserts into both
partitions' ledgers (move-out, move-in; D1); brain files carry the ledger and mtimes for
provenance. Learned vs user stays visible in the injected
`[id origin Nd]` labels, in `/memory`, and in `/health`. Rejected: quote inline in the brain file -
it spends injection budget on text only review ever reads.

Growth is bounded by construction (one row per memory operation); the tombstone never erases (D1).

### D8. Reflector

Unchanged in shape: tool-less model call (nil tools, internal/hostapi/reflector.go:341), one
schema-validated JSON proposal doc, every entry through the same intake and gate, retirement always
queued (applyReflectProposal, reflector.go:354). Its view of live memories (dedup, retirement
targets) comes from `memories.md` plus the ledger (D1) instead of store rows alone; apply
appends/supersedes lines (the superseded text trails into the ledger entry, D1) and inserts the
entry, under the D1 lock. No file-edit proposals and no lint-fix proposals: the tool-less constraint
is what keeps the reflector unable to act; lint findings steer the live agent, which has the memory
tool and file tools (open question 3).

Failed applies lose nothing: today reflectRead marks the delta seen before apply
(reflector.go:345-354) and an apply error counts as skipped with only a Warn (reflector.go:614-617);
rewired, the delta is marked seen only after every entry has applied or queued, and an entry whose
file apply failed (2s lock timeout, rename error) queues as a pending proposal whose accept applies
it - the 2s flock timeout makes file-apply failure routine under two windows, where a SQLite store
error was practically impossible.

### D9. Migration

Reconciling, idempotent, rollback-safe: section 8 has the steps. Ids preserved verbatim, so
`replaces`, `/memory forget <id>`, Lua `del`, reflector `Supersedes`, move-merge re-pointing
(internal/hostapi/memory.go:620-629) and sessions holding old digest ids resolve by scanning
`memories.md` plus the ledger (D1), whose frozen half keeps every pre-migration superseded row
living. Stored context rows (role "context") keep replaying their old digest bytes; the next rebuild
renders from files - the same rule as any memory change today. Migration writes only state dirs; no
schema is bumped (the ops table is CREATE TABLE IF NOT EXISTS).

### D10. Relationship to existing concepts

- No instruction tier in the brain: standing instructions stay where they are - the user's
  ConfigHome AGENTS.md, first in instructionFiles (prompt.go:332-341), and the repo's
  AGENTS.md/CLAUDE.md chain - and the agent's own standing notes become instruction-shaped
  one-liners (learn.InstructionShaped, learn/instruction.go:28; they queue like row 2) or ordinary
  freeform files with Read If. Nothing brain-owned rides instructionFiles, so the instruction
  budget loop (prompt.go:289-311) spends nothing on the brain and the AGENTS.md chain's budget is
  untouched; the reflector's instruction cap (reflectInstructionsMax, reflector.go:406) is untouched
  too. Trust strengthens: no brain file's body is ever injected, so outside yolo the only door an
  agent-authored standing instruction passes unasked is the review queue, whose intake already runs.
  Nothing to
  migrate; the format never shipped.
- Path-triggered recall, read and write sides: a brain file's optional `paths:` frontmatter globs
  arm the read-triggered hook nested AGENTS.md already uses (Pipeline.Instructions,
  tools.go:280-282; mainInstructions -> nearestInstructions, agent.go:2607-2611 and
  prompt.go:364-369, wired at agent.go:2672; once per conversation via instructionSent,
  prompt.go:644-646; the guarantee, instructions_test.go:10). The first read of a path matching a
  glob appends one fixed zemi-generated line naming the brain file and its Read If (the Read If text
  capped at the 200-char render constant - the 3-per-result cap bounds count, not size): lazy
  retrieval stops depending on model discipline. The same matcher arms AfterWrite (tools.go:284-287,
  wired at agent.go:2613-2619, already extended to brain paths): the first write or edit of a
  matching path appends "you edited a file mapped by <brain file> (Read If: ...); check the map
  still holds" - the edit that invalidates a map is the moment the agent holds the tool result. At
  most 3 pointers attach to one tool result; further matches collapse into one aggregate line naming
  the count, so a tool result cannot earn unbounded text. Pointers are tool-result text - outside
  the pinned prefix, no cache impact - and the glob table is built only where the catalog renders,
  so an untrusted project fires only user-partition pointers, reads and writes alike (D3).
- Skills and recipes stay separate. A freeform file may reference a skill, not replace it: skills
  have discovery, enable/disable, origin gating and the skill tool's loader.
- The recipes' secret-scrub mechanism serves brain files at both write surfaces (D5): pre-write
  intake on the memory tool, post-write scrub on approved file-tool writes, the reserved file
  included; like recipes, rewind reverts brain writes (DESIGN.md:2948).
- `/init` keeps writing AGENTS.md; the brain seeds lazily - first write creates the partition,
  `/memory <note>` creates `memories.md` - so there is no init command to delete later. The loop
  text carries the seeding instruction (section 4).

### D11. What gets deleted

Section 10 has the inventory. Net honest accounting: the write path of `internal/store/memory.go`
(6 of 9 functions) and `internal/learn/digest.go` are deleted; the store's read path stays as the
frozen-table reader; `internal/brain` is added (paths, frontmatter, load, render, memories, ledger,
lint, lock - roughly 430 lines with tests). The live store count goes from one (SQLite) to one
(files); the audit ledger stays the SQLite zemi already runs; the capability set grows. Anyone
promised fewer lines should not implement this.

## 3. Brain layout

```
~/.local/state/zemi/projects/<hash-of-TrustRoot>/   private partition (state dir; a pull cannot write it)
  brain/               maps and knowledge
    memories.md        one-liner store; RESERVED: memory tool only; flock-protected
    *.md               freeform knowledge; frontmatter required (catalog is derived)
  zemi.db              project DB: proposals + the frozen memory table + the memory_ops ledger
~/.local/share/zemi/brain/user/     user partition (same brain/ shape; ledger and frozen table
                                    live in the global DB, ~/.local/state/zemi/zemi.db)
```

No repo-side directory exists: the brain is entirely machine-local state, so a pull can never write
any byte of it.

Frontmatter: required on every brain `.md` except the reserved file - `description` and `read_if`,
both non-empty free text, plus an optional `paths:` glob (one glob or a YAML list) that arms
path-triggered recall (D10). Parsed with `skill.SplitFrontmatter` (internal/skill/skill.go:123-137)
and the yaml dependency zemi already carries (go.mod:21); no bespoke reader. Injected fields are
single-line: intake strips control characters and newlines and caps `description`/`read_if` at 200
chars each (shaped or oversized queues, D5), and the derived catalog strips again at render
(prompt.go:580-583) and caps each rendered field at the same 200-char constant, so a hand-edited
multi-KiB field truncates with a lint finding instead of eating the 4 KiB catalog budget. The
catalog scan applies `instructionFile`'s file-safety checks and strips control characters from
rendered paths (prompt.go:485-488, 605; D3). `paths:` is lint-validated and not rendered in the
catalog - Read If carries the intent.

Reserved name per partition: `memories.md` (private and user); every other `.md` is freeform and
must carry frontmatter.

`memories.md`: one memory per line, newest first (new lines and edits prepended; budget cuts and
supersedes remove lines to the ledger, D1):

```
- [k3fj2m0a learned 2026-10-05] text up to 300 chars
```

Derived catalog (render time, per partition): one line per non-reserved `.md`, sorted by path -
identical content renders identically, so the context hash is stable across machines and checkouts
(mtime ordering rejected):

```
- repo-map.md - Map of this repository's packages. (Read If: before any code change.)
- writing-style.md - Cross-project writing style rules. (Read If: writing any user-facing reply.)
```

A file with missing/empty frontmatter renders a one-line self-repair note in its place and a lint
finding (D6). The scan is bounded: at most 64
non-reserved `.md` files and 256 KiB of frontmatter text per partition, parsed from each file's
first 4 KiB; past the caps the catalog renders its cut marker naming the partition and a lint
finding fires. NOTE: the ceilings only have to terminate a pathological tree. Budget table (bytes
of Markdown; every cut marker names a path):

| Injected piece | Cap | On overflow |
|---|---|---|
| Loop text | ~1 KiB, static string | never cut |
| `<brain-warning>` block | 1 KiB | drop the last finding, then truncate to one line |
| Catalog (derived), per partition | 4 KiB | tail cut (alphabetically last), marker names the partition dir; fields capped at 200 chars each at render |
| Memory lines, per scope | 3 KiB (ScopeBudget) | write-time: oldest learned lines are cut from memories.md into the ledger, note names them; an overflow that would cut an origin=user line queues (row 3); render-time: cut at the same budget, marker names memories.md, lint finding names the partition |
| `# Brain` total backstop | 32 KiB | catalogs to zero; what remains (loop, warnings, memory) always fits |


## 4. Injection

One section, one hash, in the context message, in the slot the memory digest held - after the environment:

```
# Brain

<loop text, every project; names the partition roots present>
- The maps below are derived from file frontmatter; bodies are not injected.
  Read a brain file when its Read If matches; zemi also points at one when
  a file you read or edit matches its paths. Consult the brain before you
  build, update it after.
- When you learn the codebase's shape, write or update a private map file
  for it so the next session starts oriented.
- Update what changed: private-partition maps via the memory tool (path and
  content), user-partition files with the file tools (those writes ask),
  one-liners via the memory tool. Keep frontmatter honest; a wrong Read If
  hides the file.
- One-liners cut by the budget leave this list; the memory tool still
  resolves their ids for replaces and forget.
- Standing instructions live in the AGENTS.md chains and the user's own
  AGENTS.md, not here; the user's messages win over every file. Never
  hand-edit memories.md; use the memory tool.

<brain-warning>...</brain-warning>   zemi-generated findings, 1 KiB; in untrusted
                                     projects, user-partition findings only,
                                     never a repo path

## Catalog                           derived from frontmatter, 4 KiB each
### user                             every project
### private                          trusted projects only

## Memory                            every project
<the digest header's two contract lines, verbatim (learn/digest.go:18, minus
 the digest's own "# Memory" H1, which ## Memory replaces)>
## this project                      3 KiB
## every project                     3 KiB
- [k3fj2m0a learned 2d] text
```

The loop text names the partition roots present - the private brain dir and the user brain dir,
once each exists (D10) - so file-mode paths resolve without guessing (operator-memory
preamble.ts:190, prompt.go:540-542); the roots are zemi's own text, so naming them untrusted leaks
nothing.

The digest header's two contract lines survive verbatim into `## Memory`:
"Lines earlier sessions kept, as [id who age]. Follow a preference the user stated there unless this
conversation or the project instructions say otherwise. Any other line held when it was written:
check the file, command or flag it names before relying on it." (learn/digest.go:18).

An untrusted project's `# Brain` is the loop text, the user partition's catalog (user-owned state,
so it renders in every project; lazily seeded, D10), `## Memory`, and `<brain-warning>` lines for
user-partition findings only (D3).

Behavior:

- Placement and build: the render rides the context message only, built once at session open and
  rebuilt only at compaction, never per send; `contextKey` replays the stored role-"context" row
  while the key matches (D4), the digest's freshness-over-cache trade - so memory-tool applies and
  brain file writes surface at the next compaction rebuild or next session; the memory tool result
  keeps today's wording ("shows from the next session", hostapi/memory.go:141), and the AfterWrite
  note says the same for brain writes.
- Path-triggered pointers: one zemi line per matching read and per matching write or edit, capped
  at 3 per result, once per conversation; in untrusted projects only user-partition pointers fire
  (D10).
- Forks: caller prefix re-sent under the caller's cache key, per-fork `reflectSeen` (D4).
- Load failure of any partition: `<brain-warning>` naming the path and the repair; never drops the
  other sections; never fails the session (the operator-memory diagnostic contract, minus its
  hostage text at preamble.ts:120 that orders the model to abandon user work - the warning never
  ranks memory above the user's request, and never names a repo path in an untrusted project).

## 5. Policy table

| # | Write path | Trust | Autonomy | Origin / condition | Result | Scrub |
|---|---|---|---|---|---|---|
| 1 | memory tool, text | trusted | auto | learned, policy-clean, <20/session, budget left | apply: line + ledger entry under flock | pre-write (intake) |
| 2 | memory tool, text | trusted | auto | instruction-shaped; replaces a user memory; replaces cross-scope | queue | pre-write |
| 3 | memory tool, text | trusted | auto | scope budget reached | apply; oldest learned lines are cut from memories.md into the ledger, note names them; an overflow that would cut an origin=user line queues instead | pre-write |
| 4 | memory tool, text | trusted | ask | any learned | queue | pre-write |
| 5 | memory tool, text | untrusted, or session read outside text | any | any | queue, never auto | pre-write |
| 6 | memory tool | any | any | secret in text | refuse, name the env var | pre-write |
| 7 | memory tool | any | any | secret in quote | redact; the memory proceeds through the gate as usual (the saved note names the cut) | pre-write |
| 8 | memory tool, text | any | any | near-duplicate (75% content tokens) | refuse, point at id for `replaces` | pre-write |
| 9 | `/memory <note>` | any | n/a | origin=user | apply; learned-only overflow cuts to the ledger with a note; an overflow that would cut a user line refuses pointing at the oldest, as today (DESIGN.md:2925-2926) | pre-write |
| 10 | `/memory` keys, `zemi.learn.decide` | any | n/a | user principal | apply, under the same budget rule as row 9 | pre-write |
| 11 | `zemi.memory.add` (Lua) | trusted | as caller | model path = learned; user config = user | same rows as memory tool | pre-write |
| 12 | reflector entry | trusted | auto | learned, policy-clean | apply (memoryAuto gates trust, memory.go:1077-1079; DESIGN.md:3098-3100) | pre-write |
| 13 | reflector entry | untrusted, or any trust | any | retirement, quote-missing, instruction-shaped, failed file apply | queue (retirement always; untrusted always) | pre-write |
| 14 | memory tool, file mode (private-partition freeform files) | trusted | auto under memory.autonomy, local-only session, <20/session | policy-clean; frontmatter single-line, within 200-char fields, not instruction-shaped; creation, or overwrite of a file this session wrote whose hash still matches the write set | apply: WriteFileAtomic + ledger entry recording the prior bytes + lint note | pre-write (intake) |
| 15 | memory tool, file mode | any | any | shaped/oversized/multi-line frontmatter; overwrite of a file this session did not write, or one whose hash no longer matches the write set | queue; accept writes the file | pre-write |
| 16 | write/edit/LSP any state-dir brain path | any | any | any | ask every time (protectedWhy zemiDirs case, permmode.go:522-523), never settles (yolo aside, ask.go:114-115, DESIGN.md:2714), no "always"; ask carries the outside-text note; Lua fs.write is rejected at jail resolution, never asked (io_lua.go:111-113, 636-638) | post-write once approved: scrub on freeform files, scrub + edit ledger entry on memories.md (row 17) |
| 17 | any model write to a state-dir partition's `memories.md` via write, edit or LSP rename | any | any | any | never settles (yolo aside): protectedWhy dedicated case makes it a protected ask in every mode ("the memory tool owns this file"); no allow rule or decide settles it (ask.go:76-77); base yolo settles unconditionally (ask.go:114-115, DESIGN.md:2419); the ask appends the file's open lint findings; Lua fs.write dies earlier at jail resolution (io_lua.go:111-113, 636-638); sandbox Hide as backstop | post-write ScrubShapes under the D1 lock, edit entry per changed line, + brain.Lint (no intake runs on this path; instruction-shaped lines are detection-only findings, D5) |
| 18 | user hand-edit (terminal) | n/a | n/a | any | allowed; state-dir lines inject as today's DB rows would; divergence is a lint finding (including a hash mismatch on an edited live line) | none - the user principal; the one unscrubbed surface |

Who scrubs what: the memory tool, `/memory`, Lua and reflector paths scrub pre-write via intake;
approved state-dir writes scrub post-write via AfterWrite (D5); instruction-shaped lines are
detection-only findings; the terminal hand-edit is the one unscrubbed surface (row 18); the ledger
is never injected and needs no scrub.

## 6. Surface changes

Tools:
- change: `memory` tool (internal/tools/tools.go:1332) - text mode takes the D1 lock and writes
  `memories.md` + a ledger entry; file mode added (path + content, private partition only,
  injected-surface intake per D5, overwrite gate, settles under `memory.autonomy` and the 20/session
  cap).
- change: `protectedWhy` (permmode.go:502-526) - one added case giving `memories.md` a dedicated
  never-settles (yolo aside) reason, with the file's open lint findings appended to the ask; the
  zemiDirs case is untouched (no carve-out anywhere).
- change: `readJails` (io_lua.go:98-109) gains the private brain root for trusted projects and the
  user brain root unconditionally; `writeJails` unchanged (io_lua.go:111-113); the outside-text ask
  note (ask.go:172-176) extends to the brain roots; sandbox untouched (D2).
- change: the Instructions hook (tools.go:280-282, wired at agent.go:2607-2611, 2672) matches brain
  `paths:` globs for reads and AfterWrite (tools.go:284-287) for writes - fixed zemi pointer text,
  at most 3 per result, deduped (prompt.go:644-646); AfterWrite also scrubs and lints brain paths
  (D5).
- change: the lsp tool's rename path runs AfterWrite for every file it rewrites (today it runs for
  the named path only, lsp_tool.go:255; the protected asks already fire per file, lsp_tool.go:236),
  so a brain file a rename edits as a secondary target gets the scrub, the lint note and the
  write-set entry.

Commands:
- change: `/memory` overlay reads files + the ledger; keys unchanged (accept/drop/forget/move/edit/
  undo, DESIGN 10.4); edit and forget write their ledger entries (D7); a cross-scope move takes both
  locks user-then-private and inserts into both partitions' ledgers. `/memory <note>` keeps its
  shape; overflow now cuts learned lines to the ledger with a note and still refuses, pointing at
  the oldest, when a user line would be cut (row 9); the list shows ledger-cut and superseded lines
  in a collapsed, origin-and-date labeled group.
- change: `/health` gains a brain row (lint findings, over-budget partitions, learned/user counts);
  `/usage` "memory" row
  relabeled "brain" (agent.go:632).
- add: none. delete: none. (`zemi brain check` deliberately not built, D6.)

UI: `/memory` overlay unchanged; statusline review counter unchanged (same counts, new store).

Lua:
- change: `zemi.memory.add/list/del` keep signatures; storage swaps to files + ledger.
  `zemi.learn.*` unchanged (decide stays user-only, internal/hostapi/learnapi.go:42).
  `zemi.pending_memory` unchanged.

Config: none added, none changed, none removed. `memory.autonomy` keeps name and semantics (user
config only, internal/config/store.go:101) and now also governs file-mode applies (open question 4
covers a split).

Events: `memory.written` kept, same payload shape (internal/hostapi/event.go:24); reflector.*
allowlist unchanged.

## 7. Reflector changes

- Input: live-memory view (dedup, retirement targets) from `memories.md` plus the ledger (D1)
  instead of store rows; session-delta and seen/claim/lease/tries kv (reflector.go:252-254)
  unchanged; still reads only this session's proposals (reflector.go:409-411); instruction cap
  untouched (reflector.go:406) - no brain file rides instructionFiles.
- Proposal schema unchanged (scope, text, quote, replaces; empty text = retirement, always queued).
- Apply: prepend the line to the scope's `memories.md` (or remove it for retirement and supersede -
  the prior text trails into the ledger entry, D1/D7) and insert the entry, under the D1 lock,
  ledger first. Unchanged: intake and gate, the detached reflector's budget exemption
  (hostapi/memory.go:117-119).
- The lock order is D1's: cutMu outer, flock inner. The reflector attempts the
  flock once per batch inside cutMu (reflector.go:345-354; a 2s timeout queues the whole batch,
  never a per-entry burn), and rewind takes cutMu (rewind.go:171-177), then the partition flock(s)
  before TruncateMessages and before any brain-file restore, checkpoint or ledger-based - today's
  checkpoint restore (rewind.go:163-167) runs before cutMu, so its brain half moves inside the lock;
  a lock timeout aborts the rewind before any truncation or restore, so a failed post-truncate
  forgetLearned leaving learned memories live with no UI path cannot occur. `forgetLearned` becomes
  the ledger SELECT plus a file rewrite that appends a forget entry per id the SELECT removes (D7;
  the shape `DeleteLearnedBetween` nearly is today) - and keeps both halves of today's version
  (rewind.go:206-233) plus the two the new store adds: supersedes in the range
  re-prepend the superseded line from the ledger entry's prior text (today's Unsupersede,
  rewind.go:225-231; an entry without prior text - an id only the ledger held - restores nothing;
  a dropped import re-prepends the frozen row its superseded_by retired, the pre-migration
  supersede whose op the swap left behind);
  cut (archive) ops whose acting session is sid re-prepend their lines from the
  entry's prior text - today nothing is ever cut, so today has no un-archive half, and the design
  closes that gap in the same SELECT; a move-out re-prepends its line to the partition it left
  (unless the session wrote the line there in the range, whose add the rewind already undoes), the
  destination copy dropped by the move-in's half; file-mode brain writes restore newest-first, a range ending at
  its oldest entry's prior bytes, an entry with existed=false deleting the file it created, a
  truncation flag preferring the approved file-tool write's checkpoint, else the truncated bytes, as
  D5 names - bytes checkpoints never saw, because
  the memory tool rides Pipeline.Remember (agent.go:2665-2667), not the checkpointed file tools. The
  session write set clears. `clampReflectSeen` (rewind.go:174) untouched.

## 8. Migration

Reconciling, not one-shot: while the frozen `memory` table holds un-imported live rows, every open
reconciles them into the files - the global store at startup, a project store when its project
session opens (never a sweep over repos zemi has merely seen); all file work under the D1 lock,
user-then-private when both partitions are touched:

1. Read live rows, newest first, and the ids already present in `memories.md` + the ledger. Live
   rows whose ids are present are done - present in the file OR in the ledger, whose import entries
   are the marker separating "not yet imported" from "deleted by the user": a line the user deletes
   from `memories.md` never resurrects (row 18), and since the ledger never compacts (D7), the
   marker never erases. Missing ids compose into the full `memories.md` for the matching partition -
   a deterministic merge of the existing file lines and the new rows, sorted by each line's
   last-write date (the frozen row's updated_at, the line's date field per D1) desc, then id -
   today's digest order (store/memory.go:94), written with WriteFileAtomic only when the bytes
   differ (a crash re-runs to identical bytes). Today's intake keeps every scope within its 3 KiB
   budget, so the initial import never overflows; only new writes exercise the cut path. Superseded
   rows (superseded_by set) are not composed - the hot file holds live lines only (D1) - and are
   never dropped: they stay in the `memory` table, which no stage drops, so every pre-migration
   superseded id keeps resolving with its quote and provenance exactly as today
   (store/memory.go:86-88; hostapi/memory.go:790-802). Create missing partition dirs as needed; no
   repo file or gitignore is written.
2. Insert ledger entries (op `import`) carrying id, origin, quote, provenance_session_id,
   created/updated timestamps, text hash - skipped when the ledger already holds the id, so replay
   is idempotent. The entries follow the file write: a crash between the two re-runs (the compose
   is the half a lost tombstone cannot redo), while entries before the bytes would mark rows done
   that were never composed. Nothing the overlay or rewind reads is lost.
3. No kv flag, no schema bump: the frozen table's rows are the guard, the reconcile is an id-set
   diff over a bounded row set, and once every live row is imported the bytes stop changing. An old
   binary (rollback build, pinned version, second checkout) keeps writing rows with no knowledge of
   migration; the next new-binary open imports them, including user-typed `/memory <note>` rows
   from the mixed-version window.

- `proposals` rows: unchanged; the accept path is rewritten once to apply to files; pending
  proposals survive the upgrade.
- Forks mid-flight carry context bytes; nothing to migrate.
- Rollback: no schema bump and no drop, ever - the old binary's store is never touched; it reads the
  frozen table and knows nothing of the files, so post-swap one-liners are invisible to it.
- `/projects` delete removes the project brain directory with the DB (projects.go:284-290 claims
  the dir): audit trail for that project ends there.

## 9. Staged implementation plan

- Stage 1 - `internal/brain` package, no wiring: paths.go (TrustRoot-keyed roots), frontmatter.go
  (skill.SplitFrontmatter + yaml three-field struct, field strip/caps), load.go (ENOENT = empty,
  per-file errors preserved), render.go (caps, cut order with terminating backstop, path-sorted
  derived catalog with the file-safety checks, the render-side strip and 200-char field cap, the
  64-file/256 KiB scan ceiling, memory lines with computed age and the render-time scope cut),
  memories.go (parse/serialize, prepend for new lines and edits, supersede and budget cut to the
  ledger, forget, the id lookup over memories.md + ledger, learned-only overflow guard), ledger.go
  (memory_ops: lazy CREATE TABLE IF NOT EXISTS in the partition's DB, insert, prior file bytes on
  write, latest-state-per-id query - no replay format, no compaction, no torn tail), lint.go (seven
  findings incl. hash mismatch, explained absence, instruction-shape detection, over-cap catalog
  field), lock.go (flock, fixed user-then-private order). Tests: frontmatter parse table (incl.
  control-character strip and 200-char caps); load ENOENT and error preservation; render cap and
  backstop-terminates golden; memory line round-trip; ledger insert and idempotent import; lint
  findings table; lock contention and ordered dual-lock.
- Stage 2 - injection: prompt.go builds `# Brain` (catalog, memory with the digest header, roots
  named); agent.go `contextKey` takes the brain hash; session.go/compact.go/SetProjectTrusted
  rebuild points;
  path-triggered pointers (glob table from frontmatter, matcher in the Instructions hook and
  AfterWrite, dedupe, 3-per-result cap). Tests: the key changes at a build after a brain edit, the
  stored role-"context" row replays while it matches, nothing re-renders mid-session but a trust flip; budget
  cut-order and backstop golden; cut markers name paths; the AGENTS.md chain untouched by the brain
  render; pointers fire once on a matching read and on a matching write or edit, private globs
  trusted-only and user globs every project, capped at 3; fork prefix reuse.
- Stage 3 - store swap: memory tool text + file modes -> lock + files + ledger; `/memory` overlay,
  Lua, reflector apply (with queue-on-failure), rewind `forgetLearned` (lock-first, un-supersede,
  prior-bytes restore) rewired; migration (reconcile-on-open, idempotent); `/health`
  brain row. Tests: migration round-trip and re-run idempotency (same ids, quotes, provenance;
  compose in the digest's updated_at-desc order; a hand-deleted line never resurrects; superseded
  rows stay in the frozen table and keep resolving); the policy-table matrix (extends
  hostapi/memory_test.go patterns), incl. the origin=user overflow guard (an overflow that would cut
  an origin=user line queues, learned-only overflow cuts with named ids); the file-mode overwrite
  gate (session write set and its hash staleness check) and the write entry's restorable prior bytes incl. the truncation and
  existed-flag restores; ledger-first ordering (a crash leaves the lint finding, never a
  provenance-less live line); undo with age-preserving restore, its removal writing a forget entry
  (D1, D7); supersede trails into the ledger; rewind forgetLearned incl. lock-timeout abort,
  un-supersede, un-archive of the range's cut ops, forget entries for the removed ids, and
  newest-first prior-bytes restore; move-out explains an absence (no false divergence finding);
  reflector failed-apply queues.
- Stage 4 - guardrails: protectedWhy reserved-file reason (open findings in the ask); readJails
  brain roots; outside-text note on brain asks; AfterWrite scrub + lint on brain dirs, `memories.md`
  scrubbed under the lock with edit entries; session-start `<brain-warning>`
  with untrusted scoping. Tests: the injected-surface intake matrix (multi-line, over-cap and
  instruction-shaped description/read_if queue; freeform body exempt; over-cap hand-edited field
  truncates at render with a finding); the refusal matrix over write/edit/LSP rename (protected
  ask, yolo aside, ask lists open lint findings, approved write scrubs with edit entries, the lsp
  rename path included) and Lua fs.write (rejected at jail resolution); scrub on a brain file
  write; the untrusted golden (loop text + user catalog + memory lines byte-equal to today's digest
  lines when healthy; no byte from the private partition's catalog, map files or findings; no repo
  path in any warning); the jail matrix
  (trusted brain read settles, state-dir brain write asks - yolo aside, memories.md carries the
  dedicated reason).
- Stage 5 - deletion: delete learn/digest.go, the digest assembly and the dead write path of
  store/memory.go - the frozen `memory` table and the ops ledger stay, nothing dropped, no schema
  bump; rewrite DESIGN.md 7, 10.3, 10.4, 10.7, 10.8 (the renegotiated homes).

Stages 1-2 and 3-4 each parallelize across worktrees; stage 5 is serial after 3 and 4.

## 10. Deleted-code inventory

Deleted:
- The write path of `internal/store/memory.go`: AddMemory, UpdateMemoryText, SupersedeMemory,
  DeleteMemory, DeleteLearnedBetween, Unsupersede (store/memory.go:59-129, 6 of the 9 functions).
  Replaced by brain/memories.go + ledger.go + lock.go; rewind's restore halves move there (section
  7). The read path (scanMemory, GetMemory, ListMemory) stays: the frozen table is the
  pre-migration trail (below) and `/memory` list merges it.
- internal/learn/digest.go: rendering moves to brain/render.go; `ScopeBudget` and `LineSize` move
  to brain/memories.go, `Age` and the digest header to brain/render.go.
- `digestFor` and the digest assembly (hostapi/memory.go:826-831) - replaced by the brain build;
  the pin/drop mechanism around `ag.digest` stays under a new name.
- Memory CRUD call sites: rewind.go `forgetLearned` DB path (now ledger SELECT + file rewrite),
  memory.go overlay reads.

The `memory` table is never dropped: it freezes at the swap as the pre-migration trail, whose
superseded rows keep their text, quote and provenance forever, and the ops table's supersede and
archive entries carry the trail for everything written after the swap.

Kept unchanged: `learn/intake.go`, `learn/scrub.go`, the whole 10.4 gate, `/memory` overlay keys,
autonomy knob, 20/session cap, scope budgets, `memory.written`, statusline counts, reflector trigger
and schema, checkpoints/rewind machinery, the `proposals` table, writeJails, the sandbox policy.

Changed, not deleted: prompt.go, agent.go, session.go, compact.go, reflector.go, rewind.go,
health.go, tools.go, lsp_tool.go, permmode.go, io_lua.go, ask.go.

## 11. Open questions

1. User-partition interop path. Recommendation: keep zemi-native `~/.local/share/zemi/brain/user/`;
   add an opt-in `brain.userRoot` override only when a second harness actually needs to share the
   directory; the format is identical, so nothing precludes it later.
2. `zemi brain check` subcommand. Recommendation: do not build it; `/health` plus injected warnings
   cover repair, add a command only if field use shows a real gap.
3. Reflector proposing lint fixes (frontmatter repair) in a later version. Recommendation: never -
   the tool-less constraint keeps the reflector unable to act; lint findings steer the live agent,
   which has the memory tool and file tools and the user's eyes.
4. Autonomy granularity. File-mode map applies ride the same `memory.autonomy` knob as one-liners.
   Recommendation: one knob; split a `memory.maps` autonomy only if field use shows users who want
   maps auto but one-liners asked (or the reverse).
5. An agent-facing read path for budget-cut one-liners. Their bytes live in the ledger; the model
   can resolve their ids (`replaces`, forget) but cannot read their text, exactly today's posture.
   Recommendation: none in v1; add a memory-tool lookup mode only if field use shows models hunting
   cut lines.

## Provenance

Converged by a multi-agent council (four independent studies, five revisions, three critique rounds
with a fresh-eyes dissent member each round, a final verification gate that ended with zero blocking
and zero major findings), then amended by the owner: the shared partition is dropped - the brain is
machine-local. Standing order for every future critique pass on this document: do not invent
problems to have something to say; take neither side; "zero findings, converged" is a valid answer
when it is true; every finding must quote the exact offending text and give a concrete fix. Every
file:line claim was independently verified; the raw council transcripts are
ephemeral, this document is the artifact of record.
