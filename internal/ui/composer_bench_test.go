package ui

import (
	"strings"
	"testing"
)

// BenchmarkWrapComposer pins the draft rewrap the composer pays on every
// cursor move: the tooling audit wanted it measured before any width-cache
// fix, at the maxDraftChars cap in both plain ASCII and wide (CJK) runes.
func BenchmarkWrapComposer(b *testing.B) {
	for _, bc := range []struct {
		name string
		text string
	}{
		{"ascii", strings.Repeat("quick brown fox jumps over the lazy dog ", 512)[:_maxDraftChars]},
		{"cjk", strings.Repeat("语言涉及很多细节", _maxDraftChars/len([]rune("语言涉及很多细节"))+1)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			app, _ := bootAgentApp(b, 120, 40)
			app.composer.text = bc.text
			app.composer.cursor = len([]rune(bc.text)) / 2
			b.ResetTimer()
			for b.Loop() {
				app.wrapKey.text = "\x00bail" // the cache off: the uncapped worst case
				app.wrapComposer(80)
			}
		})
	}
}
