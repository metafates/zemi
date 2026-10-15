// net_tools.go ships the fetch built-in (DESIGN §6.4): it goes through the
// net.<host> permission subject, accepts http/https URLs only and refuses
// userinfo.

package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
