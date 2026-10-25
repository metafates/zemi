package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJobStartFailure: a command that cannot start is a runtime failure,
// so job.start and job.wait return nil, err instead of raising (DESIGN §6.0).
func TestJobStartFailure(t *testing.T) {
	t.Parallel()
	_, r := newTestHost(t)
	runUser(t, r, `
		local res, err = zemi.job.wait({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
		res, err = zemi.job.start({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
	`)
}

func TestJobHandleColonCalls(t *testing.T) {
	t.Parallel()
	_, r := newTestHost(t)
	runUser(t, r, `
		local j = zemi.job.start({ cmd = "cat", stdin = true })
		assert(j:write("hi") == true, "write")
		j:close_stdin()
		local res, err = j:wait(2000)
		assert(res and res.stdout == "hi", tostring(err))
	`)
}

// TestJobCwd: a job's cwd resolves against the project dir, and one outside
// the project starts like any other.
func TestJobCwd(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := outsideTemp(t)
	runUser(t, r, `
		local outside = "`+outside+`"
		local res = assert(zemi.job.wait({ cmd = "pwd", cwd = "sub" }))
		assert(res.stdout:match("/sub\n$"), res.stdout)
		res = assert(zemi.job.wait({ cmd = "pwd", cwd = outside }))
		assert(res.stdout:find(outside, 1, true) == 1, res.stdout)
		local h, err = zemi.job.start({ cmd = "pwd", cwd = "gone" })
		assert(not h and err:find("cwd: .*gone"), err)
	`)
}

// TestFsDataDir: user code reads and writes its data dir and may hand it to
// the editor; plugin code gets no path to it.
func TestFsDataDir(t *testing.T) {
	t.Parallel()
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
}

// TestUiEditNoRoots: zemi.ui.edit resolves a relative path against the
// project dir and opens one outside it, with the read gate still running.
func TestUiEditNoRoots(t *testing.T) {
	t.Parallel()
	_, r := newTestHost(t)
	outside := filepath.Join(outsideTemp(t), "a.txt")
	for _, c := range []struct{ name, path string }{
		{"relative", "a.txt"},
		{"outside the project", outside},
	} {
		runUser(t, r, fmt.Sprintf(`assert(pcall(zemi.ui.edit, %q, {}, function() end), %q)`, c.path, c.name))
	}
}

// TestLuaFsOutsideTheProject: a path outside the project and the config home
// is no longer refused at resolution, and the lua.fs.write gate still runs.
func TestLuaFsOutsideTheProject(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	file := filepath.Join(outsideTemp(t), "notes.txt")
	// an inert boot refuses every gated call but a read, so the refusal is
	// what shows the write reached the gate
	h.Inert = true
	if _, err := runChunk(t, r, fmt.Sprintf(`zemi.fs.write(%q, "x")`, file)); err == nil || !strings.Contains(err.Error(), "lua.fs.write") {
		t.Fatalf("the gate missed the write: %v", err)
	}
	h.Inert = false
	runUser(t, r, fmt.Sprintf(`assert(zemi.fs.write(%q, "x"), "write")`, file))
	if b, err := os.ReadFile(file); err != nil || string(b) != "x" {
		t.Fatalf("read back = %q, %v", b, err)
	}
}

// TestBrainFilesFromLua: a brain file is in no root of its own, so Lua reads
// and writes one like any other file, through the gates (DESIGN §10.8).
func TestBrainFilesFromLua(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	user, private := h.brainParts()
	for _, dir := range []string{user.Dir, private.Dir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ name, path string }{
		{"user brain", filepath.Join(user.Dir, "style.md")},
		{"private brain", filepath.Join(private.Dir, "repo-map.md")},
	} {
		runUser(t, r, fmt.Sprintf(`
			assert(zemi.fs.write(%q, "x"), "write")
			assert(zemi.fs.read(%q) == "x", "read")`, c.path, c.path))
	}
}
