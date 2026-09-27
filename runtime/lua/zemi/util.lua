-- zemi.util: small helpers Lua authors reach for (after neovim's vim.split,
-- vim.trim, vim.tbl_*, vim.validate and vim.inspect).
-- local util = require("zemi.util")
---@class zemi.util
local M = {}

---@param b integer A byte.
---@return boolean
local function is_space(b) return b == 32 or (b >= 9 and b <= 13) end

---@param s string
---@return string
function M.trim(s)
  local i, j = 1, #s
  while i <= j and is_space(s:byte(i)) do
    i = i + 1
  end
  while j >= i and is_space(s:byte(j)) do
    j = j - 1
  end
  return s:sub(i, j)
end

--- Split on a plain (not pattern) separator, keeping empty fields.
---@param s string
---@param sep string
---@return string[]
function M.split(s, sep)
  if sep == "" then error("zemi.util.split: empty separator", 2) end
  ---@type string[], integer
  local out, i = {}, 1
  while true do
    local j = s:find(sep, i, true)
    if not j then
      out[#out + 1] = s:sub(i)
      return out
    end
    out[#out + 1] = s:sub(i, j - 1)
    i = j + #sep
  end
end

--- The keys, sorted so iteration order is stable: numbers ascending first.
---@generic K
---@param t table<K, any>
---@return K[]
function M.keys(t)
  local out = {}
  for k in pairs(t) do
    out[#out + 1] = k
  end
  table.sort(out, function(a, b)
    local na, nb = type(a) == "number", type(b) == "number"
    if na and nb then return a < b end
    if na ~= nb then return na end
    return tostring(a) < tostring(b)
  end)
  return out
end

--- A nested lookup (`get(t, "a", "b")` is t.a.b) that stops at the first
--- non-table.
---@param t any
---@param ... any Keys.
---@return any
function M.get(t, ...)
  for i = 1, select("#", ...) do
    if type(t) ~= "table" then return nil end
    t = t[select(i, ...)]
  end
  return t
end

--- Raise a caller-attributed error when value is not of kind (nil passes
--- when optional).
---@param name string
---@param value any
---@param kind type
---@param optional? boolean
function M.validate(name, value, kind, optional)
  if value == nil and optional then return end
  if type(value) ~= kind then error(string.format("%s: expected %s, got %s", name, kind, type(value)), 2) end
end

--- A one-line readable form; cycles and depth (4 by default) are bounded.
---@param v any
---@param depth? integer
---@return string
function M.inspect(v, depth)
  ---@type integer, table<table, true>
  local limit, seen = depth or 4, {}
  ---@param x any
  ---@param d integer
  ---@return string
  local function go(x, d)
    if type(x) == "string" then
      -- the escapes Lua reads back
      local escaped = x:gsub(
        '[%c"\\]',
        function(c) return (c == '"' or c == "\\") and "\\" .. c or ("\\%03d"):format(c:byte()) end
      )
      return '"' .. escaped .. '"'
    end
    if type(x) ~= "table" then return tostring(x) end
    if seen[x] then return "<cycle>" end
    if d > limit then return "{...}" end
    seen[x] = true
    ---@type string[], integer
    local parts, n = {}, #x
    for i = 1, n do
      parts[#parts + 1] = go(x[i], d + 1)
    end
    for _, k in ipairs(M.keys(x)) do
      local listed = type(k) == "number" and k >= 1 and k <= n and k % 1 == 0
      if not listed then
        local ident = type(k) == "string" and k ~= "" and not k:find("[^%w_]") and not k:find("^%d")
        parts[#parts + 1] = (ident and k or "[" .. go(k, d + 1) .. "]") .. " = " .. go(x[k], d + 1)
      end
    end
    seen[x] = nil
    if #parts == 0 then return "{}" end
    return "{ " .. table.concat(parts, ", ") .. " }"
  end
  return go(v, 1)
end

return M
