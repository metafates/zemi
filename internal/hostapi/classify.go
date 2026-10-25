// classify.go is zemi.classify.ask: one judge call over the strategy ladder
// (DESIGN §6.3). The domain, the strategies and the protocol client live in
// internal/classify; this file is the Lua boundary, the permission gate and
// the billing.

package hostapi

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/classify"
	"github.com/metafates/zemi/internal/lua"
)

// classifyTimeout is how long a classify call may take: a small job, so it
// waits a minute rather than the ten minutes a model ask may.
const classifyTimeout = time.Minute

// classifyAsk is zemi.classify.ask({ model, state, questions, timeout }):
// every question judged in one call, by whichever strategy the model's provider
// answers with, billed to the live session. It costs money, and a plugin's
// manifest `uses` naming `model` is what consents to its call.
func (h *Host) classifyAsk(L *glua.LState) int {
	spec := L.CheckTable(1)
	state := classifyState(L, spec)
	questions := classifyQuestions(L, spec)
	ref := cmp.Or(rawString(L, spec, "model"), "default")
	id := h.modelRef(ref)
	stub := h.gateAsk(L, "lua.classify.ask", id, func() map[string]any {
		return map[string]any{"model": ref, "questions": slices.Sorted(maps.Keys(questions))}
	})
	timeout := classifyTimeout
	if n, ok := opt[glua.LNumber](L, spec, "timeout"); ok && n > 0 {
		timeout = time.Duration(float64(n) * float64(time.Second))
	}
	onAnswer, _ := opt[*glua.LFunction](L, spec, "on_answer")
	at := h.callerOf(L)
	// a stub stands in for the whole call, a model and all
	var req classify.Request
	sid := h.AgentSessionID()
	if stub == nil {
		if id == "" {
			L.RaiseError("zemi.classify.ask: %v", h.modelErr(id))
		}
		var err error
		if req, err = h.classifyRequest(id, sid, state, questions); err != nil {
			L.RaiseError("zemi.classify.ask: %v", err)
		}
	}
	var (
		res     classify.Result
		askErr  error
		stubbed map[string]any
	)
	ctx, cancel := context.WithCancel(context.Background())
	h.asyncTask(L, asyncTaskSpec{
		subject: "zemi.classify.ask", waitAPI: "classify:wait", guard: "classify:",
		ctx: ctx, cancel: cancel, timeout: timeout,
		stop: cancel, withCancel: true, res: &askErr,
		body: func(ctx context.Context) {
			if stub != nil {
				// a test zemi's stub stands in for the whole call, its error included:
				// a plugin's test must see the same failure a provider would give
				stubbed, askErr = stub.fields, stub.err
			} else {
				// every attempt the ladder made was billed, so what it used is
				// reported whether or not one answered
				res, askErr = classify.Ask(ctx, req)
				h.bill(sid, id, res.Usage)
			}
			if onAnswer != nil {
				// the same table wait() answers, delivered as a task, so a
				// handler that cannot suspend still acts on the answer
				var out, errArg any
				if askErr != nil {
					errArg = askErr.Error()
				} else if stubbed != nil {
					out = stubbed
				} else {
					out = classifyResultMap(h, id, res)
				}
				h.callLuaAsync("zemi.classify on_answer", at, onAnswer, out, errArg)
			}
		},
		ret: func(L *glua.LState, err error) []glua.LValue {
			if err == nil { // askErr is set only once done has closed
				err = askErr
			}
			if err != nil {
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			if stubbed != nil {
				return []glua.LValue{lua.FromGo(L, stubbed)}
			}
			return []glua.LValue{lua.FromGo(L, classifyResultMap(h, id, res))}
		},
	})
	return 1
}

// classifyRequest is how the model id is reached: a decisions model answers
// through its provider's own protocol, and everything else through the chat
// model, with token probabilities where the provider returns them. sid is the
// session the call is billed to, and its affinity rides the requests, since the
// questions of one call share a prompt.
func (h *Host) classifyRequest(id, sid string, state map[string]any, questions map[string]classify.Question) (classify.Request, error) {
	req := classify.Request{State: state, Questions: questions}
	if spec, model, ok := h.Providers.Decisions(agent.ModelID(id)); ok {
		req.Protocol = &classify.Protocol{
			BaseURL: spec.BaseURL,
			APIKey:  h.Providers.Key(spec.ID),
			Model:   model.ID,
			Headers: classifyHeaders(spec.Headers, sid),
			HTTP:    agent.ModelHTTP(),
		}
		return req, nil
	}
	chat, err := h.resolveModel(id)
	if err != nil {
		return classify.Request{}, err
	}
	req.Chat = &classify.Chat{
		Model:   chat,
		Options: h.Providers.CallOptions(agent.ModelID(id), "", sid),
		Headers: agent.SessionHeaders(sid),
		// a decoded answer is the model's own words, so it is asked greedily
		Temperature: new(0.0),
		Logprobs:    h.Providers.LogprobOptions(agent.ModelID(id)),
	}
	return req, nil
}

// classifyHeaders are a request's HTTP headers: the provider's own, with the
// session's affinity beside them.
func classifyHeaders(spec map[string]string, sid string) map[string]string {
	session := agent.SessionHeaders(sid)
	if len(spec) == 0 {
		return session
	}
	out := maps.Clone(spec)
	maps.Copy(out, session)
	return out
}

// classifyState is the data to judge: a table, so the model reads it as data
// and not as a sentence a caller composed.
func classifyState(L *glua.LState, spec *glua.LTable) map[string]any {
	tb, ok := opt[*glua.LTable](L, spec, "state")
	if !ok {
		L.RaiseError("zemi.classify.ask: state is the data to judge, a table")
	}
	state, err := lua.ToGo(tb)
	if err != nil {
		L.RaiseError("zemi.classify.ask: state: %v", err)
	}
	m, ok := state.(map[string]any)
	if !ok {
		L.RaiseError("zemi.classify.ask: state must be a table of named fields, got %s", luaShape(state))
	}
	return m
}

// classifyQuestions reads the questions a call asks, each one checked against
// what its type needs: a bool's two meanings, a choice's labels, a score's
// levels. A question zemi cannot put to a model raises here.
func classifyQuestions(L *glua.LState, spec *glua.LTable) map[string]classify.Question {
	tb, ok := opt[*glua.LTable](L, spec, "questions")
	if !ok {
		L.RaiseError("zemi.classify.ask: questions are what to judge")
	}
	out := map[string]classify.Question{}
	tb.ForEach(func(k, v glua.LValue) {
		name, ok := k.(glua.LString)
		if !ok || name == "" {
			L.RaiseError("zemi.classify.ask: a question's name is its key, a string")
		}
		q, ok := v.(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.classify.ask: question %s must be a table", name)
		}
		out[string(name)] = classifyQuestion(L, string(name), q)
	})
	if len(out) == 0 {
		L.RaiseError("zemi.classify.ask: questions is empty")
	}
	return out
}

// classifyQuestion is one question, as its type asks for it.
func classifyQuestion(L *glua.LState, name string, tb *glua.LTable) classify.Question {
	q := classify.Question{Type: classify.Type(rawString(L, tb, "type")), Instructions: rawString(L, tb, "instructions")}
	if q.Instructions == "" {
		L.RaiseError("zemi.classify.ask: question %s needs instructions: one line saying what to judge", name)
	}
	criteria, _ := opt[*glua.LTable](L, tb, "criteria")
	if criteria == nil {
		L.RaiseError("zemi.classify.ask: question %s needs criteria: %s", name, criteriaHint(q.Type))
	}
	switch q.Type {
	case classify.TypeBool:
		q.Criteria = map[string]string{}
		criteria.ForEach(func(k, v glua.LValue) {
			key, _ := k.(glua.LString)
			if key != "true" && key != "false" {
				L.RaiseError(`zemi.classify.ask: question %s is answered "true" or "false", not %q`, name, string(key))
			}
			q.Criteria[string(key)] = criteriaText(L, name, v)
		})
	case classify.TypeChoice:
		q.Criteria = map[string]string{}
		criteria.ForEach(func(k, v glua.LValue) {
			key, ok := k.(glua.LString)
			if !ok || key == "" {
				L.RaiseError("zemi.classify.ask: question %s: a label is a non-empty string", name)
			}
			q.Criteria[string(key)] = criteriaText(L, name, v)
		})
	case classify.TypeScore:
		criteria.ForEach(func(_, v glua.LValue) {
			q.Levels = append(q.Levels, criteriaText(L, name, v))
		})
	default:
		L.RaiseError("zemi.classify.ask: question %s has type %q; use bool, choice or score", name, q.Type)
	}
	// the same check a strategy runs, so a question is refused with one message
	// wherever it is asked
	if err := q.Check(); err != nil {
		L.RaiseError("zemi.classify.ask: question %s: %v", name, err)
	}
	return q
}

// criteriaHint is what a question's criteria look like.
func criteriaHint(t classify.Type) string {
	switch t {
	case classify.TypeBool:
		return `{ ["true"] = "what true means", ["false"] = "what false means" }`
	case classify.TypeChoice:
		return `{ label = "what it means", ... }`
	case classify.TypeScore:
		return `{ "the lowest level", ..., "the highest" }`
	}
	return `{ type = "bool", instructions = "...", criteria = ... }`
}

// criteriaText is one criterion as text: what it means, so a number or a
// boolean a caller wrote is read rather than refused.
func criteriaText(L *glua.LState, name string, v glua.LValue) string {
	s, ok := rawScalar(v)
	if !ok {
		L.RaiseError("zemi.classify.ask: question %s: a criterion must be text, got %s", name, v.Type())
	}
	return s
}

// classifyResultMap is a result as Lua reads it: the strategy that answered,
// one answer per question, and what the call used. A number a strategy had
// none of is left out, so a caller that needs one can say so.
func classifyResultMap(h *Host, id string, res classify.Result) map[string]any {
	answers := map[string]any{}
	for name, a := range res.Answers {
		row := map[string]any{}
		switch a.Type {
		case classify.TypeBool:
			row["value"] = a.Value
			if a.Probability != nil {
				row["probability"] = *a.Probability
			}
		case classify.TypeChoice:
			row["choice"] = a.Choice
			if len(a.Probabilities) > 0 {
				row["probabilities"] = a.Probabilities
			}
			if a.Confidence != nil {
				row["confidence"] = *a.Confidence
			}
		case classify.TypeScore:
			if a.Score != nil {
				row["score"] = *a.Score
			}
			if a.Confidence != nil {
				row["confidence"] = *a.Confidence
			}
		}
		answers[name] = row
	}
	// the same shape agent.finish carries, and the same numbers the bill used
	u := res.Usage
	in := billedInput(u)
	return map[string]any{
		"strategy": res.Strategy,
		"answers":  answers,
		"usage": map[string]any{
			"in":    in,
			"out":   u.OutputTokens,
			"total": in + u.OutputTokens,
			"cost":  h.turnCost(id, u),
		},
	}
}

func (h *Host) classifyNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("ask", L.NewFunction(h.classifyAsk))
	return tb
}
