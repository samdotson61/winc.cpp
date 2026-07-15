package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func observe(t *testing.T, s *Store, msgs []Msg) *Result {
	t.Helper()
	res, err := s.Observe(msgs)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return res
}

func TestObservePendingThenPersist(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// One-shot utility request: below the threshold, never touches disk.
	res := observe(t, s, msgsOf("generate a title"))
	if res.Conv != nil || s.Count() != 0 {
		t.Fatalf("single-message history must stay pending, got conv=%v count=%d", res.Conv, s.Count())
	}
	// A real dialogue (user/assistant/user) earns a directory.
	res = observe(t, s, msgsOf("hola", "¡hola!", "¿me recuerdas?"))
	if res.Conv == nil || !res.Created || s.Count() != 1 {
		t.Fatalf("3-message history must persist: %+v count=%d", res, s.Count())
	}
	rows, err := res.Conv.Rows()
	if err != nil || len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d (%v)", len(rows), err)
	}
	if res.Conv.Meta().Title != "hola" {
		t.Fatalf("title: got %q", res.Conv.Meta().Title)
	}
}

func TestObserveExtendsSameConversation(t *testing.T) {
	s, _ := Open(t.TempDir())
	base := msgsOf("a1", "b2", "c3", "d4")
	first := observe(t, s, base)
	ext := observe(t, s, msgsOf("a1", "b2", "c3", "d4", "e5", "f6"))
	if ext.Conv != first.Conv || ext.Created || ext.Forked {
		t.Fatalf("extension must reuse the conversation: %+v", ext)
	}
	if ext.NewMsgs != 2 || ext.Conv.Len() != 6 {
		t.Fatalf("want 2 new msgs on a 6-long chain, got %d/%d", ext.NewMsgs, ext.Conv.Len())
	}
	// Identical resend: nothing new, same conversation.
	again := observe(t, s, msgsOf("a1", "b2", "c3", "d4", "e5", "f6"))
	if again.Conv != first.Conv || again.NewMsgs != 0 {
		t.Fatalf("identical resend must be a no-op: %+v", again)
	}
}

func TestObserveRegenerateIsPrefixNotFork(t *testing.T) {
	s, _ := Open(t.TempDir())
	full := observe(t, s, msgsOf("a", "b", "c", "d", "e", "f", "g", "h", "i", "j"))
	// The client rewound the last turn (regenerate in flight): strict prefix.
	re := observe(t, s, msgsOf("a", "b", "c", "d", "e", "f", "g", "h", "i"))
	if re.Conv != full.Conv || re.Forked || re.NewMsgs != 0 {
		t.Fatalf("strict prefix must reuse without fork: %+v", re)
	}
	if full.Conv.Len() != 10 {
		t.Fatalf("prefix resend must not truncate the stored chain: %d", full.Conv.Len())
	}
}

func TestObserveEditForks(t *testing.T) {
	s, _ := Open(t.TempDir())
	parent := observe(t, s, msgsOf("a", "b", "c", "d", "e", "f", "g", "h", "i", "j"))
	// Turn 3 (index 2) edited: shared prefix of 2, divergent tail of 8.
	edited := msgsOf("a", "b", "EDITED", "d", "e", "f", "g", "h", "i", "j")
	fork := observe(t, s, edited)
	if !fork.Forked || fork.Conv == parent.Conv {
		t.Fatalf("edited history must fork: %+v", fork)
	}
	if fork.Conv.Meta().ForkedFrom != parent.Conv.ID() {
		t.Fatalf("fork lineage: got %q", fork.Conv.Meta().ForkedFrom)
	}
	rows, _ := fork.Conv.Rows()
	if len(rows) != 10 || rows[1].Text != "b" || rows[2].Text != "EDITED" {
		t.Fatalf("fork must carry shared prefix + divergent tail: %+v", rows)
	}
	prows, _ := parent.Conv.Rows()
	if len(prows) != 10 || prows[2].Text != "c" {
		t.Fatal("fork must not touch the parent")
	}
	if s.Count() != 2 {
		t.Fatalf("want 2 conversations, got %d", s.Count())
	}
}

func TestObserveClientCompactedIsNewConversation(t *testing.T) {
	s, _ := Open(t.TempDir())
	old := observe(t, s, msgsOf("one", "two", "three", "four"))
	// A client-side compaction rewrites history from position 0: no shared prefix.
	nc := observe(t, s, msgsOf("[summary of our chat so far]", "ok", "continue the work"))
	if nc.Conv == old.Conv || nc.Forked {
		t.Fatalf("client-compacted history must start a NEW conversation: %+v", nc)
	}
	if s.Count() != 2 {
		t.Fatalf("want 2 conversations, got %d", s.Count())
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir)
	c1 := observe(t, s1, msgsOf("uno", "dos", "tres", "cuatro"))
	if err := c1.Conv.AdvanceEvicted(1); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 1 {
		t.Fatalf("reopen: want 1 conversation, got %d", s2.Count())
	}
	res := observe(t, s2, msgsOf("uno", "dos", "tres", "cuatro", "cinco"))
	if res.Created || res.NewMsgs != 1 {
		t.Fatalf("reopen must rematch the same conversation: %+v", res)
	}
	if res.Conv.Evicted() != 1 {
		t.Fatalf("eviction pointer must survive reopen: %d", res.Conv.Evicted())
	}
}

func TestCorruptMetaQuarantined(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "conv-deadbeef0000")
	os.MkdirAll(cdir, 0o755)
	os.WriteFile(filepath.Join(cdir, "meta.json"), []byte("{not json"), 0o644)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatalf("corrupt conversation must not load: %d", s.Count())
	}
	if _, err := os.Stat(cdir + ".corrupt"); err != nil {
		t.Fatalf("corrupt conversation must be renamed aside: %v", err)
	}
}

func TestTornWriteHealsFromRows(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir)
	res := observe(t, s1, msgsOf("w", "x", "y", "z"))
	conv := res.Conv

	// Simulate the torn write: a row appended to the transcript whose meta
	// update never landed (chain stays at 4).
	extra := Msg{Role: "user", Text: "orphaned turn"}
	row := Row{I: 4, Role: extra.Role, Text: extra.Text, TS: "t", H: hashMsg(extra.Role, extra.Text)}
	line, _ := json.Marshal(row)
	f, _ := os.OpenFile(filepath.Join(conv.Dir(), "transcript.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(append(line, '\n'))
	f.Close()

	s2, _ := Open(dir)
	full := msgsOf("w", "x", "y", "z")
	full = append(full, extra, Msg{Role: "assistant", Text: "seen"})
	res2 := observe(t, s2, full)
	if res2.Created || res2.Forked {
		t.Fatalf("healed conversation must rematch, not fork: %+v", res2)
	}
	// The orphaned row must NOT be re-ingested: 4 original + 1 healed + 1 new.
	rows, _ := res2.Conv.Rows()
	if len(rows) != 6 {
		t.Fatalf("want 6 rows after heal (no duplicate ingest), got %d", len(rows))
	}
	count := 0
	for _, r := range rows {
		if r.Text == "orphaned turn" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("orphaned turn stored %d times, want exactly 1", count)
	}
}

func TestAdvanceEvictedBounds(t *testing.T) {
	s, _ := Open(t.TempDir())
	c := observe(t, s, msgsOf("a", "b", "c", "d", "e")).Conv
	if err := c.AdvanceEvicted(3); err != nil || c.Evicted() != 3 {
		t.Fatalf("advance to 3: %v / %d", err, c.Evicted())
	}
	c.AdvanceEvicted(2) // backwards: refused
	if c.Evicted() != 3 {
		t.Fatalf("pointer moved backwards to %d", c.Evicted())
	}
	c.AdvanceEvicted(99) // past the end: refused
	if c.Evicted() != 3 {
		t.Fatalf("pointer moved past end to %d", c.Evicted())
	}
}

func TestRemove(t *testing.T) {
	s, _ := Open(t.TempDir())
	c := observe(t, s, msgsOf("a", "b", "c")).Conv
	if err := s.Remove(c.ID()); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 0 {
		t.Fatal("conversation still listed after Remove")
	}
	if _, err := os.Stat(c.Dir()); !os.IsNotExist(err) {
		t.Fatal("conversation directory still on disk after Remove")
	}
	if err := s.Remove("conv-none"); err == nil || !strings.Contains(err.Error(), "no conversation") {
		t.Fatalf("removing unknown id must error, got %v", err)
	}
}

func TestConcurrentObserveRecallEvict(t *testing.T) {
	// A client retry or a parallel utility call can hit the same conversation
	// concurrently; nothing may race (run under -race) or corrupt the chain.
	s, _ := Open(t.TempDir())
	base := msgsOf("alpha start", "beta reply", "gamma question", "delta answer",
		"epsilon question", "zeta answer", "eta question", "theta answer")
	first := observe(t, s, base)
	done := make(chan error, 24)
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 5; i++ {
				ext := append(append([]Msg{}, base...), Msg{Role: "user", Text: "extension"}, Msg{Role: "assistant", Text: "ok"})
				if _, err := s.Observe(ext); err != nil {
					done <- err
					return
				}
				first.Conv.AdvanceEvicted(4)
				if _, err := first.Conv.Recall("gamma question", RecallOpts{TopK: 2, MaxTokens: 200, Threshold: 0.01, TotalMsgs: 10}); err != nil {
					done <- err
					return
				}
				if _, err := first.Conv.Rows(); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(g)
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := first.Conv.Rows()
	if err != nil || len(rows) != first.Conv.Len() {
		t.Fatalf("chain/rows diverged under concurrency: rows=%d chain=%d (%v)", len(rows), first.Conv.Len(), err)
	}
}

func TestTranscriptIsHumanReadableJSONL(t *testing.T) {
	s, _ := Open(t.TempDir())
	c := observe(t, s, msgsOf("¿dónde está mi código?", "aquí: 48213", "gracias")).Conv
	data, err := os.ReadFile(filepath.Join(c.Dir(), "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 JSONL lines, got %d", len(lines))
	}
	var r Row
	if err := json.Unmarshal([]byte(lines[1]), &r); err != nil || r.I != 1 || r.Role != "assistant" || r.Text != "aquí: 48213" {
		t.Fatalf("row 1 not as written: %+v (%v)", r, err)
	}
}
