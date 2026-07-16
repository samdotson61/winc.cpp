package journal

import (
	"testing"
)

func TestTokenizeUnicode(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Hello, World!", []string{"hello", "world"}},
		{"¿Dónde está el código 48213?", []string{"dónde", "está", "el", "código", "48213"}},
		{"foo_bar (baz)  ", []string{"foo", "bar", "baz"}},
		{"", nil},
	}
	for _, tc := range cases {
		got := tokenize(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func rowsOf(texts ...string) []Row {
	rows := make([]Row, len(texts))
	role := "user"
	for i, txt := range texts {
		rows[i] = Row{I: i, Role: role, Text: txt}
		if role == "user" {
			role = "assistant"
		} else {
			role = "user"
		}
	}
	return rows
}

func TestBM25RanksRelevantFirst(t *testing.T) {
	rows := rowsOf(
		"my locker code is 48213, please remember it for me",
		"noted, I will remember your locker code",
		"we discussed bunnies and carrots at some length today",
		"the weather in duluth was surprisingly cold this week",
	)
	ix := newBM25()
	ix.extend(rows, 0, len(rows))
	scores := ix.score("what is my locker code?")
	if len(scores) == 0 {
		t.Fatal("no matches for an on-topic query")
	}
	best, bestScore := -1, 0.0
	for row, s := range scores {
		if s > bestScore {
			best, bestScore = row, s
		}
	}
	if best != 0 && best != 1 {
		t.Fatalf("locker-code rows must outrank fillers, best=%d scores=%v", best, scores)
	}
	if _, hit := scores[3]; hit && scores[3] >= bestScore {
		t.Fatalf("weather row must not win a locker query: %v", scores)
	}
}

func TestBM25SkipsShortChunks(t *testing.T) {
	rows := rowsOf("ok", "thanks", "this row is long enough to be indexed properly")
	ix := newBM25()
	ix.extend(rows, 0, len(rows))
	if ix.nDocs != 1 {
		t.Fatalf("short chunks must be skipped: nDocs=%d", ix.nDocs)
	}
	if s := ix.score("ok thanks"); len(s) != 0 {
		t.Fatalf("skipped chunks must not match: %v", s)
	}
}

func TestBM25IncrementalExtend(t *testing.T) {
	rows := rowsOf(
		"the first batch talks about sailing boats on lake superior",
		"and the reply agrees about the sailing conditions",
		"the second batch is entirely about compiler design",
	)
	ix := newBM25()
	ix.extend(rows, 0, 2)
	if s := ix.score("compiler design"); len(s) != 0 {
		t.Fatalf("unindexed rows must not match yet: %v", s)
	}
	ix.extend(rows, 2, 3)
	if s := ix.score("compiler design"); len(s) == 0 {
		t.Fatal("extended rows must match after extend")
	}
}
