package editor

import (
	"strings"
	"testing"
)

func TestEditorAndMode(t *testing.T) {
	cases := []struct {
		name   string
		env    Env
		editor string
		mode   string
	}{
		{"defaults to vi outside tmux", Env{}, "vi", Fullscreen},
		{"setting beats the environment", Env{Configured: "code --wait", Visual: "nvim", Editor: "vim"}, "code --wait", Fullscreen},
		{"VISUAL beats EDITOR", Env{Visual: "nvim", Editor: "vim"}, "nvim", Fullscreen},
		{"tmux auto is a popup", Env{Editor: "nvim", TMUX: "/tmp/tmux-1/default,1,0", Window: "auto"}, "nvim", Popup},
		{"tmux split on request", Env{Editor: "nvim", TMUX: "x", Window: Split}, "nvim", Split},
		{"tmux fullscreen on request", Env{Editor: "nvim", TMUX: "x", Window: Fullscreen}, "nvim", Fullscreen},
		{"split outside tmux is fullscreen", Env{Editor: "nvim", Window: Split}, "nvim", Fullscreen},
	}
	for _, tc := range cases {
		if got := tc.env.Program(); got != tc.editor {
			t.Errorf("%s: editor %q, want %q", tc.name, got, tc.editor)
		}
		if got := tc.env.Mode(); got != tc.mode {
			t.Errorf("%s: mode %q, want %q", tc.name, got, tc.mode)
		}
	}
}

func TestCmd(t *testing.T) {
	e := Env{Editor: "nvim", TMUX: "x", TmuxPane: "%3"}
	full := e.Cmd(Fullscreen, "/p/a.go", "/p", "edit a.go", "zemi-1", 12)
	if full.Path == "" || strings.Join(full.Args, " ") != `sh -c nvim +12 "$1" sh /p/a.go` {
		t.Fatalf("fullscreen args: %q", full.Args)
	}
	pop := e.Cmd(Popup, "/p/a.go", "/p", "edit a.go", "zemi-1", 0)
	if pop.Args[0] != "tmux" || pop.Args[1] != "display-popup" || pop.Args[len(pop.Args)-1] != "/p/a.go" || strings.Contains(strings.Join(pop.Args, " "), "+0") {
		t.Fatalf("popup args: %q", pop.Args)
	}
	split := e.Cmd(Split, "/p/a.go", "/p", "edit a.go", "zemi-1", 0)
	if split.Args[1] != "split-window" || !strings.Contains(strings.Join(split.Args, " "), "tmux wait-for -S zemi-1") {
		t.Fatalf("split args: %q", split.Args)
	}
	if got := script("code --wait", 5); got != `code --wait "$1"` {
		t.Fatalf("unknown editor must not get a line flag: %q", got)
	}
}
