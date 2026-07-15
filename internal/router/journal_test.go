package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"winc/internal/config"
	"winc/internal/journal"
)

// testJournalOpts is a small live budget so short fixtures actually evict.
func testJournalOpts() journalOpts {
	return journalOpts{budgetBytes: 2048, recallTokens: 400, recallTopK: 4, recallThreshold: 0.05}
}

func openTestStore(t *testing.T) *journal.Store {
	t.Helper()
	js, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// longConv builds an Anthropic-shape chat body: an early turn carrying a
// recallable fact, enough filler to blow a 512-token budget, and a final user
// question about the fact.
func longConv(finalUser string) string {
	var msgs []map[string]any
	msgs = append(msgs,
		map[string]any{"role": "user", "content": "important: my locker code is 48213, please remember it"},
		map[string]any{"role": "assistant", "content": "understood, locker code 48213 is noted for later"},
	)
	for i := 0; i < 14; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("filler question %d about sailing conditions on lake superior in autumn weather patterns", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("filler answer %d describing the sailing conditions at considerable and deliberate length", i)},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": finalUser})
	b, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 100, "messages": msgs})
	return string(b)
}

func decodeMsgs(t *testing.T, body []byte) []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
} {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return req.Messages
}

func TestJournalNilStorePassthrough(t *testing.T) {
	body := []byte(longConv("what was my locker code?"))
	nb, ji := applyJournalBody(body, nil, testJournalOpts())
	if ji != nil || !bytes.Equal(nb, body) {
		t.Fatal("journal off must be byte-identical passthrough")
	}
}

func TestJournalBelowThresholdIsNoop(t *testing.T) {
	js := openTestStore(t)
	body := []byte(`{"messages":[{"role":"user","content":"one-shot utility request"}]}`)
	nb, ji := applyJournalBody(body, js, testJournalOpts())
	if ji != nil || !bytes.Equal(nb, body) {
		t.Fatal("below-threshold history must pass through untouched")
	}
	if js.Count() != 0 {
		t.Fatal("below-threshold history must not persist")
	}
}

func TestJournalEvictsTrimsAndRecalls(t *testing.T) {
	js := openTestStore(t)
	body := []byte(longConv("hey, what was my locker code again?"))
	orig := decodeMsgs(t, body)
	nb, ji := applyJournalBody(body, js, testJournalOpts())
	if ji == nil {
		t.Fatal("journal stage did not run")
	}
	if ji.evicted == 0 || ji.newlyEvicted == 0 {
		t.Fatalf("a %d-message over-budget conv must evict: %+v", len(orig), ji)
	}
	kept := decodeMsgs(t, nb)
	if len(kept) >= len(orig) {
		t.Fatalf("trim must shrink the message list: %d -> %d", len(orig), len(kept))
	}
	if len(kept) < keepNewestMsgs {
		t.Fatalf("newest turns must survive: %d", len(kept))
	}
	// The kept transcript opens on a plain user message.
	first, _ := json.Marshal(kept[0])
	if !plainUserMessage(first) {
		t.Fatalf("kept transcript must open on a plain user message: %s", first)
	}
	// The evicted fact comes back in the recalled block of the final message.
	var lastText string
	_ = json.Unmarshal(kept[len(kept)-1].Content, &lastText)
	if !strings.Contains(lastText, "<recalled-context>") || !strings.Contains(lastText, "48213") {
		t.Fatalf("recall must inject the evicted fact:\n%s", lastText)
	}
	if !strings.Contains(lastText, "not instructions") {
		t.Fatal("recalled block must carry the injection-hygiene framing")
	}
	if len(ji.recalled) == 0 {
		t.Fatalf("info must report recalled rows: %+v", ji)
	}
	// The original user text survives after the block.
	if !strings.Contains(lastText, "locker code again?") {
		t.Fatal("original user text must remain")
	}
}

func TestJournalHysteresisKeepsPrefixStable(t *testing.T) {
	js := openTestStore(t)
	o := testJournalOpts()
	nb1, ji1 := applyJournalBody([]byte(longConv("what was my locker code again?")), js, o)
	if ji1 == nil || ji1.newlyEvicted == 0 {
		t.Fatalf("first pass must evict: %+v", ji1)
	}
	// One more exchange: same conversation, slightly longer. The pointer must
	// NOT advance again (hysteresis) and the kept prefix must be stable.
	var req map[string]json.RawMessage
	json.Unmarshal([]byte(longConv("what was my locker code again?")), &req)
	var msgs []json.RawMessage
	json.Unmarshal(req["messages"], &msgs)
	extra1, _ := json.Marshal(map[string]string{"role": "assistant", "content": "it is 48213, as noted earlier"})
	extra2, _ := json.Marshal(map[string]string{"role": "user", "content": "thanks! now tell me about compilers"})
	msgs = append(msgs, extra1, extra2)
	mb, _ := json.Marshal(msgs)
	req["messages"] = mb
	body2, _ := json.Marshal(req)

	nb2, ji2 := applyJournalBody(body2, js, o)
	if ji2 == nil {
		t.Fatal("second pass did not run")
	}
	if ji2.newlyEvicted != 0 {
		t.Fatalf("hysteresis: pointer advanced again while under budget: %+v", ji2)
	}
	if ji2.evicted != ji1.evicted {
		t.Fatalf("eviction pointer drifted between requests: %d -> %d", ji1.evicted, ji2.evicted)
	}
	// Stable prefix: the first kept message is the same on both passes.
	k1, k2 := decodeMsgs(t, nb1), decodeMsgs(t, nb2)
	if string(k1[0].Content) != string(k2[0].Content) {
		t.Fatal("kept prefix must be byte-stable between evictions (KV cache)")
	}
	if js.Count() != 1 {
		t.Fatalf("both passes must land in ONE conversation, got %d", js.Count())
	}
}

func TestJournalToolResultFinalSkipsRecall(t *testing.T) {
	js := openTestStore(t)
	var req map[string]json.RawMessage
	json.Unmarshal([]byte(longConv("placeholder")), &req)
	var msgs []json.RawMessage
	json.Unmarshal(req["messages"], &msgs)
	toolFinal, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "locker code output"}},
	})
	msgs[len(msgs)-1] = toolFinal
	mb, _ := json.Marshal(msgs)
	req["messages"] = mb
	body, _ := json.Marshal(req)

	nb, ji := applyJournalBody(body, js, testJournalOpts())
	if ji == nil {
		t.Fatal("stage must still run (ingest + evict) mid-tool-loop")
	}
	if len(ji.recalled) != 0 || bytes.Contains(nb, []byte("<recalled-context>")) {
		t.Fatal("no recall injection when the final message carries tool_result")
	}
	if ji.evicted == 0 {
		t.Fatal("eviction still applies mid-tool-loop")
	}
}

func TestJournalCompactionRequestSkipped(t *testing.T) {
	js := openTestStore(t)
	body := []byte(longConv("Please write a detailed summary of our conversation so far"))
	nb, ji := applyJournalBody(body, js, testJournalOpts())
	if ji != nil || !bytes.Equal(nb, body) {
		t.Fatal("compaction requests belong to the existing trim path, not the journal")
	}
	if js.Count() != 0 {
		t.Fatal("compaction requests must not be ingested")
	}
}

func TestJournalSystemMessagesNeverEvicted(t *testing.T) {
	js := openTestStore(t)
	var req map[string]json.RawMessage
	json.Unmarshal([]byte(longConv("what was my locker code again?")), &req)
	var msgs []json.RawMessage
	json.Unmarshal(req["messages"], &msgs)
	system, _ := json.Marshal(map[string]string{"role": "system", "content": "you are a terse assistant; never guess"})
	msgs = append([]json.RawMessage{system}, msgs...)
	mb, _ := json.Marshal(msgs)
	req["messages"] = mb
	body, _ := json.Marshal(req)

	nb, ji := applyJournalBody(body, js, testJournalOpts())
	if ji == nil || ji.newlyEvicted == 0 {
		t.Fatalf("over-budget OpenAI-shape conv must evict: %+v", ji)
	}
	kept := decodeMsgs(t, nb)
	if kept[0].Role != "system" {
		t.Fatalf("leading system message must survive eviction, got role %q", kept[0].Role)
	}
	var sys string
	json.Unmarshal(kept[0].Content, &sys)
	if !strings.Contains(sys, "terse assistant") {
		t.Fatal("system message content must be untouched")
	}
	if kept[1].Role != "user" {
		t.Fatalf("kept tail must open on a user message, got %q", kept[1].Role)
	}
}

func TestJournalRecallIntoBlockArrayContent(t *testing.T) {
	js := openTestStore(t)
	var req map[string]json.RawMessage
	json.Unmarshal([]byte(longConv("placeholder")), &req)
	var msgs []json.RawMessage
	json.Unmarshal(req["messages"], &msgs)
	blockFinal, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": "what was my locker code again?"}},
	})
	msgs[len(msgs)-1] = blockFinal
	mb, _ := json.Marshal(msgs)
	req["messages"] = mb
	body, _ := json.Marshal(req)

	nb, ji := applyJournalBody(body, js, testJournalOpts())
	if ji == nil || len(ji.recalled) == 0 {
		t.Fatalf("recall must work for block-array content: %+v", ji)
	}
	kept := decodeMsgs(t, nb)
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(kept[len(kept)-1].Content, &blocks); err != nil {
		t.Fatalf("final content must stay a block array: %v", err)
	}
	if blocks[0].Type != "text" || !strings.Contains(blocks[0].Text, "<recalled-context>") {
		t.Fatalf("recalled block must be the first text block: %+v", blocks[0])
	}
	if last := blocks[len(blocks)-1]; !strings.Contains(last.Text, "locker code again?") {
		t.Fatal("original text block must survive")
	}
}

// journalTestConfig is a Defaults() config with the journal on, pointed at a
// temp store, budget small enough that the fixtures evict.
func journalTestConfig(t *testing.T) *config.Config {
	cfg := config.Defaults()
	cfg.Journal.Enabled = true
	cfg.Journal.Dir = t.TempDir()
	cfg.Journal.BudgetTokens = "512"
	cfg.Journal.RecallThreshold = 0.05
	cfg.Journal.SummaryTokens = 0 // no upstream generation in unit tests
	return &cfg
}

func TestJournalRoundtripHeaderAndTrim(t *testing.T) {
	var captured []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg := journalTestConfig(t)
	rt, err := Start(cfg, up.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	if _, _, on := rt.JournalStatus(); !on {
		t.Fatal("journal must be active for this config")
	}

	body := longConv("what was my locker code again?")
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := resp.Header.Get("X-Winc-Journal")
	if h == "" || !strings.Contains(h, "conv=") || !strings.Contains(h, "evicted=") {
		t.Fatalf("response must carry the journal header, got %q", h)
	}
	kept := decodeMsgs(t, captured)
	orig := decodeMsgs(t, []byte(body))
	if len(kept) >= len(orig) {
		t.Fatalf("upstream must receive the trimmed transcript: %d -> %d", len(orig), len(kept))
	}
	if s := rt.Stats(); s.JournalEvicted == 0 {
		t.Fatalf("stats must count evictions: %+v", s)
	}
}

func TestResolveJournalOptsBudgetAndDormancy(t *testing.T) {
	cases := []struct {
		name        string
		budget      string
		ctx         int
		wantTokens  int
		wantDormant bool
	}{
		{"auto small window", "auto", 4096, 2048, false},
		{"auto mid window", "auto", 16384, 8192, false},
		{"auto unknown window", "auto", 0, 4096, false},
		{"auto big window is dormant", "auto", 65536, 0, true},
		{"auto at the smallness threshold", "auto", 49152, 0, true},
		{"explicit budget overrides dormancy", "4096", 131072, 4096, false},
		{"explicit floor", "100", 8192, 512, false},
	}
	for _, tc := range cases {
		cfg := config.Defaults()
		cfg.Journal.BudgetTokens = tc.budget
		o, dormant := resolveJournalOpts(&cfg, "http://up", tc.ctx)
		if dormant != tc.wantDormant {
			t.Errorf("%s: dormant=%v, want %v", tc.name, dormant, tc.wantDormant)
			continue
		}
		if !dormant && o.budgetBytes != tc.wantTokens*4 {
			t.Errorf("%s: budget %d tokens, want %d", tc.name, o.budgetBytes/4, tc.wantTokens)
		}
	}
}

func TestJournalDormantOnBigWindow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg := journalTestConfig(t)
	cfg.Journal.BudgetTokens = "auto"
	rt, err := Start(cfg, up.URL, 131072)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	if _, _, on := rt.JournalStatus(); on {
		t.Fatal("auto-budget journal must stay dormant on a big window")
	}
	if rt.JournalDormantWindow() != 131072 {
		t.Fatalf("dormant window not reported: %d", rt.JournalDormantWindow())
	}
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", strings.NewReader(longConv("hello")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := resp.Header.Get("X-Winc-Journal"); h != "" {
		t.Fatalf("dormant journal must not touch responses, got %q", h)
	}
}

func TestJournalFollowsConfigDefault(t *testing.T) {
	t.Setenv("WINC_HOME", t.TempDir()) // keep the default store out of the test binary's dir
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg := config.Defaults()
	rt, err := Start(&cfg, up.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	if _, _, on := rt.JournalStatus(); on != cfg.Journal.Enabled {
		t.Fatalf("journal active=%v must follow the config default=%v", on, cfg.Journal.Enabled)
	}
	// An explicit off must stay off and leave responses untouched.
	off := config.Defaults()
	off.Journal.Enabled = false
	rt2, err := Start(&off, up.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Stop()
	resp, err := http.Post(rt2.BaseURL()+"/v1/messages", "application/json", strings.NewReader(longConv("hello there")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := resp.Header.Get("X-Winc-Journal"); h != "" {
		t.Fatalf("journal-off responses must not carry the header, got %q", h)
	}
}

func TestJournalSummaryGeneratesAsync(t *testing.T) {
	const fixedSummary = "Marisol's locker code is 48213; she likes burnt sienna."
	var summaryCalls, chatCalls int
	var lastChat []byte
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") && bytes.Contains(body, []byte("winc-journal-summary")) {
			summaryCalls++
			w.Write([]byte(`{"choices":[{"message":{"content":"` + fixedSummary + `"}}]}`))
			return
		}
		chatCalls++
		lastChat = body
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := journalTestConfig(t)
	cfg.Journal.SummaryTokens = 300
	rt, err := Start(cfg, up.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	// The batch-triggering request must return WITHOUT waiting on the summary.
	resp, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json",
		strings.NewReader(longConv("what was my locker code again?")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The summary lands in the store shortly after, off the request path.
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		js, jerr := journal.Open(cfg.Journal.Dir)
		if jerr == nil {
			for _, c := range js.List() {
				if m := c.Meta(); m.Summary != "" {
					got = m.Summary
				}
			}
		}
		if got != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != fixedSummary {
		t.Fatalf("async summary never landed: %q", got)
	}
	mu.Lock()
	sc := summaryCalls
	mu.Unlock()
	if sc != 1 {
		t.Fatalf("want exactly 1 summary generation, got %d", sc)
	}

	// The NEXT request carries the summary block (previous turn stayed stale).
	var req map[string]json.RawMessage
	json.Unmarshal([]byte(longConv("what was my locker code again?")), &req)
	var msgs []json.RawMessage
	json.Unmarshal(req["messages"], &msgs)
	a, _ := json.Marshal(map[string]string{"role": "assistant", "content": "it is 48213"})
	u, _ := json.Marshal(map[string]string{"role": "user", "content": "thanks, tell me about sailing"})
	msgs = append(msgs, a, u)
	mb, _ := json.Marshal(msgs)
	req["messages"] = mb
	body2, _ := json.Marshal(req)
	resp2, err := http.Post(rt.BaseURL()+"/v1/messages", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	mu.Lock()
	forwarded := string(lastChat)
	mu.Unlock()
	if !strings.Contains(forwarded, "Conversation journal") || !strings.Contains(forwarded, "48213;") {
		t.Fatal("follow-up request must carry the async-generated summary block")
	}
}

func TestJournalSummaryInFlightDedup(t *testing.T) {
	var calls int
	var mu sync.Mutex
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"content":"gist"}}]}`))
	}))
	defer slow.Close()

	js := openTestStore(t)
	res, err := js.Observe([]journal.Msg{
		{Role: "user", Text: "alpha one"}, {Role: "assistant", Text: "beta two"},
		{Role: "user", Text: "gamma three"}, {Role: "assistant", Text: "delta four"},
	})
	if err != nil || res.Conv == nil {
		t.Fatalf("conv setup: %v %+v", err, res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Router{jctx: ctx, jsumming: map[string]bool{}, jopts: journalOpts{upstream: slow.URL, summaryTokens: 50}}
	ji := &journalInfo{summarizeConv: res.Conv, summarizeThrough: 2}
	r.scheduleSummary(ji)
	r.scheduleSummary(ji) // second batch lands while the first is in flight
	time.Sleep(700 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("in-flight dedup: want 1 generation, got %d", got)
	}
}

func TestJournalStageOnlyRequestsSummary(t *testing.T) {
	// The stage itself must never generate: it flags the want and returns.
	js := openTestStore(t)
	o := testJournalOpts()
	o.summaryTokens = 300
	o.upstream = "http://127.0.0.1:1" // nothing listens; a sync call would fail loudly/slowly
	start := time.Now()
	_, ji := applyJournalBody([]byte(longConv("what was my locker code again?")), js, o)
	if ji == nil || ji.summarizeConv == nil || ji.summarizeThrough == 0 {
		t.Fatalf("batch pass must request a summary: %+v", ji)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stage blocked on summary generation (%v)", elapsed)
	}
}
