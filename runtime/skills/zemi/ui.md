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

A key is a non-printable key or a sequence that starts with one (`"ctrl+x s"`; `<leader>` is `keymap.leader`, ctrl+x). Binding a taken key rebinds it. The shipped keys are in `keymaps.lua`. Host actions: `zemi.ui.help`, `zemi.model.pick`, `zemi.ui.scroll.up`, `zemi.ui.scroll.down`, `zemi.ui.scroll.top`, `zemi.ui.scroll.bottom`, `zemi.ui.scroll.line_up`, `zemi.ui.scroll.line_down`, `zemi.ui.scroll.sticky_up` and `zemi.ui.scroll.sticky_down` (the sticky ones go to the previous or next run of sticky rows, the chat's prompts and opened runs' digests; each takes a node id after a space), `zemi.ui.focus.set <id>`, `zemi.ui.focus.next`, `zemi.ui.focus.prev`, and for the composer `zemi.composer.submit`, `zemi.composer.insert <text>`, `zemi.composer.history_prev`, `zemi.composer.history_next`, `zemi.composer.paste` and `zemi.composer.edit`. A command with its handler is in `lua.md`'s examples.

Lists, popups and prompts keep their keys (`/help` lists them): up, down, j, k, ctrl+p and ctrl+n move, ctrl+d and ctrl+u move half a page, scroll a box's text, or scroll the chat half a view, a typed line included (its own text stays; in a prompt or a filter's line ctrl+u kills back to the line's start instead). The composer popups keep j, k, home and end for the draft. `list = true` adds a key for a move (`"up"`, `"down"`, `"pgup"`, `"pgdown"`, `"home"`, `"end"`, `"ctrl+d"`, `"ctrl+u"`); it is one non-printable key other than enter, esc, tab, shift+tab, space and backspace, and elsewhere the key stays what it was (ctrl+j still scrolls the chat), as it does while a line types when the move is home or end.

## Colors and themes

A theme is one line of colors: `zemi.ui.theme.add("mine", { bg = "#1e1e2e", fg = "#cdd6f4", accent = "#89b4fa", ok = "#a6e3a1", warn = "#f9e2af", danger = "#f38ba8" })`, then `zemi.config.set("ui.theme", "mine")`. zemi derives the rest (`literal`, `dim`, `rule`, `panel`, `band`, `addbg`, `delbg`, `well`; give any to pin it), keeps `panel` and `band` near `bg`, and moves a color too faint to read on any of the three. To port a Ghostty theme (`ghostty +list-themes`, files in its `themes` directory), copy its `background` as `bg`, `foreground` as `fg` and `palette = 0..15` in order as `ansi = { ... }` (16 colors) in place of the four hues, as `theme.lua` does for the shipped ports: whorl, catppuccin, tokyonight, gruvbox, nord, dracula, solarized, rose-pine, kanagawa, one, everforest and github. A `<name>-light` theme beside it draws on a light terminal when it is registered. `/theme` picks one with the list open: enter on a row repaints the screen in it, so you can try the next. Groups name a role (`fg = "accent"`), hex or an ANSI color name and are defined once for every theme: `zemi.ui.hl.set("Accent", { fg = "#c4a7e7" })` replaces one, and groups that link to it follow (`Snail`, the mascot's shell, links to `Accent`: set it to recolor the snail alone), while those naming the role themselves (`Title`) do not, so a recolor everywhere is a theme's `accent`.

## Layout

`zemi.ui.layout.set({ root, floats })` in init.lua replaces `layout.lua` whole (the types file has every field). A `box` (`dir = "row"` or `"col"`, `sep`) holds `region`s, the one `composer` (with `frame = true` its square frame's `top`, `left` and `right` rules hold regions: the shipped layout puts only `working`, the mascot, in its `top`, and keeps the statusline on a bar below the box; `placeholder` is the dim text an empty draft shows), a statusline `bar` and `spacer`s. Each is sized in cells, `"N%"`, `"fill"` (with `weight`) or `"content"` (a region, a bar or the composer in a col only), with `min`, `max`, `hidden` and `collapse`. `stage = true` marks where prompts, overlays and toasts go (the shipped stage is the box holding the chat: a box centers on it and the toasts hug its right edge); `floats` draw a region over the tree. A hidden or collapsed child takes no `sep`. `zemi.ui.scroll(action, id)` moves a scrolled node from a click or a handler; a `top` or `bottom` issued before the node's rows arrive lands on them at the next layout, and `top` stays there until the node scrolls again.

```lua
-- statusline on top, a sidebar left of the chat
local layout = {
	root = {
		kind = "box",
		dir = "col",
		children = {
			{ kind = "bar", left = "statusline.left", right = "statusline.right" },
			{
				kind = "box",
				dir = "row",
				sep = true,
				children = {
					{ kind = "region", id = "sidebar", region = "sidebar", size = 30, min = 20, collapse = true, scroll = "top" },
					{
						kind = "box",
						dir = "col",
						stage = true,
						min = 40,
						children = {
							{ kind = "region", region = "header" },
							{ kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
							{ kind = "composer", frame = true, top = "working" },
						},
					},
				},
			},
		},
	},
}
zemi.ui.layout.set(layout)
zemi.keymap.set("<leader>b", function()
	local side = layout.root.children[2].children[1]
	side.hidden = not side.hidden
	zemi.ui.layout.set(layout)
end, { desc = "toggle the sidebar" })
zemi.keymap.set("alt+j", "zemi.ui.scroll.down sidebar", { desc = "scroll the sidebar" })
```

## Focus

A region with an id and `focusable = true` can take the keys: `zemi.ui.focus.set(id)`, the `zemi.ui.focus.set <id>` host action or a click gives them to it, `zemi.ui.focus.next` and `zemi.ui.focus.prev` cycle, and esc hands them back to the composer. Keymaps with `{ focus = id }` work only there and may be plain letters; `ctx.focus` names the node that has the keys.
