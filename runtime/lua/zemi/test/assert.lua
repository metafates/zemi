-- The zemi plugins test boot's test.eq, neq, contains and raises, in Lua so
-- they print values with zemi.util.inspect. diff walks tables in key order, a
-- pair met again counting as equal so cycles end.
local util = require("zemi.util")

local inspect = util.inspect

local function diff(got, want, path, seen)
	if type(got) == "table" and type(want) == "table" then
		seen[got] = seen[got] or {}
		if seen[got][want] then return end
		seen[got][want] = true
		local both = {}
		for k in pairs(got) do
			both[k] = true
		end
		for k in pairs(want) do
			both[k] = true
		end
		for _, k in ipairs(util.keys(both)) do
			local key = type(k) == "string" and k:find("^[%a_][%w_]*$") and "." .. k or "[" .. inspect(k) .. "]"
			local p, g, w = diff(got[k], want[k], path .. key, seen)
			if p then return p, g, w end
		end
	elseif got ~= want and (got == got or want == want) then -- NaN equals NaN
		return path, got, want
	end
end

-- clip quotes s from byte from to byte to, a cut inside a UTF-8 sequence
-- moved to its edge
local function clip(s, from, to)
	while from > 1 and s:byte(from) >= 128 and s:byte(from) < 192 do
		from = from - 1
	end
	while to < #s and s:byte(to + 1) >= 128 and s:byte(to + 1) < 192 do
		to = to + 1
	end
	local q = inspect(s:sub(from, to))
	if from > 1 then q = '"...' .. q:sub(2) end
	if to < #s then q = q:sub(1, -2) .. '..."' end
	return q
end

local function window(g, w)
	local i = 1
	while i <= #g and g:byte(i) == w:byte(i) do
		i = i + 1
	end
	local function cut(s) return clip(s, math.max(1, i - 10), math.min(#s, i + 39)) end
	local chars = select(2, g:sub(1, i - 1):gsub("[^\128-\191]", "")) + 1
	return ("got %s, want %s (first difference at char %d)"):format(cut(g), cut(w), chars)
end

-- lines splits s at "\n"; a trailing newline ends the last line
local function lines(s)
	local out = {}
	if s:sub(-1) ~= "\n" then s = s .. "\n" end
	for l in s:gmatch("(.-)\n") do
		out[#out + 1] = l
	end
	return out
end

local function fail(msg)
	error(msg, 4) -- 4: fail, the assertion, the case; gopher-lua counts error itself
end

-- rel makes the plugin copy's paths relative before a message quotes a string
---@param rel fun(s: string): string
return function(rel)
	-- strdiff says how two unequal strings differ, by line when either has several
	local function strdiff(g, w)
		g, w = rel(g), rel(w)
		local at = ""
		if g:find("\n", 1, true) or w:find("\n", 1, true) then
			local gl, wl, i = lines(g), lines(w), 1
			while i <= math.max(#gl, #wl) and gl[i] == wl[i] do
				i = i + 1
			end
			if i <= math.max(#gl, #wl) then -- else only a trailing newline differs
				at = "line " .. i .. ": "
				if not gl[i] or not wl[i] then return at .. "got " .. inspect(gl[i]) .. ", want " .. inspect(wl[i]) end
				g, w = gl[i], wl[i]
			end
		end
		if math.max(#g, #w) > 40 then return at .. window(g, w) end
		return at .. "got " .. inspect(g) .. ", want " .. inspect(w)
	end

	local M = {}

	function M.eq(...)
		if select("#", ...) < 2 then fail("test.eq: takes the value got and the value wanted") end
		local got, want = ...
		local path, g, w = diff(got, want, "", {})
		if not path then return end
		local msg = type(g) == "string" and type(w) == "string" and strdiff(g, w)
			or "got " .. inspect(g) .. ", want " .. inspect(w)
		if path ~= "" then
			local gl, wl = inspect(got), inspect(want)
			msg = path:gsub("^%.", "") .. ": " .. msg
			if #gl + #wl <= 120 then msg = msg .. "\ngot  " .. gl .. "\nwant " .. wl end
		end
		fail(msg)
	end

	function M.neq(got, other)
		if not diff(got, other, "", {}) then fail("got " .. inspect(got) .. ", want anything else") end
	end

	function M.contains(s, sub)
		if type(s) ~= "string" or type(sub) ~= "string" then
			fail(
				"test.contains: takes a string and the text it should hold, got a " .. type(s) .. " and a " .. type(sub)
			)
		end
		s = rel(s)
		if not s:find(sub, 1, true) then fail("got " .. clip(s, 1, 200) .. ", want it to contain " .. inspect(sub)) end
	end

	function M.raises(fn, sub)
		local ok, err = pcall(fn)
		if ok then fail("did not raise" .. (sub and ", want an error containing " .. inspect(sub) or "")) end
		local msg = rel(tostring(err))
		-- a driver inside fn raises for the pcall, not for the code under test
		if msg:find("cannot wait inside pcall", 1, true) then
			fail("a driver cannot run inside test.raises; drivers return nil, err instead of raising")
		end
		if sub and not msg:find(sub, 1, true) then
			fail("raised " .. inspect(msg) .. ", want it to contain " .. inspect(sub))
		end
		return msg
	end

	return M
end
