package journal

import (
	"math"
	"sort"
)

// RecallOpts tunes one recall pass. TotalMsgs is the incoming history length,
// used for the recency blend (how many turns ago an evicted row was).
type RecallOpts struct {
	TopK      int     // selected rows (pairs ride along without counting)
	MaxTokens int     // hard cap on the injected snippet budget (~4 chars/token)
	Threshold float64 // minimum blended score to select anything
	TotalMsgs int
}

// Snippet is one recalled row with its blended score.
type Snippet struct {
	Row   Row
	Score float64
}

// Recall scores the conversation's evicted rows against the query and returns
// the top snippets in chronological order. There is deliberately no "did the
// user reference the past?" detector -- scoring every turn with a threshold
// approximates one for free, and nothing selected means nothing injected.
func (c *Conv) Recall(query string, o RecallOpts) ([]Snippet, error) {
	if o.TopK <= 0 || o.MaxTokens <= 0 || query == "" {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureRowsLocked(); err != nil {
		return nil, err
	}
	evicted := c.meta.EvictedThrough
	if evicted == 0 {
		return nil, nil
	}
	if c.index == nil {
		c.index = newBM25()
		c.index.extend(c.rows, 0, evicted)
	}
	raw := c.index.score(query)
	if len(raw) == 0 {
		return nil, nil
	}

	// Recency blend: a tie-breaker toward recent turns, never a takeover.
	type cand struct {
		row   int
		score float64
	}
	cands := make([]cand, 0, len(raw))
	for row, s := range raw {
		age := float64(o.TotalMsgs - row)
		final := s * (1 + 0.5*math.Exp2(-age/20))
		if final >= o.Threshold {
			cands = append(cands, cand{row, final})
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	est := func(r Row) int { return len(r.Text)/4 + 8 } // +8 for the [turn N · role] label
	selected := map[int]float64{}
	budget := o.MaxTokens
	picks := 0
	for _, cd := range cands {
		if picks >= o.TopK {
			break
		}
		if _, dup := selected[cd.row]; dup {
			continue
		}
		if cost := est(c.rows[cd.row]); cost <= budget {
			selected[cd.row] = cd.score
			budget -= cost
			picks++
			// Pair inclusion: an answer without its question (or vice versa) is
			// often useless -- pull the adjacent partner in when budget allows.
			if pair, ok := pairOf(c.rows, cd.row, evicted); ok {
				if _, dup := selected[pair]; !dup {
					if pcost := est(c.rows[pair]); pcost <= budget {
						selected[pair] = cd.score
						budget -= pcost
					}
				}
			}
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	out := make([]Snippet, 0, len(selected))
	for row, score := range selected {
		out = append(out, Snippet{Row: c.rows[row], Score: score})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Row.I < out[j].Row.I })
	return out, nil
}

// pairOf finds the Q/A partner of a row: the user question before an assistant
// answer, or the assistant answer after a user question. Only within the
// evicted range (live rows are already in the prompt).
func pairOf(rows []Row, i, evicted int) (int, bool) {
	switch rows[i].Role {
	case "assistant":
		if i > 0 && rows[i-1].Role == "user" {
			return i - 1, true
		}
	case "user":
		if i+1 < evicted && rows[i+1].Role == "assistant" {
			return i + 1, true
		}
	}
	return 0, false
}
