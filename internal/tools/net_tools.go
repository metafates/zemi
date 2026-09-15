// net_tools.go ships the network built-ins (DESIGN §6.4, §9): fetch and
// sourcegraph. Both go through the net.<host> permission subject, accept
// http/https URLs only, refuse userinfo, and every connect is checked
// against the denied-address set on the resolved IP (per hop), so DNS
// rebinding cannot slip past a hostname rule.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

// safeClient builds an HTTP client whose dialer refuses denied IPs at
// connect time (after resolution) and whose redirect follow is driven by
// the caller through onHop.
func safeClient(onHop func(u *url.URL) error) *http.Client {
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
		if ipDenied(ip) {
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

// parseNetURL validates scheme and userinfo for every fetch hop.
func parseNetURL(raw string) (*url.URL, error) {
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
	u, err := parseNetURL(raw)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if err := netGate(p, u, "fetch url"); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	client := safeClient(func(hop *url.URL) error {
		if hop.Scheme != "http" && hop.Scheme != "https" {
			return fmt.Errorf("redirect to non-http scheme refused")
		}
		if hop.User != nil {
			return fmt.Errorf("redirect with userinfo refused")
		}
		return netGate(p, hop, "fetch redirect")
	})
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
		text = htmlToText(text)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", resp.Proto, resp.Status)
	for _, ct := range []string{"Content-Type"} {
		if v := resp.Header.Get(ct); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", ct, v)
		}
	}
	b.WriteString("\n")
	b.WriteString(truncOut(text))
	return fantasy.NewTextResponse(b.String()), nil
}

var (
	// RE2 has no backreferences, so the closing tag is matched by
	// alternation instead of \1.
	scriptRe  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)[^>]*>.*?</(script|style|noscript|svg|head)>`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	tagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe   = regexp.MustCompile(`\n\s*\n\s*`)
)

// htmlToText strips markup to a readable plain-text skeleton: enough for
// the model to decide whether a page is worth a targeted fetch.
func htmlToText(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = commentRe.ReplaceAllString(s, " ")
	s = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6])[^>]*>`).ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = htmlUnescape(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return spaceRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}

func htmlUnescape(s string) string {
	repl := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
		"&#39;", "'", "&apos;", "'", "&nbsp;", " ",
	)
	return repl.Replace(s)
}

// runSourcegraph searches public repositories via the Sourcegraph stream
// API and formats repository/path/line hits.
func runSourcegraph(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	query := str(input["query"])
	if strings.TrimSpace(query) == "" {
		return fantasy.NewTextErrorResponse("empty query"), nil
	}
	count := 10
	if v, ok := input["count"].(float64); ok && v > 0 && v <= 50 {
		count = int(v)
	}
	const host = "sourcegraph.com"
	v := p.Check(Request{Subject: "net." + host, Arg: host, Reason: "sourcegraph search"})
	if v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse("permission denied: " + v.Reason), nil
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("v", "V3")
	q.Set("t", "literal")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://"+host+"/.api/search/stream?"+q.Encode(), nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req.Header.Set("Accept", "text/event-stream")
	client := safeClient(nil)
	resp, err := client.Do(req)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fantasy.NewTextErrorResponse(fmt.Sprintf("sourcegraph: %s: %s", resp.Status, truncOut(string(body)))), nil
	}
	return fantasy.NewTextResponse(truncOut(parseSGStream(resp.Body, count))), nil
}

// parseSGStream reads the SSE body, pulling repository/path/line matches
// out of the `matches` events; malformed events are skipped.
func parseSGStream(r io.Reader, count int) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4<<20))
	var b strings.Builder
	shown := 0
	for _, ev := range strings.Split(string(data), "\n\n") {
		var payload []string
		for _, line := range strings.Split(ev, "\n") {
			if s, ok := strings.CutPrefix(line, "data: "); ok {
				payload = append(payload, s)
			}
		}
		if len(payload) == 0 {
			continue
		}
		var matches []map[string]any
		if json.Unmarshal([]byte(strings.Join(payload, "\n")), &matches) != nil {
			continue
		}
		for _, m := range matches {
			repo, _ := m["repository"].(string)
			path, _ := m["path"].(string)
			if repo == "" && path == "" {
				continue
			}
			if lms, ok := m["lineMatches"].([]any); ok && len(lms) > 0 {
				for _, lm := range lms {
					if shown >= count {
						break
					}
					lm, ok := lm.(map[string]any)
					if !ok {
						continue
					}
					line, _ := lm["line"].(string)
					num := -1
					if f, ok := lm["lineNumber"].(float64); ok {
						num = int(f)
					}
					fmt.Fprintf(&b, "%s -> %s:%d: %s\n", repo, path, num, truncLine(line))
					shown++
				}
			} else {
				if shown >= count {
					break
				}
				fmt.Fprintf(&b, "%s -> %s\n", repo, path)
				shown++
			}
		}
	}
	if shown == 0 {
		return "(no matches)"
	}
	return b.String()
}
