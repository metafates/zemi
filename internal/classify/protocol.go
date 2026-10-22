package classify

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/learn"
)

// Protocol is a provider that judges the state itself: zemi POSTs the state and
// the questions to `{base_url}/systemone` with the key as a bearer token, and
// the answers come back with real probabilities. The state and the questions
// never reach a chat prompt, which is what a decisions model is for.
type Protocol struct {
	BaseURL string
	APIKey  string
	Model   string
	Headers map[string]string
	// HTTP is the client the call goes through: the caller passes the one the
	// chat path uses, so the wait for response headers is bounded here too.
	HTTP *http.Client
}

// protocolPath is what the base URL is a base of.
const protocolPath = "/systemone"

// noul is the protocol's own name for a bool question, which is answered with
// the probability of `true` alone.
const noul = "noul"

// protocolRequest is one call's body.
type protocolRequest struct {
	Model     string                      `json:"model"`
	State     map[string]any              `json:"state"`
	Questions map[string]protocolQuestion `json:"questions"`
}

// protocolQuestion is one question on the wire: what the caller wrote, with a
// bool under the protocol's name for it. Criteria is a map of meanings for a
// bool or a choice and a list of levels for a score.
type protocolQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// protocolResponse is one call's answer: one answer per question, and the
// counts the provider billed.
type protocolResponse struct {
	Answers map[string]protocolAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// protocolAnswer is one question's answer, in the shape its type names.
type protocolAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
	Score         *float64           `json:"score"`
}

// protocolAsk sends the state and the questions and validates every answer
// against its question.
func protocolAsk(ctx context.Context, p Protocol, req Request) (Result, error) {
	body := protocolRequest{Model: p.Model, State: req.State, Questions: make(map[string]protocolQuestion, len(req.Questions))}
	for name, q := range req.Questions {
		wire := protocolQuestion{Type: string(q.Type), Instructions: q.Instructions}
		switch q.Type {
		case TypeBool:
			wire.Type, wire.Criteria = noul, q.Criteria
		case TypeChoice:
			wire.Criteria = q.Criteria
		case TypeScore:
			wire.Criteria = q.Levels
		}
		body.Questions[name] = wire
	}
	raw, err := json.Marshal(body, json.Deterministic(true))
	if err != nil {
		return Result{}, err
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + protocolPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		body := learn.Truncate(string(data), 200)
		if body != string(data) {
			body += "..."
		}
		return Result{}, fmt.Errorf("%s: %s: %s", url, resp.Status, body)
	}
	var out protocolResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Result{}, fmt.Errorf("%s: %w", url, err)
	}
	answers, err := protocolAnswers(req.Questions, out.Answers)
	// the request was billed whatever its answers were worth
	res := Result{
		Strategy: StrategyProtocol,
		Answers:  answers,
		Usage:    fantasy.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens},
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// protocolAnswers checks one answer per question: a missing one, one of
// another type, a label the question does not have, a probability outside 0..1
// and a score outside its levels are all errors.
func protocolAnswers(questions map[string]Question, got map[string]protocolAnswer) (map[string]Answer, error) {
	out := make(map[string]Answer, len(questions))
	for name, q := range questions {
		a, ok := got[name]
		if !ok {
			return nil, fmt.Errorf("the provider answered no %q", name)
		}
		answer, err := a.answer(q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = answer
	}
	return out, nil
}

// answer is one wire answer as the question asked for it.
func (a protocolAnswer) answer(q Question) (Answer, error) {
	switch q.Type {
	case TypeBool:
		if a.Type != noul {
			return Answer{}, fmt.Errorf("want a %s answer, got type %q", noul, a.Type)
		}
		if a.Noul == nil {
			return Answer{}, fmt.Errorf("the %s answer carries no probability", noul)
		}
		p := *a.Noul
		if err := probability(p); err != nil {
			return Answer{}, err
		}
		return Answer{Type: TypeBool, Value: p > 0.5, Probability: &p}, nil
	case TypeChoice:
		if a.Type != string(TypeChoice) {
			return Answer{}, fmt.Errorf("want a choice answer, got type %q", a.Type)
		}
		if err := q.checkChoice(a.Choice); err != nil {
			return Answer{}, err
		}
		probs, err := probabilities(q.labels(), a.Probabilities)
		if err != nil {
			return Answer{}, err
		}
		return Answer{Type: TypeChoice, Choice: a.Choice, Probabilities: probs, Confidence: confidence(slices.Collect(maps.Values(probs)))}, nil
	case TypeScore:
		if a.Type != string(TypeScore) {
			return Answer{}, fmt.Errorf("want a score answer, got type %q", a.Type)
		}
		if a.Score == nil {
			return Answer{}, fmt.Errorf("the score answer carries no level")
		}
		if err := q.checkScore(*a.Score); err != nil {
			return Answer{}, err
		}
		if a.Confidence != nil {
			if err := probability(*a.Confidence); err != nil {
				return Answer{}, err
			}
			return Answer{Type: TypeScore, Score: a.Score, Confidence: a.Confidence}, nil
		}
		return Answer{Type: TypeScore, Score: a.Score}, nil
	}
	return Answer{}, fmt.Errorf("unknown question type %q", q.Type)
}

// probabilities checks a label-to-probability map: only the question's labels,
// each within 0..1. Empty where the provider sent none.
func probabilities(labels []string, got map[string]float64) (map[string]float64, error) {
	if len(got) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(got))
	for _, l := range labels {
		p, ok := got[l]
		if !ok {
			continue
		}
		if err := probability(p); err != nil {
			return nil, fmt.Errorf("%s: %w", l, err)
		}
		out[l] = p
	}
	for _, l := range sortedKeys(got) {
		if _, ok := out[l]; !ok {
			return nil, fmt.Errorf("answered a probability for %q, which is not one of the labels", l)
		}
	}
	return out, nil
}

func probability(p float64) error {
	if p < 0 || p > 1 {
		return fmt.Errorf("probability %v is outside 0..1", p)
	}
	return nil
}

// confidence is how one-sided a distribution is: 1 for a unanimous answer, 0
// for a tie over n labels, absent when fewer than two answered with a
// probability.
func confidence(probs []float64) *float64 {
	n := len(probs)
	if n < 2 {
		return nil
	}
	c := (float64(n)*slices.Max(probs) - 1) / float64(n-1)
	c = min(1, max(0, c))
	return &c
}

// sortedKeys is a string map's keys, sorted, so a strategy's order is the same
// on every call.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
