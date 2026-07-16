// Package journal is winc's context-virtualization store: one directory per
// conversation, holding a verbatim transcript (JSONL, human-readable -- this
// store IS the digital notebook) plus small metadata. The router evicts old
// turns out of the live prompt into this store and recalls the most relevant
// ones back per request. Files are truth: model-written text (summaries) never
// replaces the verbatim record. Everything here is best-effort from the
// router's point of view -- callers treat every error as "skip journaling for
// this request", never as a reason to fail the request itself.
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// persistThreshold is how many messages a conversation needs before it earns a
// directory on disk. Agent clients fire many one-shot utility requests (title
// generation, safety classification) that would otherwise litter the store
// with single-turn junk; a real dialogue reaches user/assistant/user within
// one exchange. Conversations below the threshold are held in memory only --
// losing one to a restart loses nothing, because the client resends the full
// history and it is re-observed from scratch.
const persistThreshold = 3

// maxPending caps the in-memory holding pen for below-threshold conversations.
const maxPending = 128

// Row is one transcript line: {"i":0,"role":"user","text":"...","ts":"...","h":"..."}.
// Row i corresponds 1:1 to message i of the client's resent history -- eviction
// pointers and recall results are exchanged with the router as these indices.
type Row struct {
	I    int    `json:"i"`
	Role string `json:"role"`
	Text string `json:"text"`
	TS   string `json:"ts"`
	H    string `json:"h"`
}

// Meta is a conversation's meta.json. Chain[i] identifies the history through
// row i, so an incoming request's chain can be prefix-matched without reading
// the transcript.
type Meta struct {
	ID             string `json:"id"`
	Created        string `json:"created"`
	Title          string `json:"title"`
	Chain          Chain  `json:"chain"`
	EvictedThrough int    `json:"evicted_through"` // rows [0,n) are out of the live prompt
	Summary        string `json:"summary"`         // rolling gist of evicted turns (hint, never truth)
	SummaryThrough int    `json:"summary_through"` // rows the summary covers
	ForkedFrom     string `json:"forked_from,omitempty"`
}

// Conv is one conversation: its metadata (always loaded) and its transcript
// rows plus recall index (loaded lazily on first touch). Conv has its own
// lock because the router touches conversations outside the store lock and
// concurrent requests can race on the same conversation (a client retry, a
// parallel utility call); lock order is always Store.mu -> Conv.mu.
type Conv struct {
	id  string
	dir string

	mu         sync.Mutex
	meta       Meta
	rows       []Row
	rowsLoaded bool
	index      *bm25Index
}

func (c *Conv) ID() string  { return c.id }
func (c *Conv) Dir() string { return c.dir }

func (c *Conv) Meta() Meta {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.meta
}

func (c *Conv) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.meta.Chain)
}

func (c *Conv) Evicted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.meta.EvictedThrough
}

type pending struct {
	chain Chain
	msgs  []Msg
	seen  time.Time
}

// Store is the journal root: <dir>/conv-*/. One winc serve per install dir is
// the supported topology (same as the rest of winc), so an in-process mutex is
// the only locking.
type Store struct {
	dir string

	mu    sync.Mutex
	convs map[string]*Conv
	pend  []*pending
}

// Open loads (or creates) the store at dir. A conversation whose meta.json is
// unreadable is renamed aside (conv-X -> conv-X.corrupt) and skipped -- the
// store keeps serving with whatever loads cleanly.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, convs: map[string]*Conv{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "conv-") || strings.Contains(e.Name(), ".corrupt") {
			continue
		}
		cdir := filepath.Join(dir, e.Name())
		var m Meta
		data, rerr := os.ReadFile(filepath.Join(cdir, "meta.json"))
		if rerr != nil || json.Unmarshal(data, &m) != nil || m.ID == "" || len(m.Chain) == 0 {
			quarantine(cdir)
			continue
		}
		if m.EvictedThrough < 0 || m.EvictedThrough > len(m.Chain) {
			m.EvictedThrough = 0 // out-of-range pointer: keep the data, reset the derived state
		}
		s.convs[m.ID] = &Conv{id: m.ID, dir: cdir, meta: m}
	}
	return s, nil
}

// quarantine renames a broken conversation directory aside so the store can
// start fresh without deleting anything (fail-open, nothing lost).
func quarantine(cdir string) {
	for i := 0; i < 10; i++ {
		suffix := ".corrupt"
		if i > 0 {
			suffix = fmt.Sprintf(".corrupt.%d", i)
		}
		if err := os.Rename(cdir, cdir+suffix); err == nil || os.IsNotExist(err) {
			return
		}
	}
}

func (s *Store) Dir() string { return s.dir }

// Count is the number of persisted conversations.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.convs)
}

// Result is what Observe reports back to the router. Conv is nil while a
// conversation is still below the persistence threshold (nothing to evict or
// recall at that size).
type Result struct {
	Conv    *Conv
	NewMsgs int
	Created bool
	Forked  bool
}

// Observe identifies the conversation an incoming history belongs to (longest
// matching chain prefix), ingests any new turns, and returns the conversation.
// Fork/edit semantics: a history that diverges mid-chain gets a new
// conversation seeded with the shared prefix. A history that is a strict
// prefix of a known conversation (a regenerate in flight) reuses it without
// ingesting. No match at all starts a new conversation.
func (s *Store) Observe(msgs []Msg) (*Result, error) {
	if len(msgs) == 0 {
		return &Result{}, nil
	}
	chain := BuildChain(msgs)
	s.mu.Lock()
	defer s.mu.Unlock()

	best, bestLen := s.bestMatch(chain)
	if best != nil {
		// Heal before trusting the match: a torn write (rows appended, meta not
		// yet updated) leaves the chain short; reload extends it from the rows'
		// own hashes so we never re-ingest turns the transcript already holds.
		if err := best.ensureRows(); err != nil {
			return nil, err
		}
		bestLen = matchLen(chain, best.meta.Chain)
	}

	switch {
	case best == nil || bestLen == 0:
		return s.observeNew(msgs, chain)
	case bestLen == len(best.meta.Chain):
		// The known conversation is a prefix of the incoming history: same
		// conversation, possibly extended.
		n := len(msgs) - bestLen
		if n > 0 {
			if err := best.append(msgs[bestLen:], chain[bestLen:]); err != nil {
				return nil, err
			}
		}
		return &Result{Conv: best, NewMsgs: n}, nil
	case bestLen == len(msgs):
		// The incoming history is a strict prefix of the known conversation
		// (client rewound, e.g. a regenerate in flight): reuse it, ingest
		// nothing. If the retry diverges, the next request forks.
		return &Result{Conv: best}, nil
	default:
		// Divergence mid-chain: fork -- new conversation carrying the shared
		// prefix rows, then the divergent tail.
		fork, err := s.fork(best, bestLen, msgs, chain)
		if err != nil {
			return nil, err
		}
		return &Result{Conv: fork, NewMsgs: len(msgs) - bestLen, Created: true, Forked: true}, nil
	}
}

// bestMatch scans persisted conversations for the longest shared chain prefix.
// Chains are deterministic, so two conversations matching at the same length
// are the same lineage -- any winner is correct; prefer the longer match.
func (s *Store) bestMatch(chain Chain) (*Conv, int) {
	var best *Conv
	bestLen := 0
	for _, c := range s.convs {
		if l := matchLen(chain, c.meta.Chain); l > bestLen {
			best, bestLen = c, l
		}
	}
	return best, bestLen
}

// observeNew handles a history with no persisted relative: grow a pending
// conversation until it earns persistence, then materialize it.
func (s *Store) observeNew(msgs []Msg, chain Chain) (*Result, error) {
	// Extend or reuse a pending conversation when possible.
	var bestP *pending
	bestLen := 0
	for _, p := range s.pend {
		if l := matchLen(chain, p.chain); l > bestLen {
			bestP, bestLen = p, l
		}
	}
	if bestP != nil && bestLen == len(bestP.chain) {
		bestP.chain, bestP.msgs, bestP.seen = chain, msgs, time.Now()
	} else if bestP != nil && bestLen == len(msgs) {
		bestP.seen = time.Now() // incoming is a prefix of the pending history: nothing new
	} else {
		bestP = &pending{chain: chain, msgs: msgs, seen: time.Now()}
		s.pend = append(s.pend, bestP)
		s.prunePending()
	}
	if len(bestP.msgs) < persistThreshold {
		return &Result{NewMsgs: len(msgs)}, nil
	}
	c, err := s.create(bestP.msgs, bestP.chain, "")
	if err != nil {
		return nil, err
	}
	s.dropPending(bestP)
	return &Result{Conv: c, NewMsgs: len(msgs), Created: true}, nil
}

func (s *Store) prunePending() {
	if len(s.pend) <= maxPending {
		return
	}
	oldest := 0
	for i, p := range s.pend {
		if p.seen.Before(s.pend[oldest].seen) {
			oldest = i
		}
	}
	s.pend = append(s.pend[:oldest], s.pend[oldest+1:]...)
}

func (s *Store) dropPending(target *pending) {
	for i, p := range s.pend {
		if p == target {
			s.pend = append(s.pend[:i], s.pend[i+1:]...)
			return
		}
	}
}

// newID derives a directory name from the chain root; forks and same-greeting
// collisions get -2, -3, ... suffixes. IDs only need uniqueness -- matching is
// always by chain, never by id.
func (s *Store) newID(chain Chain) string {
	base := "conv-" + chain[0][:12]
	id := base
	for n := 2; ; n++ {
		if _, taken := s.convs[id]; !taken {
			if _, err := os.Stat(filepath.Join(s.dir, id)); os.IsNotExist(err) {
				return id
			}
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

// create materializes a brand-new conversation from a full message list.
func (s *Store) create(msgs []Msg, chain Chain, forkedFrom string) (*Conv, error) {
	id := s.newID(chain)
	cdir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return nil, err
	}
	c := &Conv{
		id: id, dir: cdir, rowsLoaded: true,
		meta: Meta{
			ID:         id,
			Created:    time.Now().Format(time.RFC3339),
			Title:      titleFrom(msgs),
			ForkedFrom: forkedFrom,
		},
	}
	if err := c.append(msgs, chain); err != nil {
		return nil, err
	}
	s.convs[id] = c
	return c, nil
}

// fork creates a new conversation seeded with parent rows [0,k), then ingests
// the divergent tail of the incoming history. Lock order Store.mu (held by
// Observe) -> parent.mu, same as everywhere else.
func (s *Store) fork(parent *Conv, k int, msgs []Msg, chain Chain) (*Conv, error) {
	parent.mu.Lock()
	shared := make([]Msg, k)
	for i, r := range parent.rows[:k] {
		shared[i] = Msg{Role: r.Role, Text: r.Text}
	}
	parent.mu.Unlock()
	c, err := s.create(shared, chain[:k], parent.id)
	if err != nil {
		return nil, err
	}
	if err := c.append(msgs[k:], chain[k:]); err != nil {
		return nil, err
	}
	return c, nil
}

func titleFrom(msgs []Msg) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.Join(strings.Fields(m.Text), " ")
		if len(t) > 60 {
			t = t[:60]
		}
		if t != "" {
			return t
		}
	}
	return "(untitled)"
}

// append writes new rows to transcript.jsonl (one O_APPEND write), then
// extends the chain and rewrites meta.json. Meta is written second so a crash
// between the two leaves rows the chain doesn't cover yet -- which ensureRows
// heals from the rows' own hashes on next load.
func (c *Conv) append(msgs []Msg, chain Chain) error {
	if len(msgs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := len(c.meta.Chain)
	ts := time.Now().Format(time.RFC3339)
	var b strings.Builder
	newRows := make([]Row, len(msgs))
	for i, m := range msgs {
		r := Row{I: start + i, Role: m.Role, Text: m.Text, TS: ts, H: hashMsg(m.Role, m.Text)}
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
		newRows[i] = r
	}
	f, err := os.OpenFile(filepath.Join(c.dir, "transcript.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	c.rows = append(c.rows, newRows...)
	c.meta.Chain = append(c.meta.Chain, chain...)
	return c.writeMeta()
}

// writeMeta persists meta.json atomically (tmp + rename).
func (c *Conv) writeMeta() error {
	data, err := json.MarshalIndent(c.meta, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.dir, "meta.json"))
}

// ensureRows loads the transcript on first touch and reconciles it with the
// chain: rows beyond the chain (torn write) extend the chain from their own
// hashes; a chain beyond the rows is truncated to what the transcript proves.
func (c *Conv) ensureRows() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureRowsLocked()
}

func (c *Conv) ensureRowsLocked() error {
	if c.rowsLoaded {
		return nil
	}
	rows, err := readRows(filepath.Join(c.dir, "transcript.jsonl"))
	if err != nil {
		return err
	}
	c.rows, c.rowsLoaded = rows, true
	if len(rows) > len(c.meta.Chain) {
		prev := ""
		if n := len(c.meta.Chain); n > 0 {
			prev = c.meta.Chain[n-1]
		}
		for _, r := range rows[len(c.meta.Chain):] {
			prev = chainNext(prev, r.H)
			c.meta.Chain = append(c.meta.Chain, prev)
		}
		_ = c.writeMeta() // best-effort persistence of the heal
	} else if len(rows) < len(c.meta.Chain) {
		c.meta.Chain = c.meta.Chain[:len(rows)]
		if c.meta.EvictedThrough > len(rows) {
			c.meta.EvictedThrough = len(rows)
		}
		_ = c.writeMeta()
	}
	return nil
}

// readRows reads transcript.jsonl, stopping at the first malformed line: the
// file is append-only, so everything after a torn line is suspect, and
// stopping keeps row indices aligned with chain positions.
func readRows(path string) ([]Row, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []Row
	rd := bufio.NewReader(f)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var r Row
			if json.Unmarshal(line, &r) != nil || r.I != len(rows) {
				break
			}
			rows = append(rows, r)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// Rows returns the transcript as a snapshot slice (loading it if needed).
// Concurrent appends land beyond the returned header, never inside it.
func (c *Conv) Rows() ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureRowsLocked(); err != nil {
		return nil, err
	}
	return c.rows, nil
}

// AdvanceEvicted moves the eviction pointer forward to n (rows [0,n) are out
// of the live prompt) and extends the recall index over the newly evicted
// rows. The router owns WHERE the pointer lands (plain-user boundary, keep-
// newest floor); the store only refuses to move it backwards or past the end.
func (c *Conv) AdvanceEvicted(n int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= c.meta.EvictedThrough || n > len(c.meta.Chain) {
		return nil
	}
	old := c.meta.EvictedThrough
	c.meta.EvictedThrough = n
	if c.index != nil && c.rowsLoaded {
		c.index.extend(c.rows, old, n)
	}
	return c.writeMeta()
}

// SetSummary records the rolling summary covering rows [0,through).
func (c *Conv) SetSummary(text string, through int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.meta.Summary, c.meta.SummaryThrough = text, through
	return c.writeMeta()
}

// Get returns a persisted conversation by id (nil when unknown).
func (s *Store) Get(id string) *Conv {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convs[id]
}

// List returns persisted conversations sorted by directory mtime, newest first.
func (s *Store) List() []*Conv {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Conv, 0, len(s.convs))
	for _, c := range s.convs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mtime().After(out[j].mtime()) })
	return out
}

func (c *Conv) mtime() time.Time {
	if fi, err := os.Stat(filepath.Join(c.dir, "transcript.jsonl")); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// Size is the transcript's on-disk size in bytes.
func (c *Conv) Size() int64 {
	if fi, err := os.Stat(filepath.Join(c.dir, "transcript.jsonl")); err == nil {
		return fi.Size()
	}
	return 0
}

// LastActive is the transcript's mtime.
func (c *Conv) LastActive() time.Time { return c.mtime() }

// Remove deletes a conversation from disk and memory. The only deletion path
// in the product -- nothing is ever auto-deleted.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[id]
	if !ok {
		return fmt.Errorf("no conversation %q", id)
	}
	if err := os.RemoveAll(c.dir); err != nil {
		return err
	}
	delete(s.convs, id)
	return nil
}
