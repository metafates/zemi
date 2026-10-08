package classify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// schemaAsk is the last resort: the questions go as one schema, the provider
// makes the model fill it (its own ladder: native JSON mode, a forced tool
// call, or the schema written into the prompt) and fantasy decodes the reply
// against it. It carries no probabilities, so no answer gets a confidence.
func schemaAsk(ctx context.Context, chat Chat, req Request) (Result, error) {
	props := make(map[string]*fantasy.Schema, len(req.Questions))
	for name, q := range req.Questions {
		s, err := q.schema()
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", name, err)
		}
		props[name] = &s
	}
	res, err := chat.Model.GenerateObject(ctx, fantasy.ObjectCall{
		Prompt:      []fantasy.Message{fantasy.NewSystemMessage(systemPrompt), fantasy.NewUserMessage(schemaPrompt(req))},
		Schema:      fantasy.Schema{Type: "object", Properties: props, Required: sortedKeys(req.Questions)},
		Temperature: chat.Temperature,
		Headers:     chat.Headers,
		// the schema description reaches a tool call and a prompt alike
		SchemaDescription: "one answer per question, keyed by the question's name",
		ProviderOptions:   chat.Options,
	})
	if err != nil {
		// a reply that arrived and did not decode was billed too
		none, ok := errors.AsType[*fantasy.NoObjectGeneratedError](err)
		if !ok {
			return Result{Strategy: StrategySchema}, err
		}
		return Result{Strategy: StrategySchema, Usage: usageOf(none.Usage)}, err
	}
	obj, ok := res.Object.(map[string]any)
	if !ok {
		return Result{Strategy: StrategySchema, Usage: usageOf(res.Usage)}, fmt.Errorf("the provider answered a %T, want an object", res.Object)
	}
	answers, err := schemaAnswers(req.Questions, obj)
	if err != nil {
		return Result{Strategy: StrategySchema, Usage: usageOf(res.Usage)}, err
	}
	return Result{
		Strategy: StrategySchema,
		Answers:  answers,
		Usage:    usageOf(res.Usage),
	}, nil
}

// schema is the value a question's type asks for.
func (q Question) schema() (fantasy.Schema, error) {
	if err := q.Check(); err != nil {
		return fantasy.Schema{}, err
	}
	var s fantasy.Schema
	switch q.Type {
	case TypeBool:
		s = fantasy.Schema{Type: "boolean", Description: q.Instructions}
	case TypeChoice:
		labels := q.labels()
		enum := make([]any, len(labels))
		for i, l := range labels {
			enum[i] = l
		}
		s = fantasy.Schema{Type: "string", Enum: enum, Description: q.Instructions}
	case TypeScore:
		high := q.max()
		s = fantasy.Schema{Type: "integer", Minimum: new(0.0), Maximum: &high, Description: q.Instructions}
	}
	return s, nil
}

// schemaAnswers checks one decoded value per question. A missing one, one of
// another type, a label the question does not have and a score outside its
// levels are all errors.
func schemaAnswers(questions map[string]Question, got map[string]any) (map[string]Answer, error) {
	out := make(map[string]Answer, len(questions))
	for name, q := range questions {
		v, ok := got[name]
		if !ok {
			return nil, fmt.Errorf("the provider answered no %q", name)
		}
		answer, err := q.answerValue(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = answer
	}
	return out, nil
}

// answerValue is one decoded value as the question asked for it.
func (q Question) answerValue(v any) (Answer, error) {
	switch q.Type {
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return Answer{}, fmt.Errorf("answered a %T, want a boolean", v)
		}
		return Answer{Type: TypeBool, Value: b}, nil
	case TypeChoice:
		s, ok := v.(string)
		if !ok {
			return Answer{}, fmt.Errorf("answered a %T, want a label", v)
		}
		if err := q.checkChoice(s); err != nil {
			return Answer{}, err
		}
		return Answer{Type: TypeChoice, Choice: s}, nil
	case TypeScore:
		f, ok := v.(float64)
		if !ok {
			return Answer{}, fmt.Errorf("answered a %T, want a number", v)
		}
		if err := q.checkScore(f); err != nil {
			return Answer{}, err
		}
		return Answer{Type: TypeScore, Score: &f}, nil
	}
	return Answer{}, fmt.Errorf("unknown question type %q", q.Type)
}

// schemaPrompt is the state and every question, in the one call the schema
// strategy makes.
func schemaPrompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "State:\n%s\n\nTask: answer each of the following questions about the state.\n\n", renderJSON(req.State))
	names := sortedKeys(req.Questions)
	for _, n := range names {
		b.WriteString(renderQuestion(n, req.Questions[n], nil) + "\n\n")
	}
	b.WriteString("Answer with one object holding one entry per question, " + strings.Join(names, ", ") + ".")
	return b.String()
}
