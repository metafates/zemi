---
name: zemi-ui
description: "Load before changing the TUI or anything the user sees: internal/ui, runtime/lua/zemi/tui, the layout spec, keymaps and the composer, overlays, mouse and hit testing, widgets and themes."
---

# The TUI

## Screen layout (internal/hostapi/layout.go, internal/ui/layout.go)

`zemi.ui.layout.set` hands Go a layout table; `parseLayout` validates it into immutable
`hostapi.Layout` data (no closures) behind an atomic pointer `compose` reads every frame, so
drawing never waits on Lua. The shipped `tui/layout.lua` sets the default, user config
may replace it, lower origins are refused. A reload keeps the old layout drawn (the spec is
plain data, safe across generations); `ForceReload` drops one the replay did not set again and
`DefaultLayout`, the Go copy of the shipped tree, draws. `span` sizes one box per frame from
cells, percent, content and weighted fill, takes cells back from fill first when they do not
fit, and never fails (`FuzzSpan`). The stage node is where prompts, overlays and toasts
go, the toasts drawn last (`App.compose`, after the final join and fade) so nothing a box
places can cover or dim one. A content-sized node's height is the most rows it could get, never
its last draw, so a widget that pads to `ctx.height` can still shrink. Hit testing reads the
painted segments, never the tree. No layout draws the `exit` region: `App.Farewell` renders
it once after `p.Run` returns cleanly (nothing while a boot step waits, which no dispatcher
would answer) and main prints it to the shell. Code below builtin may not add to it: its text
lands outside zemi's chrome, beside the resume command a user copies (`ctx.resume`, which
`hostapi.ResumeCommand` builds, the one place that names the CLI's resume flag). A `composer` node with `frame` draws the square frame around the composer
and puts the first line of the widgets in its `top`, `left` and `right` regions into the rules
(`composerFrame`, ui/composer.go; `top` gets the whole rule, whose left end holds the
snail of `tui/snail.lua` a cell off the corner, its eyes showing the state, how a turn ended included
(`ctx.ended`), and whose right end holds the turn's status), which is where the shipped layout keeps the mascot and the statusline: agent
state changes what a rule says, never how many rows the frame takes, so nothing above it
reflows. The statusline clusters draw from their outer edges by widget priority and a short
row drops the highest first (`fitClusters`), so what lasts sits outermost and never moves.

## Gotchas

- **The Go-reserved triggers** (`ctrl+c` cancel, `/reload`) are never routed through Lua
  keymaps and cannot be rebound - `/reload` is matched on composer submit before the command
  registry, so nothing can shadow it. Keep them out of any "rebind everything" refactor.
- **No editor modes.** The composer has the keys unless a `focusable` layout node does
  (`Host.Focus`, counted only while the last frame drew it); commands are slash commands typed
  in the composer. Keymaps are keys or key sequences (`<leader>` is `keymap.leader`; a key name
  the terminal never reports raises, `checkKey`). Printable and composer editing keys stay with
  the composer - scrolling is on `pgup`/`pgdown`/`shift+up`/`shift+down`/`ctrl+k`/`ctrl+j`/`ctrl+home`/`ctrl+end`/`alt+up`/`alt+down` - and chrome, an open
  palette and completion popups keep their keys before keymaps are consulted; a `list = true`
  binding swaps a key for a move there first (`Keymaps.ListAlias`, in Go), but for a move a typed
  line edits with while one types (`lineKey`, `App.typing`). A focused node
  takes every key but ctrl+c through `Keymaps.Lookup(focus, key)`: `{ focus = id }` bindings
  first unless a more trusted global binding holds the key; unbound keys drop and an unbound
  esc returns the keys.
- **Mouse capture is load-bearing.** `App.View` sets `MouseModeAllMotion` (unless `ui.mouse =
  false`) so the wheel scrolls the transcript (the altscreen buffer has no native scrollback)
  and plain motion drives hover. `App.target` is the one hit test for clicks and hover, so a
  highlight always means a click acts; only an item's non-blank or background-painted cells are
  a target, and lists scroll at the edge instead of recentering, so a click does not move the
  row under the pointer. `Segment.NoHover` marks a row that acts without lighting up, which is
  how chat body rows stay quiet. Selection and clicks are zemi's own (internal/ui/mouse.go): a
  drag copies the last frame's segments inside the pane or box it started in (`App.selClip`:
  the top modal's text cells, a composer popup's, else a layout leaf or float), leaving out `Segment.NoCopy` cells (decoration) and
  rejoining a row that ends in a `Segment.Soft` one with the next. `hostapi.WrapSegs` sets
  both, so a wrapped prompt, reply or code block copies as the text that was written, which is
  also why `Host.Markdown` renders glamour unwrapped (tables aside) and wraps it itself. A
  click acts on `Segment.Hit` (chrome rows) or `Segment.Click`/`Segment.Widget` (a widget's
  `on_click`, run on the background worker); a click id with no widget is a Go statusline pill
  naming its command. Helpers that rebuild segments must copy those fields, `Fill` (the
  surface under it, a selected row's band), `OnPanel` (a box drew the row on its panel, the
  surface a fade sinks toward), `Pin` (a row's part in the header stack a
  scrolled region pins, top down: a turn's prompt, then its opened run's digest down to the
  run's last row) and `Line` (a chrome line `App.join` meets with the
  lines it touches, after all placement: text, Lua content and the snail art never join); `dimRows`
  drops them (a box's answered rows are not targets); `fadeRows`, which sinks the backdrop of a modal
  toward the canvas, keeps a segment's text, style and effect, so a spinner behind a box keeps
  stepping, and drops what acts or joins (`Hit`, `Click`, `Widget`, `Line`, `Bar`), so the backdrop
  joins before it fades and no line of it reaches the top box. Overlays (internal/ui/overlay.go) are data
  (DESIGN §8 has the detail views, `danger` arming and `AskFirst`); `BodySegs` is sanitized like
  `Body`, which stays what `y` copies. Opening the id of an open overlay replaces it in place
  (`replaceOverlay`, keeping the filter and the selection on its row; only an origin that may close it), a detail
  view open over it rebuilt from its item or dropped with it (`refreshDetail`), and a Go
  screen's list-level key runs a command (`OverlayKey.Run`, builtin and user lists only; a
  `Danger` one on its second press, a refresh keeping the first and a click or a wheel
  dropping it), or with neither command nor fn names the rows' action of that hotkey for the
  footer, and a `Wide` box takes `boxInner`'s width whatever opened it (the workflow
  screens). A list shows the filter row only when it opened with `filterMin` rows or more,
  which a rebuild keeps (`listFilter.filterable`). A
  toggle list rebuilds when the overlay stacked on it closes (`closeOverlayByID`; a detail
  view only after an action) and after a reload (`SignalBoot`), so every caller acts first
  and closes after: the worker then runs the rebuild behind the action. A row that fills the
  composer closes a toggle list, which would keep the keys. A row's `verb` names enter's action
  in the hint and keeps space from toggling it.
- **The key loop never waits on Lua.** Keymap right-hand sides, Lua commands, completion
  sources and a toggle list's `Overlay.Reload` run in order on one worker goroutine
  (`App.background`); Go commands stay inline. Keymaps, clicks, one-shot timers and async
  callbacks call `refresh` when they return. The frame tick sleeps 250 ms unless something
  moves by itself (a turn, a drawn spinner, a toast, an armed flag) and always lands on a 50 ms wall boundary,
  so `ctx.frame` steps one per tick while a turn re-renders every one; a change goes through
  `App.bump`, which wakes it, and all stale widgets render as one message; a widget a message
  resized (a composer newline, a resize) renders on that message, so no frame draws lines
  rendered for another size. A render's ctx
  builds `transcript`, `agent`, `todos`, `recent`, `projects`, `regions` and `instructions` on first read (`widgetCtxTable`,
  `BenchmarkWidgetCtx`): keep new expensive fields lazy, and cache one that reads disk or the
  store (`recentSessions`, `splashProjects`, `instructionsCached`). A widget never changes a line table
  it returned: `Widget.tableSegments` converts one it saw last render only once and
  `widgetCache.lineRows` wraps it once per width, and `Host.Session` shares the transcript
  rows, which are appended or replaced whole, never written in place. A render returning
  `true` second runs again next frame and wakes the tick, which is how `chat.lua` fills a
  long session under a 30 ms budget, latest units first, each render at least a full view topped
  with the prompt the view pins (rendered out of order when the view does not reach it), so a
  loaded machine draws the view whole; a region scrolled up keeps the last
  whole render until the fill ends (`App.scrolledUp`), so the reader's rows stay put. A turn's `turnScope` context (host.go) covers compaction, the model and tools, so
  a second esc within 2s (`App.arm`, shared with the idle ctrl+c quit) stops all of them;
  a second ctrl+c while "Cancelling" quits. A job the user waits on that is not a turn
  (`/btw`, `/plugins` network work, `/model update`, `/health`, the reflector, an MCP start) holds a `Host.Busy`
  entry: the working rule draws the newest as `ctx.busy`, and the same double esc
  (`CancelBusy`, never `zemi.agent.cancel`) cancels every entry that has a cancel func.
- **Widgets read UI flags, never Go-drawn text.** The composer frame's rules and the quit, key
  sequence and scroll flags are `tui/` widgets reading `hostapi.UIState`; a new flag goes
  into `UIState` and the ctx table, not into `statusRow` or `compose`. Animation a widget
  cannot render at the 50 ms frame clock is a segment `effect` the UI applies (`shimmer`, and `spin`, which steps a `hostapi.Marks` spinner). A
  turn's end (the `ui.notify` alert, the done eyes, the recap timer) applies once per chain of
  turns, on the first frame that reads the agent idle (`onFrame`). OSC 9;4 progress and the OSC
  11 background go through `tea.View.ProgressBar` and `tea.View.BackgroundColor`, so bubbletea
  resets them on every exit, a signal included. Host chrome draws in the active palette's
  roles (`chromeStyles`, built again when the theme changes), and a segment's `Fill` says
  what it sits on: `hostBox` and the composer frame put theirs on the panel, where a
  style that paints the canvas takes the panel instead (`segStyle`).
- **Drag and drop is a paste.** `ui/drop.go` turns a paste made only of existing paths into
  attachment tokens. Files from outside the project are copied to `<data>/drops` and referenced
  as `@drop:<name>`; `expandAttachments` resolves `drop:` only inside that directory, so a
  forged reference cannot attach other files.
