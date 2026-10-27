---
name: zemi-tui-test
description: "Load to check a UI or agent change end to end, capture a screen, or reproduce a bug Go tests cannot: zemi-lab drives the real binary in tmux against a fake model, in a throwaway config tree."
---

# Testing zemi in a real terminal

Go tests cover most things; drive the binary when the change is visible (chrome, prompts,
pickers) or crosses the model loop (tools, permission asks, the view, a reset).
Use tmux: pyte-style emulators garble bubbletea's incremental renderer, tmux does not.

## The lab: zemi-lab

`zemi-lab` (next to this file) is the whole procedure in one driver. `up` builds `bin/zemi`
(`ZEMI_BIN` overrides, otherwise `make build`), makes a throwaway tree at
`$TMPDIR/zemi-lab-<name>` (all four `ZEMI_*_HOME` dirs under it via `zemi-safe`, a git repo
at `proj/`), starts `fake_openai.py` and a tmux session on its own server, and waits for the
composer instead of sleeping. The pane gets truecolor with `NO_COLOR` unset; the fake
server's files land in the tree, never in a repo. Every lab is name-scoped (tree, tmux
server, port), so parallel testers take `-n <name>`.

```sh
S=.agents/skills/zemi-tui-test
$S/zemi-lab up                     # 140x40 TUI; --asks adds a gate plugin that forces the ask overlay
$S/zemi-lab prompt 'call shell {"command":"ls","about":"list"}'
$S/zemi-lab cap | grep -v '^ *$'   # whole pane, plain text; cap -e keeps SGR colors
$S/zemi-lab wait '1 command'       # screen stopped changing and the row shows
$S/zemi-lab send Escape            # key names, as tmux spells them (C-c, Escape)
$S/zemi-lab ask y                  # answer a permission ask (y|n|t), past the grace
$S/zemi-lab size 60 15             # narrow and short layouts
$S/zemi-lab db                     # this project's zemi.db; logs is the launch log
$S/zemi-lab down                   # kill sessions and server, delete the tree (--keep keeps)
```

`prompt` clears a leftover draft first (one ctrl+c; a second would quit), types, sends
Enter, and waits for the screen to settle - never a blind sleep. It returns when the
screen settles (a settle heuristic), which can be mid-turn - not a turn-end guarantee.
`up` flags: `-x W -y H`
(default 140x40), `--theme whorl-light`, `--untrusted` (drops `--trust`, for the trust
prompt). `zemi-lab attach` sits at the lab in a real terminal.

## The fake model

`fake_openai.py` is a 40-line OpenAI-compatible server: it streams a reply with a reasoning
delta, or calls a tool once when the user message reads `call NAME {json}`, and answers
non-streaming requests (the object calls: classify, `zemi.model.ask`) with a plain
completion. Every request body lands in `last_request.json` and `requests/NNN.json` under
the tree, so two consecutive bodies diff byte for byte (prompt cache); `zemi-lab last`
prints the latest. The generated init.lua adds no second model, so nothing else writes a
request between yours.

A permission ask needs a gate plugin - zemi never asks by design (DESIGN §9): `up --asks`
installs one whose `permission.request` handler returns `{decision = "ask"}` (a plugin with
`perm` in its `uses`), and it loads live within a second of a running lab.

## Reading a screen

- A permission ask is a prompt box under the chat; `y` yes, `n` no, `t` no and tell, or
  click. Keys typed as the box opened are swallowed by the grace, past it they answer the
  ask (a draft under it is kept), which is why `ask` resends until the box is gone.
- Compare captures for row shifts: the same state change must not add or remove a row
  (house rule). Diff two captures with `diff <(...) <(...)`.
- The pane title marks state: a spinner while a turn runs, `? ` while an ask is up -
  `tmux -L zl-<name> display -p '#{pane_title}'`.
- Capture the WHOLE pane, not row ranges: popups sit mid-screen and the composer is 3 rows;
  row-range captures made a working app look hung three separate times.
- A burst of send-keys can wedge a pane's input (an unterminated bracketed paste swallows
  keys until 201~) while zemi stays healthy: `respawn-pane` revives it. The lab sends one
  atomic call per command.
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

## Clean up

Kill the tmux session and the server; leave nothing running. Delete the tree when done: a
stale tree with an old `init.lua` shape is the usual cause of a confusing second run, and
each scratch binary is 100 MB. The lab's `down` does all three.
