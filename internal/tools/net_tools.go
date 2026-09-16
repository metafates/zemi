// net_tools.go ships the network built-ins (DESIGN §6.4, §9): fetch and
// web_search. Both go through the net.<host> permission subject, accept
// http/https URLs only, refuse userinfo, and every connect is checked
// against the denied-address set on the resolved IP (per hop), so DNS
// rebinding cannot slip past a hostname rule.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

// deniedNets is the §9 SSRF set; v4-mapped v6 normalizes to v4 before
// matching. Loopback and RFC1918 are development targets the design gates
// behind an explicit user allow, which the store does not model yet: v1
// refuses them outright (safe side).
var deniedNets = func() []*net.IPNet {
	raw := []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "0.0.0.0/8", "100.64.0.0/10", "224.0.0.0/4",
		"::1/128", "fe80::/10", "fc00::/7", "64:ff9b::/96", "::/128",
	}
	var out []*net.IPNet
	for _, s := range raw {
		_, n, err := net.ParseCIDR(s)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// ipDenied matches one resolved address against the §9 set: v4-mapped v6
// normalizes to v4 first, so every family is covered by the table.
func ipDenied(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range deniedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SafeClient builds an HTTP client whose dialer refuses denied IPs at
// connect time (after resolution) and whose redirect follow is driven by
// the caller through onHop. allowPrivate skips the address check: the §9
// localhost development opt-in, which only an explicit user allow rule for
// the named host earns.
func SafeClient(onHop func(u *url.URL) error, allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("non-IP dial address %q", host)
		}
		if !allowPrivate && ipDenied(ip) {
			return fmt.Errorf("address %s is refused", ip)
		}
		return nil
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			Proxy:                 nil,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			if onHop != nil {
				return onHop(req.URL)
			}
			return nil
		},
	}
}

// ParseNetURL validates scheme and userinfo for every fetch hop.
func ParseNetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http and https urls are allowed")
	}
	if u.User != nil {
		return nil, fmt.Errorf("userinfo in urls is refused")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("url is missing a host")
	}
	return u, nil
}

// netGate checks one hop against the net.<host> subject. The resolve-time
// Control hook covers the IP side; this covers the hostname rules.
func netGate(p *Pipeline, u *url.URL, reason string) error {
	v := p.Check(Request{Subject: "net." + u.Hostname(), Arg: u.Host, Reason: reason})
	if v.Decision != perm.Allow {
		return fmt.Errorf("permission denied for %s: %s", u.Host, v.Reason)
	}
	return nil
}

func runFetch(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	raw := str(input["url"])
	u, err := ParseNetURL(raw)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if err := netGate(p, u, "fetch url"); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	client := SafeClient(func(hop *url.URL) error {
		if hop.Scheme != "http" && hop.Scheme != "https" {
			return fmt.Errorf("redirect to non-http scheme refused")
		}
		if hop.User != nil {
			return fmt.Errorf("redirect with userinfo refused")
		}
		return netGate(p, hop, "fetch redirect")
	}, false)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req.Header.Set("User-Agent", "zemi/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	text := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		text = htmlToText(text, resp.Request.URL)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", resp.Proto, resp.Status)
	if v := resp.Header.Get("Content-Type"); v != "" {
		fmt.Fprintf(&b, "Content-Type: %s\n", v)
	}
	b.WriteString("\n")
	b.WriteString(truncOut(text))
	if len(text) > len(truncOut(text)) {
		b.WriteString(p.spill("fetch", text))
	}
	return fantasy.NewTextResponse(b.String()), nil
}

var (
	// RE2 has no backreferences, so the closing tag is matched by
	// alternation instead of \1.
	scriptRe  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer|aside)\b[^>]*>.*?</(script|style|noscript|svg|head|nav|footer|aside)>`)
	linkRe    = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*["']([^"']*)["'][^>]*>(.*?)</a>`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe   = regexp.MustCompile(`\n\s*\n\s*`)
	breakRe   = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6])[^>]*>`)
)

// htmlToText strips markup to a readable plain-text skeleton. Links stay
// as markdown [text](url), resolved against base, so the model can follow
// them.
func htmlToText(s string, base *url.URL) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = commentRe.ReplaceAllString(s, " ")
	s = linkRe.ReplaceAllStringFunc(s, func(a string) string {
		m := linkRe.FindStringSubmatch(a)
		text := strings.TrimSpace(tagRe.ReplaceAllString(m[2], ""))
		ref, err := url.Parse(html.UnescapeString(strings.TrimSpace(m[1])))
		if err != nil || text == "" || strings.HasPrefix(m[1], "#") {
			return text
		}
		if base != nil {
			ref = base.ResolveReference(ref)
		}
		if ref.Scheme != "http" && ref.Scheme != "https" {
			return text
		}
		return "[" + text + "](" + ref.String() + ")"
	})
	s = breakRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return spaceRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}

// searchUA is a browser User-Agent: DuckDuckGo serves its bot check to
// anything else.
const searchUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// searchURL builds the request URL: a SearXNG JSON endpoint when the user
// set one (web.search_url), DuckDuckGo Lite otherwise.
func searchURL(p *Pipeline, query string) (*url.URL, bool, error) {
	base, searx := "https://lite.duckduckgo.com/lite/", false
	if p.SearchURL != nil {
		if s := p.SearchURL(); s != "" {
			base, searx = s, true
		}
	}
	u, err := ParseNetURL(base)
	if err != nil {
		return nil, searx, fmt.Errorf("web.search_url: %v", err)
	}
	q := u.Query()
	q.Set("q", query)
	if searx {
		q.Set("format", "json")
	}
	u.RawQuery = q.Encode()
	return u, searx, nil
}

func runWebSearch(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	query := strings.TrimSpace(str(input["query"]))
	if query == "" {
		return fantasy.NewTextErrorResponse("empty query"), nil
	}
	limit := 10
	if v, ok := input["max_results"].(float64); ok && v >= 1 && v <= 20 {
		limit = int(v)
	}
	u, searx, err := searchURL(p, query)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// The host is the user's (config), so a self-hosted SearXNG on a
	// private address is allowed; redirects are refused so a hop cannot
	// take that allowance elsewhere.
	client := SafeClient(func(*url.URL) error { return fmt.Errorf("search redirect refused") }, searx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req.Header.Set("User-Agent", searchUA)
	resp, err := client.Do(req)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	var hits []searchHit
	if searx {
		if resp.StatusCode != http.StatusOK {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("search: %s (SearXNG needs format json enabled)", resp.Status)), nil
		}
		hits, err = parseSearxng(body)
	} else {
		hits, err = parseDDGLite(resp.StatusCode, string(body))
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(formatHits(hits, limit)), nil
}

type searchHit struct{ Title, URL, Snippet string }

var (
	ddgLinkRe    = regexp.MustCompile(`(?is)<a\b[^>]*class=['"]result-link['"][^>]*>.*?</a>`)
	ddgHrefRe    = regexp.MustCompile(`(?is)\bhref=['"]([^'"]*)['"]`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<td\b[^>]*class=['"]result-snippet['"][^>]*>(.*?)</td>`)
)

// parseDDGLite reads the DuckDuckGo Lite result table: each result-link
// anchor is followed by its result-snippet cell. The bot check comes back
// as 202 or as a 200 page with the anomaly modal, and must not read as "no
// results".
func parseDDGLite(status int, page string) ([]searchHit, error) {
	if status == http.StatusAccepted || strings.Contains(page, "anomaly-modal") {
		return nil, fmt.Errorf("DuckDuckGo answered with its bot check, so this network cannot search it; do not retry. Fetch known URLs directly, or ask the user to set web.search_url to a SearXNG instance")
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("search: HTTP %d", status)
	}
	links := ddgLinkRe.FindAllStringIndex(page, -1)
	var hits []searchHit
	for i, loc := range links {
		a := page[loc[0]:loc[1]]
		h := searchHit{Title: strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(a, "")))}
		if m := ddgHrefRe.FindStringSubmatch(a); m != nil {
			h.URL = ddgTarget(html.UnescapeString(m[1]))
		}
		end := len(page)
		if i+1 < len(links) {
			end = links[i+1][0]
		}
		if m := ddgSnippetRe.FindStringSubmatch(page[loc[1]:end]); m != nil {
			h.Snippet = strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(m[1], "")))
		}
		if h.URL != "" {
			hits = append(hits, h)
		}
	}
	return hits, nil
}

// ddgTarget unwraps DuckDuckGo's //duckduckgo.com/l/?uddg=<url> redirect.
func ddgTarget(href string) string {
	if u, err := url.Parse(href); err == nil && strings.HasSuffix(u.Host, "duckduckgo.com") {
		if t := u.Query().Get("uddg"); t != "" {
			return t
		}
	}
	return href
}

func parseSearxng(body []byte) ([]searchHit, error) {
	var out struct {
		Results []struct{ Title, URL, Content string }
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("search: bad SearXNG reply: %v", err)
	}
	hits := make([]searchHit, 0, len(out.Results))
	for _, r := range out.Results {
		hits = append(hits, searchHit{r.Title, r.URL, r.Content})
	}
	return hits, nil
}

func formatHits(hits []searchHit, limit int) string {
	if len(hits) == 0 {
		return "(no results)"
	}
	var b strings.Builder
	for i, h := range hits[:min(limit, len(hits))] {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, h.Title, h.URL)
		if h.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", h.Snippet)
		}
	}
	return b.String()
}
