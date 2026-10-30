---
name: zemi-tui-test
description: "Load to check a UI or agent change end to end, capture a screen, or reproduce a bug Go tests cannot: zemi-lab drives the real binary in tmux or in a Rex block against a fake model, in a throwaway config tree."
---

# Testing zemi in a real terminal

Go tests cover most things; drive the binary when the change is visible (chrome, prompts,
pickers) or crosses the model loop (tools, permission asks, the view, a reset).
Two backends: tmux (default) and Rex (`up --rex`). pyte-style emulators garble bubbletea's
incremental renderer, tmux does not. Rex also acts on the sequences tmux swallows — OSC 7501
program status, OSC 9 notifications, titles — and reports them as events, so the
terminal-state work is testable only there.

## The lab: zemi-lab

`zemi-lab` (next to this file) is the whole procedure in one driver. `up` builds `bin/zemi`
(`ZEMI_BIN` overrides, otherwise `make build`), makes a throwaway tree at
`$TMPDIR/zemi-lab-<name>` (all four `ZEMI_*_HOME` dirs under it via `zemi-safe`, a git repo
at `proj/`), starts `fake_openai.py` and a TUI, and waits for the composer instead of
sleeping. The pane gets truecolor with `NO_COLOR` unset; the fake server's files land in the
tree, never in a repo. Every lab is name-scoped (tree, tmux server or Rex session, port), so
parallel testers take `-n <name>`. The backend is recorded in the tree's `.lab.env`; the
drive commands aim at whichever one booted.

```sh
S=.agents/skills/zemi-tui-test
$S/zemi-lab up                     # 140x40 TUI in tmux; --rex runs it in a Rex block
$S/zemi-lab prompt 'call shell {"command":"ls","about":"list"}'
$S/zemi-lab cap | grep -v '^ *$'   # whole pane, plain text; cap -e keeps colors
$S/zemi-lab wait '1 command'       # screen stopped changing and the row shows
$S/zemi-lab send Escape            # key names, as tmux spells them (C-c, Escape)
$S/zemi-lab ask y                  # answer a permission ask (y|n|t), past the grace
$S/zemi-lab size 60 15             # narrow and short layouts
$S/zemi-lab peer                   # a second zemi on the same tree and store
$S/zemi-lab -s peer prompt 'name one river'   # drive the peer; -s main|peer aims every command
$S/zemi-lab snap && $S/zemi-lab diff          # row-shift check: mask volatile spans,
                                   # exit 1 when rows moved (raw captures kept beside)
$S/zemi-lab title                  # the pane title (a spinner while a turn runs, ? on an ask)
$S/zemi-lab events                 # rex lab: what the terminal saw, one line per event
$S/zemi-lab records                # rex lab: the OSC 7501 records the terminal holds
$S/zemi-lab db                     # this project's zemi.db; logs is the launch log
$S/zemi-lab down                   # quit zemi, kill sessions and server, delete the tree
```

`prompt` clears a leftover draft first (one ctrl+c; a second would quit), types, sends
Enter, and waits for the screen to settle - never a blind sleep. It returns when the
screen settles (a settle heuristic), which can be mid-turn - not a turn-end guarantee.
`up` flags: `-x W -y H`
(default 140x40), `--theme whorl-light`, `--untrusted` (drops `--trust`, for the trust
prompt), `--asks` (installs a gate plugin whose `permission.request` handler returns
`{decision = "ask"}`, since zemi never asks by design - DESIGN §9; it loads live within a
second of a running lab), `--rex` (the Rex backend below). `zemi-lab attach` sits at the
lab in a real terminal (tmux) or prints how to open it (Rex).

## The fake model

`fake_openai.py` is a 40-line OpenAI-compatible server: it streams a reply with a reasoning
delta, or calls a tool once when the user message reads `call NAME {json}`, and answers
non-streaming requests (the object calls: classify, `zemi.model.ask`) with a plain
completion. Every request body lands in `last_request.json` and `requests/NNN.json` under
the tree, so two consecutive bodies diff byte for byte (prompt cache); `zemi-lab last`
prints the latest. The generated init.lua adds no second model, so nothing else writes a
request between yours. Control files in the tree, read per request like `drop` and
`rate_fail`: `delay_ms` holds a reply open (not a 429 or a drop), `pace_ms` spaces the
streamed chunks.

## Reading a screen

- A permission ask is a prompt box under the chat; `y` yes, `n` no, `t` no and tell, or
  click. Keys typed as the box opened are swallowed by the grace, past it they answer the
  ask (a draft under it is kept), which is why `ask` resends until the box is gone.
- Compare captures for row shifts: the same state change must not add or remove a row
  (house rule). `zemi-lab snap` saves a masked capture, `zemi-lab diff` diffs a fresh one
  against it and exits 1 on a shift; the mask blanks what moves on its own (spinner,
  elapsed seconds, token counts, cost, the clock) and the raw captures are kept beside
  the masked ones for re-checking.
- The pane title marks state: a spinner while a turn runs, `? ` while an ask is up -
  `zemi-lab title` prints it on either backend (a rex lab also streams it as
  `title_changed` events, so title churn is countable).
- Capture the WHOLE pane, not row ranges: popups sit mid-screen and the composer is 3 rows;
  row-range captures made a working app look hung three separate times.
- A burst of send-keys can wedge a pane's input (an unterminated bracketed paste swallows
  keys until 201~) while zemi stays healthy: send the terminator itself with
  `tmux send-keys -l ... $'\x1b[201~'` - a `respawn-pane` does NOT clear it (the bytes
  survive the respawn and re-wedge the fresh zemi). The lab sends one atomic call per
  command.
- send-keys cannot inject a positioned mouse click: a click path is Go-test only, with
  findCell and clickAt in internal/ui/mouse_test.go.
- `zemi.ui.screenshot()` from the Lua tool or a keymap returns the same text without styling.
- Idle CPU: `ps -o %cpu -p $(pgrep -f "zemi --trust")` over 30 s must stay near 0.
- Session rows, tool telemetry and usage are in the zemi.db `db` prints (SQLite).

## Manual fallback

When the lab does not fit (your own init.lua, another provider kind), do by hand what `up`
does: `zemi-safe` (next to this file) runs any binary with all four `ZEMI_*_HOME` dirs under
one tree - it is a script, so it works inside a tmux pane, where the caller's environment is
not forwarded, and from fish, which does not split `$VAR` into words.

```sh
T=$TMPDIR/zt; mkdir -p $T/proj $T/config && (cd $T/proj && git init -q)
make build
(cd $T && python3 .agents/skills/zemi-tui-test/fake_openai.py 18472 $T &) ; sleep 0.3
cat > $T/config/init.lua <<'L'
zemi.provider.add({ id = "fake", kind = "openai_compat",
	api_key = "x", base_url = "http://127.0.0.1:18472/v1",
	models = { { id = "m", context_window = 128000 } } })
zemi.config.set("models.default", "fake/m")
L
tmux -L zt new-session -d -s zt -x 140 -y 40 -c $T/proj \
  "env -u NO_COLOR TERM=xterm-256color COLORTERM=truecolor ZEMI_BIN=$PWD/bin/zemi \
   .agents/skills/zemi-tui-test/zemi-safe $T --trust"
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
$S/zemi-lab -n ps events program_status_changed
program_status_changed	state=idle
program_status_changed	state=working msg=call shell {"command":"ls","about":"list"}
program_status_changed	state=blocked kind=permission msg=Run this command? · list
program_status_changed	state=done msg=done, here is the reply.
$S/zemi-lab -n ps events desktop_notification   # exit 1 = none; the count IS the "one, not two" assertion
$S/zemi-lab -n ps records                       # what the terminal holds right now, as JSON
$S/zemi-lab -n ps down                          # quits zemi first, so the exit clear reaches the wire
```

- The events stream is the assertion surface: `title_changed`, `desktop_notification` (OSC
  9), `bell`, `size_changed`, `child_exited` are all there, filterable by name, exit 1 on no
  match. A `state=clear` on quit arrives as `program_status_removed`; `records` prints the
  records the terminal holds right now.
- Rex answers the OSC 7501 feature query, so a status-following build suppresses its own
  alerts there and the events stay clean; tmux never answers, which is the right control
  for the other side of that branch.
- A block takes focus at creation and no blur arrives on its own: open a second block (or
  `peer`) to steal focus before asserting on unfocused behavior.
- `down` sends zemi's armed ctrl+c quit before killing the session - a plain kill would
  strand the last record on the dead block.
- Kill a wedged lab zemi by the exact pid from `rex block call -b BLOCK process` (never a
  broad `ps | grep`). `rex kill` takes sessions only.

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
it; `--keep` spares the tree.

Never kill a zemi found by a broad `ps aux | grep zemi`: a bare PID from that grep may be the
user's own zemi (a persona kill -9'd it exactly that way on 2026-10-30). Match the lab
instead - the tree path `zemi-lab-<name>` in the command line, or `$S/zemi-lab -n <name> down`.
