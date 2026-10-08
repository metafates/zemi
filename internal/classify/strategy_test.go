package classify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// fakeOpts is a provider options value, so a call's options are its own.
type fakeOpts struct{ depth int64 }

func (*fakeOpts) Options()                   {}
func (*fakeOpts) UnmarshalJSON([]byte) error { return nil }
func (o *fakeOpts) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"depth":%d}`, o.depth), nil
}

// chatOf is a chat request on m, whose provider reports token probabilities:
// every readout depth the ladder asks for is recorded.
func chatOf(m *fakeModel, lps ...TokenLogprob) Chat {
	if len(lps) > 0 {
		m.replies = []tokenLogprobs{{metadata: logprobs(lps...)}}
	}
	return Chat{
		Model: m,
		Logprobs: func(k int64) fantasy.ProviderOptions {
			m.depth = append(m.depth, k)
			return fantasy.ProviderOptions{"fake": &fakeOpts{k}}
		},
	}
}

// TestLogprobsAsk: a bool, a choice and a score read off the next-token
// probabilities, and the label softmax is the answer, not the text.
func TestLogprobsAsk(t *testing.T) {
	m := &fakeModel{}
	chat := chatOf(m, TokenLogprob{"Yes", -0.2}, TokenLogprob{"No", -2.5})
	res, err := Ask(t.Context(), Request{State: map[string]any{"cmd": "ls"}, Questions: one("danger"), Chat: &chat})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != StrategyLogprobs {
		t.Errorf("strategy = %q", res.Strategy)
	}
	a := res.Answers["danger"]
	if !a.Value || a.Probability == nil || math.Abs(*a.Probability-0.909) > 0.01 {
		t.Errorf("danger = %+v, want P(true) about 0.91", a)
	}
	if res.Usage != (Usage{InputTokens: 10, OutputTokens: 1}) {
		t.Errorf("usage = %+v", res.Usage)
	}

	// a choice is the label with the most probability, and its letters are the
	// question's labels in sorted order
	m = &fakeModel{}
	chat = chatOf(m,
		TokenLogprob{"C", -0.1}, TokenLogprob{"B", -2.0}, TokenLogprob{"A", -3.0},
	)
	res, err = Ask(t.Context(), Request{State: map[string]any{}, Questions: one("action"), Chat: &chat})
	if err != nil {
		t.Fatal(err)
	}
	a = res.Answers["action"]
	if a.Choice != "deny" { // C is the third sorted label: allow, ask, deny
		t.Errorf("choice = %q, want deny (C is the third sorted label)", a.Choice)
	}
	if a.Probabilities["allow"] <= 0 || a.Confidence == nil || *a.Confidence <= 0.5 {
		t.Errorf("action = %+v", a)
	}

	// a score is the probability-weighted level
	m = &fakeModel{}
	chat = chatOf(m, TokenLogprob{"1", -0.1}, TokenLogprob{"0", -3.0}, TokenLogprob{"2", -3.0})
	res, err = Ask(t.Context(), Request{State: map[string]any{}, Questions: one("risk"), Chat: &chat})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Answers["risk"].Score; s == nil || math.Abs(*s-1) > 0.01 {
		t.Errorf("score = %v, want about 1", s)
	}
}

// TestLogprobsEscalates: a label the readout left out is asked for again
// deeper, and a provider that never returns it is an error.
func TestLogprobsEscalates(t *testing.T) {
	m := &fakeModel{replies: []tokenLogprobs{
		{metadata: logprobs(TokenLogprob{"No", -0.1})},
		{metadata: logprobs(TokenLogprob{"Yes", -0.1}, TokenLogprob{"No", -1.2})},
	}}
	chat := chatOf(m)
	res, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Chat: &chat})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.depth; len(got) != 2 || got[0] != readoutDepth[0] || got[1] != readoutDepth[1] {
		t.Errorf("depths asked for = %v, want %v", got, readoutDepth[:2])
	}
	if a := res.Answers["danger"]; !a.Value {
		t.Errorf("danger = %+v, want true from the deeper readout", a)
	}

	// no readout ever has it: an error naming the label, not a default
	m = &fakeModel{replies: []tokenLogprobs{{metadata: logprobs(TokenLogprob{"Maybe", -0.1})}}}
	chat = chatOf(m)
	_, err = Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Chat: &chat})
	if err == nil || !strings.Contains(err.Error(), "the provider returned no probability for Yes, No") {
		t.Errorf("err = %v, want the missing labels named", err)
	}
	if len(m.depth) != len(readoutDepth) {
		t.Errorf("depths asked for = %v, want each of %v", m.depth, readoutDepth)
	}
}

// TestSchemaAsk: the last resort decodes each question's type, and a value a
// question did not ask for is an error.
func TestSchemaAsk(t *testing.T) {
	m := &fakeModel{object: map[string]any{"danger": true, "action": "deny", "risk": float64(2)}}
	chat := Chat{Model: m}
	res, err := Ask(t.Context(), Request{State: map[string]any{"cmd": "rm -rf"}, Questions: questions, Chat: &chat})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != StrategySchema {
		t.Errorf("strategy = %q", res.Strategy)
	}
	if a := res.Answers["danger"]; !a.Value || a.Probability != nil {
		t.Errorf("danger = %+v, want a value and no probability", a)
	}
	if a := res.Answers["action"]; a.Choice != "deny" || a.Confidence != nil {
		t.Errorf("action = %+v", a)
	}
	if a := res.Answers["risk"]; a.Score == nil || *a.Score != 2 {
		t.Errorf("risk = %+v", a)
	}
	if len(m.got) != 1 {
		t.Fatalf("the schema strategy made %d calls, want 1", len(m.got))
	}
	schema := m.got[0].Schema
	if schema.Type != "object" || len(schema.Required) != 3 {
		t.Errorf("schema = %+v, want one required entry per question", schema)
	}
	if s := schema.Properties["action"]; s.Type != "string" || len(s.Enum) != 3 {
		t.Errorf("action's schema = %+v, want an enum of its labels", s)
	}
	if s := schema.Properties["risk"]; s.Type != "integer" || s.Maximum == nil || *s.Maximum != 2 {
		t.Errorf("risk's schema = %+v, want an integer within its levels", s)
	}

	for _, c := range []struct {
		name   string
		object map[string]any
		want   string
	}{
		{"a value of another type", map[string]any{"danger": "yes", "action": "deny", "risk": float64(1)}, "danger: answered a string, want a boolean"},
		{"a label the question has not", map[string]any{"danger": true, "action": "maybe", "risk": float64(1)}, `action: answered "maybe"`},
		{"a score outside its levels", map[string]any{"danger": true, "action": "deny", "risk": float64(9)}, "risk: score 9 is outside 0..2"},
		{"a question left out", map[string]any{"danger": true}, "the provider answered no"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &fakeModel{object: c.object}
			chat := Chat{Model: m}
			_, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: questions, Chat: &chat})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// TestAskLadder: a provider with no probabilities skips to the schema
// strategy, one whose labels never come back falls through to it, and one that
// fails both is an error - never a default answer.
func TestAskLadder(t *testing.T) {
	// no logprobs at all: the strategy is skipped, not attempted
	m := &fakeModel{object: map[string]any{"danger": true}}
	res, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Chat: &Chat{Model: m}})
	if err != nil || res.Strategy != StrategySchema {
		t.Fatalf("strategy = %q, err = %v", res.Strategy, err)
	}
	if m.calls != 1 {
		t.Errorf("the model was called %d times, want only the object call", m.calls)
	}

	// the logprobs strategy runs and fails: the schema strategy still answers
	m = &fakeModel{object: map[string]any{"danger": true}, replies: []tokenLogprobs{{metadata: logprobs(TokenLogprob{"Maybe", -1})}}}
	chat := chatOf(m)
	res, err = Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Chat: &chat})
	if err != nil || res.Strategy != StrategySchema {
		t.Fatalf("strategy = %q, err = %v", res.Strategy, err)
	}

	// both fail: the error names both, nothing is answered, and what the
	// abandoned attempts used is still reported
	m = &fakeModel{objectErr: errors.New("no object either"), replies: []tokenLogprobs{{metadata: logprobs(TokenLogprob{"Maybe", -1})}}}
	chat = chatOf(m)
	res, err = Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Chat: &chat})
	if err == nil || !strings.Contains(err.Error(), "no probability for Yes, No") || !strings.Contains(err.Error(), "no object either") {
		t.Errorf("err = %v, want both strategies' errors", err)
	}
	if want := (Usage{InputTokens: int64(len(readoutDepth)) * 10, OutputTokens: int64(len(readoutDepth))}); res.Usage != want {
		t.Errorf("usage = %+v, want %+v from the abandoned readouts", res.Usage, want)
	}

	// a reply that arrived and did not decode was billed too
	m = &fakeModel{objectErr: &fantasy.NoObjectGeneratedError{RawText: "not json", Usage: fantasy.Usage{InputTokens: 7, OutputTokens: 2}}}
	res, err = Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Chat: &Chat{Model: m}})
	if err == nil || res.Usage != (Usage{InputTokens: 7, OutputTokens: 2}) {
		t.Errorf("usage = %+v, err = %v; want the object call's usage", res.Usage, err)
	}

	// nothing to answer with at all
	if _, err := Ask(context.Background(), Request{State: map[string]any{}, Questions: one("danger")}); err == nil {
		t.Error("a call with no model answered")
	}
}

// TestQuestionLabelsChecked: a question no strategy could answer is refused
// before a call.
func TestQuestionLabelsChecked(t *testing.T) {
	m := &fakeModel{}
	chat := chatOf(m)
	for _, c := range []struct {
		name string
		q    Question
		want string
	}{
		{"a choice of one label", Question{Type: TypeChoice, Criteria: map[string]string{"only": "x"}}, "a choice question needs 2 to 26 labels"},
		{"a score of one level", Question{Type: TypeScore, Levels: []string{"only"}}, "a score question needs 2 to 10 levels"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: map[string]Question{"q": c.q}, Chat: &chat})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}
