// net_tools.go ships the network built-ins (DESIGN §6.4, §9): fetch and
// web_search. Both go through the net.<host> permission subject, accept
// http/https URLs only, refuse userinfo, and every connect is checked
// against the denied-address set on the resolved IP (per hop), so DNS
// rebinding cannot slip past a hostname rule.
package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"
	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	mdbase "github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	xhtml "golang.org/x/net/html"

	"github.com/metafates/zemi/internal/perm"
)

// deniedNets is the §9 SSRF set; v4-mapped v6 normalizes to v4 before
// matching. Loopback and RFC1918 are refused unless the caller passes
// SafeClient's allowPrivate (the user's search URL, a user-allowed host).
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
			return &privateAddrError{ip}
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

// privateAddrError is a dial the §9 set refused; fetch names the rule that
// would permit it.
type privateAddrError struct{ ip net.IP }

func (e *privateAddrError) Error() string { return "address " + e.ip.String() + " is private" }

func privateRefused(addr, host string) error {
	return fmt.Errorf("refused: %s is a private address; only an allow rule for net.%s in the user's init.lua permits it", addr, host)
}

// checkFetchHost refuses a host that is private as written before the gate
// asks: only a user config rule permits one (runFetch's allowPrivate), so no
// answer to the ask could. A name that resolves private is refused at dial.
func checkFetchHost(p *Pipeline, u *url.URL) error {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	ip := net.ParseIP(host)
	if (ip == nil || !ipDenied(ip)) && host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return nil
	}
	if o := p.Store.Evaluate("net."+u.Hostname(), u.String(), p.Principal); o.Decision == perm.Allow && o.Rule != nil && o.Rule.Principal == "" {
		return nil
	}
	return privateRefused(u.Hostname(), u.Hostname())
}

// ParseNetURL validates scheme and userinfo for every fetch hop.
func ParseNetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad url: %w", err)
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

// netGate checks a redirect hop the way the tool gate checked the first URL:
// the whole URL under net.<host>, so the same rules answer both. The
// resolve-time Control hook covers the IP side.
func netGate(p *Pipeline, u *url.URL, reason string) error {
	if err := checkFetchHost(p, u); err != nil {
		return err
	}
	v := p.Check(Request{Subject: "net." + u.Hostname(), Arg: u.String(), Reason: reason})
	if v.Decision != perm.Allow {
		return errors.New(DeniedText(v, "net."+u.Hostname()+" "+u.String()))
	}
	return nil
}

func runFetch(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	// the tool gate already checked this URL; a second check here would ask
	// the user again
	raw := str(input["url"])
	u, err := ParseNetURL(raw)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// §9 localhost development opt-in: a rule the user wrote in config for
	// this host, never a session "always" answer (principal model)
	allowPrivate := p.verdict.Rule != nil && p.verdict.Rule.Principal == ""
	client := SafeClient(func(hop *url.URL) error {
		if hop.Scheme != "http" && hop.Scheme != "https" {
			return fmt.Errorf("redirect to non-http scheme refused")
		}
		if hop.User != nil {
			return fmt.Errorf("redirect with userinfo refused")
		}
		return netGate(p, hop, "fetch redirect")
	}, allowPrivate)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req.Header.Set("User-Agent", "zemi/0.1")
	resp, err := client.Do(req)
	var private *privateAddrError
	if errors.As(err, &private) {
		return fantasy.NewTextErrorResponse(privateRefused(private.ip.String(), u.Hostname()).Error()), nil
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	ct := resp.Header.Get("Content-Type")
	text := string(body)
	switch {
	case strings.Contains(ct, "text/html"):
		text = htmlToText(text, resp.Request.URL)
	case looksBinary(body):
		// like read: bytes the model cannot act on are worse than an error
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%s answered with %d bytes of binary (%s); fetch reads text", raw, len(body), cmp.Or(ct, "no content type"))), nil
	}
	// the status only when it is news (a non-200 body is returned, not an
	// error) and the type only when the body is still in it
	var b strings.Builder
	if resp.StatusCode != http.StatusOK {
		b.WriteString(resp.Status + "\n")
	}
	if ct != "" && !strings.Contains(ct, "text/html") {
		b.WriteString("Content-Type: " + ct + "\n")
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	if cut := TruncOut(text, 10000); cut != text {
		b.WriteString(cut + p.spill("fetch", text))
	} else {
		b.WriteString(text)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// RE2 has no backreferences, so the closing tag is matched by
// alternation instead of \1.
var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// mdConv renders HTML as markdown. The table plugin is not in the default
// set, and a docs page's parameter table is half of what it says.
var mdConv = sync.OnceValue(func() *converter.Converter {
	return converter.NewConverter(converter.WithPlugins(
		mdbase.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))
})

// htmlToText renders a page as markdown for the model: readability picks
// the article out of the navigation, ads and boilerplate around it, and the
// whole document is converted when it finds none. Links come out absolute
// against base, so the model can follow them.
func htmlToText(s string, base *url.URL) string {
	node, err := xhtml.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	// FromDocument rewrites the tree it is given, so the fallback parses again
	if art, err := readability.FromDocument(node, base); err == nil && art.Node != nil {
		node = art.Node
	} else if node, err = xhtml.Parse(strings.NewReader(s)); err != nil {
		return s
	}
	domain := ""
	if base != nil {
		domain = base.Scheme + "://" + base.Host
	}
	md, err := mdConv().ConvertNode(node, converter.WithDomain(domain))
	if err != nil {
		return s
	}
	return strings.TrimSpace(string(md))
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
		return nil, searx, fmt.Errorf("web.search_url: %w", err)
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
	if v, ok := input["max_results"].(float64); ok && v >= 1 {
		limit = int(min(v, 20))
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
		return nil, fmt.Errorf("search: bad SearXNG reply: %w", err)
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
