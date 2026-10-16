-- Helpers the default widgets share:
-- require("zemi.tui.text").
---@class zemi.tui.text
local M = {}

--- A token count as "812", "24.5k", "1.2M" or "1.5B"; nil for none.
---@param n integer|nil
---@return string|nil
function M.tokens(n)
	if not n or n <= 0 then return nil end
	local s = string.format("%d", n)
	if n >= 1000000000 then
		s = string.format("%.1fB", n / 1000000000)
	elseif n >= 1000000 then
		s = string.format("%.1fM", n / 1000000)
	elseif n >= 1000 then
		s = string.format("%.1fk", n / 1000)
	end
	return (s:gsub("%.0([kMB])$", "%1"))
end

--- A cost in dollars as "$0.42", "<$0.01" under half a cent; nil for none (an unpriced model).
---@param usd number|nil
---@return string|nil
function M.cost(usd)
	usd = tonumber(usd) or 0
	if usd <= 0 then return nil end
	return usd < 0.005 and "<$0.01" or string.format("$%.2f", usd)
end

--- A duration as "4.2s" under ten seconds, then "42s", "3m 05s", "1h 02m".
--- A clock that ticks passes whole: its seconds only grow in width.
---@param s number seconds
---@param whole boolean? Whole seconds under ten too ("4s").
---@return string
function M.duration(s, whole)
	if s < 9.95 and not whole then return string.format("%.1fs", math.max(s, 0)) end
	s = math.floor(math.max(s, 0) + (whole and 0 or 0.5))
	if s < 60 then return s .. "s" end
	if s < 3600 then return string.format("%dm %02ds", math.floor(s / 60), s % 60) end
	return string.format("%dh %02dm", math.floor(s / 3600), math.floor(s / 60) % 60)
end

--- A unix time as a wall clock, "14:05", as ctx.time reads.
---@param t integer
---@return string
function M.clock(t) return os.date("%H:%M", t) end

--- A count with its noun: "1 file", "3 files".
---@param n integer
---@param one string
---@param many string
---@return string
function M.count(n, one, many) return n .. " " .. (n == 1 and one or many) end

--- The work running besides this turn as one string, "2 workflows, 1 agent",
--- nil when nothing runs.
---@param ctx zemi.WidgetCtx
---@return string?
function M.running(ctx)
	local flows, agents, jobs = ctx.workflows_running or 0, ctx.agents_running or 0, ctx.jobs_running or 0
	if flows == 0 and agents == 0 and jobs == 0 then return nil end
	---@type string[]
	local parts = {}
	if flows > 0 then parts[#parts + 1] = M.count(flows, "workflow", "workflows") end
	if agents > 0 then parts[#parts + 1] = M.count(agents, "agent", "agents") end
	if jobs > 0 then parts[#parts + 1] = M.count(jobs, "job", "jobs") end
	return table.concat(parts, ", ")
end

--- A pointer to what deals with something: the noun in Title (hl overrides
--- it, Danger for a failure), then the command or key in Dim ("errors
--- /errors", "↓ 25 ctrl+end"). Both segments click as click, a command's own
--- name when click is left out.
---@param noun string
---@param cmd string
---@param click string?
---@param hl string?
---@return zemi.Line
function M.hint(noun, cmd, click, hl)
	click = click or cmd:match("^/(%S+)$")
	return { { text = noun, hl = hl or "Title", click = click }, { text = " " .. cmd, hl = "Dim", click = click } }
end

--- What the session still waits on as one row: the runs and jobs of other
--- work, the jobs the user waits on, and a compaction. Static text, so an
--- hour-long job holds no frame clock; nil when nothing pends.
---@param ctx zemi.WidgetCtx
---@return zemi.Line?
function M.pending(ctx)
	---@type string[]
	local parts = {}
	-- the live rail already names the run the turn waits on: counts are other work
	if not ctx.agent_active then
		local running = M.running(ctx)
		if running then parts[#parts + 1] = running end
	end
	for _, job in ipairs(ctx.busy or {}) do
		parts[#parts + 1] = M.lower(job.label)
	end
	if (ctx.agent or {}).compacting then parts[#parts + 1] = "compacting the conversation" end
	if #parts == 0 then return nil end
	---@type zemi.Mark
	local mark = zemi.ui.marks.bg
	---@type zemi.Line
	local row = { { text = mark.glyph .. " ", hl = mark.hl, copy = false, hover = false } }
	for i, part in ipairs(parts) do
		if i > 1 then row[#row + 1] = { text = " · ", hl = "Dim", copy = false, hover = false } end
		row[#row + 1] = { text = part, hl = "Dim" }
	end
	return row
end

--- Whether the splash owns the chat this frame: an empty transcript with no
--- turn running and no todo left. The chat yields the region to it, since the
--- splash draws the pending row itself in a block that fits; two widgets whose
--- rows overflow the region scroll the splash away as boot work comes and goes.
---@param ctx zemi.WidgetCtx
---@return boolean
function M.empty_chat(ctx)
	if ctx.headless or ctx.agent_active or (ctx.transcript_count or 0) > 0 then return false end
	for _, td in ipairs(ctx.todos or {}) do
		if td.status ~= "done" then return false end
	end
	return true
end

--- The on_click of a widget whose click ids are a command and its
--- arguments ("resume 3f9c", "profile"), or "fill <text>", which puts the text
--- in the composer for the user to send.
---@param ev zemi.WidgetClick
function M.run_click(ev)
	local fill = ev.id:match("^fill (.*)$")
	if fill then return zemi.composer.set(fill) end
	local name, args = ev.id:match("^(%S+)%s*(.*)$")
	assert(zemi.command.run(name, args))
end

--- The label a splash or exit row leads with, padded so the values line up.
---@param name string
---@return zemi.Segment
function M.label(name) return { text = string.format("%-9s", name), hl = "Dim" } end

--- What a tool call does, as its row and the working rule say it.
---@type table<string, string>
M.VERB = {
	read = "Read",
	write = "Write",
	edit = "Edit",
	glob = "Find",
	grep = "Search",
	fetch = "Fetch",
	shell = "Run",
	job_start = "Start",
	job_read = "Check job",
	job_list = "List jobs",
	job_kill = "Stop job",
	memory = "Remember",
	diagnostics = "Check",
	todos = "Update todos",
	question = "Ask",
	plan = "Plan",
	skill = "Load skill",
	tool_search = "Find tools",
	lsp = "Look up",
	lua = "Run",
	update = "Tell",
}

--- A call family the chat counts: its verb, then its noun, one and many; a
--- verb alone never counts. A call of any other name keeps a row of its own,
--- and M.family adds the entry an MCP call counts under.
---@type table<string, { [1]: string, [2]: string?, [3]: string? }>
M.COUNT = {
	read = { "read", "file", "files" },
	glob = { "searched for", "pattern", "patterns" },
	grep = { "searched for", "pattern", "patterns" },
	shell = { "ran", "command", "commands" },
	fetch = { "fetched", "page", "pages" },
	skill = { "loaded", "skill", "skills" },
	memory = { "saved", "memory", "memories" },
	diagnostics = { "checked", "file", "files" },
	job_read = { "checked", "job", "jobs" },
	job_list = { "listed jobs" },
	job_kill = { "stopped", "job", "jobs" },
	todos = { "updated todos" },
	agent = { "started", "agent run", "agent runs" },
	workflow = { "ran", "workflow", "workflows" },
	lua = { "ran", "script", "scripts" },
	tool_search = { "looked for", "tool", "tools" },
	lsp = { "looked up", "symbol", "symbols" },
}

-- An MCP call counts under its server: its name is mcp_<server>_<tool> and a
-- server name holds no "_", so the entry built here is the server's, one per
-- server however many of its tools ran.
---@type table<string, { [1]: string, [2]: string, [3]: string }>
local servers = {}

--- The family a call counts as, nil for a call whose name has no entry.
---@param name string
---@return { [1]: string, [2]: string?, [3]: string? }?
function M.family(name)
	local f = M.COUNT[name]
	if f then return f end
	local server = name:match("^mcp_([^_]+)_")
	if not server then return nil end
	if not servers[server] then servers[server] = { "called", server .. " tool", server .. " tools" } end
	return servers[server]
end

--- A transcript row's or a live call's tool name.
---@param t zemi.TranscriptRow|zemi.ToolCall
---@return string
function M.tool(t) return t.tool or t.name or "?" end

--- Whether a call folds into its turn's digest: a family counts it, it went
--- well and it shows no diff of its own (a command's diff is news).
---@param t zemi.TranscriptRow|zemi.ToolCall
---@return boolean
function M.looks(t)
	return (t.kind or "tool") == "tool" and t.status == "ok" and M.family(M.tool(t)) ~= nil and (t.diff or "") == ""
end

--- Calls counted by noun, first seen first: "2 files, 1 command". With verb,
--- each part is led by what the calls did: "read 2 files". commands are the
--- commands a shell family ran, listed after its part, which then reads last.
---@param calls (zemi.TranscriptRow|zemi.ToolCall)[]
---@param commands string[]? The commands a shell family ran.
---@param verb boolean? Lead each part with its family's verb.
---@return string
function M.counts(calls, commands, verb)
	---@type string[], table<string, integer>, table<string, { [1]: string, [2]: string?, [3]: string? }>
	local order, n, c = {}, {}, {}
	for _, t in ipairs(calls) do
		local k = assert(M.family(M.tool(t)))
		local key = k[2] or k[1]
		if not n[key] then
			order[#order + 1] = key
			n[key], c[key] = 0, k
		end
		n[key] = n[key] + 1
	end
	---@type string[]
	local parts = {}
	---@type string?
	local ran
	for _, key in ipairs(order) do
		local k = c[key]
		local part = not k[2] and k[1] or (verb and k[1] .. " " or "") .. M.count(n[key], k[2], k[3])
		if commands and k == M.COUNT.shell then
			ran = part .. ": " .. table.concat(commands, ", ")
		else
			parts[#parts + 1] = part
		end
	end
	if ran then parts[#parts + 1] = ran end
	return table.concat(parts, ", ")
end

--- The widest thinking text runs, in cells: dim prose reads better narrow.
M.PROSE = 80

--- How long a thought took, as a digest or a thought row says it.
---@param secs number
---@return string
function M.thought(secs) return secs >= 0.05 and "thought " .. M.duration(secs) or "thought" end

--- What rows[first..last] of a turn fold into, the chat's digest row:
--- "6 files, 8 commands · thought 8s", a run holding thinking alone as
--- "thought 8s"; "" when it holds neither, so its rail stays.
---@param rows zemi.TranscriptRow[]
---@param first integer
---@param last integer
---@return string
function M.digest(rows, first, last)
	---@type zemi.TranscriptRow[]
	local calls = {}
	local secs = 0
	for i = first, last do
		local m = rows[i]
		if m.kind == "reasoning" then
			secs = secs + (m.thought or 0)
		elseif M.looks(m) then
			calls[#calls + 1] = m
		end
	end
	---@type string[]
	local parts = {}
	if #calls > 0 then parts[#parts + 1] = M.counts(calls) end
	if secs >= 0.05 then parts[#parts + 1] = M.thought(secs) end
	return table.concat(parts, " · ")
end

--- A tool name as words for a row with no verb of its own: underscores split,
--- an MCP tool's mcp_ dropped, and no capital, which the row's M.lower would
--- take back ("mcp_tracker_list_issues" -> "tracker list issues").
---@param name string
---@return string
function M.title(name) return (name:gsub("^mcp_", ""):gsub("_", " ")) end

--- The calls the turn waits on: their work is another agent loop, so nothing
--- streams until they end and their row says what holds the turn.
---@type table<string, boolean>
M.WAIT = { agent = true, workflow = true }

--- A call's headline as a verb and the rest: "Read", " a.go". A command's
--- about is itself a verb phrase ("Run the tests"), so its first word is the
--- verb; an unlabelled call reads as its input; a call the turn waits on says
--- so in place of its verb ("waiting on workflow review"). A row lowercases
--- the verb with M.lower, so nothing here is capitalized.
---@param name string
---@param label string The model's description of the call, or "".
---@param input string The call's input as the row shows it, or "".
---@param status string? The call's status; "run" is a call running now.
---@return string verb
---@return string rest
function M.head(name, label, input, status)
	local rest = label ~= "" and " " .. M.lower(label) or input ~= "" and " " .. input or ""
	if status == "run" and M.WAIT[name] then return "waiting on " .. name, rest end
	if name == "shell" and label ~= "" then return label:match("^(%S+)(.*)$") end
	return M.VERB[name] or M.title(name), rest
end

--- Text with its first letter lowercased, so it reads on after a verb,
--- unless that letter starts an acronym ("README").
---@param s string
---@return string
function M.lower(s)
	if s:match("^%u%u") then return s end
	return (s:gsub("^%u", string.lower))
end

--- Text with every path under cwd written relative to it, and every other
--- path under home from ~, as ctx.cwd is written.
---@param s string
---@param cwd string?
---@param home string?
---@return string
function M.relpath(s, cwd, home)
	if home and home ~= "" and home ~= "/" then s = s:gsub(home:gsub("%p", "%%%0") .. "/", "~/") end
	if not cwd or cwd == "" then return s end
	return (s:gsub(cwd:gsub("%p", "%%%0") .. "/", ""))
end

--- The cells a line of segments takes.
---@param segs zemi.Line
---@return integer
function M.cells(segs)
	local w = 0
	for _, sg in ipairs(segs) do
		w = w + zemi.ui.width(sg.text)
	end
	return w
end

--- A pattern matching one UTF-8 character.
---@type string
M.UTF8_CHAR = "[%z\1-\127\194-\244][\128-\191]*"

--- A cut that lands inside a word drops the fragment it would keep when that
--- is at most this many cells, so its ellipsis follows a whole word
--- (ui/agent.go's ellipsizeSegs names the same number clipFragment).
---@type integer
local FRAGMENT = 12

--- Text cut to room cells, ending with an ellipsis when it cuts, never after a
--- space and never inside a word short enough to drop whole (FRAGMENT): a path
--- or an identifier, holding no space, keeps its cut.
---@param text string
---@param room integer
---@return string
function M.clip(text, room)
	if zemi.ui.width(text) <= room then return text end
	if room < 1 then return "" end
	---@type string[]
	local chars = {}
	---@type string?
	local cut
	local w = 0
	for ch in text:gmatch(M.UTF8_CHAR) do
		local cw = zemi.ui.width(ch)
		if w + cw > room - 1 then
			cut = ch
			break
		end
		chars[#chars + 1] = ch
		w = w + cw
	end
	if cut ~= " " then
		local at, frag = nil, 0
		for i = #chars, 1, -1 do
			if chars[i] == " " then
				at = i
				break
			end
			frag = frag + zemi.ui.width(chars[i])
		end
		if at and frag <= FRAGMENT then
			for i = at, #chars do
				chars[i] = nil
			end
		end
	end
	return (table.concat(chars):gsub("%s+$", "")) .. "…"
end

return M
