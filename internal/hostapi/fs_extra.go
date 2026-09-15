package hostapi

import (
	"os"

	glua "github.com/yuin/gopher-lua"
)

// fsExtraFns adds list, stat, exists, mkdir and remove to zemi.fs. Paths
// resolve inside the jails like read and write, reads pass the lua.fs.read
// gate and changes lua.fs.write; runtime failures return nil, err.
func (h *Host) fsExtraFns(L *glua.LState, tb *glua.LTable) {
	fail := func(L *glua.LState, err error) int {
		L.Push(glua.LNil)
		L.Push(glua.LString(err.Error()))
		return 2
	}
	resolve := func(L *glua.LState, api string, write bool) (string, bool) {
		roots, subject := h.readJails(), "lua.fs.read"
		if write {
			roots, subject = h.writeJails(), "lua.fs.write"
		}
		resolved, err := h.resolveJailed(L.CheckString(1), roots)
		if err != nil {
			L.RaiseError("zemi.fs.%s: %v", api, err)
			return "", false
		}
		if err := h.luaGate(L, subject, resolved); err != nil {
			return "", false
		}
		return resolved, true
	}
	kind := func(fi os.FileInfo) string {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return "link"
		case fi.IsDir():
			return "dir"
		}
		return "file"
	}

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		dir, ok := resolve(L, "list", false)
		if !ok {
			return 0
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fail(L, err)
		}
		out := L.NewTable()
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				continue
			}
			row := L.NewTable()
			row.RawSetString("name", glua.LString(e.Name()))
			row.RawSetString("type", glua.LString(kind(fi)))
			row.RawSetString("size", glua.LNumber(fi.Size()))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("stat", L.NewFunction(func(L *glua.LState) int {
		path, ok := resolve(L, "stat", false)
		if !ok {
			return 0
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return fail(L, err)
		}
		out := L.NewTable()
		out.RawSetString("type", glua.LString(kind(fi)))
		out.RawSetString("size", glua.LNumber(fi.Size()))
		out.RawSetString("mtime", glua.LNumber(fi.ModTime().UnixMilli()))
		L.Push(out)
		return 1
	}))
	tb.RawSetString("exists", L.NewFunction(func(L *glua.LState) int {
		path, ok := resolve(L, "exists", false)
		if !ok {
			return 0
		}
		_, err := os.Lstat(path)
		L.Push(glua.LBool(err == nil))
		return 1
	}))
	tb.RawSetString("mkdir", L.NewFunction(func(L *glua.LState) int {
		path, ok := resolve(L, "mkdir", true)
		if !ok {
			return 0
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fail(L, err)
		}
		L.Push(glua.LTrue)
		return 1
	}))
	// remove deletes one file or empty directory (never a tree).
	tb.RawSetString("remove", L.NewFunction(func(L *glua.LState) int {
		path, ok := resolve(L, "remove", true)
		if !ok {
			return 0
		}
		if err := os.Remove(path); err != nil {
			return fail(L, err)
		}
		L.Push(glua.LTrue)
		return 1
	}))
}
