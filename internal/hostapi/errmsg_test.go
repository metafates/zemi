package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
)

func TestFriendlyError(t *testing.T) {
	h, _ := newTestHost(t)
	h.Providers.Reset(agent.Catalog{Specs: []agent.Spec{{ID: "b", Kind: agent.KindOpenAI, KeyEnv: "B_KEY"}}, Keys: map[string]string{"b": "k"}})
	retry := func(err error) error { return &fantasy.RetryError{Errors: []error{err}} }
	var v any
	garbled := json.Unmarshal([]byte("{broken json"), &v)
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"cancel", context.Canceled, "interrupted"},
		{"rate limit through retry", retry(&fantasy.ProviderError{StatusCode: 429, Message: "slow down"}), "rate limited"},
		{"auth", &fantasy.ProviderError{StatusCode: 401}, "the provider rejected the API key: check api_key in init.lua"},
		{"auth from a provider", agent.ModelError{Provider: "fake", Err: &fantasy.ProviderError{StatusCode: 401}}, "check api_key for fake in init.lua"},
		{"auth from a builtin", agent.ModelError{Provider: "b", Err: &fantasy.ProviderError{StatusCode: 401}}, "the provider rejected the API key: check B_KEY"},
		{"context", &fantasy.ProviderError{StatusCode: 400, ContextTooLargeErr: true}, "/compact"},
		{"overload", &fantasy.ProviderError{StatusCode: 529}, "having trouble (529)"},
		{"bad request", &fantasy.ProviderError{StatusCode: 400, Message: "bad tool schema"}, "bad tool schema"},
		{"dns", retry(&url.Error{Op: "Post", URL: "https://llm.example/v1", Err: &net.DNSError{Name: "llm.example"}}), "cannot resolve llm.example"},
		{"refused", &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, "refused by 127.0.0.1:1"},
		{"timeout", context.DeadlineExceeded, "timed out"},
		{"garbled stream", fantasy.WrapTransportError(garbled), "the provider sent a reply zemi could not read: send again"},
		{"unknown stays raw", errors.New("agent: no models registered"), "agent: no models registered"},
	} {
		if got := h.friendlyError(c.err); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, got, c.want)
		}
	}
}
