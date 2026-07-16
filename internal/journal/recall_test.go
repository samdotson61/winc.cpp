package journal

import (
	"strings"
	"testing"
)

// buildConv persists a conversation from alternating user/assistant texts and
// advances the eviction pointer.
func buildConv(t *testing.T, texts []string, evict int) *Conv {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := observe(t, s, msgsOf(texts...))
	if res.Conv == nil {
		t.Fatal("conversation did not persist")
	}
	if err := res.Conv.AdvanceEvicted(evict); err != nil {
		t.Fatal(err)
	}
	return res.Conv
}

var convTexts = []string{
	"my locker code is 48213 and my cat is named Tesoro",               // 0 (user)
	"understood: locker 48213, cat Tesoro. anything else today?",       // 1 (assistant)
	"let's talk about the weather in duluth for a while",               // 2
	"duluth is cold in the winter and mild in the summer months",       // 3
	"tell me something interesting about compilers instead",            // 4
	"compilers turn source text into machine code through many passes", // 5
	"now recommend a pasta recipe for tonight please",                  // 6
	"try cacio e pepe: pasta, pecorino, black pepper, nothing else",    // 7
}

func TestRecallFindsEvictedFact(t *testing.T) {
	c := buildConv(t, convTexts, 6) // rows 0-5 evicted, 6-7 live
	snips, err := c.Recall("what is my locker code?", RecallOpts{TopK: 2, MaxTokens: 400, Threshold: 0.1, TotalMsgs: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(snips) == 0 {
		t.Fatal("locker fact must be recalled from the evicted range")
	}
	found := false
	for _, sn := range snips {
		if strings.Contains(sn.Row.Text, "48213") {
			found = true
		}
		if sn.Row.I >= 6 {
			t.Fatalf("live rows must never be recalled: row %d", sn.Row.I)
		}
	}
	if !found {
		t.Fatalf("recall missed the fact: %+v", snips)
	}
}

func TestRecallPairInclusion(t *testing.T) {
	c := buildConv(t, convTexts, 6)
	snips, err := c.Recall("what did we decide about duluth weather?", RecallOpts{TopK: 1, MaxTokens: 400, Threshold: 0.1, TotalMsgs: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(snips) != 2 {
		t.Fatalf("a Q/A pair should ride along with a single pick, got %d: %+v", len(snips), snips)
	}
	if snips[0].Row.I+1 != snips[1].Row.I {
		t.Fatalf("pair must be adjacent, got rows %d,%d", snips[0].Row.I, snips[1].Row.I)
	}
}

func TestRecallChronologicalOrder(t *testing.T) {
	c := buildConv(t, convTexts, 6)
	snips, _ := c.Recall("locker code and duluth weather and compilers", RecallOpts{TopK: 4, MaxTokens: 800, Threshold: 0.01, TotalMsgs: 8})
	for i := 1; i < len(snips); i++ {
		if snips[i-1].Row.I >= snips[i].Row.I {
			t.Fatalf("snippets must render in transcript order: %+v", snips)
		}
	}
}

func TestRecallThresholdGates(t *testing.T) {
	c := buildConv(t, convTexts, 6)
	snips, _ := c.Recall("what is my locker code?", RecallOpts{TopK: 4, MaxTokens: 400, Threshold: 1e9, TotalMsgs: 8})
	if len(snips) != 0 {
		t.Fatalf("an impossible threshold must select nothing: %+v", snips)
	}
	// Off-topic queries score low; a sane threshold keeps them out.
	snips, _ = c.Recall("zzz qqq xxx", RecallOpts{TopK: 4, MaxTokens: 400, Threshold: 0.1, TotalMsgs: 8})
	if len(snips) != 0 {
		t.Fatalf("no-term-overlap query must recall nothing: %+v", snips)
	}
}

func TestRecallTokenCap(t *testing.T) {
	long := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		long = append(long, strings.Repeat("shared marker words padding sentence ", 20)+"tail")
	}
	c := buildConv(t, long, 4)
	// Each row is ~740 chars (~185 tokens); a 200-token cap fits exactly one.
	snips, _ := c.Recall("shared marker words", RecallOpts{TopK: 4, MaxTokens: 200, Threshold: 0.0001, TotalMsgs: 8})
	if len(snips) != 1 {
		t.Fatalf("token cap must bound the selection, got %d snippets", len(snips))
	}
}

func TestRecallNothingEvictedNothingRecalled(t *testing.T) {
	c := buildConv(t, convTexts, 0)
	snips, err := c.Recall("what is my locker code?", RecallOpts{TopK: 4, MaxTokens: 400, Threshold: 0.1, TotalMsgs: 8})
	if err != nil || len(snips) != 0 {
		t.Fatalf("nothing evicted -> nothing recalled: %v %+v", err, snips)
	}
}

func TestRecallRecencyBreaksTies(t *testing.T) {
	// Two near-identical rows; the later one should win on the recency blend.
	texts := []string{
		"the migration plan covers the database schema in detail",
		"acknowledged, first note recorded for later reference",
		"the migration plan covers the database schema in detail",
		"acknowledged, second note recorded for later reference",
		"filler turn to keep the conversation moving along nicely",
		"more filler to push the interesting rows into history",
	}
	c := buildConv(t, texts, 4)
	snips, _ := c.Recall("migration plan database schema", RecallOpts{TopK: 1, MaxTokens: 100, Threshold: 0.01, TotalMsgs: 30})
	if len(snips) == 0 {
		t.Fatal("expected a recall")
	}
	if snips[0].Row.I != 2 {
		t.Fatalf("recency blend should prefer the later duplicate: got row %d", snips[0].Row.I)
	}
}
