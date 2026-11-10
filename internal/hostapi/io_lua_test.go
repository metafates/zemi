package hostapi

import (
	"os"
	"path/filepath"
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
