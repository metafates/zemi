---
name: zemi-tui-test
description: "Load to check a UI or agent change end to end, capture a screen, or reproduce a bug Go tests cannot: the real binary in tmux against a fake model, in a throwaway config tree."
---

# Testing zemi in a real terminal

Go tests cover most things; drive the binary when the change is visible (chrome, prompts,
pickers) or crosses the model loop (tools, permission asks, compaction, resume).
Use tmux: pyte-style emulators garble bubbletea's incremental renderer, tmux does not. Give every run its own server (`tmux -L <name>`): parallel testers share the default one, and killing it kills everyone's sessions.

## 1. A throwaway tree, always

Never run `bin/zemi` bare: it opens the real `~/.config/zemi` and can save a trust decision
or a session there. `zemi-safe` (next to this file) runs the binary with all four
`ZEMI_*_HOME` dirs under one scratch tree (`~/.agents/skills` still loads from `HOME`, so
a prompt capture can differ by machine); it is a script, so it also works inside a tmux
pane, where the caller's environment is not forwarded, and from fish, which does not split
`$VAR` into words. `make first-run` is for looking at a newcomer's screens (fresh `HOME`,
real keys); `zemi-safe` is for tests (no keys, a fake model).

Nor type `zemi` in a pane's shell: that is the owner's installed binary on the real config.
When the model under test must find `zemi` on `PATH` (`zemi plugins test`), put a wrapper
that execs `zemi-safe` first on the pane's `PATH`.

```sh
S=$PWD/.agents/skills/zemi-tui-test; T=/tmp/zt
mkdir -p $T/proj $T/config && (cd $T/proj && git init -q)
make build                       # or: go build -o $T/zemi . && export ZEMI_BIN=$T/zemi
```

Testers running in parallel each take their own `$T`, tmux server name and port: on a shared
port the other tester's server answers, and on a shared tree its `init.lua` loads.

## 2. A fake model

`fake_openai.py` (next to this file) is a 40-line OpenAI-compatible server: streams a reply
with a reasoning delta, or a tool call when the user message reads `call NAME {json}`, answers
non-streaming requests (the object calls: classify, `zemi.model.ask`), and writes every request body to
`last_request.json` so you can diff two consecutive requests byte for byte (prompt cache).

```sh
(cd $T && python3 $S/fake_openai.py 18472 &) ; sleep 0.3
cat > $T/config/init.lua <<'L'
zemi.provider.add({ id = "fake", kind = zemi.provider.kind.OPENAI_COMPAT,
	api_key = "x", base_url = "http://127.0.0.1:18472/v1",
	models = { { id = "m", context_window = 128000 } } })
zemi.config.set("models.chat", "fake/m")
zemi.config.set("session.auto_title", false) -- else the title request overwrites last_request.json
L
```

## 3. Drive it

```sh
tmux -L zt new-session -d -s zt -x 140 -y 40 "cd $T/proj && $S/zemi-safe $T --trust"
sleep 1.5; tmux -L zt capture-pane -t zt -p            # -e keeps colors (SGR) for theme checks
tmux -L zt send-keys -t zt "call shell {\"command\":\"ls\",\"about\":\"list\"}" Enter
sleep 1; tmux -L zt capture-pane -t zt -p
tmux -L zt send-keys -t zt "/model" Enter               # pickers, palettes, overlays
tmux -L zt send-keys -t zt Escape
tmux -L zt resize-window -t zt -x 60 -y 15              # narrow and short layouts
tmux -L zt send-keys -t zt C-c C-c                      # quit
tmux -L zt kill-session -t zt; pkill -f "fake_openai.py 18472"
```

Headless checks need no tmux: `$S/zemi-safe $T health`, `$S/zemi-safe $T -C $T/proj run --trust
--json "call read {\"path\":\"a.txt\"}" </dev/null` (without `</dev/null` `zemi run` waits 3 s on a
non-tty stdin). `--profile` and `-m` apply after `init.lua`.

Colors: a pane gets 256 colors, since a detached tmux has no client to report RGB and
`COLORTERM` is ignored under `TERM=tmux*`. For the theme's exact hues start the pane's command
with `env TERM=xterm-256color COLORTERM=truecolor`; `capture-pane -e` then shows `38;2;r;g;b`.
For the light palette add `zemi.config.set("ui.theme", "whorl-light")` to the tree's `init.lua`.

## 4. Reading a screen

- A permission ask is a prompt box under the chat; answer with the key its rows show
  (`y` yes, `n` no, `t` no and tell) or click.
- Compare captures for row shifts: the same state change must not add or remove a row
  (house rule). Diff two captures with `diff <(...) <(...)`.
- `zemi.ui.screenshot()` from the Lua tool or a keymap returns the same text without styling.
- Idle CPU: `ps -o %cpu -p $(pgrep -f "zemi --trust")` over 30 s must stay near 0.
- Session rows, tool telemetry and usage are in `$T/state/projects/<hash>/zemi.db` (SQLite).

## 5. Clean up

Kill the tmux session and the server; leave nothing running. Delete `$T` when done: a stale
tree with an old `init.lua` shape is the usual cause of a confusing second run, and each
scratch binary is 100 MB.
