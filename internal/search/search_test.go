package search

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The live DuckDuckGo HTML page (captured 2026-10-06, trimmed to 3 results):
// titles, the REAL target URLs decoded out of the uddg= redirect, snippets.
func TestParseDuckDuckGo(t *testing.T) {
	rs, err := parseDuckDuckGo(fixture(t, "ddg.html"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(rs), rs)
	}
	if rs[0].URL != "https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md" {
		t.Errorf("redirect not decoded: %q", rs[0].URL)
	}
	if !strings.Contains(rs[0].Title, "llama.cpp") || rs[0].Snippet == "" {
		t.Errorf("title/snippet missing: %+v", rs[0])
	}
	for _, r := range rs {
		if strings.Contains(r.URL, "duckduckgo.com/l/") || strings.Contains(r.Title, "<") || strings.Contains(r.Snippet, "<b>") {
			t.Errorf("raw markup or redirect leaked: %+v", r)
		}
	}
	// n caps the list.
	if rs, _ := parseDuckDuckGo(fixture(t, "ddg.html"), 2); len(rs) != 2 {
		t.Errorf("cap ignored: %d", len(rs))
	}
}

// A challenge page is an ERROR naming the cause -- never "0 results".
func TestDuckDuckGoChallengeIsAnError(t *testing.T) {
	body := fixture(t, "ddg_captcha.html")
	if !looksBlocked(body) {
		t.Fatal("challenge page not recognized")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	d := &duckduckgo{client: srv.Client(), base: srv.URL + "/html/"}
	rs, err := d.Search(context.Background(), "x", 5)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("want ErrBlocked, got rs=%v err=%v", rs, err)
	}
}

// DDG's own "no results" page is a legitimate empty answer; a page with no
// recognizable blocks and no such marker is an error (layout drift).
func TestDuckDuckGoEmptyVsUnparsable(t *testing.T) {
	rs, err := parseDuckDuckGo(fixture(t, "ddg_noresults.html"), 5)
	if err != nil || len(rs) != 0 {
		t.Fatalf("no-results page: rs=%v err=%v", rs, err)
	}
	if _, err := parseDuckDuckGo([]byte("<html><body>something else entirely</body></html>"), 5); err == nil {
		t.Fatal("unparsable page must be an error, not an empty list")
	}
}

func TestParseBrave(t *testing.T) {
	rs, err := parseBrave(fixture(t, "brave.json"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].URL != "https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md" {
		t.Fatalf("bad parse: %+v", rs)
	}
	if strings.Contains(rs[0].Snippet, "<strong>") || strings.Contains(rs[0].Snippet, "&amp;") {
		t.Errorf("markup/entities not cleaned: %q", rs[0].Snippet)
	}
	if _, err := parseBrave([]byte("<html>login</html>"), 5); err == nil {
		t.Error("non-JSON must be an error")
	}
}

func TestParseSearXNG(t *testing.T) {
	rs, err := parseSearXNG(fixture(t, "searxng.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].URL != "https://github.com/ggml-org/llama.cpp" || rs[0].Snippet != "LLM inference in C/C++" {
		t.Fatalf("bad parse: %+v", rs)
	}
}

// HTTP failures name their cause: 429 is the rate limit, 5xx is the status.
func TestHTTPErrorsAreLoud(t *testing.T) {
	for _, c := range []struct {
		status int
		want   error
		text   string
	}{
		{http.StatusTooManyRequests, ErrRateLimited, "429"},
		{http.StatusInternalServerError, nil, "HTTP 500"},
		{http.StatusUnauthorized, nil, "HTTP 401"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte("nope"))
		}))
		b := &brave{key: "k", client: srv.Client(), base: srv.URL}
		rs, err := b.Search(context.Background(), "x", 5)
		srv.Close()
		if err == nil || rs != nil {
			t.Fatalf("status %d: want error, got rs=%v err=%v", c.status, rs, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("status %d: err %v is not %v", c.status, err, c.want)
		}
		if !strings.Contains(err.Error(), c.text) {
			t.Errorf("status %d: error %q does not name the cause %q", c.status, err, c.text)
		}
	}
}

// A hung engine surfaces as a timeout error, not a stalled agent turn.
func TestTimeoutIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	defer srv.Close()
	cl := srv.Client()
	cl.Timeout = 50 * time.Millisecond
	s := &searxng{base: srv.URL, client: cl}
	_, err := s.Search(context.Background(), "x", 5)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
}

func TestResolveProvider(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Provider: "auto"}, "duckduckgo"},
		{Config{}, "duckduckgo"},
		{Config{Provider: "auto", BraveAPIKey: "k"}, "brave"},
		{Config{Provider: "auto", SearXNGURL: "http://x"}, "searxng"},
		{Config{Provider: "auto", BraveAPIKey: "k", SearXNGURL: "http://x"}, "brave"},
		{Config{Provider: "off", BraveAPIKey: "k"}, "off"},
		{Config{Provider: "DuckDuckGo"}, "duckduckgo"},
		{Config{Provider: "brave"}, "brave"}, // explicit but keyless: resolves, New() then fails loudly
	} {
		if got, _ := ResolveProvider(c.cfg); got != c.want {
			t.Errorf("%+v -> %q, want %q", c.cfg, got, c.want)
		}
	}
	if _, err := New(Config{Provider: "brave"}); err == nil {
		t.Error("brave without a key must not construct silently")
	}
	if _, err := New(Config{Provider: "off"}); !errors.Is(err, ErrOff) {
		t.Errorf("off -> %v, want ErrOff", err)
	}
	if _, err := New(Config{Provider: "bing"}); err == nil {
		t.Error("unknown provider must error")
	}
	if !Enabled(Config{}) || Enabled(Config{Provider: "off"}) {
		t.Error("Enabled mismatch")
	}
}

func TestClampN(t *testing.T) {
	for _, c := range [][3]int{{0, 0, 5}, {0, 3, 3}, {7, 5, 7}, {50, 5, 10}, {-1, 8, 8}} {
		if got := ClampN(c[0], c[1]); got != c[2] {
			t.Errorf("ClampN(%d,%d)=%d want %d", c[0], c[1], got, c[2])
		}
	}
}

func TestFormat(t *testing.T) {
	out := Format("q", []Result{{Title: "T", URL: "https://u", Snippet: "S"}})
	for _, w := range []string{"1. T", "https://u", "S", `for "q"`} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in %q", w, out)
		}
	}
	if !strings.Contains(Format("q", nil), "No results") {
		t.Error("empty answer must say so")
	}
}
