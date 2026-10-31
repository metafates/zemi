-- The chat widget: the transcript, the live turn and the todo list. A turn's
-- activity (thinking and tool calls) reads as a rail, which once settled
-- folds the calls a family counts into one digest row, its rail a click away.
local fmt = require("zemi.tui.text")
local util = require("zemi.util")

-- A tool row: a settled transcript row or a call of the live turn.
---@alias zemi.tui.Tool zemi.TranscriptRow|zemi.ToolCall

-- How a run of activity rows draws: its digest alone (settled), that digest
-- over its whole rail (a digest clicked open), or a rail of one row per step
-- (the live turn, an agent run's view).
---@alias zemi.tui.RunMode "fold"|"unfold"|"rail"

-- The settled transcript is rendered in units, a message or a run of
-- activity rows, kept across frames by the index of their first message:
-- while a reply streams the widget renders many times a second and only the
-- live turn changes. `settled` is the units' lines in order, which a render
-- hands back as they are while the transcript has `count` rows.
---@class zemi.tui.ChatUnit
---@field lines zemi.Line[]
---@field next integer Key of the message after the unit, the row's pos().
---@field mode zemi.tui.RunMode|"" How its activity run drew; "" for any other unit.
---@field cut integer The head cut the unit drew at: its rows' anchors and calls name the window of that render.
---@field open boolean? A reply's readout decision is still open: no unnamed row stood after it at render.
---@field rows integer? The transcript length the unit rendered at, which an open decision re-checks on growth.
---@type table<integer, zemi.tui.ChatUnit>
local units = {}
local units_key = ""
---@type { count: integer, lines: zemi.Line[], n: integer, active: boolean, replying: boolean, block: string? }?
local settled
-- the activity run a list walks open (the messages picker's rail view): a
-- render that opens or closes one cannot reuse the settled lines
local rail_open

-- The folds a click opens in place, by index: a step's calls (a summary row,
-- not an action), a run's whole rail and a notice or queued burst. A call or
-- a thought never folds here: its click opens a view of its own off the
-- widget (/call). Cleared when the transcript is replaced, and the step half
-- when a new turn starts, since the indexes are then someone else's.
---@type table<integer, true>
local steps = {}
---@type table<integer, true>
local unfolded = {}
-- runs shown whole, by first index (0 the live turn): a rail keeps its last
-- RAIL_KEEP rows and folds the earlier ones into one row until clicked
---@type table<integer, true>
local whole = {}
local RAIL_KEEP = 6
-- the queued burst of notices drawn whole: its older rows sit behind one
-- fold row until clicked, and go back behind it when the queue empties
-- The air a rail row keeps after the text it cuts, so an ellipsis reads as the
-- row's own and never runs into the window's edge; a row's right-aligned tail
-- keeps the measure's edge instead
local RAIL_EDGE = 4
-- notice rows of one burst kept in view, the older ones behind a fold row
local NOTICES_KEEP = 3

-- The label and click target of the one row that stands for a folded run:
-- a notice burst and a rail fold the same way, and opened it says what its
-- click does.
---@param open boolean?
---@param n integer
---@param click string
---@return string, string
local function foldRow(open, n, click) return open and "… collapse" or "… " .. n .. " earlier", click end

-- Whether a call folds into its step's counted row: the same test steps
-- groups by, so a fold can walk a rail's entries without drawing them.
local function grouped(t) return fmt.looks(t) and (t.label or "") == "" end

-- The tool the rail entry after i starts at, steps' entry loop walked off
-- the list: a fold needs the entry count and the kept tail's first tool
-- before any row is built, so a long turn draws its kept entries alone.
---@param list zemi.tui.Tool[]
---@param i integer
---@param last integer
---@return integer
local function nextStep(list, i, last)
	local t = list[i]
	if t.kind == "reasoning" or not grouped(t) then return i + 1 end
	local g = 0
	while i + g <= last and grouped(list[i + g]) do
		g = g + 1
	end
	if g == 1 then return i + 1 end
	return i + g
end
-- the transcript those tables belong to: its epoch, or the agent run on screen
---@type integer|string
local opened_in = 0
local live_tools = 0
-- the live run's first row, kept across the renders whose settled cache skips
-- the transcript read: the count and the rail name it, and a new view voids both
local live_count, live_rail, live_known = -1, -1, 0
-- the head rows the window has shed, the ctx's transcript_cut last seen: the
-- tables above key rows by transcript index plus it, so a trim moves every
-- key by one arithmetic delta instead of dropping what the reader opened
local cut_seen = 0
-- The cache key of transcript row n: its place in the log's tail, the window
-- index plus the head cut, which a trim shifts without a rebuild. The live
-- turn's negative indexes are no transcript rows and stay.
---@param n integer
---@return integer
local function pos(n)
	if n > 0 then return n + cut_seen end
	return n
end

---@class zemi.tui.chat
---@field click fun(ev: zemi.WidgetClick) The shipped chat's `on_click`, keyed by its click ids, so a replacement chat keeps its rows acting.
local M = {}

-- How a finished row's status marks it. A call running or not started yet
-- spins, unless the open prompt asks about it: that one waits on the user,
-- so it neither spins nor counts.
---@type table<string, string>
local MARK_OF = { ok = "ok", err = "err", stopped = "stop", bg = "bg", warn = "warn" }

-- A detail row hangs under its head by one corner, every later row the same
-- cells in, two less when the row is nested under a branch mark, whose own
-- cells stand above it.
local HANG, INDENT = "  ⎿ ", "    "

-- A row's mark: the spinner while it runs, else how it ended. A mark never
-- hovers: a hovered header changes only its text.
---@param kind string A zemi.ui.marks key.
---@param click string?
---@return zemi.Segment
local function mark(kind, click)
	local m = zemi.ui.marks[kind]
	local effect = kind == "spin" and "spin" or nil
	return { text = m.glyph .. " ", hl = m.hl, copy = false, hover = false, effect = effect, click = click }
end

-- The rail a turn's activity stands on: column 0, its rows' marks from column
-- 2 and their text from column 4.
---@param pad string?
---@return zemi.Segment
local function bar(pad) return { text = "│ " .. (pad or ""), hl = "ThinkingBar", copy = false, hover = false } end
-- A rail row with only the bar: air around prose on the rail.
---@return zemi.Line
local function air() return { { text = "│", hl = "ThinkingBar", copy = false, hover = false } } end

-- A clipped block sinks at its top the way a scrolled region's edge does: its
-- first SINK rows, deepest first, easing off as the hidden rows come back.
local SINK = 4
---@param k integer Row's place in the block, 0 the top.
---@param past integer Rows the block hides above it.
---@return number
local function sink(k, past)
	if k >= SINK or past == 0 then return 0 end
	return (SINK - k) / (SINK + 1) * math.min(1, past / SINK)
end

--- Whether a row is thinking or a call: a run of them is one unit.
---@param m zemi.TranscriptRow
---@return boolean
local function activity(m) return m.kind == "reasoning" or m.kind == "tool" end

--- Whether a row is a notice: a burst of them draws as one block.
---@param m zemi.TranscriptRow
---@return boolean
local function notice(m) return m.kind == "notice" end

-- The block a row belongs to: blocks are one blank row apart, activity rows
-- stand together.
---@param m zemi.TranscriptRow
---@return "user"|"activity"|"text"|"note"
local function block_of(m)
	local k = m.kind or ""
	if k == "shell" or k == "" and m.role == "user" then return "user" end
	if k == "" then return "text" end
	return activity(m) and "activity" or "note"
end

-- "+4 -1": the lines a diff adds and removes, in the colors a diff marks the
-- same counts with (Ok adds, Danger removes). A count of zero is left out: the
-- sign alone would report nothing, and the "+++"/"---" headers are no changes.
-- Counted once per row, whose diff the text settles on: a render asks for a
-- row's diff again and again.
---@type table<integer, { diff: string, add: integer, del: integer }>
local counts = {}
---@param diff string
---@param idx integer The row's transcript index, negative for a live call.
---@return zemi.Line
local function diffstat(diff, idx)
	local c = counts[pos(idx)]
	if not c or c.diff ~= diff then
		local add, del = 0, 0
		for l in diff:gmatch("[^\n]+") do
			if l:match("^%+") and not l:match("^%+%+%+ ") then
				add = add + 1
			elseif l:match("^%-") and not l:match("^%-%-%- ") then
				del = del + 1
			end
		end
		c = { diff = diff, add = add, del = del }
		counts[pos(idx)] = c
	end
	---@type zemi.Line
	local out = {}
	if c.add > 0 then out[#out + 1] = { text = "+" .. c.add, hl = "Ok" } end
	if c.del > 0 then out[#out + 1] = { text = (c.add > 0 and " -" or "-") .. c.del, hl = "Danger" } end
	return out
end

-- The row builders one render uses, drawing at width into B.lines.
---@param ctx zemi.WidgetCtx
---@param width integer
local function builder(ctx, width)
	---@class zemi.tui.ChatBuilder
	---@field lines zemi.Line[]
	---@field prev string? The block the last row belongs to.
	local B = { lines = {} }
	---@param row zemi.Line
	local function push(row) B.lines[#B.lines + 1] = row end
	-- The bar-only row that bounds the rail and sets its entries apart; never
	-- two in a row, so a rail opening on one draws no second.
	local function frame()
		local prev = B.lines[#B.lines]
		if prev and #prev == 1 and prev[1].text == "│" then return end
		push(air())
	end
	function B.blank() push({ { text = "" } }) end
	---@param block "user"|"activity"|"text"|"note"|"stats"|"queued"
	function B.gap(block)
		if B.prev and not (B.prev == "activity" and block == "activity") then B.blank() end
		B.prev = block
	end
	---@param s string
	---@return string
	local function relpath(s) return fmt.relpath(s, ctx.cwd, ctx.home) end
	-- row ending in tail, a line of segments, flush with the right edge
	---@param row zemi.Line
	---@param tail zemi.Line
	---@param click string?
	---@return zemi.Line
	local function suffixed(row, tail, click)
		if #tail == 0 then return row end
		row[#row + 1] = { text = string.rep(" ", math.max(2, width - fmt.cells(row) - fmt.cells(tail))) }
		for _, sg in ipairs(tail) do
			row[#row + 1] = { text = sg.text, hl = sg.hl, click = click }
		end
		return row
	end

	-- A reply is indented, not bulleted: its lines share the user's left edge.
	---@param text string
	function B.markdown(text)
		for _, segs in ipairs(zemi.ui.markdown(text, width - 2)) do
			local row = { { text = "  ", hl = "Normal", copy = false } }
			for _, sg in ipairs(segs) do
				row[#row + 1] = sg
			end
			push(row)
		end
	end
	-- The user's own words: the bar on every row, in the accent for a message
	-- that went and dim for one still waiting its turn. A sent message's rows
	-- are sticky: the chat pins its first row while the turn's rows fill the
	-- view, and the bar never hovers there.
	---@param text string
	---@param waiting boolean? Waiting for the turn's next step boundary.
	function B.user(text, waiting, foreign)
		local bar_hl = waiting and "Queued" or foreign and "Dim" or "UserMsg"
		-- the queued tag rides the first row (2 bar + 10 chip cells): the wrap
		-- leaves them out, or the drawn row runs past the region and the
		-- terminal wraps it onto an unbarred line of its own
		local room = waiting and width - 12 or width - 2
		local wrapped = zemi.ui.wrap(text, room)
		-- a user row past the composer's own paste threshold folds: the model
		-- read every line, the chat shows the head and names the way to the rest
		local draw = #wrapped > 10 and 9 or #wrapped
		for i, segs in ipairs(wrapped) do
			if i > draw then break end
			local row = { { text = "▌ ", hl = bar_hl, copy = false, hover = false, sticky = not waiting } }
			if waiting and i == 1 then
				-- the row is provisional: it is the queue's until the turn reads
				-- it, and settles elsewhere without the tag. The dot keeps the
				-- tag apart from content that itself starts with "queued"
				row[#row + 1] = { text = "queued · ", hl = "Queued", copy = false }
			end
			for _, sg in ipairs(segs) do
				sg.hl = waiting and "Dim" or "Normal"
				row[#row + 1] = sg
			end
			push(row)
		end
		if #wrapped > draw then
			push({
				{ text = "▌ ", hl = bar_hl, copy = false, hover = false, sticky = not waiting },
				{ text = "… +" .. (#wrapped - draw) .. " lines · ctrl+m to read", hl = "Dim", copy = false },
			})
		end
		-- another window wrote this: name it under the message, the reply
		-- readout's shape, so "who wrote this" reads off the row
		if foreign then
			B.lines[#B.lines + 1] = { { text = "  ", copy = false }, { text = "· other window", hl = "Dim" } }
		end
	end

	-- A subagent's or a plugin's message waiting for the turn's next step
	-- Thinking text on the rail at column 4, prose wide at most, its last
	-- tail lines when tail is set, sunk at the top while lines are hidden
	-- above; a blank line keeps only the bar. Its rows are body: no hover,
	-- but a click acts as the row above them.
	---@param text string
	---@param tail integer?
	---@param click string?
	---@return integer rows
	function B.reasoning(text, tail, click)
		local body = zemi.ui.wrap((text:gsub("%s+$", "")), math.max(math.min(width - 4, fmt.PROSE), 20))
		local from = tail and math.max(1, #body - tail + 1) or 1
		for n = from, #body do
			local row, blank = { bar("  ") }, true
			local sunk = sink(n - from, from - 1)
			for _, sg in ipairs(body[n]) do
				sg.hl, sg.click, sg.hover, sg.fade = "AgentReason", click, false, sunk
				row[#row + 1] = sg
				blank = blank and sg.text == ""
			end
			push(blank and air() or row)
		end
		return #body - from + 1
	end
	-- "│ ✻ thought 8s"; the caller draws the thought's text itself when it
	-- unrolls, so this is the header row alone.
	---@param secs number
	---@param click string
	function B.thought(secs, click)
		push({ bar(), mark("thought", click), { text = fmt.thought(secs), hl = "Dim", click = click } })
	end
	-- The model thinking now: drawn outside the rail's own loop, so it frames
	-- itself as an entry does. The label breathes while the row lives; a
	-- settled thought is a thought row, static.
	---@param tail string? The thinking so far, whose last lines show under it.
	function B.thinking(tail)
		frame()
		push({ bar(), mark("spin"), { text = "thinking", hl = "Dim", effect = "shimmer" } })
		if tail then B.reasoning(tail, 4) end
		frame()
	end

	-- What counted calls did: "read 2 files, ran 1 command: go vet".
	---@param group zemi.tui.Tool[]
	---@return string
	local function said(group)
		---@type string[]
		local commands = {}
		for _, t in ipairs(group) do
			if fmt.tool(t) == "shell" then
				commands[#commands + 1] = relpath(t.text or t.input or ""):gsub("%s+", " ")
			end
		end
		return fmt.counts(group, commands, true)
	end
	-- A step's unlabelled counted calls as one rail row saying what they
	-- did, marked as they are: every call it counts went well.
	---@param group zemi.tui.Tool[]
	---@param click string
	function B.group(group, click)
		push({
			bar(),
			mark("ok", click),
			{ text = fmt.clip(said(group), width - 4 - RAIL_EDGE), hl = "Normal", click = click },
		})
	end

	-- One call as its own row: the verb and the label or input, what it
	-- changed or how long it runs at the right, and under it what it said,
	-- unless terse folds it into that header alone.
	---@param t zemi.tui.Tool
	---@param idx integer Transcript index, negative for a live-turn call.
	---@param o { nest: string?, terse: boolean? } nest: the two cells a row hangs by, its branch mark; terse: a superseded call of the live rail reads as its header.
	function B.tool(t, idx, o)
		local name = fmt.tool(t)
		-- NOTE: every running row reads as asked about while a prompt is open,
		-- a parallel call that runs on meanwhile included.
		local asked = t.status == "run" and (ctx.waiting or "") ~= ""
		-- a call nested under its step's row hangs by its branch mark, and the
		-- rows under it keep a line in that mark's place: a bar while more
		-- siblings follow, a gap under the last one, so the tree stays joined
		local pad = o.nest
		local below = pad and (pad == "└ " and "  " or "│ ") or nil
		local terse = o.terse
		-- a running row reads Dim beside its spinner until it ends
		local dim = not asked and MARK_OF[t.status] == nil
		local inner = width - 2 - (pad and 2 or 0)
		local input = relpath(t.text or t.input or "")
		-- the row is one line; a heredoc's lines wait in its click view
		input = (input:gsub("%s+", " "))
		local label = t.label or ""
		-- the row's right-aligned tail: what it changed, then how long it ran
		---@type zemi.Line
		local tail = (t.diff or "") ~= "" and diffstat(t.diff, idx) or {}
		---@param text string
		local function add(text)
			if #tail > 0 then tail[#tail + 1] = { text = " · ", hl = "Dim" } end
			tail[#tail + 1] = { text = text, hl = "Dim" }
		end
		if t.status == "run" and not asked and (t.elapsed or 0) >= 1 then add(fmt.duration(t.elapsed, true)) end
		local sw = fmt.cells(tail)
		local verb, rest = fmt.head(name, label, input, t.status)
		verb = fmt.lower(verb)
		local room = inner - 2 - zemi.ui.width(verb) - (sw > 0 and sw + 2 or 1 + RAIL_EDGE)
		---@param text string
		---@param n integer
		---@return string
		local function fit(text, n) return fmt.clip(text, n) end
		-- a command is highlighted where it is what the row reads, the head;
		-- under its label it stays a Dim detail
		local code = name == "shell"
				and input ~= ""
				and label == ""
				and not dim
				and zemi.ui.highlight(fit(input, room - 1), "bash")
			or nil
		-- a click opens the row's own view; an agent's row opens its transcript
		-- and a workflow's its view (ids must differ between adjacent rows)
		local click = (t.run or "") ~= "" and ((name == "workflow" and "workflow:" or "agent:") .. t.run)
			or ("call:" .. pos(idx))
		-- a call that only looked around reads plainly, as its step's row does
		local vhl = dim and "Dim" or fmt.looks(t) and "Normal" or "AgentToolName"
		local head = { mark(asked and "wait" or MARK_OF[t.status] or "spin"), { text = verb, hl = vhl } }
		if code and label == "" then
			head[3] = { text = " ", hl = "Normal" }
			for _, sg in ipairs(code[1] or {}) do
				head[#head + 1] = sg
			end
		elseif rest ~= "" then
			head[3] = { text = fit(rest, room), hl = dim and "Dim" or "Normal" }
		end
		for _, sg in ipairs(head) do
			sg.click = click
		end
		-- the bar opens every row and the mark follows it; a nested row hangs by
		-- its branch mark where its own would stand, its step's state for it
		if pad then
			head[1] = bar(pad)
		else
			table.insert(head, 1, bar(pad))
		end
		local first = #B.lines + 1
		push(suffixed(head, tail, click))
		local hang, indent = pad and "⎿ " or HANG, pad and "  " or INDENT
		local corner = true
		---@param text string|zemi.Line
		---@param hl string
		---@param id string? The row's click id, the call's by default.
		local function detail(text, hl, id)
			local row = { { text = corner and hang or indent, hl = "Dim", copy = false } }
			for _, sg in ipairs(type(text) == "table" and text or { { text = text, hl = hl } }) do
				row[#row + 1] = { text = sg.text, hl = sg.hl, click = id or click, hover = false }
			end
			push(row)
			corner = false
		end
		-- an answered question's output holds "question\tanswer\tnote" lines,
		-- which take the place of the question under its header
		local output = t.output or ""
		local answered = name == "question" and output:find("\t") ~= nil
		-- the command a description stands for, then progress, then the job
		-- a timed-out command went on as, which the row carries
		if not terse then
			if not answered and label ~= "" and input ~= "" and not fmt.looks(t) then
				-- a labelled counted call on the rail shows its command
				detail(code and code[1] or fit(input, inner - 2 - RAIL_EDGE), "Dim")
			end
			-- a zoom or date row previews the line it opened: the first line
			-- only, so a split zoom's second child stays behind the click
			if (name == "zoom" or name == "date") and t.status == "ok" and output ~= "" then
				local line =
					fit((output:match("^([^\n]*)"):gsub("^%d+%+%d+|", ""):gsub("%s+", " ")), inner - 2 - RAIL_EDGE)
				if line ~= "" then detail(line, "Dim") end
			end
			if (t.detail or "") ~= "" then detail(fit((t.detail:gsub("%s+", " ")), inner - 2 - RAIL_EDGE), "Dim") end
			-- the job a timed-out command went on as opens its own view; the
			-- wording holds after the job exits and after a restart
			if (t.job or "") ~= "" then detail("backgrounded as " .. t.job, "Dim", "job:" .. t.job) end
		end
		-- the result's last lines: always for a failure, for what the user
		-- answered (question, plan); what the
		-- user just decided needs no restating: the model's text carries the
		-- reason and the instructions, the row the verdict. A running call
		-- keeps its line to the row; its view says the rest.
		if output:match("^permission denied %(user") then output = "denied by you" end
		if output:match("^The user dismissed") then output = "dismissed by you" end
		if output ~= "" and (t.status == "err" or t.status == "stopped" or name == "question" or name == "plan") then
			local out = util.split(output, "\n")
			-- a failure shows three lines where it says what went wrong: a
			-- tool's first, a command's last (its stderr and exit status); a
			-- click shows the rest
			local err, fromTail = t.status == "err", name == "shell"
			local from, to = 1, #out
			if err and #out > 3 then
				if fromTail then
					from = #out - 2
				else
					to = 3
				end
			end
			-- the host's own cut stands for the lines it dropped; NOTE: the
			-- marker's wording restates hostapi.MoreLines, so a Go change
			-- stops the count and a cut reads as one line
			local hidden = 0
			for i = 1, #out do
				local n = out[i]:match("^… (%d+) more lines?$")
				if i < from or i > to then hidden = hidden + (n and tonumber(n) or 1) end
			end
			local cut = "… " .. fmt.count(hidden, "more line", "more lines")
			if hidden > 0 and from > 1 then detail(cut, "Dim") end
			for i = from, to do
				local q, a, note = out[i]:match("^([^\t]*)\t([^\t]*)\t(.*)$")
				if answered and q then
					if zemi.ui.width(q) + 2 + zemi.ui.width(a) <= inner - 4 then
						detail({ { text = q, hl = "Dim" }, { text = "  " .. a, hl = "Normal" } }, "Dim")
					else
						detail(q, "Dim")
						detail(a, "Normal")
					end
					if note ~= "" then detail("note: " .. note, "Dim") end
				else
					detail(out[i], err and i == (fromTail and #out or 1) and "Danger" or "Dim")
				end
			end
			if hidden > 0 and from == 1 then detail(cut, "Dim") end
		end
		for n = first + 1, #B.lines do
			table.insert(B.lines[n], 1, bar(below))
		end
	end

	-- A folded run is its digest alone, a click away from its whole rail: what
	-- the run did, as one line; a digest of nothing folds nothing, and the rail
	-- is drawn alone. An opened digest heads its rail: the chat pins it under the
	-- prompt down to the rail's last row.
	---@param list zemi.TranscriptRow[]
	---@param first integer
	---@param last integer
	---@param opened boolean
	function B.fold(list, first, last, opened)
		-- the messages picker's rail view names this run: it draws whole, so
		-- every call the list walks has a row to band
		local full = (ctx.rail or 0) == first
		local digest = fmt.digest(list, first, last, ctx.cwd, ctx.home)
		if digest == "" then
			-- no digest row to pin: the rail's own head takes the section, so
			-- the prompt's pin never draws over the rail's rows - the rail
			-- draws alone opened or not, so the tear is the same
			local at = #B.lines
			B.rail(list, first, last, { full = full, head = "section" })
			if #B.lines > at then B.lines[#B.lines][1].sticky = "end" end
			return
		end
		local head = opened and "section" or nil
		-- a collapsed run of exactly one thought opens its view at once: a
		-- rail of one row expanded draws what the view shows. Opened, the
		-- digest's click closes it again.
		local one = first == last and list[first].kind == "reasoning"
		local click = one and not opened and "call:" .. pos(first) or "turn:" .. pos(first)
		-- another window's turn reads marked, as its user and reply rows do
		local foreign = false
		for i = first, last do
			if list[i].foreign then
				foreign = true
				break
			end
		end
		B.digestRow(digest, click, head, foreign)
		if not opened then return end
		local at = #B.lines
		-- a lone thought opens as its text: the digest above it already says how long it thought
		B.rail(list, first, last, { unroll = first == last and list[first].kind == "reasoning" or nil, full = full })
		if #B.lines > at then B.lines[#B.lines][1].sticky = "end" end
	end

	-- The digest row a run folds to: the live turn draws it for a thought
	-- that has not settled yet, so what the settle draws stands here already.
	---@param digest string
	---@param click string?
	---@param sticky string?
	---@param foreign boolean?
	function B.digestRow(digest, click, sticky, foreign)
		local mark = foreign and " · other window" or ""
		push({
			{ text = "· ", hl = "Dim", copy = false, hover = false, click = click, sticky = sticky },
			{
				text = fmt.clip(digest, math.max(width - 2 - RAIL_EDGE - #mark, 8)),
				hl = "Dim",
				click = click,
			},
			{ text = mark, hl = "Dim", copy = false, click = click },
		})
	end

	-- A notice row: the status mark it came in with, what it says, and any
	-- detail in brackets after it. A work row's click opens the run it
	-- reports for, the same view its call's row opens.
	---@param status string?
	---@param text string
	---@param detail string?
	---@param click string?
	---@return zemi.Line
	function B.note(status, text, detail, click)
		local row = { mark(MARK_OF[status] or "note", click), { text = text, hl = "Dim", click = click } }
		if (detail or "") ~= "" then
			row[3] = { text = " (" .. detail .. ")", hl = "Dim", copy = false, click = click }
		end
		return row
	end

	-- A burst of notice rows as one block, no blank row between them: reports
	-- and updates arrive together, and the older ones of a long burst sit
	-- behind one row a click opens, as a long rail's do.
	---@param list zemi.TranscriptRow[]
	---@param first integer
	---@param last integer
	function B.notices(list, first, last)
		B.gap("note")
		local hidden = last - first + 1 - NOTICES_KEEP
		if hidden > 1 then
			local label, click = foldRow(whole[pos(first)], hidden, "more:" .. pos(first))
			B.lines[#B.lines + 1] = {
				{ text = "  ", copy = false, hover = false },
				{ text = label, hl = "Dim", click = click },
			}
			if not whole[pos(first)] then first = last - NOTICES_KEEP + 1 end
		end
		for i = first, last do
			local m = list[i]
			-- a work row names the run it reports for; a workflow's report
			-- opens the workflow's view, an agent's its transcript (the attr
			-- matches whole, so an agent named "workflows" stays an agent)
			local click
			if (m.run or "") ~= "" then
				local flow = m.tool == "workflow" or (m.tool or ""):match("^workflow:")
				click = flow and "workflow:" .. m.run or "agent:" .. m.run
			end
			B.lines[#B.lines + 1] = B.note(m.status, m.text, m.detail, click)
		end
	end

	-- A run of activity rows as a rail, one row per entry: a step's unlabelled
	-- counted calls, two or more of them, as one row whose click opens one
	-- row per call, a lone one as its own row, a thought a row of its own, every
	-- other call as its own row: a label tells what its call did. A call's or
	-- a thought's click opens its own view.
	-- NOTE: a step is the calls between two thoughts, so a model that does not
	-- think runs its steps together into one row.
	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { full: boolean?, live: boolean?, run: integer?, unroll: boolean?, head: string? } o.full draws every row of the run starting at run with the bodies folded; o.unroll draws a thought as its text; o.head marks the rail's first row sticky.
	function B.rail(list, first, last, o)
		frame()
		local at = #B.lines
		-- a long rail keeps its last entries in view, each with the rows under
		-- it, the earlier ones behind one row that a click opens or closes. A
		-- closed fold's earlier entries are never drawn: the entry count walks
		-- the tool list and only the kept tail builds rows.
		local key = o.live and 0 or pos(o.run or first)
		local n = 0
		if not o.full then
			local i = first
			while i <= last do
				i, n = nextStep(list, i, last), n + 1
			end
			n = n - RAIL_KEEP
		end
		local from = first
		if n >= 2 and not whole[key] then
			for _ = 1, n do
				from = nextStep(list, from, last)
			end
		end
		-- the fold row bounds the rail's head where its bar would stand, so
		-- the kept tail draws no opening bar of its own
		B.steps(list, from, last, o, from > first)
		if o.head and #B.lines > at then B.lines[at + 1][1].sticky = o.head end
		frame()
		if n < 2 then return end
		local label, click = foldRow(whole[key], n, "more:" .. key)
		table.insert(B.lines, at + 1, { bar(), { text = label, hl = "Dim", click = click } })
		table.insert(B.lines, at + 2, air())
	end

	-- The rows a call or a thought draws carry its transcript index, so a
	-- list naming one call (a rail's own) bands those rows alone.
	---@param at integer
	---@param idx integer
	local function markCall(at, idx)
		for k = at, #B.lines do
			local row = B.lines[k]
			if row[1] then row[1].call = idx end
		end
	end

	---@param list zemi.tui.Tool[]
	---@param first integer
	---@param last integer
	---@param o { full: boolean?, live: boolean?, run: integer?, unroll: boolean? }
	---@param headed boolean? A bar already bounds the rail above: the first entry draws without one.
	function B.steps(list, first, last, o, headed)
		local i = first
		-- The rail opens on a bar and closes on one; between entries one stands
		-- where their kinds differ, and around one that failed or was stopped.
		local prev, prev_fail = nil, false
		while i <= last do
			local t = list[i]
			local idx = o.live and -i or i
			-- a counted call with no sibling is its own row: a "read 1 file"
			-- row over it would hide which file
			local lone = grouped(t) and not grouped(list[i + 1] or {})
			local kind = t.kind == "reasoning" and "thought" or fmt.tool(t)
			local fail = t.status == "err" or t.status == "stopped"
			if (prev ~= nil or not headed) and (prev ~= kind or prev_fail or fail) then frame() end
			prev, prev_fail = kind, fail
			if t.kind == "reasoning" then
				local click = "call:" .. pos(idx)
				local at = #B.lines + 1
				if o.unroll and (t.text or "") ~= "" then
					B.reasoning(t.text, nil, click)
				else
					B.thought(t.thought or 0, click)
				end
				markCall(at, idx)
				i = i + 1
			elseif grouped(t) and not lone then
				---@type zemi.tui.Tool[]
				local group = {}
				while i + #group <= last and grouped(list[i + #group]) do
					group[#group + 1] = list[i + #group]
				end
				-- the step's row heads its calls, nested two cells in, and its
				-- click closes them again
				B.group(group, "step:" .. pos(idx))
				if o.full or steps[pos(idx)] then
					for k, g in ipairs(group) do
						local n = i + k - 1
						local at = #B.lines + 1
						B.tool(g, o.live and -n or n, {
							nest = k == #group and "└ " or "├ ",
						})
						markCall(at, o.live and -n or n)
					end
				end
				i = i + #group
			else
				local at = #B.lines + 1
				-- a live rail folds a call the turn has superseded into its
				-- header: the newest entry, a running one, a failure and an ask
				-- keep the rows under theirs
				local keep = i == last or t.status == "run" or fail or kind == "question" or kind == "plan"
				local terse = o.live and not o.full and not keep or nil
				B.tool(t, idx, { terse = terse })
				markCall(at, idx)
				i = i + 1
			end
		end
	end
	return B
end

-- A click on the chat's rows: on a call's or a thought's row opens a view of
--- it, on an agent's row opens its preview, on a workflow's its view, on a
--- job's line its output, and on a fold row (a digest, a step, a burst's
--- `… N earlier`) opens or closes what it stands for in place.
---@param ev zemi.WidgetClick
function M.click(ev)
	local run, flow, call, job =
		ev.id:match("^agent:(.+)"),
		ev.id:match("^workflow:(.+)"),
		ev.id:match("^call:(-?%d+)$"),
		ev.id:match("^job:(.+)$")
	if run then
		assert(zemi.command.run("agents", "show " .. run))
	elseif flow then
		assert(zemi.command.run("workflows", "view " .. flow))
	elseif call then
		-- the id names the row by its place in the log's tail; /call speaks the
		-- window's indexing
		local n = tonumber(call)
		assert(zemi.command.run("call", tostring(n > 0 and n - cut_seen or n)))
	elseif job then
		-- a restart sweeps handed-off jobs; a stale click does nothing
		zemi.command.run("jobs", "view " .. job)
	else
		local kind, id = ev.id:match("^(%a+):(-?%d+)$")
		local n = tonumber(id)
		if not n then return end
		---@type integer?
		local stop
		for first, u in pairs(units) do
			if first <= n and n < u.next then
				stop = u.next
				units[first] = nil
			end
		end
		settled = nil
		local set = kind == "turn" and unfolded or kind == "step" and steps or whole
		set[n] = not set[n] or nil
		-- what was opened in a folded run has no row left to close it by
		if kind == "turn" and not set[n] and stop then
			for _, s in ipairs({ steps, whole }) do
				for i = n, stop - 1 do
					s[i] = nil
				end
			end
		end
	end
end

-- Chat transcript. User rows carry an accent bar; a turn's activity, its
-- markdown reply and stats follow, a blank row apart.
zemi.ui.widget.add({
	id = "chat",
	region = "chat",
	priority = 10,
	wrap = true,
	on_click = M.click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	---@return boolean more
	render = function(ctx)
		local width = ctx.width or 80
		local count = ctx.transcript_count or 0
		-- the splash owns the empty chat: it draws the pending row too, in a
		-- block that fits, so nothing here can scroll it off the top
		if fmt.empty_chat(ctx) then return {} end
		-- work that outlives this view of the session, drawn at the tail: a
		-- session's counts are about other work, so a run's view leaves it out
		local pending = fmt.pending(ctx)
		if ctx.view then pending = nil end
		-- An agent run's transcript mutates in place and reports epoch 0: the
		-- run's own revision says when it did, so its rows are cached like the
		-- session's, its last row aside.
		local epoch = ctx.transcript_epoch or 0
		local rev = ctx.view_revision or 0
		-- rows are keyed by transcript index, and an agent view reports epoch 0
		-- for whichever run it shows: that run is the identity instead
		local ident = ctx.view and ctx.view.id or epoch
		-- Rows key by pos(): the index plus the head cut, so a trim moves every
		-- key down by the delta and the keys at or under the cut are the rows
		-- the window shed. A replaced window (a reset, a splice) renumbers
		-- under its epoch: the tables drop and the new cut is adopted.
		local cut = ctx.transcript_cut or 0
		if ident ~= opened_in or cut < cut_seen then
			steps, unfolded, whole, opened_in = {}, {}, {}, ident
			live_count, live_rail, live_known = -1, -1, 0
			cut_seen = cut
		elseif cut > cut_seen then
			for k in pairs(units) do
				if k <= cut then units[k] = nil end
			end
			for k in pairs(counts) do
				if k > 0 and k <= cut then counts[k] = nil end
			end
			for _, folds in ipairs({ unfolded, steps, whole }) do
				for k in pairs(folds) do
					if k > 0 and k <= cut then folds[k] = nil end
				end
			end
			if live_known > 0 then live_known = math.max(live_known - (cut - cut_seen), 0) end
			settled = nil
			cut_seen = cut
		end
		-- The live turn's calls are keyed by negative index and the host restarts
		-- that list every turn, so a shorter list means those keys now stand for
		-- another turn's rows.
		local ntools = #((ctx.agent or {}).tools or {})
		if ntools < live_tools then
			for n in pairs(steps) do
				if n < 0 then steps[n] = nil end
			end
			whole[0] = nil
		end
		live_tools = ntools
		local active = ctx.agent_active or false
		local B = builder(ctx, width)
		-- Every drawn row of a unit carries its first transcript index, which a
		-- following list (the messages picker) names to scroll the chat here and
		-- to band the whole message. The rows from upto on are meta (a reply's
		-- readout), outside the message. Go reads the anchor and never draws it.
		---@param lines zemi.Line[]
		---@param first integer
		---@param upto integer?
		local function anchored(lines, first, upto)
			for i = 1, upto or #lines do
				local row = lines[i]
				if row[1] and fmt.cells(row) > 0 then row[1].anchor = first end
			end
		end
		-- The messages first..last as one unit: a run of activity rows, else one.
		---@param tr zemi.TranscriptRow[]
		---@param first integer
		---@param last integer
		---@param mode zemi.tui.RunMode|""
		---@return zemi.tui.ChatUnit
		local function unit(tr, first, last, mode)
			local m = tr[first]
			local content = nil -- where the message ends and meta begins
			local open = nil -- a reply's readout decision is still open (see the scan below)
			B.lines, B.prev = {}, first > 1 and block_of(tr[first - 1]) or nil
			if mode == "fold" or mode == "unfold" then
				B.gap("activity")
				B.fold(tr, first, last, mode == "unfold")
			elseif mode ~= "" then
				B.gap("activity")
				-- the picker's rail view draws the run whole, the live turn's own
				-- included: every call it walks has a row to band
				B.rail(tr, first, last, { run = first, full = (ctx.rail or 0) == first })
			elseif m.kind == "shell" then
				-- a !command: the command, then its output, already cut to a head
				-- and a tail by the host; a failure's mark takes the prompt's place
				B.gap("user")
				B.lines[#B.lines + 1] = {
					m.status == "err" and mark("err") or { text = "$ ", hl = "PromptShell", copy = false },
					{ text = m.text, hl = "Normal" },
				}
				for _, ol in ipairs(util.split(m.detail or "", "\n")) do
					B.lines[#B.lines + 1] = { { text = "  ", copy = false }, { text = ol, hl = "Dim" } }
				end
			elseif notice(m) then
				B.notices(tr, first, last)
			elseif m.kind == "error" then
				-- the headline, then the raw error as its detail
				B.gap("note")
				local cancelled = m.status == "cancelled"
				for n, text in ipairs(util.split(m.text, "\n")) do
					if n == 1 then
						B.lines[#B.lines + 1] = {
							mark(cancelled and "stop" or "err"),
							{ text = text, hl = cancelled and "Dim" or "Danger" },
						}
					else
						B.lines[#B.lines + 1] = {
							{ text = n == 2 and HANG or INDENT, hl = "Dim", copy = false },
							{ text = text, hl = "Dim" },
						}
					end
				end
			elseif m.role == "user" then
				-- plain text: what the user typed is not markdown
				B.gap("user")
				B.user(m.text, nil, m.foreign)
			else
				B.gap("text")
				B.markdown(m.text)
				-- another window wrote this reply: name it under the text, the
				-- user row's mark, so a spliced exchange reads as whose it was
				if m.foreign then
					B.lines[#B.lines + 1] = { { text = "  ", copy = false }, { text = "· other window", hl = "Dim" } }
				end
				-- The readout rides the reply that ends the exchange, and only the
				-- next text row decides it: a call, notice or error after it changes
				-- nothing, while another reply means a sentence between runs. No
				-- unnamed row after it yet leaves the decision open: the unit says
				-- so (u.open), and a row landing below redraws it.
				---@type zemi.Line
				local stats = {}
				local final = true
				open = true
				for i = last + 1, #tr do
					local r = tr[i]
					if (r.kind or "") == "" then
						final, open = r.role == "user", false
						break
					end
				end
				if final then
					---@param text string
					local function stat(text)
						if #stats > 0 then stats[#stats + 1] = { text = " · ", hl = "Dim" } end
						stats[#stats + 1] = { text = text, hl = "Dim" }
					end
					local tout = fmt.tokens(m.out)
					if (m.model or "") ~= "" then stat(m.model) end
					if (m.elapsed or 0) > 0 then stat(fmt.duration(m.elapsed)) end
					if tout then stat(tout .. " out") end
					if (m.ended or 0) > 0 then stat(fmt.clock(m.ended)) end
					-- the model leads and sheds first: a narrow row keeps what the
					-- turn cost over the name that wrote it
					if #stats > 1 and (m.model or "") ~= "" and 2 + fmt.cells(stats) > width then
						table.remove(stats, 2)
						table.remove(stats, 1)
					end
				end
				content = #B.lines
				if #stats > 0 then
					B.gap("stats")
					local row = { { text = "  ", copy = false } }
					for _, sg in ipairs(stats) do
						row[#row + 1] = sg
					end
					B.lines[#B.lines + 1] = row
				end
			end
			anchored(B.lines, first, content)
			return { lines = B.lines, next = pos(last + 1), mode = mode, cut = cut_seen, open = open, rows = #tr }
		end

		-- markdown and code carry the theme's colors: a light terminal's
		-- variant renders them anew. A settled row whose text grew in place (a
		-- joined reasoning chunk) re-renders its unit, which the 30 ms budget
		-- bounds; the rows the reader opened keep their place.
		-- The view is in the key: two runs report revisions of their own.
		local key = table.concat({
			epoch,
			width,
			ctx.cwd or "",
			zemi.ui.theme.get(),
			tostring(ctx.transcript_edit or 0),
			ctx.view and ctx.view.id or "",
			rev,
		}, "|")
		local reset = key ~= units_key
		if reset then counts = {} end
		if (ctx.rail or 0) ~= (rail_open or 0) then
			rail_open, settled = ctx.rail or 0, nil
		end
		-- a printed run is a snapshot and reports epoch 0: only the session and
		-- a shown run cache their rows
		local cacheable = epoch ~= 0 or ctx.view ~= nil
		if reset or not cacheable then
			units, units_key, settled = {}, key, nil
		end
		-- A window swapped in whole (a reset's wipe), a new width or a new
		-- theme leave every unit to render, far more than one render may
		-- take: the latest come first, and once they fill the view and 30 ms
		-- passed, the render returns them and asks for another, so a slow or
		-- loaded machine still draws the view whole; the header above them,
		-- the prompt the view pins, goes on top.
		local more = false
		local ag = ctx.agent or {}
		-- A reply closes the run it follows at once, streaming or settled: the run
		-- the turn is working in is its last activity run, while that reaches the
		-- tail and the turn is not answering.
		local replying = (ag.text or "") ~= ""
		-- A viewed run's rows change under a count that does not: its view
		-- never takes the whole-output shortcut, its tail redraws instead.
		local done = not ctx.view
			and settled
			and settled.count == count
			and settled.active == active
			and settled.replying == replying
			and settled
		-- the row the run this turn is still adding to starts at: the live turn's
		-- calls are no unit of their own, but the picker names that run by it.
		-- Reading the transcript converts every row, so it is read only when
		-- what names the run changed - the count or the rail - and the last
		-- answer stands otherwise. The run stands while the turn is active
		-- even once the reply streams (calls open beside it), but only an
		-- answering turn folds its settled rows: live_first is tail_first then.
		local tail_first = 0
		if active then
			local rail = ctx.rail or 0
			if live_count ~= count or live_rail ~= rail then
				local tr = ctx.transcript or {}
				if #tr > 0 and activity(tr[#tr]) then
					tail_first = #tr
					while tail_first > 1 and activity(tr[tail_first - 1]) do
						tail_first = tail_first - 1
					end
				end
				live_count, live_rail, live_known = count, rail, tail_first
			else
				tail_first = live_known
			end
		end
		local live_first = not replying and tail_first or 0
		if not done then
			-- ctx.transcript copies every row into Lua, so only a miss reads it
			local tr = ctx.transcript or {}
			---@param first integer
			---@return zemi.tui.RunMode|""
			local function mode_of(first)
				if not activity(tr[first]) then return "" end
				-- the live turn's own run reads whole; every other run folds,
				-- the one an agent view shows included
				if first == live_first then return "rail" end
				if (ctx.rail or 0) == first then return "unfold" end
				return unfolded[pos(first)] and "unfold" or "fold"
			end
			local deadline = cacheable and os.clock() + 0.03
			---@type zemi.tui.ChatUnit[]
			local shown = {}
			local rendered, got = 0, 0
			---@param tail boolean? The group holding the last row: a run rewrites it in place as it streams, so it always redraws.
			local function stale(first, last, tail)
				if tail then return true end
				local u = units[pos(first)]
				-- a reply whose readout scan found no unnamed row after it
				-- decided nothing yet: a row landing below can end the
				-- exchange the readout rides, so it redraws on growth. A
				-- decided unit (u.open == false) keeps its draw.
				return not u or u.next ~= pos(last + 1) or u.mode ~= mode_of(first) or u.open and u.rows ~= #tr
			end
			-- A unit drawn before the last trim: its content is good, its
			-- anchors and calls name the window it drew in, so it restamps
			-- rather than rebuilds - and fills like a rebuild does.
			---@param first integer
			---@return boolean
			local function shifted(first)
				local u = units[pos(first)]
				return u ~= nil and u.cut ~= cut_seen
			end
			-- A trim moved every row down since the unit drew: its rows' anchors
			-- and calls still name the window they were drawn in, so a re-shown
			-- unit's rows are copied with theirs shifted. A copy is a line table
			-- the conversion has not seen, so the host reads the fresh numbers.
			---@param u zemi.tui.ChatUnit
			local function restamp(u)
				local by = cut_seen - u.cut
				for i, row in ipairs(u.lines) do
					local s = row[1]
					if s and ((s.anchor or 0) > 0 or (s.call or 0) > 0) then
						-- the head's numbers move in place: the converter keys
						-- its cache on the line table, which is new here, so it
						-- reads the segment fresh and the old line that shared
						-- it is dropped from the unit
						s.anchor = (s.anchor or 0) > 0 and s.anchor - by or nil
						s.call = (s.call or 0) > 0 and s.call - by or nil
						local line = {}
						for j = 1, #row do
							line[j] = row[j]
						end
						u.lines[i] = line
					end
				end
				u.cut = cut_seen
			end
			local function cached(first, last, tail)
				local k = pos(first)
				local u = units[k]
				if u and u.cut ~= cut_seen then
					restamp(u)
					-- a restamp reconverts the unit's rows like a rebuild
					-- does, so it spends the budget the same way
					if not tail then rendered = rendered + 1 end
				end
				if stale(first, last, tail) then
					units[k] = unit(tr, first, last, mode_of(first))
					-- the tail redraws every frame and is no work the budget
					-- needs to bound: only a group the cache lost counts
					if not tail then rendered = rendered + 1 end
				end
				return units[k]
			end
			local last = #tr
			local tail = true
			while last >= 1 do
				local first = last
				if notice(tr[last]) then
					while first > 1 and notice(tr[first - 1]) do
						first = first - 1
					end
				else
					while first > 1 and activity(tr[first]) and activity(tr[first - 1]) do
						first = first - 1
					end
				end
				if
					deadline
					and rendered > 0
					and got >= ctx.height
					and (stale(first, last) or shifted(first))
					and os.clock() > deadline
				then
					more = true
					break
				end
				local u = cached(first, last, tail)
				tail = false
				shown[#shown + 1] = u
				got = got + #u.lines
				last = first - 1
			end
			-- a prompt: the row the view pins
			local function pins(m) return (m.kind or "") == "" and m.role == "user" end
			if more and not pins(tr[last + 1]) then
				for h = last, 1, -1 do
					if pins(tr[h]) then
						shown[#shown + 1] = cached(h, h)
						break
					end
				end
			end
			---@type zemi.Line[]
			local all = {}
			-- the transcript's first row stands one row off the window's top
			-- edge; a printed run's output keeps its own framing
			if last == 0 and #tr > 0 then all[1] = { { text = "" } } end
			-- the window is the log's tail, not the chat's start: with the head
			-- drawn, name what the cut left out and the way back to it
			local older = last == 0 and ctx.log_count or 0
			if older > 0 then
				table.insert(all, 1, {
					{
						text = "… "
							.. older
							.. " earlier "
							.. (older == 1 and "message" or "messages")
							.. " · ctrl+m to browse",
						hl = "Dim",
						copy = false,
						hover = false,
					},
				})
			end
			for n = #shown, 1, -1 do
				for _, l in ipairs(shown[n].lines) do
					all[#all + 1] = l
				end
			end
			done = {
				count = count,
				lines = all,
				n = #all,
				active = active,
				replying = replying,
				block = tr[#tr] and block_of(tr[#tr]),
			}
			settled = not more and done or nil
		end
		-- the live turn's lines go after the settled ones in the same list
		B.lines, B.prev = done.lines, done.block
		for n = #B.lines, done.n + 1, -1 do
			B.lines[n] = nil
		end

		-- Live turn (§6.3) from the host-owned ctx.agent snapshot, drawn on the
		-- rail after the rows of it already settled: the thinking, the calls,
		-- the reply.
		local tools, text, queued = ag.tools or {}, ag.text or "", ag.steering or {}
		if ag.active or #tools > 0 or text ~= "" or #queued > 0 then
			-- the thinking shows while the model thinks: a reasoning block of its own
			-- is open, so a thought that ended does not go on spinning over the
			-- calls that follow it. Each finished thought is a rail row of its own,
			-- framed as an entry is.
			if ag.thinking and (ag.reasoning or "") ~= "" and text == "" then
				B.gap("activity")
				B.thinking(ag.reasoning)
				-- The thought no longer draws as the thinking block (the reply
				-- streams): the digest row it will settle as stands here already,
				-- so the settle shifts nothing. A thought the run above takes (a
				-- call between), one a run's view places at its finish, and one
				-- too short to name a digest draw nothing instead.
			elseif
				tail_first == 0
				and not ctx.view
				and (ag.thought or 0) >= 0.05
				and (ag.reasoning or ""):find("%S")
			then
				B.gap("activity")
				B.digestRow(fmt.thought(ag.thought or 0))
			elseif
				text == ""
				and #tools == 0
				and ag.active
				and not ctx.view -- a run's view draws its own rows: its calls and reply never reach tools or text
				and not ag.retry
				and not ctx.cancelling
				and ctx.waiting == ""
				and (ag.elapsed or 0) >= 0.3
			then
				-- the wait before any output: the thinking row opens with the turn,
				-- after the same 300ms that guards the working rule's block
				B.gap("activity")
				B.thinking(nil)
			end
			local live_from = 0
			if #tools > 0 then
				live_from = #B.lines + 1
				B.gap("activity")
				-- the picker's rail view opens this run as it opens a settled one:
				-- every row, until the menu closes back to its own short rail. A
				-- negative rail names the live-only run a text tail leaves the
				-- calls in, which has no settled rows to unfold.
				B.rail(tools, 1, #tools, {
					live = true,
					full = (ctx.rail or 0) < 0 or tail_first > 0 and tail_first == (ctx.rail or 0),
				})
			end
			-- the live rail is rows of the run it extends: the settled tail's
			-- run, else the live-only one the picker names by its first call.
			-- The thinking rows above carry no marker, so the reader's place
			-- stops at the last settled call; the band's span covers them.
			if live_from > 0 then
				local run = tail_first > 0 and tail_first or -1
				for n = live_from, #B.lines do
					local row = B.lines[n]
					if row[1] and fmt.cells(row) > 0 then row[1].anchor = run end
				end
			end
			if text ~= "" then
				B.gap("text")
				B.markdown(text)
			end
			-- what waits for the turn's next step boundary: the user's own
			-- words as typed, folding past the paste threshold like any user
			-- row. The model's own markup draws no row here: its work row
			-- landed where it was delivered.
			for _, q in ipairs(queued) do
				B.gap("queued")
				B.user(q.text, true)
			end
		end
		-- What will change this conversation later sits between it and the
		-- todos: a static row, so nothing here keeps the frame clock awake.
		if pending then
			if #B.lines > 0 then B.blank() end
			B.lines[#B.lines + 1] = pending
		end
		local lines = B.lines
		-- The todo list stays under the conversation until every item is done.
		local todos, left = ctx.todos or {}, 0
		for _, td in ipairs(todos) do
			if td.status ~= "done" then left = left + 1 end
		end
		if left > 0 then
			B.blank()
			-- at most 12 rows and a third of the chat, in the order the model
			-- wrote them: the window trails the item in progress, so a long
			-- list never hides what is being worked on
			local cap = math.min(12, math.max(3, math.floor((ctx.height or 36) / 3)))
			local live = 0
			for n, td in ipairs(todos) do
				if td.status == "in_progress" then live = n end
			end
			local first = math.max(1, math.min(live - cap + 1, #todos - cap + 1))
			local last = math.min(#todos, first + cap - 1)
			-- a header in the composer rule's voice: the name, how far along,
			-- and a meter over the whole list: two cells per item while they
			-- fit, else scaled, so the meter never changes meaning
			local head = {
				{ text = "Todos ", hl = "TodoTitle" },
				{ text = (#todos - left) .. "/" .. #todos .. "  ", hl = "Dim" },
			}
			local cells = 2 * math.min(#todos, cap)
			-- a cell fills at the done fraction, rounded up: a part-done list
			-- lights its first cells even when no cell's whole range is done,
			-- so the meter never reads emptier than the count; hue keeps the
			-- in-flight cell
			local done = #todos - left
			local filled = math.ceil(done * cells / #todos)
			for n = 1, cells do
				-- the items cell n's share spans, its top bound rounded up:
				-- an item two cells wide lights both of them
				local lo, hi = math.floor((n - 1) * #todos / cells), math.ceil(n * #todos / cells)
				local live = false
				for i = lo + 1, hi do
					if todos[i].status == "in_progress" then live = true end
				end
				head[#head + 1] = {
					text = "━",
					hl = live and "TodoMark" or n <= filled and "TodoDone" or "Rule",
					copy = false,
				}
			end
			lines[#lines + 1] = head
			---@param hidden integer
			---@param above boolean?
			local function fold(hidden, above)
				lines[#lines + 1] = {
					{ text = "  … ", hl = "Dim", copy = false },
					{ text = fmt.count(hidden, "more todo", "more todos") .. (above and " above" or ""), hl = "Dim" },
				}
			end
			if first > 1 then fold(first - 1, true) end
			for n = first, last do
				local td = todos[n]
				local tick, mhl, hl, item = "  ○ ", "Rule", "Normal", td.content
				if td.status == "done" then
					tick, mhl, hl = "  ✓ ", "TodoDone", "Dim"
				elseif td.status == "in_progress" then
					tick, mhl, hl = "  ▸ ", "TodoMark", "TodoActive"
					if (td.active_form or "") ~= "" then item = td.active_form end
				end
				lines[#lines + 1] = { { text = tick, hl = mhl, copy = false }, { text = item, hl = hl } }
			end
			if last < #todos then fold(#todos - last) end
		end
		return lines, more
	end,
})

return M
