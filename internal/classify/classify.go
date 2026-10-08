// Package classify judges data with a model: a question in, a validated
// answer out, through whichever strategy the model's provider answers with
// (DESIGN §6.3). Nothing here parses a sentence: a strategy either returns
// typed answers or an error, and an answer a question did not ask for is an
// error, never a default.
package classify

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

// Strategy is how a call was answered.
type Strategy string

const (
	// StrategyProtocol is a provider that judges the state itself: it is sent
	// the state and the questions and answers them with real probabilities.
	StrategyProtocol Strategy = "protocol"
	// StrategyLogprobs is a chat model whose provider returns token
	// probabilities: each question is one prompt whose answer is a single
	// label, read as a softmax over the label tokens.
	StrategyLogprobs Strategy = "logprobs"
	// StrategySchema is a chat model that returns no probabilities for its
	// answers: the questions go as one schema and the reply is decoded against
	// it. It carries no confidence.
	StrategySchema Strategy = "schema"
)

// Type is what a question's answer is.
type Type string

const (
	TypeBool   Type = "bool"
	TypeChoice Type = "choice"
	TypeScore  Type = "score"
)

// Question is one thing to judge about the state.
type Question struct {
	Type         Type
	Instructions string
	// Criteria is a bool's `true` and `false` meanings, or a choice's labels,
	// each with what it means.
	Criteria map[string]string
	// Levels are a score's levels, lowest first; the answer is the one it
	// lands on, 0-based.
	Levels []string
}

// labels are a choice's labels in the order the strategies map them: sorted,
// so a label's letter is the same on every call.
func (q Question) labels() []string {
	return sortedKeys(q.Criteria)
}

// max is a score's highest level.
func (q Question) max() float64 { return float64(len(q.Levels) - 1) }

// Answer is one question's answer, in the shape its type asks for: Value and
// Probability for a bool, Choice and Probabilities for a choice, Score for a
// score. Whatever the strategy had no number for is absent.
type Answer struct {
	Type Type
	// Value is a bool answer; Probability is P(true) where a probability
	// answered it.
	Value       bool
	Probability *float64
	// Choice is a choice answer, one of the question's labels; Probabilities
	// is what each label scored where probabilities answered it.
	Choice        string
	Probabilities map[string]float64
	// Score is a score answer, a level (0-based, fractional where it is a
	// probability-weighted one).
	Score *float64
	// Confidence is how one-sided the answer was, 1 for a unanimous one and 0
	// for a tie; absent where no probability answered it.
	Confidence *float64
}

// Result is what a call answered. Usage is what the call cost even when it
// failed: the strategies a later one replaced were billed too.
type Result struct {
	Strategy Strategy
	Answers  map[string]Answer
	Usage    Usage
}

// Usage is what a call used.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

func (u *Usage) add(v Usage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
}

// Chat is the chat model the last two strategies run on, and what the caller
// knows about its provider.
type Chat struct {
	Model fantasy.LanguageModel
	// Logprobs are the provider options that ask for the top k token
	// probabilities of the next token, nil when this provider returns none:
	// the ladder then skips that strategy rather than guessing.
	Logprobs func(k int64) fantasy.ProviderOptions
	// Options ride every call (a gateway's routing, the session's affinity).
	Options fantasy.ProviderOptions
	// Temperature is what the decoded strategies ask with, 0 when the caller
	// pinned it. A probability readout leaves it alone: the provider reports
	// the distribution its own sampling produced, and a greedy one is a
	// one-hot that says nothing about the labels.
	Temperature *float64
}

// Request is one classify call: what to judge, what to ask, and how to reach a
// model. Exactly one of Chat and Protocol is set, since a decisions model is
// never a chat model.
type Request struct {
	State     map[string]any
	Questions map[string]Question
	Chat      *Chat
	Protocol  *Protocol
}

// Ask runs the questions through the ladder and returns the first strategy
// that answered. A strategy the model cannot answer through is skipped, one
// that failed does not stop the next, and when the last one fails the call is
// an error: a caller that cannot get a verdict asks the user. What every
// attempt used is in the result either way, since every attempt was billed.
func Ask(ctx context.Context, req Request) (Result, error) {
	var used Usage
	var errs []error
	done := func(res Result, err error) (Result, error) {
		used.add(res.Usage)
		res.Usage = used
		return res, err
	}
	if req.Protocol != nil {
		res, err := protocolAsk(ctx, *req.Protocol, req)
		if res, err = done(res, err); err == nil {
			return res, nil
		}
		errs = append(errs, err)
	}
	if req.Chat != nil {
		if req.Chat.Logprobs != nil {
			res, err := logprobsAsk(ctx, *req.Chat, req)
			if res, err = done(res, err); err == nil {
				return res, nil
			}
			errs = append(errs, err)
		}
		res, err := schemaAsk(ctx, *req.Chat, req)
		if res, err = done(res, err); err == nil {
			return res, nil
		}
		errs = append(errs, err)
	}
	return Result{Usage: used}, fmt.Errorf("no strategy answered: %w", errors.Join(errs...))
}
