package lua

import (
	"path/filepath"
	"strings"
	"sync"
)

// Origin is what a chunk of Lua is attributed to (DESIGN §5, §9). Origin is
// per call, derived from the EvalSymlinks-resolved path of the file the
// chunk came from, never from the requirer.
type Origin string

const (
	OriginUser    Origin = "user"
	OriginPlugin  Origin = "plugin"
	OriginProject Origin = "project"
	OriginLearned Origin = "learned"
	OriginBuiltin Origin = "builtin"
	OriginHost    Origin = "host"
)

// Rank orders origins by trust. Higher wins collisions and replaces lower
// (DESIGN §6.2, §11): user > builtin > plugin > project > learned.
func (o Origin) Rank() int {
	switch o {
	case OriginUser:
		return 40
	case OriginBuiltin:
		return 30
	case OriginPlugin:
		return 20
	case OriginProject:
		return 10
	case OriginLearned:
		return 5
	}
	return 0
}

// PathClasses maps a resolved absolute directory to the origin class its
// files run under. Set up by the runtime boot.
type PathClasses []PathClass

// PathClass is one prefix rule.
type PathClass struct {
	Prefix string // absolute directory
	Origin Origin
}

// Classify resolves the origin of a path. The path must already be
// symlink-resolved by the caller. Prefixes are symlink-resolved too
// (memoized), so a class registered under a path containing symlinked
// segments still matches its files. The longest matching prefix wins, so a
// project checked out under the config home is still project code. Unknown
// paths return "" and are refused at load time.
func (pc PathClasses) Classify(resolvedPath string) Origin {
	resolvedPath = filepath.Clean(resolvedPath)
	best, bestLen := Origin(""), 0
	for _, c := range pc {
		if c.Prefix == "" {
			continue
		}
		for _, p := range []string{c.Prefix, resolvePrefix(c.Prefix)} {
			if len(p) > bestLen && strings.HasPrefix(resolvedPath, p+string(filepath.Separator)) {
				best, bestLen = c.Origin, len(p)
			}
		}
	}
	return best
}

// prefixResolution memoizes EvalSymlinks of registered prefixes: Classify
// runs on every file load and prefix resolution is filesystem work.
var prefixResolution sync.Map

func resolvePrefix(p string) string {
	if v, ok := prefixResolution.Load(p); ok {
		return v.(string)
	}
	res, err := filepath.EvalSymlinks(p)
	if err != nil {
		res = p
	}
	prefixResolution.Store(p, res)
	return res
}

// ChunkName renders the debug name for a chunk: origin-tagged so tracebacks
// and error messages can be redacted per origin.
func (o Origin) ChunkName(rel string) string {
	return "@" + string(o) + ":" + rel
}
