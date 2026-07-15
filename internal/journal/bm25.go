package journal

import (
	"math"
	"strings"
	"unicode"
)

// bm25Index is an in-memory inverted index over a conversation's EVICTED rows
// (live rows are already in the prompt). Built lazily on first recall, updated
// incrementally on eviction; never persisted -- transcript.jsonl is the only
// durable artifact, and at this scale (thousands of short texts) a rebuild is
// milliseconds.
//
// Why BM25 and not embeddings: zero new deps, zero extra RAM, zero extra model
// download -- and on the low-end devices this feature targets, an embedding
// model would mean a second engine process. Embeddings can upgrade the same
// interface later, gated by RAM tier.
type bm25Index struct {
	postings map[string][]posting
	docLen   map[int]int // row -> token count
	totalLen int
	nDocs    int
}

type posting struct {
	row int
	tf  int
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// minChunkChars skips rows too short to be worth indexing ("ok", "thanks").
	minChunkChars = 20
)

// tokenize lowercases and splits on any non-letter/digit rune (Unicode-aware).
// No stemming, no stopword list: BM25's IDF downweights common words naturally,
// which keeps scoring language-neutral (EN/ES) for free.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func newBM25() *bm25Index {
	return &bm25Index{postings: map[string][]posting{}, docLen: map[int]int{}}
}

// extend indexes rows [from,to). Chunk = one message.
func (ix *bm25Index) extend(rows []Row, from, to int) {
	for i := from; i < to && i < len(rows); i++ {
		if len(rows[i].Text) < minChunkChars {
			continue
		}
		toks := tokenize(rows[i].Text)
		if len(toks) == 0 {
			continue
		}
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		for t, n := range tf {
			ix.postings[t] = append(ix.postings[t], posting{row: i, tf: n})
		}
		ix.docLen[i] = len(toks)
		ix.totalLen += len(toks)
		ix.nDocs++
	}
}

// score runs standard BM25 for a query over the index, returning row -> score
// for every row that matched at least one query term.
func (ix *bm25Index) score(query string) map[int]float64 {
	if ix.nDocs == 0 {
		return nil
	}
	avgLen := float64(ix.totalLen) / float64(ix.nDocs)
	scores := map[int]float64{}
	seen := map[string]bool{}
	for _, term := range tokenize(query) {
		if seen[term] {
			continue // repeated query terms don't double-count
		}
		seen[term] = true
		plist := ix.postings[term]
		if len(plist) == 0 {
			continue
		}
		df := float64(len(plist))
		idf := math.Log(1 + (float64(ix.nDocs)-df+0.5)/(df+0.5))
		for _, p := range plist {
			dl := float64(ix.docLen[p.row])
			tf := float64(p.tf)
			scores[p.row] += idf * (tf * (bm25K1 + 1)) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgLen))
		}
	}
	return scores
}
