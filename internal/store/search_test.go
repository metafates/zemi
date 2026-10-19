package store

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// partsJSON is the stored shape of a message with these text parts.
func partsJSON(t *testing.T, role string, texts ...string) string {
	t.Helper()
	type part struct {
		Type string `json:"type"`
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	m := struct {
		Role    string `json:"role"`
		Content []part `json:"content"`
	}{Role: role}
	for _, s := range texts {
		p := part{Type: "text"}
		p.Data.Text = s
		m.Content = append(m.Content, p)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func addMessage(t *testing.T, db *DB, sid, role, parts string) {
	t.Helper()
	if err := db.AddMessage(&Message{SessionID: sid, Role: role, PartsJSON: parts}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
}

// seedSearch is one store: a session with a user and an assistant hit (plus a
// tool row, a compaction tail copy and a reasoning part around them), a
// session whose hits a per-session cap cuts to five, and a subagent run.
func seedSearch(t *testing.T) *DB {
	t.Helper()
	db := openTest(t)
	for _, s := range []struct{ id, parent, title string }{
		{"aaaa", "", "Postgres tuning"},
		{"bbbb", "", "Планировщик"},
		{"cccc", "aaaa", "subagent run"},
	} {
		if err := db.CreateSession(s.id, s.parent, s.title, ""); err != nil {
			t.Fatal(err)
		}
	}
	addMessage(t, db, "aaaa", "user", partsJSON(t, "user", "Why does Postgres pick a seq scan here?"))
	addMessage(t, db, "aaaa", "assistant", partsJSON(t, "assistant", "Postgres planner estimates rows badly for a skewed column."))
	addMessage(t, db, "aaaa", "tool", partsJSON(t, "tool", "psql says Postgres chose a seq scan"))
	addMessage(t, db, "aaaa", "tail", partsJSON(t, "user", "Postgres summary, copied for the tail"))
	addMessage(t, db, "aaaa", "assistant", `{"role":"assistant","content":[{"type":"reasoning","data":{"text":"Postgres reasoning"}}]}`)
	for i := range 7 {
		addMessage(t, db, "bbbb", "user", partsJSON(t, "user", fmt.Sprintf("Планировщик выбирает seq scan %d", i)))
	}
	addMessage(t, db, "cccc", "user", partsJSON(t, "user", "the subagent looked at Postgres too"))
	return db
}

// TestSearchMessages: one query per store finds text the way the picker needs
// it: every word ANDed, case folded past ASCII, only what a human wrote and
// only top-level sessions, the newest snippets kept with all hits counted.
func TestSearchMessages(t *testing.T) {
	db := seedSearch(t)
	for _, c := range []struct {
		name    string
		words   []string
		perSess int
		maxRows int
		want    []string // session id per row, in order
		counts  []int    // the hit count each row carries
		first   string   // a substring of the first row's text
	}{
		{
			name: "a word finds both roles of a session", words: []string{"Postgres"}, perSess: 5, maxRows: 50,
			want: []string{"aaaa", "aaaa"}, counts: []int{2, 2}, first: "Postgres planner",
		},
		{
			name: "every word must match", words: []string{"postgres", "PLANNER"}, perSess: 5, maxRows: 50,
			want: []string{"aaaa"}, counts: []int{1}, first: "planner estimates",
		},
		{
			name: "a word no row has", words: []string{"sqlite"}, perSess: 5, maxRows: 50,
		},
		{
			name: "the per-session cap keeps the newest, the count sees all", words: []string{"планировщик"}, perSess: 5, maxRows: 50,
			want: []string{"bbbb", "bbbb", "bbbb", "bbbb", "bbbb"}, counts: []int{7, 7, 7, 7, 7}, first: "seq scan 6",
		},
		{
			name: "maxRows stops the whole result", words: []string{"postgres"}, perSess: 5, maxRows: 1,
			want: []string{"aaaa"}, counts: []int{2},
		},
		{name: "no words is no query", perSess: 5, maxRows: 50},
	} {
		t.Run(c.name, func(t *testing.T) {
			hits, err := db.SearchMessages(c.words, c.perSess, c.maxRows)
			if err != nil {
				t.Fatalf("SearchMessages: %v", err)
			}
			got := make([]string, len(hits))
			counts := make([]int, len(hits))
			for i, h := range hits {
				got[i], counts[i] = h.SessionID, h.Count
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("sessions = %v, want %v", got, c.want)
			}
			if !slices.Equal(counts, c.counts) {
				t.Fatalf("counts = %v, want %v", counts, c.counts)
			}
			if len(hits) > 0 && !strings.Contains(hits[0].Text, c.first) {
				t.Fatalf("first text = %q, want %q in it", hits[0].Text, c.first)
			}
		})
	}
}
