-- The default screen layout (DESIGN §8). User config replaces it whole with
-- its own zemi.ui.layout.set, or edits a copy from zemi.ui.layout.get().

---@type zemi.Layout
local layout = {
	root = {
		kind = "box",
		dir = "row",
		children = {
			{ kind = "spacer", size = "fill" },
			-- the main column, centered past its max
			{
				kind = "box",
				dir = "col",
				size = "fill",
				weight = 1000,
				min = 30,
				max = 100,
				children = {
					{ kind = "region", region = "header" },
					-- the chat and the thinking pane: the spacer sits in the chat
					-- column, so the separator meets the composer's top rule;
					-- prompts, overlays, toasts and completion popups go over the stage
					{
						kind = "box",
						dir = "row",
						sep = true,
						stage = true,
						children = {
							{
								kind = "box",
								dir = "col",
								weight = 1000,
								min = 63,
								max = 100,
								children = {
									{
										kind = "region",
										id = "chat",
										region = "chat",
										scroll = "bottom",
										size = "fill",
										min = 3,
									},
									{ kind = "spacer" },
								},
							},
							{
								kind = "box",
								dir = "col",
								weight = 1,
								min = 36,
								max = 56,
								collapse = true,
								hidden = true,
								children = {
									{ kind = "region", region = "thinking.head" },
									{
										kind = "region",
										id = "thinking",
										region = "thinking",
										scroll = "bottom",
										size = "fill",
									},
								},
							},
						},
					},
					-- the mascot lives on the top rule and the statusline on the bottom
					-- one, so no agent state adds or removes a row
					{
						kind = "composer",
						placeholder = "Ask anything...",
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

local column = layout.root.children[2]
local pane = column.children[2].children[2]

-- ui.reasoning = "pane" draws the thinking pane, and the column grows by
-- what the pane takes
local function set()
	local on = zemi.config.get("ui.reasoning") == "pane"
	column.max, pane.hidden = on and 157 or 100, not on
	zemi.ui.layout.set(layout)
end
set()
-- a layout user config set owns the screen, and setting this one over it
-- raises
zemi.config.watch("ui.reasoning", function() pcall(set) end)
