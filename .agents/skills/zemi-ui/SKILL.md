---
name: zemi-ui
description: "Load before changing the TUI or anything the user sees: internal/ui, the layout spec, keymaps and the composer, overlays, mouse and hit testing, widgets and themes."
---

# The TUI

## Screen layout (internal/ui/layout.go)

The screen is one fixed tree, `screen`, in Go: no Lua and no config set it. Each node is a box
of children or a leaf, laid out along its `Dir`; `span` sizes one box per frame from cells,
percent, content and weighted fill, takes cells back from fill first when they do not fit, and
never fails (`FuzzSpan`). A leaf names the region it draws, one of the `region` constants
(`regionChat`, `regionWorking`, `regionStatusLeft`, `regionStatusRight`); a region no widget
fills draws nothing. `App.shownWidgets` walks the solved tree, so a widget of a region the
layout does not draw costs no render.

The stage node is where prompts, overlays and toasts go, the toasts drawn last (`App.compose`,
after the final join and fade) so nothing a box places can cover or dim one. A content-sized
node's height is the most rows it could get, never its last draw, so a widget that pads to
its region's height can still shrink. Hit testing reads the painted segments, never the tree.

`screenLayout` keeps what the rest of the app asks of a solved frame: the stage's cells, the
composer's (`App.floatPopup` anchors the palette and the completion list to it), the scrolled
region (`regionChat`: `App.scrolls`, `pendingScroll` and `pendingAnchor` are keyed by region)
and the leaves a selection may not leave. A `composer` node with `Frame` draws the square
frame around the composer and puts its `Top` region's first row in the top rule
(`composerFrame`, ui/composer.go; that rule's left end holds the snail, its eyes showing the
state, how a turn ended included, and its right end the turn's status), which is where the
shipped tree keeps the mascot, with the statusline on a bar row below the box: agent state
changes what a rule says, never how many rows the frame takes, so nothing above it reflows.
The statusline clusters draw from their outer edges by widget priority and a short row drops
the highest first (`fitClusters`), so what lasts sits outermost and never moves.

## Animation (internal/anim)

Every ramp lives in `internal/anim`; its package doc is the recipe. The three patterns: a widget
that comes and goes rides a `Curtain` — give the `widget` `eases: true` and `widgetFaded` routes
its lines through one, no per-widget code; widgets that share a region's seat (`solo: true`, the
chat and the splash) hand over through the outgoing one's sink, by region rule, never by id;
rows that rise inside a widget (a rail call, a readout) ride a `Rise` keyed by what names the
element — `Spread` when it grows while it rises, `Present` + `Prune` when it comes and goes,
`Stamp` when it rises once. Rows fade by `anim.Faded(row, 1-alpha, spare)`; a `spare` (like
`spareRail`) leaves a row's chrome standing while its content rises. The toast tray eases too:
its leave sinks, its news is instant (`JumpIn`). A test with a pinned clock settles what it
stamped with `settleRamps`/`Rise.Settle` — an unsettled ramp freezes mid-rise, and a settle must
mark the frame dirty or View-driven tests recompose stale bytes.

## Gotchas

- **The Go-reserved triggers** (`ctrl+c` cancel, `/reload`) are never routed through Lua
  keymaps and cannot be rebound - `/reload` is matched on composer submit before the command
  registry, so nothing can shadow it. Keep them out of any "rebind everything" refactor.
- **No editor modes.** The composer has the keys; commands are slash commands typed
  in the composer. Keymaps are keys or key sequences (`<leader>` is `keymap.leader`; a key name
  the terminal never reports raises, `checkKey`). Printable and composer editing keys stay with
  the composer - scrolling is not on them (DESIGN §8, Default keymaps) - and chrome, an open
  palette and completion popups keep their keys before keymaps are consulted; the chat cursor
  `tab` hands the keys to, between popup and keymap (`App.chatCursorKey`), and any key it does
  not take is ignored while the chat holds focus (`App.chatFoldKey` works the selected run's
  rail, or a long prompt's fold, through the widget's own click path). A `list = true`
  binding swaps a key for a move there first (`Keymaps.ListAlias`, in Go), but for a move a typed
  line edits with while one types (`lineKey`, `App.typing`). Keys go through
  `Keymaps.Lookup(key)` once no overlay or popup claims them; unbound keys drop.
- **Mouse capture is load-bearing.** `App.View` sets `MouseModeAllMotion` (unless `ui.mouse =
  false`) so the wheel scrolls the transcript (the altscreen buffer has no native scrollback)
  and plain motion drives hover. `App.target` is the one hit test for clicks and hover, so a
  highlight always means a click acts; only an item's non-blank or background-painted cells are
  a target, and lists scroll at the edge instead of recentering, so a click does not move the
  row under the pointer. `Segment.NoHover` marks a row that acts without lighting up, which is
  how chat body rows stay quiet; a link on a body row is the exception, the
  one thing acting there, and lights alone on hover. Selection and clicks are zemi's own (internal/ui/mouse.go): a
  drag copies the last frame's segments inside the pane or box it started in (`App.selClip`:
  the top modal's text cells, a composer popup's, else a layout leaf), leaving out `Segment.NoCopy` cells (decoration) and
  rejoining a row that ends in a `Segment.Soft` one with the next. `hostapi.WrapSegs` sets
  both, so a wrapped prompt, reply or code block copies as the text that was written, which is
  also why `Host.Markdown` renders glamour unwrapped (tables aside) and wraps it itself. A
  click acts on `Segment.Hit` (chrome rows) or `Segment.Click`/`Segment.Widget` (the widget
  that drew the row; an id it has no handler for runs as a command, on the
  background worker), which also leaves the tail of the scrolled
  region that widget draws in (`App.leaveTail`), so the rows the click has it redraw stay
  under the reader; a click id with no widget is a Go statusline pill
  naming its command. Helpers that rebuild segments must copy those fields, `Fill` (the
  surface under it, a selected row's band), `OnPanel` (a box drew the row on its panel, the
  surface a fade sinks toward), `Pin` (a row's part in the header stack a
  scrolled region pins, top down: a turn's prompt, then its opened run's digest down to the
  run's last row) and `Line` (a chrome line `App.join` meets with the
  lines it touches, after all placement: text and the snail art never join); `dimRows`
  drops them (a box's answered rows are not targets); `fadeRows`, which sinks the backdrop of a modal
  toward the canvas, keeps a segment's text, style and effect, so a spinner behind a box keeps
  stepping, and drops what acts or joins (`Hit`, `Click`, `Widget`, `Line`, `Bar`), so the backdrop
  joins before it fades and no line of it reaches the top box. Overlays (internal/ui/overlay.go) are data
  (DESIGN §8 has the detail views, `danger` arming and `AskFirst`); `BodySegs` is sanitized like
  `Body`, which stays what `y` copies, and a Go view's `Parts` draw each block the way its text
  is written (markdown, `diff`, a chroma language in a canvas well) at the width the box takes. A list with `Overlay.Follow` moves the stage's scrolled
  region to the unit its selected item names by `OverlayItem.Anchor`, the number the chat stamps on
  every row of a unit (`Segment.Anchor`, `unitAt`, `jumpTop`: centered when the message fits, else
  its first row under the pins), and bands the rows the pick draws with `FillBand` (`bandSpan`, the
  scrollbar cell aside, and the reply's readout, which `internal/ui/chat.go` keeps out of the anchor) while
  keeping it out of the modal fade (`fadeRows`); it opens on the item the reader is at (`followRow`:
  the row the view was placed at, kept in `scrollState.focus` with the top its jump left in `placed`
  because a clamped centered placement cannot name its message from the viewport alone, and good only
  while the view still sits there; `scrollRows` drops it when the scroll lands at the transcript's
  end, and so do `leaveTail` and `followStage`; failing that, the last message when the view reaches
  the end of the transcript, else the message whose rows hold the view's middle row, else the last
  one above it, so a wheel line or two keeps a centered message and scrolling on moves to the ones it
  passes), `syncFollow` in `compose` carries a selection change out before the paint,
  and the topmost *following* list answers rather than the top overlay, so a screen a row
  opened (`o` on a call) keeps the run open and the band on its row under it, enter closes
  keeping the place, and `cancelOverlay` puts the chat back on the mark its state took when it
  opened (`followMark`, focus included).
  Opening the id of an open overlay replaces it in place
  (`replaceOverlay`, keeping the filter and the selection on its row; no origin check), a detail
  view open over it rebuilt from its item or dropped with it (`refreshDetail`), and a Go
  screen's list-level key runs a command (`OverlayKey.Run`, whatever opened the list; a
  `Danger` one on its second press, a refresh keeping the first and a click or a wheel
  dropping it), or with neither command nor fn names the rows' action of that hotkey for the
  footer, and a `Wide` box takes `boxInner`'s width whatever opened it (the workflow
  screens). A list shows the filter row only when it opened with `filterMin` rows or more,
  which a rebuild keeps (`listFilter.filterable`). A
  toggle list rebuilds when the overlay stacked on it closes (`closeOverlayByID`; a detail
  view only after an action) and after a reload (`SignalBoot`), so every caller acts first
  and closes after: the worker then runs the rebuild behind the action. A row that fills the
  composer closes a toggle list, which would keep the keys. A row's `verb` names enter's action
  in the hint and keeps space from toggling it. Every hint's label is lowercase, the
  verbs' grammar too (`h back`, `o open the view`), and a row's two keys on one letter
  raise: a key runs one action.
- **The key loop never waits on Lua.** Keymap right-hand sides, Lua commands, completion
  sources and a toggle list's `Overlay.Reload` run in order on one worker goroutine
  (`App.background`); Go commands stay inline. Keymaps, clicks, one-shot timers and async
  callbacks call `refresh` when they return. The frame tick sleeps 250 ms unless something
  moves by itself (a turn, a shown run, a drawn spinner or shimmer, a toast, an armed flag) and always lands on a 50 ms wall boundary,
  so `ScreenState.Frame` steps one per tick while a turn re-renders every one; a change goes through
  `App.bump`, which wakes it, and all stale widgets render as one message; a widget a message
  resized (a composer newline, a resize) renders on that message, so no frame draws lines
  rendered for another size. `Host.ScreenState` builds a frame's
  `todos`, `projects`, `instructions` and log row count on first read: keep new
  expensive fields lazy, and cache one that reads disk or the
  store (`recentSessions`). A widget's rows are wrapped once per
  width and cached, and the chat keeps its settled rows until the state they were
  built from moves (`chatFolds.settledKey`, `internal/ui/chat.go`: the log's epoch, cut
  and edit, its row count, the width, the rail's view, the folds' version, the run on
  screen and whether a reply streams), so a live frame rebuilds only the tail.
  `Host.Session` shares the transcript
  rows, which are appended or replaced whole, never written in place. A render returning
  `true` second runs again next frame and wakes the tick, which is how the chat fills a
  long chat under a 30 ms budget, latest units first, each render at least a full view topped
  with the prompt the view pins (rendered out of order when the view does not reach it), so a
  loaded machine draws the view whole; a region scrolled up keeps the last
  whole render until the fill ends (`App.scrolledUp`), so the reader's rows stay put, and a
  view switch drops the cached transcript (`syncChatScroll`) so the frame it lands on draws the
  incoming one whole, never the other view's rows and never its scroll state measured against
  them. A turn's `turnScope` context (host.go) covers the model and tools, so
  a second esc within 2s (`App.arm`, shared with the idle ctrl+c quit) stops all of them;
  a second ctrl+c while "Cancelling" quits. A job the user waits on that is not a turn
  (`/btw`, `/plugin` network work, `/model update`, `/health`, an MCP start,
  the idle recap) holds a `Host.Busy` entry: `ScreenState.Busy` is the list of
  them, newest first, the chat's pending row names the oldest as one part (`and N more`
  when more run), the working rule draws
  `Busy[0]`, and the same double esc
  (`CancelBusy`, never `zemi.agent.cancel`) cancels every entry that has a cancel func.
  An empty session has one such row and no chat rows: `internal/ui/textfmt.go`'s `emptyChat` is the
  one predicate both widgets read, the chat yielding the region and `splash.go` drawing
  the pending row on a row its block reserves, since the splash pads to the whole region
  and a row of the chat's would scroll it out of the top as boot work starts and ends.
- **Widgets read the frame's state, never Go-drawn text.** The statusline widgets, the
  composer's rule and the quit, key sequence and scroll flags are Go widgets reading
  `hostapi.ScreenState`, which `Host.ScreenState` fills from the app's `hostapi.UIState` and
  the host's own state; a new flag goes into those two, not into `statusRow` or `compose`.
  No Lua draws. Animation a widget
  cannot render at the 50 ms frame clock is a segment `effect` the UI applies (`shimmer`, and `spin`, which steps a `hostapi.Marks` spinner). A
  turn's end (the `ui.notify` alert, the done eyes, the recap timer) applies once per chain of
  turns, on the first frame that reads the agent idle (`onFrame`). OSC 9;4 progress and the OSC
  11 background go through `tea.View.ProgressBar` and `tea.View.BackgroundColor`, so bubbletea
  resets them on every exit, a signal included. Host chrome draws in whorl's
  roles (`chromeStyles`, built once), and a segment's `Fill` says
  what it sits on: `hostBox` and the composer frame put theirs on the panel, where a
  style that paints the canvas takes the panel instead (`segStyle`).
- **Drag and drop is a paste.** `ui/drop.go` turns a paste made only of existing paths into
  composer tokens via `hostapi.AttachWord`, the same form the `@` popup inserts for a project
  file: an `@<path>` reference the user can go on editing, which the completion picks up again.
  Every attachment rides the `@` word: a project file project-relative (`./` when its bare name
  would answer as a skill), a file from outside the project `~`-spelled or absolute, a directory
  with the trailing slash a completion walks into (a project directory answers by name from the
  corpus, and its name plus the slash lists it). A word a bare token cannot carry - one holding
  whitespace, or ending in the punctuation a sentence ends on - rides quoted, the project frame
  kept (`'` when the name itself holds `"`): `@"docs/My Notes.md"` inside the project,
  `@"~/My Docs/a.txt"` outside, which
  `_attachRef`'s grammar reads back (the closing quote optional, so a
  word still being typed resolves too; a space closes an open-quoted word
  whose path resolves, so prose after it rides outside). A clipboard image has no source path: it lands a copy as
  `pasted-<ts>.png` in `<data>/drops` and inserts the copy's `@` word. `expandAttachments` looks
  the whole word up as a skill first, then resolves the path - `~` under home, the rest against
  the project, `/` as itself - wherever it lives, a symlink out of the project attaching its
  target; a directory attaches its own entries, as `ls` lists them - one
  name per line, a subdirectory's trailing its slash - capped with the
  remainder named at the tail; a word no path answers falls to
  `attachEntity`, which resolves a saved workflow or an agent definition by
  name and attaches the run hint (kind, description, the exact call), a name
  held by both going to the workflow; what attached shows in the attach echo, and
  a refusal names why with the absolute
  path (the read tool refuses `~`).
