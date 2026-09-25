-- whorl, the shipped theme: greys, one accent (periwinkle), and the terminal's own
-- red, green and yellow so the user's palette decides those.
--
-- Widgets should reach for the semantic groups - Accent, Dim, Rule, Title,
-- Selected, Button, Danger, Warn, Ok - and everything else links to one of
-- them. Red means a failure or danger, yellow a warning.
-- That way a widget cannot pick a color that is off the identity, and
-- retinting the UI is a matter of changing the palette.

---@param palette table<string, string>
---@return table
local function theme(palette)
  local hl = {
    -- Normal carries the app background: the resolver fills groups that
    -- have no bg of their own with it, so zemi paints its own canvas and
    -- the terminal's background never shows through.
    Normal = { fg = "fg", bg = "bg" },
    Accent = { fg = "accent" },
    Dim = { fg = "dim" },
    Rule = { fg = "rule" },
    -- the composer frame while the draft goes to the shell, not the model
    RuleShell = { link = "Warn" },
    RuleCommand = { fg = "accent" },
    Title = { fg = "accent", bold = true },
    Selected = { fg = "accent", bold = true },
    Button = { fg = "accent" },
    Danger = { fg = "red" },
    Warn = { fg = "yellow" },
    Ok = { fg = "green" },

    Comment = { link = "Dim" },
    UserMsg = { link = "Accent" },
    ToastInfo = { link = "Title" },
    ToastWarn = { link = "Warn", bold = true },
    ToastErr = { link = "Danger", bold = true },
    MarkdownHeading = { link = "Title" },
    -- an inline span reads by its color: a filled background would hang
    -- off the text as a stray block where it wraps
    MarkdownCode = { fg = "accent" },
    MarkdownLink = { fg = "accent", underline = true },
    MarkdownQuote = { fg = "dim", italic = true },
    AgentReason = { link = "Dim" },
    ThinkingMark = { link = "Accent" },
    ThinkingBar = { link = "Rule" },
    AgentToolName = { fg = "fg", bold = true },
    -- a diff keeps the terminal's green and red on the sign and the line
    -- number, and tints the row no further than it has to; the two tints
    -- stay apart on a 256-color terminal
    DiffAdd = { fg = "fg", bg = "addbg" },
    DiffAddNum = { fg = "green", bg = "addbg" },
    DiffDel = { fg = "fg", bg = "delbg" },
    DiffDelNum = { fg = "red", bg = "delbg" },
    DiffCtx = { link = "Normal" },
    DiffCtxNum = { link = "Dim" },
    TodoTitle = { link = "Title" },
    TodoMark = { link = "Accent" },
    TodoDone = { link = "Ok" },
    TodoActive = { fg = "fg", bold = true },
    StatusUsage = { link = "Dim" },
    StatusUsageWarn = { link = "Warn" },
    StatusUsageErr = { link = "Danger", bold = true },
    -- the statusline's mode and the composer prompt: a read-only agent
    -- plans; yolo answers every ask by itself, so nothing else on screen
    -- shows it is on, and it is the only red in the statusline
    ModePlan = { link = "Ok" },
    ModeAuto = { link = "Accent" },
    ModeYolo = { link = "Danger", bold = true },
    -- the shell glyph: what follows goes to the shell, not to the model
    PromptShell = { link = "Warn", bold = true },
    PromptCommand = { fg = "accent", bold = true },
    Queued = { link = "Dim" },
    SplashKey = { link = "Button" },
    ScrollbarTrack = { link = "Rule" },
    ScrollbarThumb = { link = "Accent" },
    -- hover draws over the text's own style: keys left out keep it. A row
    -- header takes the accent and nothing else; body rows opt out entirely
    -- (hover = false) and say a click acts with the pointer shape alone.
    Hover = { link = "Accent" },
    HoverItem = { link = "Accent" },
  }
  return { palette = palette, hl = hl }
end

zemi.ui.theme.add(
  "whorl",
  theme({
    bg = "#141414",
    fg = "#d8d8d8",
    dim = "#7a7a7a",
    rule = "#3a3a3a",
    accent = "#9aa5f2",
    addbg = "#183018",
    delbg = "#361010",
  })
)

-- picked automatically when the terminal reports a light background
zemi.ui.theme.add(
  "whorl-light",
  theme({
    bg = "#fafafa",
    fg = "#1b1b1b",
    dim = "#8e8e8e",
    rule = "#cfcfcf",
    accent = "#6b78d6",
    addbg = "#e7f2e2",
    delbg = "#fae7e7",
  })
)
