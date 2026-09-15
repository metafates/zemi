// Package runtime embeds the shipped Lua runtime (DESIGN §5, §12): the
// lowest-precedence runtimepath entry providing the zemi.* stdlib and the
// default UI registrations under lua/.
package runtime

import "embed"

// FS is the embedded runtime filesystem, rooted at this package directory
// (containing lua/...), matching the lua.Runtime's readEmbedded layout.
//
//go:embed lua
var FS embed.FS
