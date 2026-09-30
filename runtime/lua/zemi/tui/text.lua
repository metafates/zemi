-- Helpers the default widgets share:
-- require("zemi.tui.text").
---@class zemi.tui.text
local M = {}

--- A token count as "812", "24.5k" or "1.2M"; nil for none.
---@param n integer|nil
---@return string|nil
function M.tokens(n)
	if not n or n <= 0 then return nil end
	local s = string.format("%d", n)
	if n >= 1000000 then
		s = string.format("%.1fM", n / 1000000)
	elseif n >= 1000 then
		s = string.format("%.1fk", n / 1000)
	end
	return (s:gsub("%.0([kM])$", "%1"))
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

--- A pointer to what deals with something: the noun in Title (hl overrides
--- it, Danger for a failure), then the command or key in Dim ("errors
--- /errors", "↓ 25 ctrl+end"). Both segments click as click.
---@param noun string
---@param cmd string
---@param click string?
---@param hl string?
---@return zemi.Line
function M.hint(noun, cmd, click, hl)
	return { { text = noun, hl = hl or "Title", click = click }, { text = " " .. cmd, hl = "Dim", click = click } }
end

--- The on_click of a widget whose click ids are a command and its
--- arguments ("resume 3f9c", "mode"), or "fill <text>", which puts the text
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
	web_search = "Search web",
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
	agent = "Agent",
	workflow = "Workflow",
	lua = "Run Lua",
	tool_search = "Find tools",
	lsp = "Look up",
}

--- A call that only looks around, as the chat counts it: its verb, then its
--- noun, one and many, and the short forms a row's suffix uses; a verb alone
--- never counts. Every other call keeps a row of its own.
---@type table<string, { [1]: string, [2]: string?, [3]: string?, [4]: string?, [5]: string? }>
M.COUNT = {
	read = { "read", "file", "files" },
	glob = { "searched for", "pattern", "patterns" },
	grep = { "searched for", "pattern", "patterns" },
	web_search = { "searched the web for", "query", "queries" },
	shell = { "ran", "command", "commands", "cmd", "cmds" },
	fetch = { "fetched", "page", "pages" },
	skill = { "loaded", "skill", "skills" },
	memory = { "saved", "memory", "memories" },
	diagnostics = { "checked", "file", "files" },
	job_read = { "checked", "job", "jobs" },
	job_list = { "listed jobs" },
	job_kill = { "stopped", "job", "jobs" },
	todos = { "updated todos" },
}

--- A transcript row's or a live call's tool name.
---@param t zemi.TranscriptRow|zemi.ToolCall
---@return string
function M.tool(t) return t.tool or t.name or "?" end

--- Whether a call folds into its turn's digest: it only looked around, went
--- well and changed nothing (a command's diff is news).
---@param t zemi.TranscriptRow|zemi.ToolCall
---@return boolean
function M.looks(t)
	return (t.kind or "tool") == "tool" and t.status == "ok" and M.COUNT[M.tool(t)] ~= nil and (t.diff or "") == ""
end

--- Calls counted by noun, first seen first: "2 files, 1 command", or short
--- for a row's suffix, "2 files · 1 cmd".
---@param calls (zemi.TranscriptRow|zemi.ToolCall)[]
---@param short boolean?
---@return string
function M.counts(calls, short)
	---@type string[], table<string, integer>, table<string, string[]>
	local order, n, c = {}, {}, {}
	for _, t in ipairs(calls) do
		local k = M.COUNT[M.tool(t)]
		local key = k[2] or k[1]
		if not n[key] then
			order[#order + 1] = key
			n[key], c[key] = 0, k
		end
		n[key] = n[key] + 1
	end
	---@type string[]
	local parts = {}
	for i, key in ipairs(order) do
		local k = c[key]
		parts[i] = not k[2] and k[1] or short and k[4] and M.count(n[key], k[4], k[5]) or M.count(n[key], k[2], k[3])
	end
	return table.concat(parts, short and " · " or ", ")
end

--- How long a thought took, as a digest or a thought row says it.
---@param secs number
---@return string
function M.thought(secs) return secs >= 0.05 and "thought " .. M.duration(secs) or "thought" end

--- What rows[first..last] of a turn fold into, the chat's digest and the
--- thinking pane's header: "6 files, 8 commands · thought 8s"; "" when
--- nothing there folds.
---@param rows zemi.TranscriptRow[]
---@param first integer
---@param last integer
---@return string
function M.digest(rows, first, last)
	---@type zemi.TranscriptRow[]
	local calls = {}
	local secs, thought = 0, false
	for i = first, last do
		local m = rows[i]
		if m.kind == "reasoning" then
			secs, thought = secs + (m.thought or 0), true
		elseif M.looks(m) then
			calls[#calls + 1] = m
		end
	end
	---@type string[]
	local parts = {}
	if #calls > 0 then parts[1] = M.counts(calls) end
	if thought then parts[#parts + 1] = M.thought(secs) end
	return table.concat(parts, " · ")
end

--- A tool name as words for a row with no verb of its own: underscores
--- split, the first word capitalized, and an MCP tool's mcp_ dropped
--- ("mcp_tracker_list_issues" -> "Tracker list issues").
---@param name string
---@return string
function M.title(name)
	local s = name:gsub("^mcp_", ""):gsub("_", " ")
	return s:sub(1, 1):upper() .. s:sub(2)
end

--- A call's headline as a verb and the rest: "Read", " a.go". A command's
--- about is itself a verb phrase ("Run the tests"), so its first word is the
--- verb; an unlabelled call reads as its input.
---@param name string
---@param label string The model's description of the call, or "".
---@param input string The call's input as the row shows it, or "".
---@return string verb
---@return string rest
function M.head(name, label, input)
	if name == "shell" and label ~= "" then
		local verb, rest = label:match("^(%S+)(.*)$")
		return verb:sub(1, 1):upper() .. verb:sub(2), rest
	end
	local rest = label ~= "" and " " .. M.lower(label) or input ~= "" and " " .. input or ""
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

--- A pattern matching one UTF-8 character.
---@type string
M.UTF8_CHAR = "[%z\1-\127\194-\244][\128-\191]*"

--- Text cut to room cells, ending with an ellipsis when it cuts, never
--- after a space.
---@param text string
---@param room integer
---@return string
function M.clip(text, room)
	if zemi.ui.width(text) <= room then return text end
	if room < 1 then return "" end
	---@type string[]
	local chars = {}
	local w = 0
	for ch in text:gmatch(M.UTF8_CHAR) do
		local cw = zemi.ui.width(ch)
		if w + cw > room - 1 then break end
		chars[#chars + 1] = ch
		w = w + cw
	end
	return (table.concat(chars):gsub("%s+$", "")) .. "…"
end

return M
