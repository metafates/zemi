// chatflow_test.go pins the turn flip: every main-chat turn is a fresh call
// that opens on the view, the log dual-writes beside the old persist path,
// and zoom/date/note read and mark the eternal chat.

package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// sendTurns runs the turns a test names and returns every non-side prompt
// the stub saw.
func sendTurns(t *testing.T, h *Host, m *replyStubModel, texts ...string) [][]fantasy.Message {
	t.Helper()
	for _, text := range texts {
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.prompts)
}

// viewPart is a request's view message, the user message after the context.
func viewPart(t *testing.T, prompt []fantasy.Message) fantasy.Message {
	t.Helper()
	if len(prompt) < 3 {
		t.Fatalf("prompt has %d messages, want system, context, view, user", len(prompt))
	}
	return prompt[2]
}

// TestPendingLineNamesAndClips: a line whose node is unbuilt stands in with
// the message's kind (a tool's name too) and its own head, clipped to the
// line budget, so a huge row cannot bloat the view.
func TestPendingLineNamesAndClips(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	for _, c := range []struct {
		name string
		kind store.LogKind
		meta string
		text string
		want string
	}{
		{"a reply", store.KindAgent, "", "hello there", "agent: hello there"},
		{"a call", store.KindTool, `{"tool":"shell"}`, `{"command":"ls"}`, `tool shell: {"command":"ls"}`},
		{
			"a long row", store.KindUser, "", strings.Repeat("x", 4*_nodeBytes),
			"user: " + strings.Repeat("x", _nodeBytes-6) + "...",
		},
	} {
		seq, err := h.DB.AppendLog(c.kind, c.text, c.meta, "", "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := h.chat.pendingLine(seq); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestNoticesPersist: a recap, a goal verdict and every other status notice
// the chat draws carries a log row, so a resumed window reads the same rows
// back.
func TestNoticesPersist(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	if err := h.Note("chapter zero"); err != nil { // the log is not empty, as any chat a notice lands in
		t.Fatal(err)
	}
	h.notice("Recap: working on the import", "turn off in /config")
	h.notice("Goal met: the tests pass", "")
	rows, err := h.DB.LogTail(3)
	if err != nil || len(rows) != 3 ||
		rows[1].Kind != store.KindNotice || rows[1].Text != "Recap: working on the import" ||
		rows[2].Kind != store.KindNotice || rows[2].Text != "Goal met: the tests pass" {
		t.Fatalf("notice rows = %+v, %v", rows, err)
	}
	// a rebuilt window draws the same rows, detail included
	tr := logRows(nil, rows[1:])
	if len(tr) != 2 || tr[0].Kind != RowNotice || tr[0].Text != "Recap: working on the import" ||
		tr[0].Detail != "turn off in /config" || tr[1].Text != "Goal met: the tests pass" {
		t.Fatalf("rebuilt rows = %+v", tr)
	}
	// the live rows carry their seq from the append
	h.tMu.RLock()
	live := h.Session().Transcript
	h.tMu.RUnlock()
	if n := len(live); n < 3 || live[n-2].Seq != rows[1].Seq || live[n-1].Seq != rows[2].Seq {
		t.Fatalf("live rows unstamped: %+v", live[max(n-2, 0):])
	}
}

// TestBuilderRebuildsAfterTheWritersReset: the writer's /reset shrank the
// log under this builder's cursor; its next wake must rebuild and build the
// fresh seqs instead of stalling until restart.
func TestBuilderRebuildsAfterTheWritersReset(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	line := strings.Repeat("u", 600)
	for range 3 {
		if _, err := h.chat.User(line, ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 2); err != nil {
			return fmt.Errorf("pre-reset leaf 2 unbuilt: %w", err)
		}
		return nil
	})
	// a reset wipes the log under the builder; the fresh rows are new seqs
	if err := h.DB.ResetLog(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := h.chat.User("after the writer's reset", ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 1); err != nil {
			return fmt.Errorf("post-reset leaf 1 unbuilt: %w", err)
		}
		return nil
	})
	if m.calls.Load() == 0 {
		t.Fatal("no builder call after the writer's reset")
	}
}

// zoomCall runs the zoom tool once, the way a model call lands.
func zoomCall(t *testing.T, h *Host, input string) fantasy.ToolResponse {
	t.Helper()
	res, err := h.zoomTool().Run(t.Context(), fantasy.ToolCall{ID: "z", Name: "zoom", Input: input})
	if err != nil {
		t.Fatalf("zoom(%s): %v", input, err)
	}
	return res
}

// dateCall runs the date tool once.
func dateCall(t *testing.T, h *Host, input string) fantasy.ToolResponse {
	t.Helper()
	res, err := h.dateTool().Run(t.Context(), fantasy.ToolCall{ID: "d", Name: "date", Input: input})
	if err != nil {
		t.Fatalf("date(%s): %v", input, err)
	}
	return res
}

// TestZoomAndDateRoundTrip: zoom replays template leaves, verbatim leaves
// and paged long echoes, opens children, and every bad input is a tool
// error, never a panic.
func TestZoomAndDateRoundTrip(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "ok") // the children's model merge reads it
	if err := h.chat.Echo("shell", true, strings.Repeat("e", 20_000), StreamTool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.chat.User("short one", ""); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{strings.Repeat("u", 450), strings.Repeat("v", 450)} {
		if _, err := h.chat.User(line, ""); err != nil {
			t.Fatal(err)
		}
	}
	// the leaves' nodes land in their appends or by builder call
	waitFor(t, func() error {
		for seq := range int64(4) {
			if _, err := h.DB.NodeAt(0, seq); err != nil {
				return err
			}
		}
		return nil
	})
	t.Run("template echo leaf", func(t *testing.T) {
		res := zoomCall(t, h, `{"id":0,"n":1}`)
		if !strings.HasPrefix(res.Content, "echo shell: ") {
			t.Fatalf("zoom = %.80q", res.Content)
		}
	})
	t.Run("long echo pages", func(t *testing.T) {
		res := zoomCall(t, h, `{"id":0,"n":1}`)
		if !strings.Contains(res.Content, "(page 1 of 3") {
			t.Fatalf("page 1 ends %q", res.Content[max(0, len(res.Content)-80):])
		}
		next := zoomCall(t, h, `{"id":0,"n":1,"page":2}`)
		if next.Content == "" || strings.Contains(next.Content, "(page 1 of") {
			t.Fatalf("page 2 = %.80q", next.Content)
		}
		if res := zoomCall(t, h, `{"id":0,"n":1,"page":9}`); !res.IsError {
			t.Fatal("a page past the end is not a tool error")
		}
	})
	t.Run("verbatim leaf and children", func(t *testing.T) {
		if res := zoomCall(t, h, `{"id":1,"n":1}`); res.Content != "user: short one" {
			t.Fatalf("zoom = %q", res.Content)
		}
		// the pair joins into 2+2 once the builder merged them
		waitFor(t, func() error {
			_, err := h.DB.NodeAt(1, 1)
			return err
		})
		res := zoomCall(t, h, `{"id":2,"n":2}`)
		if !strings.Contains(res.Content, "2+1|") || !strings.Contains(res.Content, "3+1|") {
			t.Fatalf("children = %q", res.Content)
		}
	})
	t.Run("date gives the row's time", func(t *testing.T) {
		row, err := h.DB.LogAt(1)
		if err != nil {
			t.Fatal(err)
		}
		want := time.UnixMilli(row.CreatedAt).Format(time.RFC3339)
		if got := dateCall(t, h, `{"id":1}`).Content; got != want {
			t.Fatalf("date = %q, want %q", got, want)
		}
	})
	t.Run("n rounds up to a power of two", func(t *testing.T) {
		res := zoomCall(t, h, `{"id":0,"n":3}`)
		if res.IsError || !strings.Contains(res.Content, "0+2") {
			t.Fatalf("zoom(n 3) = %.60q, want the next power of two's merge", res.Content)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for _, c := range []struct {
			name, input string
		}{
			{"unknown id", `{"id":99}`},
			{"negative n", `{"id":0,"n":-2}`},
			{"line past the end", `{"id":3,"n":4}`},
			{"negative id", `{"id":-1}`},
		} {
			t.Run(c.name, func(t *testing.T) {
				if res := zoomCall(t, h, c.input); !res.IsError {
					t.Fatalf("zoom(%s) = %.60q, want a tool error", c.input, res.Content)
				}
			})
		}
		t.Run("date of an unknown id", func(t *testing.T) {
			if got := dateCall(t, h, `{"id":99}`).Content; !strings.Contains(got, "no message") {
				t.Fatalf("date = %q", got)
			}
		})
	})
}

// TestUserImagesZoomBack: a user message's images ride the log's parts_json
// and zoom replays them.
func TestUserImagesZoomBack(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	msg := fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "what is this"},
			fantasy.FilePart{Filename: "shot.png", MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		},
	}
	if err := h.persistMessage(msg); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.LogTail(1)
	if err != nil || len(rows) != 1 || rows[0].Kind != store.KindUser || rows[0].Text != "what is this" {
		t.Fatalf("user rows = %+v, %v", rows, err)
	}
	if rows[0].PartsJSON == "" {
		t.Fatal("the image did not ride the row")
	}
	res := zoomCall(t, h, `{"id":0,"n":1}`)
	if res.Type != "image" || res.MediaType != "image/png" {
		t.Fatalf("zoom replayed %s/%s, want the image", res.Type, res.MediaType)
	}
}

// TestLogHoldsTheThought: a reasoning stretch lands in the log as the thought
// row it drew, ahead of the reply, with the wall time its flush measured -
// a stretch no call follows included, whose flush never happens.
func TestLogHoldsTheThought(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	var calls atomic.Int64
	h.modelFn = func() (fantasy.LanguageModel, error) {
		m := &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			if calls.Add(1) == 1 {
				return append(reasoningParts("pondering the ask."), textParts("kept")...)
			}
			return textParts("done")
		}}
		return slowThought{LanguageModel: m, gap: 60 * time.Millisecond}, nil
	}
	h.SetUIHooks(&fakeUI{})
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	rows, err := h.DB.LogTail(3)
	if err != nil || len(rows) != 3 || rows[0].Kind != store.KindUser || rows[0].Text != "go" ||
		rows[1].Kind != store.KindThought || rows[1].Text != "pondering the ask." ||
		rows[2].Kind != store.KindAgent || rows[2].Text != "kept" {
		t.Fatalf("log rows = %+v, %v", rows, err)
	}
	var meta struct {
		MS int64 `json:"ms"`
	}
	if err := json.Unmarshal([]byte(rows[1].MetaJSON), &meta); err != nil || meta.MS < 50 {
		t.Fatalf("thought row meta = %q (%v), want the stretch's wall time", rows[1].MetaJSON, err)
	}
	tr := logRows(nil, rows)
	if len(tr) != 3 || tr[1].Kind != RowReasoning || tr[1].Text != "pondering the ask." ||
		tr[1].Stat == nil || tr[1].Stat.ThoughtMS < 50 || tr[1].Seq != rows[1].Seq {
		t.Fatalf("rebuilt rows = %+v", tr)
	}
}

// slowThought streams a model's parts with a wall gap before each one after
// the first, so a thought accrues time the way a streaming provider's does.
type slowThought struct {
	fantasy.LanguageModel

	gap time.Duration
}

func (m slowThought) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	resp, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	first := true
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range resp {
			if !first {
				time.Sleep(m.gap)
			}
			first = false
			if !yield(part) {
				return
			}
		}
	}, nil
}

// TestResumedWindowNamesADeadTail: a log whose last turn holds no reply and
// no error row is a window that closed on it — the boot rebuild ends with
// the death marker, the way an interrupt's "■ Interrupted" does; a tail
// whose turn answered draws none.
func TestResumedWindowNamesADeadTail(t *testing.T) {
	for _, c := range []struct {
		name   string
		seed   func(h *Host) error
		marker bool
	}{
		{"a bare prompt", func(h *Host) error {
			_, err := h.chat.User("go", "")
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
			return err
		}, true},
		{"a settled call with no reply", func(h *Host) error {
			if _, err := h.chat.User("go", ""); err != nil {
				return err
			}
			if err := h.chat.Tool("read", `{"path":"a.go"}`, ""); err != nil {
				return err
			}
			return h.chat.Echo("read", true, "data", StreamTool{})
		}, true},
		{"a turn that answered", func(h *Host) error {
			if _, err := h.chat.User("go", ""); err != nil {
				return err
			}
			return h.chat.Agent("The answer.")
		}, false},
		{"an interrupted turn", func(h *Host) error {
			if _, err := h.chat.User("go", ""); err != nil {
				return err
			}
			return h.chat.Error("Interrupted", true)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHostUI(t, &fakeClock{now: _testEpoch})
			h.SetUIHooks(&fakeUI{})
			if err := c.seed(h); err != nil {
				t.Fatal(err)
			}
			rows, err := h.DB.LogTail(_chatWindowRows)
			if err != nil {
				t.Fatal(err)
			}
			h.SetTranscript(logRows(nil, rows))
			h.markDeadTail()
			tr := h.Session().Transcript
			last := tr[len(tr)-1]
			if c.marker != (last.Kind == RowError && last.Text == "the window closed mid-turn") {
				t.Fatalf("the tail ends with %+v, marker wanted %v", last, c.marker)
			}
		})
	}
}

// TestTurnStatSkipsPromptWait: time the user spent at a prompt (ask, plan,
// question) is not billed to the model in the reply's stat.
func TestTurnStatSkipsPromptWait(t *testing.T) {
	clock := &fakeClock{now: _testEpoch}
	h := newTestHostUI(t, clock)
	h.SetUIHooks(&fakeUI{})
	if _, err := h.chat.User("go", ""); err != nil {
		t.Fatal(err)
	}
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
	h.agents.mu.Lock()
	h.agents.turnStart = _testEpoch
	h.agents.promptWait = 6 * time.Second
	h.agents.mu.Unlock()
	h.streamHooks().TextDelta("The answer.")
	clock.set(_testEpoch.Add(7500 * time.Millisecond))
	(&streamAdapter{h: h}).TurnDone(nil, fantasy.Usage{OutputTokens: 100})
	rows, err := h.DB.LogTail(_chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(nil, rows))
	tr := h.Session().Transcript
	stat := tr[len(tr)-1].Stat
	if stat == nil || stat.ElapsedMS != 1500 {
		t.Fatalf("the reply's stat billed the prompt wait: %+v", stat)
	}
}
