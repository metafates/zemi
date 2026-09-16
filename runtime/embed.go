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

// Skills holds the builtin skills (skills/<name>/SKILL.md): zemi's
// knowledge about itself, copied to disk and discovered like any skill.
//
//go:embed skills
var Skills embed.FS

// Workflows holds the builtin workflow scripts (workflows/<name>.lua).
//
//go:embed workflows
var Workflows embed.FS

// Types holds the LuaLS annotations for the zemi.* API (types/zemi.lua),
// written to the data dir on every start so an editor and zemi itself can
// read the API surface instead of guessing at it.
//
//go:embed types
var Types embed.FS
