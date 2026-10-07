// Package search is winc's local web search: the backend behind the
// `mcp__winc__web_search` tool that `winc mcp-search` serves to the sandboxed
// Claude Code over MCP. It exists because Claude Code's built-in WebSearch is
// executed SERVER-SIDE by Anthropic's API -- under winc the "API" is
// llama-server, which has no search backend, so every built-in search came
// back as 0 results with no error. Here a failure is always an error that
// names its cause; an empty list is returned only for a real empty answer.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ToolName is the fully qualified tool name Claude Code exposes for the
// `web_search` tool of the MCP server registered as "winc".
const ToolName = "mcp__winc__web_search"

// Config mirrors the [search] section of winc.toml.
type Config struct {
	Provider    string `toml:"provider"`      // auto | brave | searxng | duckduckgo | off
	BraveAPIKey string `toml:"brave_api_key"` // https://brave.com/search/api/
	SearXNGURL  string `toml:"searxng_url"`   // base URL of a SearXNG instance with the JSON format enabled
	MaxResults  int    `toml:"max_results"`   // default results per query (1..10)
}

const (
	DefaultMaxResults = 5
	HardMaxResults    = 10
	// Timeout bounds every request: a hung search engine must surface as an
	// error, never as a silent stall of the agent's turn.
	Timeout = 10 * time.Second
	// userAgent is a real browser-shaped UA (DuckDuckGo's HTML endpoint serves
	// bot-shaped ones a challenge page) with winc identified after it.
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) winc.cpp"
)

// Result is one search hit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Backend runs one query. Implementations return a non-nil error for every
// failed request (HTTP status, rate limit, challenge page, timeout, unparsable
// body); a nil error with zero results means the engine genuinely found nothing.
type Backend interface {
	Name() string
	Search(ctx context.Context, query string, n int) ([]Result, error)
}

// Errors backends wrap so callers (and tests) can tell the causes apart.
var (
	ErrRateLimited = errors.New("rate limited (HTTP 429)")
	ErrBlocked     = errors.New("the search engine returned a CAPTCHA / bot-challenge page instead of results")
	ErrOff         = errors.New("web search is turned off in winc.toml ([search] provider = \"off\")")
)

// ResolveProvider turns the config into the concrete provider that WOULD run
// and a one-line reason -- read-only, for `winc doctor` and the launch gate.
// "auto" prefers Brave (key set), then SearXNG (URL set), else DuckDuckGo.
func ResolveProvider(c Config) (provider, reason string) {
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "off":
		return "off", "disabled in winc.toml"
	case "brave":
		if strings.TrimSpace(c.BraveAPIKey) == "" {
			return "brave", "brave_api_key is NOT set - every search will fail until it is"
		}
		return "brave", "brave_api_key set"
	case "searxng":
		if strings.TrimSpace(c.SearXNGURL) == "" {
			return "searxng", "searxng_url is NOT set - every search will fail until it is"
		}
		return "searxng", "searxng_url = " + strings.TrimSpace(c.SearXNGURL)
	case "duckduckgo", "ddg":
		return "duckduckgo", "HTML endpoint, no key needed"
	case "", "auto":
		if strings.TrimSpace(c.BraveAPIKey) != "" {
			return "brave", "auto: brave_api_key set"
		}
		if strings.TrimSpace(c.SearXNGURL) != "" {
			return "searxng", "auto: searxng_url = " + strings.TrimSpace(c.SearXNGURL)
		}
		return "duckduckgo", "auto: no key or URL set - DuckDuckGo HTML endpoint"
	}
	return c.Provider, "unknown provider (use auto, brave, searxng, duckduckgo or off)"
}

// Enabled reports whether a search tool should be registered at all.
func Enabled(c Config) bool {
	p, _ := ResolveProvider(c)
	return p != "off"
}

// New builds the backend for the config, or an error for an unusable one
// (provider off, unknown, or missing its key / URL).
func New(c Config) (Backend, error) {
	p, reason := ResolveProvider(c)
	cl := &http.Client{Timeout: Timeout}
	switch p {
	case "off":
		return nil, ErrOff
	case "brave":
		if strings.TrimSpace(c.BraveAPIKey) == "" {
			return nil, fmt.Errorf("brave: %s", reason)
		}
		return &brave{key: strings.TrimSpace(c.BraveAPIKey), client: cl, base: "https://api.search.brave.com/res/v1/web/search"}, nil
	case "searxng":
		base := strings.TrimRight(strings.TrimSpace(c.SearXNGURL), "/")
		if base == "" {
			return nil, fmt.Errorf("searxng: %s", reason)
		}
		return &searxng{base: base, client: cl}, nil
	case "duckduckgo":
		return &duckduckgo{client: cl, base: "https://html.duckduckgo.com/html/"}, nil
	}
	return nil, fmt.Errorf("[search] provider = %q: %s", c.Provider, reason)
}

// ClampN normalizes a requested result count to [1, HardMaxResults], using
// def (or DefaultMaxResults) when n <= 0.
func ClampN(n, def int) int {
	if def <= 0 {
		def = DefaultMaxResults
	}
	if n <= 0 {
		n = def
	}
	if n > HardMaxResults {
		n = HardMaxResults
	}
	return n
}

// get performs a GET with the standard headers and maps transport and status
// failures to descriptive errors. The body is returned for 2xx only.
func get(ctx context.Context, cl *http.Client, u string, hdr map[string]string) (status int, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := cl.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout") || strings.Contains(err.Error(), "deadline") {
			return 0, nil, fmt.Errorf("timed out after %s", Timeout)
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, nil
}

// statusError maps a non-2xx status to a named error.
func statusError(engine string, status int, body []byte) error {
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", engine, ErrRateLimited)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if looksBlocked(body) {
			return fmt.Errorf("%s: HTTP %d: %w", engine, status, ErrBlocked)
		}
		return fmt.Errorf("%s: HTTP %d (check the API key / access)", engine, status)
	case status == http.StatusAccepted && looksBlocked(body):
		return fmt.Errorf("%s: %w", engine, ErrBlocked)
	case status < 200 || status > 299:
		return fmt.Errorf("%s: HTTP %d", engine, status)
	}
	return nil
}

// looksBlocked recognizes DuckDuckGo's anomaly / bot-challenge pages (and
// generic CAPTCHA interstitials) so they are reported as such, not parsed as
// "no results".
func looksBlocked(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, m := range []string{"anomaly-modal", "bots use duckduckgo", "captcha", "unusual traffic", "verify you are human", "challenge-form"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ---- Brave ----------------------------------------------------------------

type brave struct {
	key    string
	base   string
	client *http.Client
}

func (b *brave) Name() string { return "brave" }

func (b *brave) Search(ctx context.Context, q string, n int) ([]Result, error) {
	u := b.base + "?q=" + url.QueryEscape(q) + fmt.Sprintf("&count=%d", n)
	status, body, err := get(ctx, b.client, u, map[string]string{
		"X-Subscription-Token": b.key, "Accept": "application/json", "Accept-Encoding": "identity",
	})
	if err != nil {
		return nil, fmt.Errorf("brave: %w", err)
	}
	if err := statusError("brave", status, body); err != nil {
		return nil, err
	}
	return parseBrave(body, n)
}

// parseBrave reads Brave's web search JSON (web.results[].{title,url,description}).
func parseBrave(body []byte, n int) ([]Result, error) {
	var r struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("brave: response is not the expected JSON: %w", err)
	}
	out := make([]Result, 0, len(r.Web.Results))
	for _, x := range r.Web.Results {
		if x.URL == "" {
			continue
		}
		out = append(out, Result{Title: clean(x.Title), URL: x.URL, Snippet: clean(x.Description)})
		if len(out) == n {
			break
		}
	}
	return out, nil
}

// ---- SearXNG --------------------------------------------------------------

type searxng struct {
	base   string
	client *http.Client
}

func (s *searxng) Name() string { return "searxng" }

func (s *searxng) Search(ctx context.Context, q string, n int) ([]Result, error) {
	u := s.base + "/search?format=json&q=" + url.QueryEscape(q)
	status, body, err := get(ctx, s.client, u, map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	if err := statusError("searxng", status, body); err != nil {
		if status == http.StatusForbidden {
			return nil, fmt.Errorf("searxng: HTTP 403 - the instance must allow format=json (search.formats in its settings.yml)")
		}
		return nil, err
	}
	return parseSearXNG(body, n)
}

// parseSearXNG reads SearXNG's JSON (results[].{title,url,content}).
func parseSearXNG(body []byte, n int) ([]Result, error) {
	var r struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("searxng: response is not the expected JSON (is format=json enabled on the instance?): %w", err)
	}
	out := make([]Result, 0, len(r.Results))
	for _, x := range r.Results {
		if x.URL == "" {
			continue
		}
		out = append(out, Result{Title: clean(x.Title), URL: x.URL, Snippet: clean(x.Content)})
		if len(out) == n {
			break
		}
	}
	return out, nil
}

// ---- DuckDuckGo (HTML endpoint, no key) -----------------------------------

type duckduckgo struct {
	base   string
	client *http.Client
}

func (d *duckduckgo) Name() string { return "duckduckgo" }

func (d *duckduckgo) Search(ctx context.Context, q string, n int) ([]Result, error) {
	u := d.base + "?q=" + url.QueryEscape(q)
	status, body, err := get(ctx, d.client, u, nil)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: %w", err)
	}
	if err := statusError("duckduckgo", status, body); err != nil {
		return nil, err
	}
	if looksBlocked(body) {
		return nil, fmt.Errorf("duckduckgo: %w - set brave_api_key or searxng_url in [search] for a keyed backend", ErrBlocked)
	}
	return parseDuckDuckGo(body, n)
}

var (
	ddgTitle   = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	ddgTags    = regexp.MustCompile(`<[^>]+>`)
)

// parseDuckDuckGo reads the HTML endpoint's result blocks. Each organic result
// is a `result__body` block carrying a `result__a` link (title + redirect URL
// with the real target in `uddg=`) and a `result__snippet`. Ad blocks are
// skipped. A page that parses to nothing is an ERROR unless it carries DDG's
// own no-results marker -- the layout changing underneath must not masquerade
// as an empty answer.
func parseDuckDuckGo(body []byte, n int) ([]Result, error) {
	s := string(body)
	blocks := strings.Split(s, "result__body")
	out := make([]Result, 0, n)
	for _, b := range blocks[1:] {
		if strings.Contains(b, "result--ad") || strings.Contains(b, "result__sponsored") {
			continue
		}
		m := ddgTitle.FindStringSubmatch(b)
		if m == nil {
			continue
		}
		link := html.UnescapeString(m[1])
		if i := strings.Index(link, "uddg="); i >= 0 {
			target := link[i+len("uddg="):]
			if j := strings.IndexByte(target, '&'); j >= 0 {
				target = target[:j]
			}
			if dec, err := url.QueryUnescape(target); err == nil {
				link = dec
			}
		} else if strings.HasPrefix(link, "//") {
			link = "https:" + link
		}
		r := Result{Title: clean(m[2]), URL: link}
		if sm := ddgSnippet.FindStringSubmatch(b); sm != nil {
			r.Snippet = clean(sm[1])
		}
		out = append(out, r)
		if len(out) == n {
			break
		}
	}
	if len(out) == 0 {
		if strings.Contains(s, "no-results") || strings.Contains(s, "No results.") {
			return out, nil
		}
		return nil, errors.New("duckduckgo: no result blocks found in the page (layout changed, or an unrecognized interstitial) - try again or set brave_api_key / searxng_url")
	}
	return out, nil
}

// clean strips tags, unescapes entities and collapses whitespace.
func clean(s string) string {
	s = ddgTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// Format renders results as the numbered text the tool returns.
func Format(query string, rs []Result) string {
	if len(rs) == 0 {
		return fmt.Sprintf("No results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q:\n", len(rs), query)
	for i, r := range rs {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return b.String()
}

// Failing is a Backend whose every search fails with err -- used when the
// [search] config is unusable (missing key, unknown provider, off) so the tool
// still exists and each call tells the model exactly what to fix.
func Failing(err error) Backend { return failing{err} }

type failing struct{ err error }

func (f failing) Name() string { return "config" }
func (f failing) Search(context.Context, string, int) ([]Result, error) {
	return nil, f.err
}
