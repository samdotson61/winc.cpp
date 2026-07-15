package journal

import (
	"crypto/sha256"
	"encoding/hex"
)

// Msg is one chat message reduced to what identity, storage, and recall need:
// the role and the flattened text content (reasoning.ContentText output --
// tool_use/tool_result blocks as their text rendering, images skipped). RawLen
// carries the message's on-the-wire size so eviction can reason in the same
// bytes-based token estimate the router uses everywhere.
type Msg struct {
	Role   string
	Text   string
	RawLen int
}

// Chain is the running prefix-hash chain over a conversation's messages:
// Chain[i] identifies the history through message i. The API is stateless --
// there is no session id -- but the full resent history IS the identity: two
// requests belong to the same conversation iff one's message list is a prefix
// of the other's, which the chain makes an O(1) comparison at any length.
type Chain []string

// hashMsg is the per-message identity hash: role and text, separated by a byte
// that cannot appear in either, so ("user","ab") never collides with ("usera","b").
func hashMsg(role, text string) string {
	h := sha256.Sum256([]byte(role + "\x00" + text))
	return hex.EncodeToString(h[:8])
}

func chainNext(prev, msgHash string) string {
	h := sha256.Sum256([]byte(prev + msgHash))
	return hex.EncodeToString(h[:8])
}

// BuildChain computes the chain for a full message list.
func BuildChain(msgs []Msg) Chain {
	c := make(Chain, len(msgs))
	prev := ""
	for i, m := range msgs {
		prev = chainNext(prev, hashMsg(m.Role, m.Text))
		c[i] = prev
	}
	return c
}

// matchLen returns the length of the longest shared prefix of two chains.
// Chains are deterministic, so agreement at k implies agreement at every
// position below k -- binary search for the divergence point.
func matchLen(a, b Chain) int {
	n := min(len(a), len(b))
	if n == 0 {
		return 0
	}
	if a[n-1] == b[n-1] {
		return n
	}
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		if a[mid] == b[mid] {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
