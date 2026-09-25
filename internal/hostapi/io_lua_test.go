package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestJobStartFailure: a command that cannot start is a runtime failure,
// so job.start and job.wait return nil, err instead of raising (DESIGN §6.0).
func TestJobStartFailure(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local res, err = zemi.job.wait({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
		res, err = zemi.job.start({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
	`)
}

func TestJobHandleColonCalls(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local j = zemi.job.start({ cmd = "cat", stdin = true })
		assert(j:write("hi") == true, "write")
		j:close_stdin()
		local res, err = j:wait(2000)
		assert(res and res.stdout == "hi", tostring(err))
	`)
}

// TestJobGateSeesCommandLine: lua.job.start rules match the whole normalized
// line, so arguments are scoped and a compound command cannot ride.
func TestJobGateSeesCommandLine(t *testing.T) {
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "true*", Decision: perm.Allow})
	runUser(t, r, `
		assert(zemi.job.wait({ cmd = "true", args = { "x" } }), "allowed")
		assert(not pcall(zemi.job.start, { cmd = "echo", args = { "true" } }), "the program leads the line")
	`)
}

// TestReadJailCoversRepo: started in a subdirectory of a trusted repository,
// reads elsewhere in it settle; secret shapes, writes and an untrusted
// project still ask.
func TestReadJailCoversRepo(t *testing.T) {
	h, _ := newTestHost(t)
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	for _, d := range []string{filepath.Join(root, ".git"), sub} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.Dirs.ProjectDir = sub
	sibling, secret := filepath.Join(root, "other", "main.go"), filepath.Join(root, ".env")
	for _, c := range []struct {
		name    string
		trusted bool
		subject string
		arg     string
		want    perm.Decision
	}{
		{"untrusted sibling", false, "fs.read", sibling, perm.Ask},
		{"trusted sibling", true, "fs.read", sibling, perm.Allow},
		{"secret", true, "fs.read", secret, perm.Ask},
		{"write", true, "fs.write", sibling, perm.Ask},
	} {
		h.SetProjectTrusted(c.trusted)
		if got := h.fsDefault(c.subject, c.arg); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		// the write jail is the launch dir by design; the ask says so and
		// shows the sibling's path from the repository root, not absolute
		if c.subject == "fs.write" {
			req := tools.Request{Subject: c.subject, Arg: c.arg}
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			h.askHook()(req)
			if len(ui.asked) != 1 || !strings.Contains(ui.asked[0].Reason, "outside pkg/, the directory zemi was started in") {
				t.Errorf("%s: ask = %+v", c.name, ui.asked)
			}
			if got := h.shownArg(req); got != filepath.Join("other", "main.go") {
				t.Errorf("%s: shown %q", c.name, got)
			}
			if label, _ := h.alwaysChoice(req); label != "allow edits in other/ and below" {
				t.Errorf("%s: always label %q", c.name, label)
			}
		}
	}
}

// TestJobCwdAndSandbox: cwd stays in the read jail, and sandbox = true runs
// under the policy, which keeps writes inside it.
func TestJobCwdAndSandbox(t *testing.T) {
	h, r, _, _ := sandboxedBash(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	runUser(t, r, `
		local outside = "`+outside+`"
		local res = assert(zemi.job.wait({ cmd = "pwd", cwd = "sub" }))
		assert(res.stdout:match("/sub\n$"), res.stdout)
		assert(not pcall(zemi.job.wait, { cmd = "pwd", cwd = outside }), "cwd outside the jail")
		local h, err = zemi.job.start({ cmd = "zz-no-such-program", sandbox = true })
		assert(not h and err:find("not found"), err)
		h, err = zemi.job.start({ cmd = "pwd", cwd = "gone", sandbox = true })
		assert(not h and err:find("cwd: .*gone"), err)
		for _, c in ipairs({
			{ "in.txt", true, 0 },
			{ outside .. "/out.txt", true, 1 },
			{ outside .. "/free.txt", false, 0 },
		}) do
			res = assert(zemi.job.wait({ cmd = "touch", args = { c[1] }, sandbox = c[2] }))
			assert((res.code == 0) == (c[3] == 0), c[1] .. ": " .. res.stderr)
		end
	`)
}

// TestFsDatadir: user code reads and, with a rule, writes its data dir and
// may hand it to the editor; plugin code gets neither the path nor the jail.
func TestFsDatadir(t *testing.T) {
	h, r := newTestHost(t)
	h.Dirs.DataHome = t.TempDir()
	h.Perm.Add(perm.Rule{Subject: "lua.fs.write", Pattern: "*", Decision: perm.Allow})
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "pwd", Decision: perm.Allow})
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
	if out, err := h.runModelLua(context.Background(), `return zemi.fs.read(zemi.fs.data_dir("notes") .. "/a.txt")`, luaToolChunk); err == nil || !strings.Contains(err.Error(), "outside the jail") {
		t.Fatalf("an unapproved Lua tool script read the data dir: %q %v", out, err)
	}
	err := r.ExecuteChunk(context.Background(), `
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
	h.Perm.Add(perm.Rule{Subject: "lua.fs.write", Pattern: "*", Decision: perm.Allow})
	for _, trusted := range []string{"false", "true"} {
		h.SetProjectTrusted(trusted == "true")
		runUser(t, r, `
			local file = "`+root+`/a.txt"
			assert(not pcall(zemi.fs.write, file, "x"), "write")
			assert(pcall(zemi.ui.edit, file, {}, function() end) == `+trusted+`, "edit, trusted `+trusted+`")`)
	}
}
