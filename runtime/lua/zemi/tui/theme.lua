-- The highlight groups every theme draws with, and the shipped themes.
--
-- Widgets reach for the semantic groups - Accent, Dim, Rule, Title,
-- Literal, Danger, Warn, Ok - and everything else links to one of them. A
-- group names a palette role, never a color, so a theme is one line of
-- colors: the accent is zemi and what acts, the literal inline code and a
-- string or number in code, danger a failure, warn a warning, ok done.

local groups = {
	-- the canvas is the palette's bg, which every group without a bg of its
	-- own sits on; a bg set here recolors Normal text alone
	Normal = { fg = "fg", bg = "bg" },
	Accent = { fg = "accent" },
	Dim = { fg = "dim" },
	Rule = { fg = "rule" },
	-- the composer frame while the draft goes to the shell, not the model
	RuleShell = { link = "Warn" },
	RuleCommand = { link = "Accent" },
	Title = { fg = "accent", bold = true },
	Literal = { fg = "literal" },
	Danger = { fg = "danger" },
	Warn = { fg = "warn" },
	Ok = { fg = "ok" },

	-- the snail's shell, zemi's mark: hl.set("Snail", ...) recolors it alone
	Snail = { link = "Accent" },
	UserMsg = { link = "Accent" },
	ToastInfo = { link = "Title" },
	ToastWarn = { link = "Warn", bold = true },
	ToastErr = { link = "Danger", bold = true },
	MarkdownHeading = { link = "Title" },
	-- an inline span reads by its color: a filled background would hang
	-- off the text as a stray block where it wraps
	MarkdownCode = { link = "Literal" },
	MarkdownLink = { fg = "accent", underline = true },
	MarkdownQuote = { fg = "dim", italic = true },
	AgentReason = { link = "Dim" },
	ThinkingMark = { link = "Accent" },
	ThinkingBar = { link = "Rule" },
	AgentToolName = { fg = "fg", bold = true },
	-- a diff marks the sign and the line number in Ok and Danger and tints
	-- the row no further than it has to
	DiffAdd = { fg = "fg", bg = "addbg" },
	DiffAddNum = { fg = "ok", bg = "addbg" },
	DiffDel = { fg = "fg", bg = "delbg" },
	DiffDelNum = { fg = "danger", bg = "delbg" },
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
	PromptCommand = { link = "Title" },
	Queued = { link = "Dim" },
	SplashKey = { link = "Accent" },
	ScrollbarTrack = { link = "Rule" },
	ScrollbarThumb = { link = "Accent" },
	-- a scrolled region's pinned header (a prompt over its turn): chrome, so
	-- grey over a rule the width of the view, never read as a message
	Pinned = { fg = "dim" },
	-- hover draws over the text's own style: keys left out keep it. A row
	-- header takes the accent and nothing else; body rows opt out entirely
	-- (hover = false) and say a click acts with the pointer shape alone. A
	-- list row underlines, so it never takes the selection's or a match's accent.
	Hover = { link = "Accent" },
	HoverItem = { underline = true },
}
for name, spec in pairs(groups) do
	zemi.ui.hl.set(name, spec)
end

-- whorl, zemi's own: slate greys, a periwinkle accent and an apricot
-- literal, with red, amber and mint for state.
zemi.ui.theme.add("whorl", {
	bg = "#17181e",
	fg = "#e7e7ec",
	dim = "#898b98",
	rule = "#4b4c58",
	panel = "#202128",
	accent = "#a2adfd",
	literal = "#f9b379",
	ok = "#68d7a1",
	warn = "#edcf6f",
	danger = "#f17070",
})

-- picked automatically when the terminal reports a light background
zemi.ui.theme.add("whorl-light", {
	bg = "#fafafc",
	fg = "#202127",
	dim = "#696b78",
	rule = "#bcbdc6",
	panel = "#eff0f3",
	accent = "#5458c8",
	literal = "#ab5819",
	ok = "#007f52",
	-- NOTE: 3.85:1 on white (APCA Lc 64), which the 3.5 floor on the band
	-- darkens to 4.8:1 on bg; a darker amber reads as danger to deuteranopes.
	warn = "#a97500",
	danger = "#c72d2e",
})

-- Ported from the Ghostty theme each names (the schemes are MIT): the
-- terminal's bg, fg and 16 colors, the rest derived. A "-light" twin draws
-- on a light terminal.
-- stylua: ignore start
-- Ghostty "Catppuccin Mocha"
zemi.ui.theme.add("catppuccin", { bg = "#1e1e2e", fg = "#cdd6f4", ansi = { "#45475a", "#f38ba8", "#a6e3a1", "#f9e2af", "#89b4fa", "#f5c2e7", "#94e2d5", "#a6adc8", "#585b70", "#f37799", "#89d88b", "#ebd391", "#74a8fc", "#f2aede", "#6bd7ca", "#bac2de" } })
-- Ghostty "Catppuccin Latte"
zemi.ui.theme.add("catppuccin-light", { bg = "#eff1f5", fg = "#4c4f69", ansi = { "#5c5f77", "#d20f39", "#40a02b", "#df8e1d", "#1e66f5", "#ea76cb", "#179299", "#acb0be", "#6c6f85", "#de293e", "#49af3d", "#eea02d", "#456eff", "#fe85d8", "#2d9fa8", "#bcc0cc" } })
-- Ghostty "TokyoNight"
zemi.ui.theme.add("tokyonight", { bg = "#1a1b26", fg = "#c0caf5", ansi = { "#15161e", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#a9b1d6", "#414868", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#c0caf5" } })
-- Ghostty "TokyoNight Day"
zemi.ui.theme.add("tokyonight-light", { bg = "#e1e2e7", fg = "#3760bf", ansi = { "#e9e9ed", "#f52a65", "#587539", "#8c6c3e", "#2e7de9", "#9854f1", "#007197", "#6172b0", "#a1a6c5", "#f52a65", "#587539", "#8c6c3e", "#2e7de9", "#9854f1", "#007197", "#3760bf" } })
-- Ghostty "Gruvbox Dark"
zemi.ui.theme.add("gruvbox", { bg = "#282828", fg = "#ebdbb2", ansi = { "#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984", "#928374", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#ebdbb2" } })
-- Ghostty "Gruvbox Light"
zemi.ui.theme.add("gruvbox-light", { bg = "#fbf1c7", fg = "#3c3836", ansi = { "#fbf1c7", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#7c6f64", "#928374", "#9d0006", "#79740e", "#b57614", "#076678", "#8f3f71", "#427b58", "#3c3836" } })
-- Ghostty "Nord"
zemi.ui.theme.add("nord", { bg = "#2e3440", fg = "#d8dee9", ansi = { "#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0", "#596377", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4" } })
-- Ghostty "Dracula"
zemi.ui.theme.add("dracula", { bg = "#282a36", fg = "#f8f8f2", ansi = { "#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#bd93f9", "#ff79c6", "#8be9fd", "#f8f8f2", "#6272a4", "#ff6e6e", "#69ff94", "#ffffa5", "#d6acff", "#ff92df", "#a4ffff", "#ffffff" } })
-- Ghostty "iTerm2 Solarized Dark"
zemi.ui.theme.add("solarized", { bg = "#002b36", fg = "#839496", ansi = { "#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5", "#335e69", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3" } })
-- Ghostty "iTerm2 Solarized Light"
zemi.ui.theme.add("solarized-light", { bg = "#fdf6e3", fg = "#657b83", ansi = { "#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#bbb5a2", "#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3" } })
-- Ghostty "Rose Pine"
zemi.ui.theme.add("rose-pine", { bg = "#191724", fg = "#e0def4", ansi = { "#26233a", "#eb6f92", "#31748f", "#f6c177", "#9ccfd8", "#c4a7e7", "#ebbcba", "#e0def4", "#6e6a86", "#eb6f92", "#31748f", "#f6c177", "#9ccfd8", "#c4a7e7", "#ebbcba", "#e0def4" } })
-- Ghostty "Rose Pine Dawn"
zemi.ui.theme.add("rose-pine-light", { bg = "#faf4ed", fg = "#575279", ansi = { "#f2e9e1", "#b4637a", "#286983", "#ea9d34", "#56949f", "#907aa9", "#d7827e", "#575279", "#9893a5", "#b4637a", "#286983", "#ea9d34", "#56949f", "#907aa9", "#d7827e", "#575279" } })
-- Ghostty "Kanagawa Wave"
zemi.ui.theme.add("kanagawa", { bg = "#1f1f28", fg = "#dcd7ba", ansi = { "#090618", "#c34043", "#76946a", "#c0a36e", "#7e9cd8", "#957fb8", "#6a9589", "#c8c093", "#727169", "#e82424", "#98bb6c", "#e6c384", "#7fb4ca", "#938aa9", "#7aa89f", "#dcd7ba" } })
-- Ghostty "Kanagawa Lotus"
zemi.ui.theme.add("kanagawa-light", { bg = "#f2ecbc", fg = "#545464", ansi = { "#1f1f28", "#c84053", "#6f894e", "#77713f", "#4d699b", "#b35b79", "#597b75", "#545464", "#8a8980", "#d7474b", "#6e915f", "#836f4a", "#6693bf", "#624c83", "#5e857a", "#43436c" } })
-- Ghostty "Atom One Dark"
zemi.ui.theme.add("one", { bg = "#21252b", fg = "#abb2bf", ansi = { "#21252b", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#abb2bf", "#767676", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#abb2bf" } })
-- Ghostty "Atom One Light"
zemi.ui.theme.add("one-light", { bg = "#f9f9f9", fg = "#2a2c33", ansi = { "#000000", "#de3e35", "#3f953a", "#d2b67c", "#2f5af3", "#950095", "#3f953a", "#bbbbbb", "#000000", "#de3e35", "#3f953a", "#d2b67c", "#2f5af3", "#a00095", "#3f953a", "#ffffff" } })
-- Ghostty "Everforest Dark Hard"
zemi.ui.theme.add("everforest", { bg = "#1e2326", fg = "#d3c6aa", ansi = { "#7a8478", "#e67e80", "#a7c080", "#dbbc7f", "#7fbbb3", "#d699b6", "#83c092", "#f2efdf", "#a6b0a0", "#f85552", "#8da101", "#dfa000", "#3a94c5", "#df69ba", "#35a77c", "#fffbef" } })
-- Ghostty "GitHub Dark Default"
zemi.ui.theme.add("github", { bg = "#0d1117", fg = "#e6edf3", ansi = { "#484f58", "#ff7b72", "#3fb950", "#d29922", "#58a6ff", "#bc8cff", "#39c5cf", "#b1bac4", "#6e7681", "#ffa198", "#56d364", "#e3b341", "#79c0ff", "#d2a8ff", "#56d4dd", "#ffffff" } })
-- Ghostty "GitHub Light Default"
zemi.ui.theme.add("github-light", { bg = "#ffffff", fg = "#1f2328", ansi = { "#24292f", "#cf222e", "#116329", "#4d2d00", "#0969da", "#8250df", "#1b7c83", "#6e7781", "#57606a", "#a40e26", "#1a7f37", "#633c01", "#218bff", "#a475f9", "#3192aa", "#8c959f" } })
-- stylua: ignore end
