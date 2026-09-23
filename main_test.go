package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sandbox"
)

// On Linux the sandbox probe and Wrap re-exec this binary as the helper.
func TestMain(m *testing.M) {
	sandbox.DispatchHelper()
	os.Exit(m.Run())
}

// TestMainCommandArgs: each command declares the arguments it reads, so a
// stray one errors instead of being dropped (the host has no session store).
func TestMainCommandArgs(t *testing.T) {
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := hostapi.NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(mainCommands(func() {}, func() {})...)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, args, want string }{
		{"new", "idea: add caching", "/new takes no arguments"},
		{"resume", "some-id", "session store unavailable"},
	} {
		if got := fmt.Sprint(h.RunCommand(context.Background(), c.name, c.args)); got != c.want {
			t.Errorf("/%s %s = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}
