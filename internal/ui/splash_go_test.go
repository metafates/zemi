package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestSplashShedsInOrder: a short chat drops the version, then the logo's
// art, and always keeps the tip; the row the pending work takes is reserved,
// so each drop happens one row earlier.
func TestSplashShedsInOrder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		h         int
		want, not []string
	}{
		{16, []string{"v1", "+#%#@%@%%#%#:", "● Tip"}, nil},
		{15, []string{"+#%#@%@%%#%#:", "● Tip"}, []string{"v1"}},
		{14, []string{"zemi", "● Tip"}, []string{"+#%#@%@%%#%#:"}},
	} {
		st := &hostapi.ScreenState{Width: 80, Height: c.h, Version: "v1", Model: "a/b", Trusted: true, Trustable: true, Now: time.Unix(1767225600, 0)}
		frame := strings.Join(rowTexts(splashRows(st)), "\n")
		for _, w := range c.want {
			if !strings.Contains(frame, w) {
				t.Errorf("%d rows: no %q\n%s", c.h, w, frame)
			}
		}
		for _, n := range c.not {
			if strings.Contains(frame, n) {
				t.Errorf("%d rows: %q\n%s", c.h, n, frame)
			}
		}
	}
}

// TestSplashTipPrecedence: the splash ends with the first tip that applies,
// the no-model hint wrapped under its text, and a tip that wraps moves
// nothing above it.
func TestSplashTipPrecedence(t *testing.T) {
	t.Parallel()
	hint := "export ANTHROPIC_API_KEY and start zemi again, or press enter on a provider in /model to paste its key"
	for _, c := range []struct {
		name string
		st   hostapi.ScreenState
		want string
	}{
		{"init.lua failed", hostapi.ScreenState{InitError: "/cfg/init.lua:12: boom"}, "● Tip  init.lua:12 failed: /errors"},
		{"no model", hostapi.ScreenState{ModelHint: hint}, "● Tip  No model yet: export ANTHROPIC_API_KEY and start zemi again, or\npress enter on a provider in /model to paste its key"},
		{"untrusted", hostapi.ScreenState{Model: "a/b", Trusted: false, Trustable: true}, "● Tip  /trust lets zemi read this project's instructions and run code"},
		{"at home", hostapi.ScreenState{Model: "a/b", Trusted: false, Trustable: false}, "● Tip  /btw  asks a side question the conversation does not keep"},
	} {
		st := c.st
		st.Width, st.Height, st.Now = 100, 31, time.Unix(1767225600, 0)
		frame := strings.Join(strings.Fields(strings.Join(rowTexts(splashRows(&st)), "\n")), " ")
		if !strings.Contains(frame, strings.Join(strings.Fields(c.want), " ")) {
			t.Errorf("%s: want %q\n%s", c.name, c.want, frame)
		}
	}
}

// rowTexts is each row's text, right-trimmed.
func rowTexts(rows [][]hostapi.Segment) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(hostapi.SegsText(r), " ")
	}
	return out
}
