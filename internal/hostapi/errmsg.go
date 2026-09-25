package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
)

// explain turns a turn failure into what happened and what to do about it;
// advice is "" when the raw error is all there is. The raw error still
// goes to the log and /errors.
func (h *Host) explain(err error) (cause, advice string) {
	var pe *fantasy.ProviderError
	var dns *net.DNSError
	var ne net.Error
	var garbled *json.SyntaxError
	key := "check api_key in init.lua"
	if me, ok := errors.AsType[agent.ModelError](err); ok {
		key = "check api_key for " + me.Provider + " in init.lua"
		if spec, ok := h.Providers.Lookup(me.Provider); ok && spec.KeyEnv != "" {
			key = "check " + spec.KeyEnv
		}
	}
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, context.Canceled):
		return "interrupted", ""
	case errors.As(err, &pe) && pe.IsContextTooLarge():
		return "the conversation no longer fits the model's context", "/compact or /new"
	case pe != nil && (pe.AuthError || pe.StatusCode == http.StatusUnauthorized || pe.StatusCode == http.StatusForbidden):
		return "the provider rejected the API key", key
	case pe != nil && pe.StatusCode == http.StatusTooManyRequests:
		return "rate limited by the provider", "wait a bit and send again"
	case pe != nil && pe.StatusCode == http.StatusNotFound:
		return "the provider does not know this model", "pick another with /model"
	case pe != nil && (pe.StatusCode >= 500 || pe.StatusCode == http.StatusRequestTimeout):
		return fmt.Sprintf("the provider is having trouble (%d)", pe.StatusCode), "try again shortly"
	case pe != nil && pe.StatusCode >= 400:
		return fmt.Sprintf("the provider refused the request (%d): %s", pe.StatusCode, pe.Message), ""
	case errors.As(err, &dns):
		return "network: cannot resolve " + dns.Name, "check your connection or VPN"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "network: connection refused" + hostOf(err), "is the endpoint up?"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "network: the connection dropped mid-reply", "send again"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "network: the provider timed out", "check your connection and send again"
	case errors.As(err, &garbled):
		return "the provider sent a reply zemi could not read", "send again"
	}
	return err.Error(), ""
}

// friendlyError is the headline a person can act on: the cause, then the
// advice.
func (h *Host) friendlyError(err error) string {
	cause, advice := h.explain(err)
	if advice == "" {
		return cause
	}
	return cause + ": " + advice
}

// hostOf names the host of a failed request, when the error carries one.
func hostOf(err error) string {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			return " by " + u.Host
		}
	}
	return ""
}

// errorRowText is the transcript error row: the headline, capitalized as
// a transcript headline is, then the raw error on a second line when it
// adds detail.
func (h *Host) errorRowText(err error) string {
	head, raw := h.friendlyError(err), err.Error()
	pe, _ := errors.AsType[*fantasy.ProviderError](err)
	detail := !strings.Contains(head, raw) && !errors.Is(err, context.Canceled) &&
		(pe == nil || pe.Message == "" || !strings.Contains(head, pe.Message))
	if r, n := utf8.DecodeRuneInString(head); n > 0 {
		head = string(unicode.ToUpper(r)) + head[n:]
	}
	if detail {
		head += "\n" + raw
	}
	return head
}
