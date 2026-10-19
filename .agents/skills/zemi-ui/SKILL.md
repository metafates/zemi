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
  the composer - scrolling is not on them (DESIGN §8, Default keymaps) - and chrome, an open
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
  `on_click`, run on the background worker), which also leaves the tail of the scrolled
  region that widget draws in (`App.leaveTail`), so the rows the click has it redraw stay
  under the reader; a click id with no widget is a Go statusline pill
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
  `Body`, which stays what `y` copies. A list with `Overlay.Follow` moves the stage's scrolled
  region to the unit its selected item names by `OverlayItem.Anchor`, the number the chat stamps on
  every row of a unit (`Segment.Anchor`, `unitAt`, `jumpTop`: centered when the message fits, else
  its first row under the pins), and bands that message with `FillBand` (`bandMessage`, the
  scrollbar cell aside) while keeping it out of the modal fade (`fadeRows`); `anchorAt` takes the
  first anchored row at or below the view's top, so a blank line there picks the message under it;
  it opens on the item the reader is at (`followRow`, which asks each message where its own
  `jumpTop` would put the view and takes the one landing where it already is, so a jump, an enter
  and the next open agree), `syncFollow` in `compose` carries a selection change out before the
  paint, enter closes keeping the place, and `cancelOverlay` puts the chat back on the mark its
  state took when it opened (`followMark`).
  Opening the id of an open overlay replaces it in place
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
  (`/btw`, `/plugins` network work, `/model update`, `/health`, the reflector, an MCP start,
  the idle recap, a compaction round) holds a `Host.Busy` entry: `ctx.busy` is the list of
  them, newest first, the chat's pending row names the oldest as one part (`and N more`
  when more run), the working rule draws
  `ctx.busy[1]`, and the same double esc
  (`CancelBusy`, never `zemi.agent.cancel`) cancels every entry that has a cancel func.
  An empty session has one such row and no chat rows: `zemi.tui.text.empty_chat` is the
  one predicate both widgets read, the chat yielding the region and `splash.lua` drawing
  the pending row on a row its block reserves, since the splash pads to the whole region
  and a row of the chat's would scroll it out of the top as boot work starts and ends.
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
  composer tokens via `hostapi.AttachPath`, and the `@` popup inserts the same form for a
  project file via `hostapi.AttachLabel`: an `@<path>` reference the user can go on editing,
  which the completion picks up again. A file from outside the project zemi copies in becomes
  `[Image <name>]` or `[File <name>]`, the kind read from the extension, the name the copy's
  own basename, capped at 60 cells; a name the project root already holds is refused, a
  directory or a binary or oversized file keeps its quoted path,
  and a
  clipboard image becomes `pasted-<ts>.png` in `<data>/drops`. A project path an `@` word
  would not read back whole - one holding whitespace, or ending in the punctuation a sentence
  ends on - keeps the chip, named project-relative with the `./` a bare root name needs; one
  no chip carries either (`]` beside one of those, a tab, a newline) is the file's path,
  quoted, or the drops copy a drop makes of it. `expandAttachments` resolves a bracketed name
  in the drops directory first, then project-relative, so a forged reference cannot attach
  other files; a `@name` resolves in the project alone, whatever the user or the popup put
  there.
