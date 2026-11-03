# Keys and UI

The screen is drawn in Go (`internal/ui`): the splash, the chat, the composer's rule and the statusline. A plugin adds nothing to it directly.

## Keys

```lua
zemi.keymap.set("<leader>r", "review", { desc = "review changes" }) -- a command name or a host action
zemi.keymap.set("ctrl+t", function() zemi.agent.send("run the tests") end, { desc = "run tests" })
zemi.keymap.del("ctrl+y") -- shipped keys unbind too
zemi.keymap.set("ctrl+j", "down", { list = true }) -- moves in every list, popup and prompt
zemi.keymap.set("ctrl+k", "up", { list = true })
```

A key is a non-printable key or a sequence that starts with one (`"ctrl+x s"`; `<leader>` is `keymap.leader`, ctrl+x). Binding a taken key rebinds it. The shipped keys are Go's (`defaultKeymap` in `internal/hostapi/keymap.go`: pgup and pgdown, shift+up and shift+down, ctrl+k and ctrl+j, ctrl+home, ctrl+b, alt+up and alt+down, ctrl+m, alt+o); `zemi.keymap.set` rebinds one and `keymap.del` unbinds it. Host actions: `zemi.ui.help`, `zemi.model.pick`, the chat's scroll moves - `zemi.ui.scroll.up`, `zemi.ui.scroll.down`, `zemi.ui.scroll.top`, `zemi.ui.scroll.bottom`, `zemi.ui.scroll.line_up`, `zemi.ui.scroll.line_down`, and `zemi.ui.scroll.sticky_up` / `zemi.ui.scroll.sticky_down` for the previous or next pinned row, a turn's prompt or an opened run's digest - and for the composer `zemi.composer.submit`, `zemi.composer.insert <text>`, `zemi.composer.history_prev`, `zemi.composer.history_next`, `zemi.composer.paste` and `zemi.composer.edit`. A command with its handler is in `lua.md`'s examples.

Lists, popups and prompts keep their keys (`/help` lists them): up, down, j, k, ctrl+p and ctrl+n move, ctrl+d and ctrl+u move half a page, scroll a box's text, or scroll the chat half a view, a typed line included (its own text stays; in a prompt or a filter's line ctrl+u kills back to the line's start instead). The composer popups keep j, k, home and end for the draft. `list = true` adds a key for a move (`"up"`, `"down"`, `"pgup"`, `"pgdown"`, `"home"`, `"end"`, `"ctrl+d"`, `"ctrl+u"`); it is one non-printable key other than enter, esc, tab, shift+tab, space and backspace, and elsewhere the key stays what it was (ctrl+j still scrolls the chat), as it does while a line types when the move is home or end.

## Colors

The palette is whorl, baked in Go (`internal/hostapi/themes.go`): a set of roles (`bg`, `fg`, `accent`, `literal`, `ok`, `warn`, `danger`, `dim`, `rule`, `panel`, `band`, `addbg`, `delbg`, `well`), derived where the spec leaves a role out so every text role stays readable on every surface and `panel` and `band` stay near `bg` (DESIGN §6.6). There are no other themes and no theme command.

Groups are defined once, for whorl, in the same file: `Accent`, `Dim`, `Rule`, `Title`, `Literal`, `Danger`, `Warn`, `Ok` and the rest, each naming a role, a hex or an ANSI color, or linking to another group (`Snail`, the mascot's shell, links to `Accent`). A plugin colors what it draws by naming these groups in an overlay's segments and never a hex: an `hl` a group does not answer to draws unstyled.

## Layout

The layout is one fixed tree in Go (`internal/ui/layout.go`). The main column centers past 124 cells, the stage (the chat, a blank row under it) centers at up to 100, the composer sits framed under it with the snail's rule above, and the statusline is a bar under that. Nothing in config or Lua moves it and there are no node ids: prompts, overlays and toasts go over the stage, and a box takes its width on the frame it opens in. `zemi.ui.scroll(action)` scrolls the chat as the `zemi.ui.scroll.*` host actions do, for a click or a handler; a `top` or `bottom` issued before the chat's rows arrive lands on them when they do, and `top` stays until the chat scrolls again. `zemi.ui.screenshot()` is the screen as text.
