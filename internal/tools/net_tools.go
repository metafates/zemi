// net_tools.go ships the network built-ins (DESIGN §6.4): fetch and
// web_search. Both go through the net.<host> permission subject, accept
// http/https URLs only and refuse userinfo.

package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	mdbase "github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	xhtml "golang.org/x/net/html"
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
				return fmt.Errorf("stopped after 5 redirects")
			}
			if _, err := ParseNetURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect: %w", err)
			}
			return onHop(req.URL)
		},
	}
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
// the whole URL under net.<host>.
func netGate(p *Pipeline, u *url.URL) error {
	v := p.CheckDenied(Request{Tool: ToolFetch, Subject: "net." + u.Hostname(), Arg: u.String(), Reason: "fetch redirect"})
	if !v.Allowed {
		return errors.New(DeniedText(v, "net."+u.Hostname()+" "+u.String()))
	}
	return nil
}

// fetchMax is how much of a body fetch reads.
const fetchMax = 1 << 20

func runFetch(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	// the tool gate already checked this URL; a second check here would ask
	// the user again
	raw := str(input["url"])
	if _, err := ParseNetURL(raw); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	client := SafeClient(func(hop *url.URL) error { return netGate(p, hop) })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req.Header.Set("User-Agent", "zemi/0.1")
	// docs sites that serve markdown send it whole, where readability drops
	// headings and prose
	req.Header.Set("Accept", "text/markdown, text/plain;q=0.9, text/html;q=0.8, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMax+1))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	cut := len(body) > fetchMax
	if cut {
		body = body[:fetchMax]
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
	if cut {
		b.WriteString("(only the first 1 MB of the page was read")
		if resp.ContentLength > 0 {
			fmt.Fprintf(&b, ", of %d bytes", resp.ContentLength)
		}
		b.WriteString(")\n")
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(p.cut("fetch", text, resultMaxOut))
	return fantasy.NewTextResponse(b.String()), nil
}

// RE2 has no backreferences, so the closing tag is matched by
// alternation instead of \1.
var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// mdConv renders HTML as markdown. The table plugin is not in the default
// set, and a docs page's parameter table is half of what it says.
var mdConv = sync.OnceValue(func() *converter.Converter {
	return converter.NewConverter(converter.WithPlugins(
		mdbase.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin(),
	))
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

func runWebSearch(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	limit := 10
	if v, ok := input["max_results"].(float64); ok && v >= 1 {
		limit = int(min(v, 20))
	}
	u := p.search
	if u == nil {
		return fantasy.NewTextErrorResponse("empty query"), nil
	}
	// the gate approved this one URL, so a redirect elsewhere is refused
	client := SafeClient(func(*url.URL) error { return fmt.Errorf("search redirect refused") })
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
	hits, err := parseDDGLite(resp.StatusCode, string(body))
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
		return nil, fmt.Errorf("DuckDuckGo answered with its bot check, so this network cannot search it; do not retry. Fetch known URLs directly")
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
