package hostapi

// The theme groups Go itself draws segments in. The vocabulary belongs to
// the Lua themes (a theme names or links each group it styles, and an
// unknown group draws unstyled), so these are consts, not a closed type.
const (
	GroupDim          = "Dim"
	GroupTitle        = "Title"
	GroupOk           = "Ok"
	GroupWarn         = "Warn"
	GroupDanger       = "Danger"
	GroupRule         = "Rule"
	GroupThinkingMark = "ThinkingMark"
)
