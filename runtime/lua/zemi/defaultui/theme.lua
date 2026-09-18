-- The default theme: greys, one accent (periwinkle), and the terminal's own
-- red, green and yellow so the user's palette decides those.
--
-- Widgets should reach for the semantic groups - Accent, Dim, Rule, Title,
-- Selected, Button, Danger, Ok - and everything else links to one of them.
-- That way a widget cannot pick a color that is off the identity, and
-- retinting the UI is a matter of changing the palette.

---@param palette table<string, string>
---@return table
local function theme(palette)
  local hl = {
    Normal = { fg = "fg" },
    Accent = { fg = "accent" },
    Dim = { fg = "dim" },
    Rule = { fg = "rule" },
    -- the composer frame while the draft goes to the shell, not the model
    RuleShell = { fg = "yellow" },
    Title = { fg = "accent", bold = true },
    Selected = { fg = "accent", bold = true },
    Button = { fg = "accent" },
    Danger = { fg = "red" },
    Ok = { fg = "green" },

    Comment = { link = "Dim" },
    UserMsg = { link = "Accent" },
    Toast = { fg = "fg", bg = "panel" },
    ToastTitle = { fg = "fg", bg = "panel", bold = true },
    ToastInfo = { fg = "accent", bg = "panel" },
    ToastWarn = { fg = "yellow", bg = "panel" },
    ToastErr = { fg = "red", bg = "panel" },
    ToastError = { fg = "red", bg = "panel", bold = true },
    MarkdownHeading = { link = "Title" },
    MarkdownCode = { fg = "fg", bg = "panel" },
    MarkdownLink = { fg = "accent", underline = true },
    MarkdownQuote = { fg = "dim", italic = true },
    AgentReason = { link = "Dim" },
    ThinkingMark = { link = "Accent" },
    ThinkingBar = { link = "Rule" },
    AgentToolName = { fg = "fg", bold = true },
    AgentToolRun = { link = "Dim" },
    AgentToolOk = { link = "Ok" },
    AgentToolErr = { link = "Danger" },
    -- a diff keeps the terminal's green and red on the sign and the line
    -- number, and tints the row no further than it has to
    DiffAdd = { bg = "addbg" },
    DiffAddNum = { fg = "green", bg = "addbg" },
    DiffDel = { bg = "delbg" },
    DiffDelNum = { fg = "red", bg = "delbg" },
    DiffCtx = { link = "Normal" },
    DiffCtxNum = { link = "Dim" },
    TodoActive = { fg = "fg", bold = true },
    StatusUsage = { link = "Dim" },
    StatusUsageWarn = { fg = "yellow" },
    StatusUsageErr = { fg = "red", bold = true },
    ModePlan = { link = "Ok" },
    ModeAuto = { link = "Accent" },
    -- yolo answers every ask by itself, so nothing else on screen shows it
    -- is on: it is the only red in the statusline
    ModeYolo = { fg = "red", bold = true },
    PromptPlan = { link = "Ok" },
    PromptAuto = { link = "Accent" },
    PromptYolo = { fg = "red", bold = true },
    -- the shell glyph: what follows goes to the shell, not to the model
    PromptShell = { fg = "yellow", bold = true },
    Queued = { link = "Dim" },
    SplashKey = { link = "Button" },
    ScrollbarTrack = { link = "Dim" },
    ScrollbarThumb = { link = "Accent" },
    -- hover draws over the text's own style: keys left out keep it. A row
    -- header takes the accent and nothing else; body rows opt out entirely
    -- (hover = false) and say a click acts with the pointer shape alone.
    Hover = { link = "Accent" },
    HoverItem = { link = "Accent" },
  }
  return { palette = palette, hl = hl }
end

zemi.ui.theme.add("default", theme({
  bg = "#141414",
  fg = "#d8d8d8",
  dim = "#7a7a7a",
  rule = "#3a3a3a",
  accent = "#9aa5f2",
  panel = "#1e1e1e",
  addbg = "#1b2417",
  delbg = "#2a1a1a",
}))

-- picked automatically when the terminal reports a light background
zemi.ui.theme.add("default-light", theme({
  bg = "#fafafa",
  fg = "#1b1b1b",
  dim = "#8e8e8e",
  rule = "#cfcfcf",
  accent = "#6b78d6",
  panel = "#f0f0f0",
  addbg = "#e7f2e2",
  delbg = "#fae7e7",
}))

zemi.ui.theme.set("default")
