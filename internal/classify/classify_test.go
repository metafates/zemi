package classify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	openaisdk "github.com/charmbracelet/openai-go"
)

// server answers one classification the way a decisions provider does, and
// reports what it was sent.
func server(t *testing.T, reply string) (*Protocol, func() string) {
	t.Helper()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		seen = string(body)
		if got := r.Header.Get("authorization"); got != "Bearer k" {
			t.Errorf("authorization = %q, want the bearer key", got)
		}
		if !strings.HasSuffix(r.URL.Path, protocolPath) {
			t.Errorf("posted to %s, want %s", r.URL.Path, protocolPath)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return &Protocol{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTP: srv.Client()}, func() string { return seen }
}

// questions is the three question types, one of each.
var questions = map[string]Question{
	"danger": {Type: TypeBool, Instructions: "is it dangerous", Criteria: map[string]string{"true": "it deletes work", "false": "it is a build"}},
	"action": {Type: TypeChoice, Instructions: "what does it do", Criteria: map[string]string{"deny": "refuses", "allow": "permits", "ask": "asks"}},
	"risk":   {Type: TypeScore, Instructions: "how risky", Levels: []string{"none", "some", "lots"}},
}

// TestProtocolAsk: the wire body maps a bool to the protocol's own type, and
// every answer comes back as its question asked for it.
func TestProtocolAsk(t *testing.T) {
	p, sent := server(t, `{"answers": {
		"danger": {"type": "noul", "noul": 0.92},
		"action": {"type": "choice", "choice": "deny", "probabilities": {"allow": 0.05, "deny": 0.9, "ask": 0.05}, "confidence": 0.85},
		"risk": {"type": "score", "score": 2, "confidence": 0.7}
	}, "usage": {"input_tokens": 120, "output_tokens": 8}}`)
	res, err := Ask(t.Context(), Request{State: map[string]any{"cmd": "rm -rf build"}, Questions: questions, Protocol: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != StrategyProtocol {
		t.Errorf("strategy = %q", res.Strategy)
	}
	if a := res.Answers["danger"]; !a.Value || a.Probability == nil || *a.Probability != 0.92 {
		t.Errorf("danger = %+v", a)
	}
	if a := res.Answers["action"]; a.Choice != "deny" || a.Probabilities["deny"] != 0.9 || a.Confidence == nil {
		t.Errorf("action = %+v", a)
	}
	if a := res.Answers["risk"]; a.Score == nil || *a.Score != 2 || a.Confidence == nil {
		t.Errorf("risk = %+v", a)
	}
	if res.Usage != (Usage{InputTokens: 120, OutputTokens: 8}) {
		t.Errorf("usage = %+v", res.Usage)
	}
	body := sent()
	for _, want := range []string{
		`"questions":{`,
		`"type":"noul"`,
		`"instructions":"is it dangerous"`,
		`"it deletes work"`,
		`"it is a build"`,
		`"criteria":["none","some","lots"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the request does not carry %s: %s", want, body)
		}
	}
}

// one is a single question of the set above, so a reply that leaves another
// out does not answer for it.
func one(name string) map[string]Question { return map[string]Question{name: questions[name]} }

// TestProtocolAnswersRejected: every answer that does not fit its question is
// an error, never a nearest match.
func TestProtocolAnswersRejected(t *testing.T) {
	for _, c := range []struct {
		name, reply, want string
		questions         map[string]Question
	}{
		{"an answer outside 0..1", `{"answers": {"danger": {"type": "noul", "noul": 1.4}}}`, "probability 1.4 is outside 0..1", nil},
		{"an answer of another type", `{"answers": {"danger": {"type": "bool", "noul": 0.5}}}`, `want a noul answer, got type "bool"`, nil},
		{"a label the question has not", `{"answers": {"action": {"type": "choice", "choice": "maybe"}}}`, `answered "maybe", which is not one of allow, ask, deny`, one("action")},
		{"a probability for a label it has not", `{"answers": {"action": {"type": "choice", "choice": "deny", "probabilities": {"deny": 0.6, "later": 0.4}}}}`, `answered a probability for "later"`, one("action")},
		{"a score outside its levels", `{"answers": {"risk": {"type": "score", "score": 7}}}`, "score 7 is outside 0..2", one("risk")},
		{"no answer at all", `{"answers": {}}`, `the provider answered no "danger"`, one("danger")},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := server(t, c.reply)
			qs := c.questions
			if qs == nil {
				qs = one("danger")
			}
			_, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: qs, Protocol: p})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	// a body that is not JSON, and a status a provider would send
	p, _ := server(t, "not json")
	if _, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Protocol: p}); err == nil {
		t.Error("a body that is not JSON passed")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("no key"))
	}))
	t.Cleanup(bad.Close)
	_, err := Ask(t.Context(), Request{State: map[string]any{}, Questions: one("danger"), Protocol: &Protocol{BaseURL: bad.URL}})
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "no key") {
		t.Errorf("err = %v, want the provider's status and body", err)
	}
}

// tokenLogprobs is a reply's log probabilities for the next token, in the shape
// a provider reports: the token it gave and its alternatives.
type tokenLogprobs struct {
	metadata fantasy.ProviderMetadata
	err      error
}

// fakeModel answers Generate with one logprob distribution and GenerateObject
// with object, counting the calls and the depth the last readout asked for.
type fakeModel struct {
	depth     []int64
	replies   []tokenLogprobs
	object    any
	objectErr error
	got       []fantasy.ObjectCall
	calls     int
}

func (m *fakeModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	i := min(m.calls, len(m.replies)-1)
	m.calls++
	if m.replies[i].err != nil {
		return nil, m.replies[i].err
	}
	return &fantasy.Response{
		Content:          fantasy.ResponseContent{fantasy.TextContent{Text: "Yes"}},
		FinishReason:     fantasy.FinishReasonStop,
		Usage:            fantasy.Usage{InputTokens: 10, OutputTokens: 1},
		ProviderMetadata: m.replies[i].metadata,
	}, nil
}

func (m *fakeModel) GenerateObject(_ context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.calls++
	m.got = append(m.got, call)
	if m.objectErr != nil {
		return nil, m.objectErr
	}
	return &fantasy.ObjectResponse{Object: m.object, RawText: "{}", Usage: fantasy.Usage{InputTokens: 20, OutputTokens: 4}}, nil
}

func (m *fakeModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, fmt.Errorf("no stream")
}

func (m *fakeModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("no stream")
}

func (m *fakeModel) Provider() string { return "fake" }
func (m *fakeModel) Model() string    { return "m" }

// logprobs is one next-token readout: the token the model gave first, then the
// alternatives beside it, in the provider's own metadata shape.
func logprobs(top ...TokenLogprob) fantasy.ProviderMetadata {
	m := &openai.ProviderMetadata{}
	if len(top) > 0 {
		first := openaisdk.ChatCompletionTokenLogprob{Token: top[0].Token, Logprob: top[0].Logprob}
		for _, t := range top[1:] {
			first.TopLogprobs = append(first.TopLogprobs, openaisdk.ChatCompletionTokenLogprobTopLogprob{Token: t.Token, Logprob: t.Logprob})
		}
		m.Logprobs = []openaisdk.ChatCompletionTokenLogprob{first}
	}
	return fantasy.ProviderMetadata{openai.Name: m}
}
