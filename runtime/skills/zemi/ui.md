# Keys and UI

Everything on screen but the composer and the Go-drawn prompts is a Lua widget. The shipped UI is readable source in the tui directory the system prompt names: `theme.lua`, `layout.lua`, `keymaps.lua`, the widgets in `statusline.lua`, `working.lua`, `chat.lua`, `splash.lua` and `exit.lua`, and the helpers they share in `text.lua` and `snail.lua`. Read the one you change. Restyling the shipped UI (a replaced widget, `hl.set`, a theme pick, the layout) is user config, in init.lua.

## Keys

```lua
zemi.keymap.set("<leader>r", "review", { desc = "review changes" }) -- a command name or a host action
zemi.keymap.set("ctrl+t", function() zemi.agent.send("run the tests") end, { desc = "run tests" })
zemi.keymap.del("ctrl+y") -- shipped keys unbind too
zemi.keymap.set("ctrl+j", "down", { list = true }) -- moves in every list, popup and prompt
zemi.keymap.set("ctrl+k", "up", { list = true })
```

A key is a non-printable key or a sequence that starts with one (`"ctrl+x s"`; `<leader>` is `keymap.leader`, ctrl+x). Binding a taken key rebinds it: ctrl+t above replaces the shipped next profile key. The shipped keys are in `keymaps.lua`. Host actions: `zemi.ui.help`, `zemi.model.pick`, `zemi.ui.reasoning.toggle`, `zemi.ui.scroll.up`, `zemi.ui.scroll.down`, `zemi.ui.scroll.top`, `zemi.ui.scroll.bottom`, `zemi.ui.scroll.line_up`, `zemi.ui.scroll.line_down`, `zemi.ui.scroll.sticky_up` and `zemi.ui.scroll.sticky_down` (the sticky ones go to the previous or next run of sticky rows, the chat's prompts and report notices; each takes a node id after a space), `zemi.ui.focus.set <id>`, `zemi.ui.focus.next`, `zemi.ui.focus.prev`, `zemi.permission.mode.next`, `zemi.permission.mode.prev`, `zemi.agent.profile.next`, `zemi.agent.profile.prev`, and for the composer `zemi.composer.submit`, `zemi.composer.insert <text>`, `zemi.composer.history_prev`, `zemi.composer.history_next`, `zemi.composer.paste` and `zemi.composer.edit`. A command with its handler is in `lua.md`'s examples.

Lists, popups and prompts keep their keys (`/help` lists them): up, down, j, k, ctrl+p and ctrl+n move, ctrl+d and ctrl+u move half a page or scroll a box's text. A typed line keeps ctrl+u for its text (ctrl+d then scrolls nothing), and the composer popups keep j, k, home, end and ctrl+u for the draft. `list = true` adds a key for a move (`"up"`, `"down"`, `"pgup"`, `"pgdown"`, `"home"`, `"end"`, `"ctrl+d"`, `"ctrl+u"`); it is one non-printable key other than enter, esc, tab, shift+tab, space and backspace, user config only, and elsewhere the key stays what it was (ctrl+j is still the composer's newline), as it does while a line types when the move is home, end or ctrl+u.

## Widgets

A widget registered with a shipped id replaces it; a new id adds one.

Shipped ids: `splash` and `chat` (region `chat`); `working` (the composer frame's top rule: the mascot and the turn status); in `statusline.left` `statusline.mode`, `statusline.quit`, `statusline.cwd`, `statusline.goal`, `statusline.loop`, `statusline.agents`, `statusline.view`, `statusline.learn`, `statusline.keys`, `statusline.scroll`; in `statusline.right` `statusline.profile`, `statusline.model`, `statusline.context`, `statusline.percent`, `statusline.cost`, `statusline.hints` (the `/` and `@` keys, until the first message); `exit` (region `exit`, which no layout draws: printed to the shell on quit, foreground colors only, and only user config adds to it; `zemi.ui.widget.del("exit")` turns it off, `ctx.resume` is the command that reopens the session). zemi draws a separator between the statusline widgets that draw, so each returns its text alone. Each side fills from its outer edge inward in rising priority (the right side from the right edge), and a short row drops the highest first. So a lasting widget takes a low priority and one that comes and goes a high one, where it moves nothing, and a widget one above a shipped one's priority (`zemi.ui.widget.list()` has them) sits just inside it.

Rules for `render(ctx)`:

- `ctx` is a `zemi.WidgetCtx` (the types list every field): `width`, `height`, `transcript`, `agent`, `usage`, `frame` and `ended` are the common ones.
- It returns lines of segments `{ text, hl }`, `{}` for none. A segment may add `click = "id"` (the widget's `on_click(ev)` gets it as `ev.id`), `effect = "shimmer"` or, on a `zemi.ui.marks.spin` glyph, `effect = "spin"` (the UI steps the spinner), `copy = false` (decoration a selection skips), `hover = false` (a clickable row hovering does not restyle), `soft_wrap = true` (the line continues on the next row) and, on a row's first segment, `sticky = true` (the row heads what follows: a scrolled region pins it over its first row while it is above the view, drawn in the `Pinned` group, as the chat does with a prompt).
- It is pure and fast: only read-only calls (`config.get`, `state.get`, `ui.width`, `session.messages`, ...) work in it, and not `zemi.fs` or a handle's `wait`; anything else raises, and it has about 10 ms. Slow work runs in a `zemi.job` or `zemi.timer` callback that sets a local the render reads; job, http and one-shot timer callbacks redraw when they return.
- A widget redraws only when something else does. One that changes on its own calls `zemi.ui.redraw` from `zemi.timer.every`.
- Returning the same line table again is free; never change one after returning it. With more work than fits, return what is ready and `true` as a second result, and zemi renders it again next frame (the chat fills a long session this way, 30 ms a render at most).
- Colors are theme groups: `Accent`, `Dim`, `Rule`, `Title`, `Literal`, `Danger`, `Warn`, `Ok`, and the rest in `theme.lua`, which link to one of them.
- A replacement statusline widget keeps its click with `on_click = function(ev) zemi.command.run(ev.id) end`. A replacement chat keeps reasoning toggles (`zemi.ui.reasoning.toggle()`), agent rows (`zemi.command.run("agents", "view " .. row.run)`) and tool output folding (keyed by the row's `tool:<idx>` click id).

```lua
-- a tip of your own in place of the default splash's
local default = require("zemi.tui")
zemi.ui.widget.add({
	id = "splash",
	region = "chat",
	priority = 5,
	render = function(ctx)
		if ctx.transcript_count > 0 or ctx.agent_active then return {} end
		local rows = default.splash(ctx)
		for i, row in ipairs(rows) do
			if row[2] and row[2].text == "● Tip  " then
				rows[i] = { row[1], row[2], { text = "hack the planet", hl = "Dim" } }
			end
		end
		return rows
	end,
})

-- the git branch: a job fills a local, the widget reads it
zemi.permission.rule.add({
	subject = "lua.job.start",
	pattern = "git rev-parse *",
	decision = zemi.permission.decision.ALLOW,
	match = "git rev-parse --abbrev-ref HEAD",
})
local branch = ""
zemi.ui.widget.add({
	id = "statusline.branch",
	region = "statusline.right",
	priority = 50,
	render = function() return { { { text = branch, hl = "Dim" } } } end,
})
local function poll()
	zemi.job.start({
		cmd = "git",
		args = { "rev-parse", "--abbrev-ref", "HEAD" },
		on_exit = function(res) branch = res.stdout:gsub("%s+$", "") end,
	})
end
poll()
zemi.timer.every(30000, poll)
```

Keep such state in a local, not `zemi.state`: that store is shared by every project, so a branch kept there shows in the wrong repository.

## Colors and themes

A theme is one line of colors: `zemi.ui.theme.add("mine", { bg = "#1e1e2e", fg = "#cdd6f4", accent = "#89b4fa", ok = "#a6e3a1", warn = "#f9e2af", danger = "#f38ba8" })`, then `zemi.config.set("ui.theme", "mine")`. zemi derives the rest (`literal`, `dim`, `rule`, `panel`, `band`, `addbg`, `delbg`; give any to pin it) and moves a color too faint to read on `bg`. To port a Ghostty theme (`ghostty +list-themes`, files in its `themes` directory), copy its `background` as `bg`, `foreground` as `fg` and `palette = 0..15` in order as `ansi = { ... }` (16 colors) in place of the four hues, as `theme.lua` does for the shipped ports: whorl, catppuccin, tokyonight, gruvbox, nord, dracula, solarized, rose-pine, kanagawa, one, everforest and github. A `<name>-light` theme beside it draws on a light terminal. `/theme` picks one with the list open, repainting as you move. Groups name a role (`fg = "accent"`), hex or an ANSI color name and are defined once for every theme: `zemi.ui.hl.set("Accent", { fg = "#c4a7e7" })` replaces one, and groups that link to it follow (`Snail`, the mascot's shell, links to `Accent`: set it to recolor the snail alone).

## Layout

`zemi.ui.layout.set({ root, floats })` in init.lua replaces `layout.lua` whole (the types file has every field). A `box` (`dir = "row"` or `"col"`, `sep`) holds `region`s, the one `composer` (with `frame = true` its square frame's `top`, `left` and `right` rules hold regions: the shipped layout keeps the mascot and the statusline there; `placeholder` is the dim text an empty draft shows), a statusline `bar` and `spacer`s. Each is sized in cells, `"N%"`, `"fill"` (with `weight`) or `"content"`, with `min`, `max`, `hidden` and `collapse`. `stage = true` marks where prompts, overlays and toasts go; `floats` draw a region over the tree.

```lua
-- statusline on top, a todo sidebar left of the chat
zemi.ui.widget.add({
	id = "user.todos",
	region = "sidebar",
	render = function(ctx)
		local rows = {}
		for _, td in ipairs(ctx.todos) do
			rows[#rows + 1] = { { text = td.content, hl = "Dim" } }
		end
		return #rows > 0 and rows or { { { text = "no todos" } } }
	end,
})
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
