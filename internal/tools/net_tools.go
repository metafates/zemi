// net_tools.go ships the network built-ins (DESIGN §6.4, §9): fetch and
// web_search. Both go through the net.<host> permission subject, accept
// http/https URLs only, refuse userinfo, and every connect is checked
// against the denied-address set on the resolved IP (per hop), so DNS
// rebinding cannot slip past a hostname rule.
package tools

import (
	"bytes"
	"cmp"
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
	"unicode"

	"charm.land/fantasy"
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
	v := p.Check(Request{Subject: "net." + u.Hostname(), Arg: u.String(), Reason: reason})
	if v.Decision != perm.Allow {
		return fmt.Errorf("permission denied for %s: %s", u.Host, v.Reason)
	}
	return nil
}

func runFetch(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	// the tool gate already checked this URL; a second check here would ask
	// the user again
	raw := str(input["url"])
	if _, err := ParseNetURL(raw); err != nil {
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
	ct := resp.Header.Get("Content-Type")
	text := string(body)
	switch {
	case strings.Contains(ct, "text/html"):
		text = htmlToText(text, resp.Request.URL)
	case bytes.IndexByte(body[:min(len(body), 8000)], 0) >= 0:
		// like read: bytes the model cannot act on are worse than an error
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%s answered with %d bytes of binary (%s); fetch reads text", raw, len(body), cmp.Or(ct, "no content type"))), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", resp.Proto, resp.Status)
	if ct != "" {
		fmt.Fprintf(&b, "Content-Type: %s\n", ct)
	}
	b.WriteString("\n")
	if cut := TruncOut(text, 10000); cut != text {
		b.WriteString(cut + p.spill("fetch", text))
	} else {
		b.WriteString(text)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

var (
	// RE2 has no backreferences, so the closing tag is matched by
	// alternation instead of \1.
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe = regexp.MustCompile(`\n{3,}`)
)

// htmlSkip names subtrees with nothing to read in them.
var htmlSkip = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true,
	"head": true, "title": true, "nav": true, "footer": true, "aside": true,
	"form": true, "template": true, "iframe": true,
}

// htmlBlock names tags whose content starts a paragraph.
var htmlBlock = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"ul": true, "ol": true, "dl": true, "table": true, "blockquote": true,
	"header": true, "figure": true, "hr": true,
}

// htmlToText renders HTML as markdown: headings, list items, table rows and
// code blocks keep their shape, which is what a docs page's parameter table
// needs to stay readable. Links stay as [text](url), resolved against base,
// so the model can follow them.
func htmlToText(s string, base *url.URL) string {
	w := &htmlText{base: base}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	for {
		switch tt := z.Next(); tt {
		case xhtml.ErrorToken:
			return w.done()
		case xhtml.TextToken:
			w.text(string(z.Text()))
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			raw, _ := z.TagName()
			name, attr := string(raw), ""
			if name == "a" {
				attr = tagAttr(z, "href")
			}
			w.open(name, attr, tt == xhtml.StartTagToken)
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			w.close(string(name))
		}
	}
}

func tagAttr(z *xhtml.Tokenizer, want string) string {
	for {
		k, v, more := z.TagAttr()
		if string(k) == want {
			return string(v)
		}
		if !more {
			return ""
		}
	}
}

// buf is a strings.Builder that remembers its last byte, so the writer can
// tell whether it owes a separator without copying what it holds.
type buf struct {
	b    strings.Builder
	last byte
}

func (b *buf) write(s string) {
	if s != "" {
		b.b.WriteString(s)
		b.last = s[len(s)-1]
	}
}

// htmlText turns a token stream into markdown. Text lands in the open link,
// then the open table cell, then the page.
type htmlText struct {
	base      *url.URL
	out, cell buf
	link      buf
	href      string
	row       []string
	head      bool // the open row's cells are <th>
	firstRow  bool // no row of this table is written yet
	inLink    bool
	inCell    bool
	skip, pre int
	nl        int // newlines owed before the next text
}

func (w *htmlText) target() *buf {
	switch {
	case w.inLink:
		return &w.link
	case w.inCell:
		return &w.cell
	}
	return &w.out
}

func (w *htmlText) write(s string) {
	t := w.target()
	if t == &w.out {
		for ; w.nl > 0; w.nl-- {
			w.out.write("\n")
		}
	}
	t.write(s)
}

// space separates inline text; a boundary already pending is separator enough.
func (w *htmlText) space() {
	t := w.target()
	if t.b.Len() == 0 || w.nl > 0 || t.last == ' ' || t.last == '\n' {
		return
	}
	t.write(" ")
}

// br owes n newlines to the page; inside a cell or a link a block boundary
// is only a space, since neither can hold a line of its own.
func (w *htmlText) br(n int) {
	if w.inCell || w.inLink {
		w.space()
		return
	}
	if w.out.b.Len() > 0 {
		w.nl = max(w.nl, n)
	}
}

func (w *htmlText) text(s string) {
	if w.skip > 0 || s == "" {
		return
	}
	if w.pre > 0 {
		w.write(s)
		return
	}
	body := strings.Join(strings.Fields(s), " ")
	if body == "" {
		w.space()
		return
	}
	if strings.IndexFunc(s, unicode.IsSpace) == 0 {
		w.space()
	}
	w.write(body)
	if unicode.IsSpace(rune(s[len(s)-1])) {
		w.space()
	}
}

func (w *htmlText) open(name, href string, paired bool) {
	if htmlSkip[name] {
		if paired {
			w.skip++
		}
		return
	}
	if w.skip > 0 {
		return
	}
	switch {
	case name == "br":
		w.br(1)
	case name == "a":
		w.href, w.inLink = href, true
	case name == "li":
		w.br(1)
		w.write("- ")
	case name == "pre":
		w.br(2)
		w.write("```")
		w.br(1)
		w.pre++
	case name == "table":
		w.br(2)
		w.firstRow = true
	case name == "tr":
		w.row, w.head = nil, false
	case name == "td" || name == "th":
		w.cell, w.inCell, w.head = buf{}, true, w.head || name == "th"
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		w.br(2)
		w.write(strings.Repeat("#", int(name[1]-'0')) + " ")
	case htmlBlock[name]:
		w.br(2)
	}
}

func (w *htmlText) close(name string) {
	if htmlSkip[name] {
		w.skip = max(0, w.skip-1)
		return
	}
	if w.skip > 0 {
		return
	}
	switch {
	case name == "a":
		w.closeLink()
	case name == "pre":
		w.pre = max(0, w.pre-1)
		if w.out.last != '\n' {
			w.write("\n")
		}
		w.write("```")
		w.br(2)
	case name == "td" || name == "th":
		w.inCell = false
		w.row = append(w.row, strings.TrimSpace(w.cell.b.String()))
	case name == "tr":
		w.closeRow()
	case name == "li":
		w.br(1)
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		w.br(2)
	case htmlBlock[name]:
		w.br(2)
	}
}

func (w *htmlText) closeLink() {
	text, href := strings.TrimSpace(w.link.b.String()), w.href
	w.link, w.inLink, w.href = buf{}, false, ""
	ref, err := url.Parse(html.UnescapeString(strings.TrimSpace(href)))
	if err != nil || text == "" || strings.HasPrefix(href, "#") {
		w.write(text)
		return
	}
	if w.base != nil {
		ref = w.base.ResolveReference(ref)
	}
	if ref.Scheme != "http" && ref.Scheme != "https" {
		w.write(text)
		return
	}
	w.write("[" + text + "](" + ref.String() + ")")
}

// closeRow writes the row as a markdown table row, with the header rule
// under the first one so the columns stay paired up.
func (w *htmlText) closeRow() {
	if len(w.row) == 0 {
		return
	}
	w.br(1)
	w.write("| " + strings.Join(w.row, " | ") + " |")
	if w.firstRow && w.head {
		w.br(1)
		w.write("|" + strings.Repeat(" --- |", len(w.row)))
	}
	w.row, w.firstRow = nil, false
	w.br(1)
}

func (w *htmlText) done() string {
	var lines []string
	for l := range strings.SplitSeq(w.out.b.String(), "\n") {
		lines = append(lines, strings.TrimRight(l, " \t"))
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
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
