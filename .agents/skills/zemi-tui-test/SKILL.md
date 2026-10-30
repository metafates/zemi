---
name: zemi-tui-test
description: "Load to check a UI or agent change end to end, capture a screen, or reproduce a bug Go tests cannot: zemi-lab drives the real binary in tmux or in a Rex block against a fake model, in a throwaway config tree."
---

# Testing zemi in a real terminal

Go tests cover most things; drive the binary when the change is visible (chrome, prompts,
pickers) or crosses the model loop (tools, permission asks, the view, a reset).
Two backends: tmux (default) and Rex (`up --rex`). pyte-style emulators garble bubbletea's
incremental renderer, tmux does not. Rex also acts on the sequences tmux swallows — OSC 7501
(the terminal's program-status channel: working, blocked, done), OSC 9 (desktop
notification), titles — and reports them as events, so the terminal-state work is testable
only there.

## The lab: zemi-lab

`zemi-lab` (next to this file) is the whole procedure in one driver. `up` builds `bin/zemi`
(`ZEMI_BIN` overrides, otherwise `make build`), makes a throwaway tree at
`$TMPDIR/zemi-lab-<name>` (all four `ZEMI_*_HOME` dirs under it via `zemi-safe`, a git repo
at `proj/`), starts `fake_openai.py` and a TUI, and waits for the composer (the input box) instead of
sleeping. The pane gets truecolor with `NO_COLOR` unset; the fake server's files land in the
tree, never in a repo. Every lab is name-scoped (tree, tmux server or Rex session, port), so
parallel testers take `-n <name>` (the default name is `zt`; two bare `up`s collide). The
backend is recorded in the tree's `.lab.env`; the drive commands aim at whichever one
booted. `up` refuses while a lab of that name is still
up — `down` reboots, and it reaps every session labeled `zemi-lab-<name>`, even one a moved
tree left behind.

```sh
S=.agents/skills/zemi-tui-test
$S/zemi-lab up                     # 140x40 TUI in tmux; --rex runs it in a Rex block
$S/zemi-lab prompt 'call shell {"command":"ls","about":"list"}'
$S/zemi-lab cap | grep -v '^ *$'   # whole pane, plain text; cap -e keeps colors
$S/zemi-lab wait '1 command'       # screen stopped changing and the row shows
$S/zemi-lab wait ask               # wait for any overlay (ask, question, plan);
                                   # reads the terminal's state, not screen text
$S/zemi-lab send Escape            # key names, as tmux spells them (C-c, Escape)
$S/zemi-lab type 'partial text'    # literal text into the composer; returns once it shows
$S/zemi-lab ask y                  # answer a permission ask (y|n|t); refuses when none is up
$S/zemi-lab up -x 60 -y 15         # narrow layouts are baked into up
$S/zemi-lab size 60 15             # resize after the fact; rex fails loudly when it did not stick
$S/zemi-lab peer                   # a second zemi on the same tree and store,
                                   # its own fake model and request stream (req-peer)
$S/zemi-lab -s peer prompt 'name one river'   # drive the peer; -s main|peer aims every command
$S/zemi-lab snap && $S/zemi-lab diff          # row-shift check: mask volatile spans,
                                   # exit 1 when rows moved (raw captures kept beside)
$S/zemi-lab title                  # the pane title (? while an ask is up)
$S/zemi-lab events                 # rex lab: what the terminal saw, one line per event
$S/zemi-lab records                # rex lab: the OSC 7501 records the terminal holds
$S/zemi-lab db                     # prints the zemi.db path (SQLite; read rows yourself)
$S/zemi-lab logs                   # prints the zemi log path (read it yourself)
$S/zemi-lab status                 # the tree, port, session and every block id
$S/zemi-lab down                   # quit zemi, kill sessions and server, delete the tree
```

`prompt` clears a leftover draft first (ctrl+c until the composer box empties - verified
against the composer box's own rows, however tall a narrow pane folds them, never the
whole screen, which transcript rows echo), types, sends Enter, then waits for the request
to reach the model (the fake's request counter, which counts only requests the fake
accepted - a 429 refusal never reached a model - because the screen cannot prove a turn
started), that a request carries the prompt's text with its marker (each `prompt` appends
a unique `[zl:PID]` to the text and the match wants marker and full text together: the
marker alone is the caller's pid, readable off the composer and typeable by another
window - which is why every window boots its own fake model writing under its own dir
(`req-<window>`; `-s peer last` reads the peer's), so a pair typed into a peer can only
land in the peer's stream. The store is still shared: every window's DELIVERED words ride
each later request's `<chat>` fold in every window, which is why the match reads only
requests past the prompt's baseline, and the per-tree lock - no window delivers while a
prompt polls - plus the marker are what hold that fold route. Within one window only a
delivery of this very text - as a verbatim user message, or inside a later `<chat>` fold
built from delivered messages - can put the pair in a request: neither a count alone
(fires on any request landing after the baseline) nor a duplicate text (a re-sent older
turn reads as delivered) can pass for it, and an
interrupt-restored draft leaves no trace) and for the screen to settle (the 10s window is
matched-poll time; under load the same loop stretches to ~25s wall). Prompts serialize on
a per-tree lock: concurrent prompts share one composer and one Enter would submit both
texts glued as one message nobody typed. It refuses while
any overlay waits on the user: a permission ask (`ask y|n` first) or a question/plan
overlay (`send Escape` first) - past one, its first letters answer the overlay and the
rest lands as a mangled draft. On tmux the mark cannot name the kind, so the refusal
names both remedies instead of a key that would answer the wrong one. A steer sent while
a turn runs can have an ask rise over
it: `prompt` then names the overlay instead of blaming a stuck zemi - settle it
(`ask y` answers a permission ask, `send Escape` dismisses a plan/question one)
and wait again. It returns when the screen settles (a settle heuristic),
which can be mid-turn - not a turn-end guarantee; a turn outlasting the 30s settle
window still exits 0 with a note, because the delivery already passed - and
`wait PATTERN` can match the
PREVIOUS turn's reply: after an errored turn (a mid-stream drop) the settled screen still
shows turn N-1's rows, so `wait 'done, here is the reply.'` exits 0 on the old row. Assert
the turn via the request counter (`last`) or the events, never `wait` alone.
`up` flags: `-x W -y H`
(default 140x40), `--theme whorl-light`, `--untrusted` (drops `--trust`; the boot then
stops at the trust overlay, which `up` reports and both backends see as a permission
ask, so `ask y` answers it on either), `--asks` (installs a
gate plugin whose `permission.request` handler returns
`{decision = "ask"}`, since zemi never asks by design - DESIGN §9), `--rex` (the Rex
backend below). A running lab can be made to ask without a reboot: drop the same two files
into `$TREE/data/plugins/local/askgate/.zemi/` (plugin.json + init.lua; their contents are
the `write_config()` block in the zemi-lab script) and an action asks: on an idle lab the
re-load lands at once, so the first action after planting already asks; mid-turn the first
one starts before the plugin re-loads and the one after it asks. `zemi-lab attach` sits at the
lab in a real terminal (tmux) or prints how to open it (Rex). `zemi-lab health [args...]`
runs `zemi-safe health` against the lab tree.

## The fake model

`fake_openai.py` is a 40-line OpenAI-compatible server: it streams a reply with a reasoning
delta, or calls a tool once when the user message reads `call NAME {json}`, and answers
non-streaming requests (the object calls: classify, `zemi.model.ask`) with a plain
completion. Every accepted request body lands in `last_request.json` and
`requests/NNN.json` under
the tree, so two consecutive bodies diff byte for byte (prompt cache); `zemi-lab last`
prints the latest. A refused request (a 429) writes nothing - that is what makes the
counter a reached-the-model check. The generated init.lua adds no second model, so nothing
else writes a
request between yours. Control files in the tree, read per request like `drop` and
`rate_fail`: `delay_ms` holds a reply open (not a 429 or a drop), `pace_ms` spaces the
streamed chunks.

## Reading a screen

- A permission ask is a prompt box under the chat; `y` yes, `n` no, `t` no and tell, or
  click. Keys typed as the box opened are swallowed by the grace, past it they answer the
  ask (a draft under it is kept), which is why `ask` resends until the box is gone.
- Compare captures for row shifts: the same state change must not add or remove a row
  (house rule). `zemi-lab snap` saves a masked capture, `zemi-lab diff` diffs a fresh one
  against it and exits 1 on a shift; the mask blanks what moves on its own - the
  statusline's figures (elapsed, token counts, cost, the clock), the braille spinner, the
  snail's eyes, the right rail's thumb cell, a working border's figures (elapsed clock,
  retry countdown), a finished turn's `model · Ns · N out · clock` telemetry, a row's
  trailing `· Ns` and a running tool's right-aligned seconds - and never the user's own
  words in the rows, so
  a diff shows text as written. With a peer up, one window's diff conflates the peer's
  rows (its queued `· other window` row, its error rows): diff a single-window lab, or
  with the peer idle. The raw captures are kept beside
  the masked ones for re-checking (`snap m1.txt` keeps `m1.raw` - the suffix replaces
  `.txt`; a relative name lands in `$TREE/snap/`).
- The pane title marks state: `? ` while an ask is up, plain otherwise -
  `zemi-lab title` prints it on either backend (a rex lab also streams each change as a
  `title_changed` event, so the count of title writes is assertable - count from a
  marker event onward; boot writes one title). `wait ask` and
  `ask` read this state (rex: the blocked record's kind - Rex's parser keeps
  permission/question/auth; zemi's plan kind rides the wire but Rex drops that record,
  so a plan ask reads as kindless `blocked`, which the guards cover with the Escape
  remedy; tmux: the mark, which cannot
  tell a permission ask from a question or plan overlay - it answers whatever holds it,
  and a plan or question overlay eats every y|n, so a stuck `ask` on tmux dies naming
  Escape (send it twice: the y's may have opened the plan's editor under the overlay);
  use a rex lab when the distinction matters).
- Capture the WHOLE pane, not row ranges: popups sit mid-screen and the composer is 3 rows;
  row-range captures made a working app look hung three separate times.
- A burst of send-keys can wedge a pane's input (an unterminated bracketed paste swallows
  keys until 201~) while zemi stays healthy: send the terminator itself with
  `tmux send-keys -l ... $'\x1b[201~'` - a `respawn-pane` does NOT clear it (the bytes
  survive the respawn and re-wedge the fresh zemi). The lab sends one atomic call per
  command.
- SIGSTOP experiments run on the rex backend: the block's fg pid stops cleanly (state
  `Ts+`) and `wait`/`prompt` refuse it in 0s. The tmux pane's pid IS zemi (the boot line
  execs down), but a `kill -STOP` from a tool shell does not take on it (state stays
  `Ss+`, three shells observed; SIGKILL lands fine). A stopped zemi's frozen screen
  reads as settled to a compare, so `wait` and `prompt` refuse one (`kill -CONT` to
  resume).
- Direct `tmux -L zl-<name>` probes need the lab's socket dir: the lab exports
  `TMUX_TMPDIR=$TMPDIR`, so set it the same way or tmux looks in `/tmp/tmux-*`
  and finds no server.
- send-keys cannot inject a positioned mouse click: a click path is Go-test only, with
  findCell and clickAt in internal/ui/mouse_test.go.
- `zemi.ui.screenshot()` from the Lua tool or a keymap returns the same text without styling.
- Idle CPU: `ps -o %cpu -p "$pid"` over 30 s must stay near 0, with the pid from the
  backend, not a bare `pgrep -f "zemi --trust"`, which hits every lab's zemi: tmux's
  pane pid IS zemi (`zemi-safe` execs it), rex's is the block's foreground pid.
- `db` prints the path to this project's zemi.db (SQLite) - session rows, tool telemetry
  and usage live there; reading them is your own sqlite3 one-liner.

## Manual fallback

When the lab does not fit (your own init.lua, another provider kind), do by hand what `up`
does: `zemi-safe` (next to this file) runs any binary with all four `ZEMI_*_HOME` dirs under
one tree - it is a script, so it works inside a tmux pane, where the caller's environment is
not forwarded, and from fish, which does not split `$VAR` into words.

```sh
T=$TMPDIR/zt; mkdir -p $T/proj $T/config && (cd $T/proj && git init -q)
make build
(python3 "$PWD/.agents/skills/zemi-tui-test/fake_openai.py" 18472 $T &) ; sleep 0.3
cat > $T/config/init.lua <<'L'
zemi.provider.add({ id = "fake", kind = "openai_compat",
	api_key = "x", base_url = "http://127.0.0.1:18472/v1",
	models = { { id = "m", context_window = 128000 } } })
zemi.config.set("models.default", "fake/m")
L
tmux -L zt new-session -d -s zt -x 140 -y 40 -c $T/proj \
  "env -u NO_COLOR TERM=xterm-256color COLORTERM=truecolor ZEMI_BIN=$PWD/bin/zemi \
   $PWD/.agents/skills/zemi-tui-test/zemi-safe $T --trust"
tmux -L zt capture-pane -t zt -p            # -e keeps colors (SGR) for theme checks
```

A detached tmux pane reports 256 colors; `env -u NO_COLOR ... COLORTERM=truecolor` (what
the lab does) is what makes `capture-pane -e` show `38;2;r;g;b`. `-m` applies
after init.lua. Parallel testers each take their own `$T`, tmux server name and port: on a
shared port the other tester's server answers, and on a shared tree its `init.lua` loads.

## The rex lab: what tmux never sees

`up --rex` puts zemi in a Rex block (session `zemi-lab-<name>`): an events recorder block
writes every event the terminal sees to `$TREE/events.jsonl`, the fake model and zemi run as
labeled blocks, and the drive commands (prompt, cap, wait, ask, send, size, snap, diff)
work as in tmux, with key names translated. Use it whenever the change under test is one of
the sequences tmux eats:

```sh
$S/zemi-lab -n ps up --rex --asks
$S/zemi-lab -n ps prompt 'call shell {"command":"ls","about":"list"}'
$S/zemi-lab -n ps wait ask
$S/zemi-lab -n ps ask y
$S/zemi-lab -n ps events program_status_changed
program_status_changed	state=idle
program_status_changed	state=working msg=call shell {"command":"ls","about":"list"} [zl:PID]
program_status_changed	state=blocked kind=permission msg=Run this command? · list
program_status_changed	state=done msg=done, here is the reply.
$S/zemi-lab -n ps events desktop_notification   # exit 1 = none; the count IS the "one, not two" assertion
$S/zemi-lab -n ps records                       # what the terminal holds right now, as JSON
$S/zemi-lab -n ps down                          # quits zemi first, so the exit clear reaches the wire
```

- The events stream is the assertion surface, and a change-feed: a record updated to
  exactly its previous content (same state, msg and owner) emits nothing, so per-turn
  counts can undercount - assert on set membership (`grep -q done`) and on `records` (with
  its `updated_at`), not on the order or presence of records (with an instant fake the
  `working` record can be swallowed entirely, and a turn whose reply repeats the previous
  one can leave no trace at all - not even a fresh `updated_at`; `delay_ms` or a varied
  reply makes a turn show). Also in the
  stream, unfiltered noise: `process_changed`, `pwd_changed`,
  `progress_report`, client-driven
  `size_changed`. A `state=clear` on quit arrives as `program_status_removed`; `records`
  prints what the terminal holds right now. The recorder is a block of its own (id in
  `status`); `events` checks a live stream has a writer - the kernel's list of files held
  open, not the block's pulse, since a frozen stream passes every grep - and after a
  `down --keep` it reads the kept tree's final stream.
- Rex answers the OSC 7501 feature query, so a status-following build suppresses its own
  alerts there and the events stay clean; tmux never answers, which is the right control
  for the other side of that branch.
- A block takes focus at creation and no blur arrives on its own: open a second block (or
  `peer`) to steal focus before asserting on unfocused behavior.
- `down` dismisses an open ask or overlay (one escape) and then quits zemi by its armed
  ctrl+c - a plain kill would strand the last record on the dead block, and with an ask
  up even a bare ctrl+c is swallowed. Keys go to the zemi blocks only (a ctrl+c to the
  recorder's block would kill the stream mid-write), and `down` waits for the terminal
  to report `program_status_removed` before killing the session: a mid-turn zemi eats
  the first ctrl+c as an interrupt and a drafted one as a draft-clear, so it keeps
  sending (bounded) until the clear lands. The exit clear is assertable only with
  `down --keep` (a plain `down` deletes the tree with the stream): then
  `events program_status_removed` shows `reason=clear` - the session is gone by
  then, which is why `events` falls back to the kept tree's final stream (and warns
  when that stream ends before the tree's last write: a recorder killed mid-lab makes
  absence assertions false-pass). A recorder already dead at teardown marks the tree,
  and `events` on that kept tree refuses - absence assertions on a stream that stopped
  early false-pass; read `events.jsonl` directly when you only want what did arrive.
- `down` on a lab whose zemi is already dead refuses: the quit dance would burn 4s and
  land no exit clear anyone could assert - `down --force` reaps without it, and without
  the dance (a corpse's keys land nowhere).
- `zemi-lab status` prints the session and every block id (zemi is `main`); kill a wedged
  lab zemi by the exact pid from `rex block call -b BLOCK process` (never a broad
  `ps | grep`). `rex kill` takes sessions only. A drive command after such a kill dies
  loudly instead of serving the corpse's stale screen - a block outlives its process -
  and `down` refuses too (`down --force` reaps without the exit clear).

## The chaos driver

`chaos-qa` runs a seeded random action sequence - turns, esc, overlays, armed
429s, mid-stream drops, resizes, SIGKILLs (its pane is a supervisor loop, so a
kill is one round, not a teardown) - and checks the invariants after every
round: store integrity, view coverage exactly-once over the log, dense seqs,
the process alive. A failure names the seed, so a broken run replays:

```sh
.agents/skills/zemi-tui-test/chaos-qa up 40 11   # rounds, seed; log to stdout
.agents/skills/zemi-tui-test/chaos-qa check      # the invariants only
.agents/skills/zemi-tui-test/chaos-qa down
```

The fake server's `drop` control file (a positive count of replies cut
mid-stream) is what drives the network-drop path; `rate_fail` and
`rate_limit` are described at its top.

## The recall eval

`recall-qa` (next to `chaos-qa`) plants nonce facts into a lab chat and
scores which survived into the summary the model reads. Each planted message
carries a unique nonce - head for even i, tail for odd i - and filler past
nodeBytes (512B), so the builder makes a real compaction call per message;
a padded nonce-free flush closes the round so the latest call's `<chat>`
carries every digest. Score greps that `<chat>`:

```sh
.agents/skills/zemi-tui-test/recall-qa -n recallqa up 6   # lab + 6 nonces + flush
.agents/skills/zemi-tui-test/recall-qa -n recallqa score  # per-nonce survived/lost
.agents/skills/zemi-tui-test/recall-qa -n recallqa down
```

With the fake (digest = the 400B head of the message) head nonces survive
and tail ones are lost - the loss boundary, made measurable. A real model
is graded by the same score.

## Clean up

Tear the lab down with `down` (tmux session and server, or the Rex session) and leave
nothing running. Delete the tree when done: a stale tree with an old `init.lua` shape is the
usual cause of a confusing second run, and each scratch binary is 100 MB. `down` does all of
it; `--keep` spares the tree, and a later plain `down` re-tears a kept tree (no `--force`
needed). A failed `up` leaves the tree for inspection; `down` clears
it.

Never kill a zemi found by a broad `ps aux | grep zemi`: a bare PID from that grep may be the
user's own zemi (a persona kill -9'd it exactly that way on 2026-10-30). Match the lab
instead - the tree path `zemi-lab-<name>` in the command line, or `$S/zemi-lab -n <name> down`.
