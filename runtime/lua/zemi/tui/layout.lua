-- The default screen layout (DESIGN §8). User config replaces it whole with
-- its own zemi.ui.layout.set, or edits a copy from zemi.ui.layout.get().

---@type zemi.Layout
local layout = {
	root = {
		kind = "box",
		dir = "row",
		sep = true,
		children = {
			-- the main column
			{
				kind = "box",
				dir = "col",
				min = 30,
				children = {
					{ kind = "region", region = "header" },
					-- prompts, overlays, toasts and completion popups go over the stage
					{
						kind = "box",
						dir = "col",
						stage = true,
						min = 1,
						children = {
							{
								kind = "region",
								id = "chat",
								region = "chat",
								scroll = "bottom",
								size = "fill",
								min = 3,
							},
						},
					},
					{ kind = "spacer" },
					-- the mascot lives on the top rule and the statusline on the bottom
					-- one, so no agent state adds or removes a row
					{
						kind = "composer",
						frame = true,
						top = "working",
						left = "statusline.left",
						right = "statusline.right",
					},
				},
			},
		},
	},
}

zemi.ui.layout.set(layout)
