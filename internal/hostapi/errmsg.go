package hostapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"

	"charm.land/fantasy"
)

// friendlyError turns a turn failure into a headline a person can act on
// (what happened, what to do). The raw error still goes to the log and
// /errors.
func friendlyError(err error) string {
	var pe *fantasy.ProviderError
	var dns *net.DNSError
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "interrupted"
	case errors.As(err, &pe) && pe.IsContextTooLarge():
		return "the conversation no longer fits the model's context: /compact or /new"
	case pe != nil && (pe.AuthError || pe.StatusCode == 401 || pe.StatusCode == 403):
		return "the provider rejected the API key: check it with /health"
	case pe != nil && pe.StatusCode == 429:
		return "rate limited by the provider: wait a bit and send again"
	case pe != nil && pe.StatusCode == 404:
		return "the provider does not know this model: pick another with /model"
	case pe != nil && (pe.StatusCode >= 500 || pe.StatusCode == 408):
		return fmt.Sprintf("the provider is having trouble (%d): try again shortly", pe.StatusCode)
	case pe != nil && pe.StatusCode >= 400:
		return fmt.Sprintf("the provider refused the request (%d): %s", pe.StatusCode, pe.Message)
	case errors.As(err, &dns):
		return "network: cannot resolve " + dns.Name + ": check your connection or VPN"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "network: connection refused" + hostOf(err) + ": is the endpoint up?"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "network: the connection dropped mid-reply: send again"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "network: the provider timed out: check your connection and send again"
	}
	return err.Error()
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

// errorRowText is the transcript error row: the headline, then the raw
// error on a second line when it adds detail.
func errorRowText(err error) string {
	head, raw := friendlyError(err), err.Error()
	if head == raw || strings.Contains(head, raw) || errors.Is(err, context.Canceled) {
		return head
	}
	return head + "\n" + raw
}
