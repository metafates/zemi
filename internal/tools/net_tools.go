// net_tools.go holds the URL validation and HTTP client the Lua zemi.http
// API shares: http/https only, no userinfo, redirects left to the caller.

package tools

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// SafeClient builds an HTTP client whose redirects, each a URL ParseNetURL
// takes, the caller follows through onHop.
func SafeClient(onHop func(u *url.URL) error) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			Proxy:                 nil,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if _, err := ParseNetURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect: %w", err)
			}
			return onHop(req.URL)
		},
	}
}

// ParseNetURL validates scheme and userinfo for every http hop.
func ParseNetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https urls are allowed")
	}
	if u.User != nil {
		return nil, errors.New("userinfo in urls is refused")
	}
	if u.Hostname() == "" {
		return nil, errors.New("url is missing a host")
	}
	return u, nil
}
