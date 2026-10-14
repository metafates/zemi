package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/lua"
)

// TestJobStartFailure: a command that cannot start is a runtime failure,
// so job.start and job.wait return nil, err instead of raising (DESIGN §6.0).
func TestJobStartFailure(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		local res, err = zemi.job.wait({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
		res, err = zemi.job.start({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
	`)
}

func TestJobHandleColonCalls(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		local j = zemi.job.start({ cmd = "cat", stdin = true })
		assert(j:write("hi") == true, "write")
		j:close_stdin()
		local res, err = j:wait(2000)
		assert(res and res.stdout == "hi", tostring(err))
	`)
}

// TestJobCwd: a job's cwd resolves in the read jail, so no job starts
// outside it.
func TestJobCwd(t *testing.T) {
	h, r := newTestHost(t)
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := outsideTemp(t)
	runUser(t, r, `
		local outside = "`+outside+`"
		local res = assert(zemi.job.wait({ cmd = "pwd", cwd = "sub" }))
		assert(res.stdout:match("/sub\n$"), res.stdout)
		assert(not pcall(zemi.job.wait, { cmd = "pwd", cwd = outside }), "cwd outside the jail")
		local h, err = zemi.job.start({ cmd = "pwd", cwd = "gone" })
		assert(not h and err:find("cwd: .*gone"), err)
	`)
}

// TestFsDataDir: user code reads and writes its data dir and may hand it to
// the editor; plugin code gets neither the path nor the jail.
func TestFsDataDir(t *testing.T) {
	h, r := newTestHost(t)
	h.Dirs.DataHome = t.TempDir()
	runUser(t, r, `
		local dir = zemi.fs.data_dir("notes")
		assert(zemi.fs.write(dir .. "/a.txt", "hi"))
		assert(zemi.fs.read(dir .. "/a.txt") == "hi")
		assert(zemi.job.wait({ cmd = "pwd", cwd = dir }).stdout:find("/lua/notes\n$"))
		zemi.ui.edit(dir .. "/a.txt", {}, function() end)
		for _, bad in ipairs({ "", "..", "a/b" }) do
			assert(not pcall(zemi.fs.data_dir, bad), bad)
		end
	`)
	file := filepath.Join(h.Dirs.DataHome, "lua", "notes", "a.txt")
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runModelLua(t.Context(), `return zemi.fs.read(zemi.fs.data_dir("notes") .. "/a.txt")`, luaToolChunk); err == nil || !strings.Contains(err.Error(), "outside the jail") {
		t.Fatalf("an unapproved Lua tool script read the data dir: %q %v", out, err)
	}
	err := r.ExecuteChunk(t.Context(), `
		assert(not pcall(zemi.fs.data_dir, "notes"), "data_dir")
		for _, call in ipairs({ "read", "write" }) do
			local ok, err = pcall(zemi.fs[call], "`+file+`", "x")
			assert(not ok and tostring(err):find("outside the jail"), call .. ": " .. tostring(err))
		end
	`, "p.lua", lua.OriginPlugin)
	if err != nil {
		t.Fatal(err)
	}
}

// TestEditRepoRoot: started in a subdirectory of a trusted repository,
// zemi.ui.edit reaches the root and zemi.fs.write stays in the launch dir.
func TestEditRepoRoot(t *testing.T) {
	h, r := newTestHost(t)
	root := h.Dirs.ProjectDir
	h.Dirs.ProjectDir = filepath.Join(root, "sub")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.Dirs.ProjectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []string{"false", "true"} {
		h.SetProjectTrusted(trusted == "true")
		runUser(t, r, `
			local file = "`+root+`/a.txt"
			assert(not pcall(zemi.fs.write, file, "x"), "write")
			assert(pcall(zemi.ui.edit, file, {}, function() end) == `+trusted+`, "edit, trusted `+trusted+`")`)
	}
}

// TestBrainReadJails: the user brain's reads resolve in every project, and a
// trusted project's private brain reads too (DESIGN §9); a write there dies
// at jail resolution in TestLuaFsWriteBrainRefused.
func TestBrainReadJails(t *testing.T) {
	h, r := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	user, private := h.brainParts()
	userPath, privatePath := filepath.Join(user.Dir, "style.md"), filepath.Join(private.Dir, "repo-map.md")
	if err := os.MkdirAll(user.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(private.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name       string
		trusted    bool
		target     string
		wantJailed bool
	}{
		{"user brain read, untrusted", false, userPath, false},
		{"user brain read, trusted", true, userPath, false},
		{"private brain read, untrusted", false, privatePath, true},
		{"private brain read, trusted", true, privatePath, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.SetProjectTrusted(c.trusted)
			err := runChunk(t, r, fmt.Sprintf(`
				local ok, v = pcall(zemi.fs.read, %q)
				if not ok and tostring(v):find("outside the jail", 1, true) then error("jailed") end
				assert(ok and v == "x", v)`, c.target))
			if (err != nil) != c.wantJailed {
				t.Fatalf("err = %v, wantJailed %v", err, c.wantJailed)
			}
		})
	}
}

// TestLuaFsWriteBrainRefused: a brain path is in no write jail, so a Lua
// write dies at jail resolution and is never asked (DESIGN §9).
func TestLuaFsWriteBrainRefused(t *testing.T) {
	h, r := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	user, private := h.brainParts()
	mem := filepath.Join(user.Dir, brain.MemFile)
	runUser(t, r, `
		for _, file in ipairs({ "`+mem+`", "`+private.Dir+`/repo-map.md" }) do
			local ok, err = pcall(zemi.fs.write, file, "x")
			assert(not ok and tostring(err):find("outside the jail"), file .. ": " .. tostring(err))
		end
	`)
	if len(ui.asked) != 0 {
		t.Errorf("a brain write reached the user: %+v", ui.asked)
	}
}
