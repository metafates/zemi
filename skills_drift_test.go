package main

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/runtime"
)

// TestSkillsCoverBuiltinCommands: every Go-owned slash command appears in
// a builtin skill (internal/hostapi covers the rest of the surface).
func TestSkillsCoverBuiltinCommands(t *testing.T) {
	var text strings.Builder
	_ = fs.WalkDir(runtime.Skills, "skills", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := fs.ReadFile(runtime.Skills, path)
			text.Write(data)
		}
		return err
	})
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := hostapi.NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(mainCommands(func() {}, func() {})...)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.Commands.List() {
		if !strings.Contains(text.String(), "/"+c.Name) {
			t.Errorf("no builtin skill mentions /%s", c.Name)
		}
	}
}
