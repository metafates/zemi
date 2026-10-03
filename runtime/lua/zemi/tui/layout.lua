-- The default screen layout (DESIGN §8). User config replaces it whole with
-- its own zemi.ui.layout.set, or edits a copy from zemi.ui.layout.get().

-- The stage stays narrow enough to read; the composer's rules run wider by
-- SLACK cells, so the working block and the statusline keep their widgets.
local SLACK = 24

-- the chat and a spacer, so the transcript's last row stands a row above the
-- composer's frame; prompts, overlays, toasts and completion popups go over the
-- stage
local stage = {
	kind = "box",
	dir = "col",
	stage = true,
	weight = 1000,
	min = 63,
	max = 100,
	children = {
		{
			kind = "region",
			id = "chat",
			region = "chat",
			scroll = "bottom",
			-- rows scrolled past fade in under the pinned headers
			fade = 4,
			size = "fill",
			min = 3,
		},
		{ kind = "spacer" },
	},
}

---@type zemi.Layout
local layout = {
	root = {
		kind = "box",
		dir = "row",
		children = {
			{ kind = "spacer", size = "fill" },
			-- the main column, centered past its max: the composer's frame is the
			-- column's width, wider than the stage centered inside it
			{
				kind = "box",
				dir = "col",
				size = "fill",
				weight = 1000,
				min = 30,
				max = 100 + SLACK,
				children = {
					{ kind = "region", region = "header" },
					{
						kind = "box",
						dir = "row",
						children = {
							{ kind = "spacer", size = "fill" },
							stage,
							{ kind = "spacer", size = "fill" },
						},
					},
					-- the mascot lives on the top rule and the statusline on the bottom
					-- one, so no agent state adds or removes a row
					{
						kind = "composer",
						placeholder = "Ask anything…",
						frame = true,
						top = "working",
						left = "statusline.left",
						right = "statusline.right",
					},
				},
			},
			{ kind = "spacer", size = "fill" },
		},
	},
}

zemi.ui.layout.set(layout)
