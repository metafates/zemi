-- The splash that fills the empty chat until the first message.
local fmt = require("zemi.defaultui.text")
---@class zemi.defaultui.splash
local M = {}

---@type string[]
local LOGO = {
  "███████╗███████╗███╗   ███╗██╗",
  "╚══███╔╝██╔════╝████╗ ████║██║",
  "  ███╔╝ █████╗  ██╔████╔██║██║",
  " ███╔╝  ██╔══╝  ██║╚██╔╝██║██║",
  "███████╗███████╗██║ ╚═╝ ██║██║",
  "╚══════╝╚══════╝╚═╝     ╚═╝╚═╝",
}

--- The splash rows: the wordmark, what zemi will run with, recent sessions
--- to pick up and the keys worth knowing, centered in ctx.width by ctx.height.
---@param ctx zemi.WidgetCtx
---@return zemi.Line[]
function M.render(ctx)
  -- the block is at most 72 cells wide, so long paths clip instead of
  -- pushing the logo against the left edge
  ---@type integer, zemi.Line[]
  local width, rows = ctx.width or 80, {}
  local maxw = math.min(width, 72)
  ---@param segs zemi.Line
  local function row(segs) rows[#rows + 1] = segs end
  -- a short chat drops the recent sessions first, then the big logo, so
  -- the bottom-anchored chat never cuts the block's first rows (it shows
  -- height - 1 of them)
  local recent, fit = ctx.recent or {}, (ctx.height or 0) > 0 and ctx.height - 1 or math.huge
  local extra = #recent > 0 and #recent + 3 or 0
  if 12 + extra > fit then recent, extra = {}, 0 end
  if width >= 36 and 12 + extra <= fit then
    for i, l in ipairs(LOGO) do row({ { text = l, hl = "Logo" .. i } }) end
  else
    row({ { text = "zemi", hl = "Title" } })
  end
  local ver = (ctx.version or "") ~= "" and (ctx.version .. " · ") or ""
  row({ { text = ver .. "a coding agent that learns how you work", hl = "Comment" } })
  row({ { text = "" } })
  row({ { text = "model    ", hl = "Comment" }, { text = (ctx.model ~= "" and ctx.model) or "none yet: add a provider in init.lua", hl = "Normal" } })
  local cwd, room = ctx.cwd or "", maxw - 10
  if fmt.cells(cwd) > room and room > 1 then
    -- keep the tail: the project name is at the end
    ---@type string[]
    local chars = {}
    for ch in cwd:gmatch(fmt.UTF8_CHAR) do chars[#chars + 1] = ch end
    cwd = "…" .. table.concat(chars, "", #chars - room + 2)
  end
  row({ { text = "project  ", hl = "Comment" }, { text = cwd, hl = "Normal" } })
  if #recent > 0 then
    row({ { text = "" } })
    row({ { text = "recent", hl = "Title" } })
    for _, r in ipairs(recent) do
      local title = (r.title ~= "" and r.title) or "(untitled)"
      row({ { text = string.format("  %-8s ", r.ago), hl = "Comment" }, { text = title, hl = "Normal" } })
    end
    row({ { text = "  /resume", hl = "SplashKey" }, { text = " continues the latest, ", hl = "Comment" },
      { text = "/sessions", hl = "SplashKey" }, { text = " lists them", hl = "Comment" } })
  end
  row({ { text = "" } })
  local tips = { { "/", "commands" }, { "@", "attach or drop files" }, { "!", "shell" }, { "tab", "permission mode" } }
  ---@type zemi.Line
  local tip = {}
  for i, t in ipairs(tips) do
    if i > 1 then tip[#tip + 1] = { text = "   " } end
    tip[#tip + 1] = { text = t[1], hl = "SplashKey" }
    tip[#tip + 1] = { text = " " .. t[2], hl = "Comment" }
  end
  row(tip)

  -- Center the block as a whole (rows stay left-aligned to each other),
  -- clipping to the width so a narrow window never wraps the logo.
  local widest = 0
  for i, r in ipairs(rows) do
    ---@type integer, zemi.Line
    local n, clipped = 0, {}
    for _, seg in ipairs(r) do
      local left = maxw - n
      if left <= 0 then break end
      local text = fmt.clip(seg.text, left)
      clipped[#clipped + 1] = { text = text, hl = seg.hl }
      n = n + fmt.cells(text)
    end
    rows[i] = clipped
    widest = math.max(widest, n)
  end
  local pad = string.rep(" ", math.max(0, math.floor((width - widest) / 2)))
  ---@type zemi.Line[]
  local out = {}
  local top = math.max(0, math.floor(((ctx.height or 0) - #rows) / 2))
  for _ = 1, top do out[#out + 1] = { { text = "" } } end
  for _, r in ipairs(rows) do
    table.insert(r, 1, { text = pad, copy = false })
    out[#out + 1] = r
  end
  -- the chat is bottom-anchored: trailing rows keep the block centered
  for _ = 1, math.max(0, (ctx.height or 0) - top - #rows - 1) do out[#out + 1] = { { text = "" } } end
  return out
end

-- Register a widget with id "splash" in init.lua to replace it (ctx carries
-- version, model, cwd, recent, width and height).
zemi.ui.widget.add({
  id = "splash",
  region = "chat",
  priority = 5,
  ---@param ctx zemi.WidgetCtx
  ---@return zemi.Line[]
  render = function(ctx)
    if ctx.transcript_count > 0 or ctx.agent_active then return {} end
    return M.render(ctx)
  end,
})

return M
