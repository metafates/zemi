-- Formatting helpers the default widgets share:
-- require("zemi.defaultui.text").
---@class zemi.defaultui.text
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

--- A duration as "4.2s" or "3m05s"; nil for none.
---@param s number|nil seconds
---@return string|nil
function M.secs(s)
  if not s or s <= 0 then return nil end
  if s >= 60 then return string.format("%dm%02ds", s / 60, s % 60) end
  return string.format("%.1fs", s)
end

--- Elapsed time as "42s", "3m 05s" or "1h 02m 05s".
---@param s number|nil seconds
---@return string
function M.elapsed(s)
  s = math.floor(s or 0)
  if s < 60 then return s .. "s" end
  if s < 3600 then return string.format("%dm %02ds", math.floor(s / 60), s % 60) end
  return string.format("%dh %02dm %02ds", math.floor(s / 3600), math.floor(s / 60) % 60, s % 60)
end

--- Display width of a string: one cell per UTF-8 character.
--- NOTE: a wide glyph (CJK, emoji) counts as one, so clipped model text can
--- overflow into a wrapped row; exact widths need a host width call.
---@param s string
---@return integer
function M.cells(s)
  return select(2, s:gsub("[^\128-\191]", ""))
end

--- A pattern matching one UTF-8 character.
---@type string
M.UTF8_CHAR = "[%z\1-\127\194-\244][\128-\191]*"

--- Text cut to room cells, ending with an ellipsis when it cuts.
---@param text string
---@param room integer
---@return string
function M.clip(text, room)
  if M.cells(text) <= room then return text end
  if room < 1 then return "" end
  ---@type string[]
  local chars = {}
  for ch in text:gmatch(M.UTF8_CHAR) do
    if #chars == room - 1 then break end
    chars[#chars + 1] = ch
  end
  return table.concat(chars) .. "…"
end

--- Text split into pieces of at most room cells (code wraps, never clips).
---@param text string
---@param room integer
---@return string[]
function M.chunks(text, room)
  ---@type string[], string[]
  local out, cur = {}, {}
  for ch in text:gmatch(M.UTF8_CHAR) do
    if #cur >= room and #cur > 0 then
      out[#out + 1] = table.concat(cur)
      cur = {}
    end
    cur[#cur + 1] = ch
  end
  if #cur > 0 or #out == 0 then out[#out + 1] = table.concat(cur) end
  return out
end

return M
