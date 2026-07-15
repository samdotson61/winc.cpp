// journalbench drives a running winc serve with synthetic conversations to measure
// the journal (context virtualization) against its ship gates:
//
//   - needle recall: facts planted at turn 1, probed at controlled distances
//     (scored by substring match on deterministic, temperature-0 output)
//   - TTFT per turn (streaming; time to the first text delta)
//   - forwarded prompt size (from the X-Winc-Journal header when present)
//
// Run one condition per serve: (a) baseline = journal off, (b) trim-only =
// journal on with recall_top_k=0, (c) journal on. Each run is a fresh
// conversation (the session tag in turn 1 keeps chains distinct), so runs
// never contaminate each other's stores.
//
// Usage:
//
//	journalbench -url http://127.0.0.1:8199 -condition journal -distances 10,20,40 -out results.jsonl
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// factSet is one seeded variant: the planted statements, the substring each
// probe is scored on, and three probe phrasings per fact --
//
//	standard: full lexical overlap with the plant (BM25's home turf)
//	partial:  exactly ONE planted content word survives in the probe
//	zero:     no planted content words at all (stopword-tier overlap only) --
//	          lexical retrieval gets nothing; only the rolling summary (or the
//	          live context, in baseline runs) can carry these
type factSet struct {
	plants   []string
	expects  []string
	standard []string
	partial  []string
	zero     []string
}

var factSets = []factSet{
	{ // seed 1
		plants: []string{
			"my full name is Marisol Quintero-Vance", "my locker code is 48213",
			"my favorite color is burnt sienna", "I live in the city of Duluth",
			"my pet iguana is named Tesoro",
		},
		expects: []string{"quintero", "48213", "sienna", "duluth", "tesoro"},
		standard: []string{
			"What is my full name?", "What is my locker code?", "What is my favorite color?",
			"Which city do I live in?", "What is my pet iguana's name?",
		},
		partial: []string{
			"When I introduce myself formally, what name do I give?",
			"What's the code for my gym storage?",
			"Which color did I say I love most?",
			"Which city do I call home?",
			"What's my iguana called?",
		},
		zero: []string{
			"How should I sign a formal letter — who am I?",
			"What's the combination to open my storage at the fitness center?",
			"What shade do I prefer above all others?",
			"Where's home for me — which town?",
			"What do I call my scaly companion?",
		},
	},
	{ // seed 2
		plants: []string{
			"my full name is Theodore Okonkwo-Reyes", "my locker code is 73951",
			"my favorite color is cerulean frost", "I live in the city of Missoula",
			"my pet gecko is named Pistachio",
		},
		expects: []string{"okonkwo", "73951", "cerulean", "missoula", "pistachio"},
		standard: []string{
			"What is my full name?", "What is my locker code?", "What is my favorite color?",
			"Which city do I live in?", "What is my pet gecko's name?",
		},
		partial: []string{
			"When I introduce myself formally, what name do I give?",
			"What's the code for my gym storage?",
			"Which color did I say I love most?",
			"Which city do I call home?",
			"What's my gecko called?",
		},
		zero: []string{
			"How should I sign a formal letter — who am I?",
			"What's the combination to open my storage at the fitness center?",
			"What shade do I prefer above all others?",
			"Where's home for me — which town?",
			"What do I call my scaly companion?",
		},
	},
	{ // seed 3
		plants: []string{
			"my full name is Anneliese Vandermeer", "my locker code is 20647",
			"my favorite color is vermilion", "I live in the city of Tulsa",
			"my pet cockatiel is named Ziggurat",
		},
		expects: []string{"vandermeer", "20647", "vermilion", "tulsa", "ziggurat"},
		standard: []string{
			"What is my full name?", "What is my locker code?", "What is my favorite color?",
			"Which city do I live in?", "What is my pet cockatiel's name?",
		},
		partial: []string{
			"When I introduce myself formally, what name do I give?",
			"What's the code for my gym storage?",
			"Which color did I say I love most?",
			"Which city do I call home?",
			"What's my cockatiel called?",
		},
		zero: []string{
			"How should I sign a formal letter — who am I?",
			"What's the combination to open my storage at the fitness center?",
			"What shade do I prefer above all others?",
			"Where's home for me — which town?",
			"What do I call my feathered companion?",
		},
	},
}

// fillerTopics are deliberately disjoint from every fact's vocabulary, so a
// probe can only be answered from the planted turn, never from filler bleed.
var fillerTopics = []string{
	"the finer points of sourdough starter maintenance and hydration ratios",
	"opening principles in chess and why early queen sorties usually backfire",
	"trail selection for shoulder-season hiking when ridgelines still hold ice",
	"companion planting in a small vegetable garden and which pairings fail",
	"the mechanics of tides and why some coastlines see four tides a day",
	"how bicycle gearing ratios trade cadence against climbing torque",
	"the history of movable type and its effect on regional dialects",
	"why cast iron pans are restored rather than replaced, and how",
	"reading nautical charts and what the depth soundings actually reference",
	"the lifecycle of monarch butterflies across their multi-generation migration",
}

type turnMetric struct {
	Condition string  `json:"condition"`
	Distance  int     `json:"distance"`
	Kind      string  `json:"kind"` // plant | filler | probe
	Index     int     `json:"index"`
	TTFTms    float64 `json:"ttft_ms"`
	TotalMs   float64 `json:"total_ms"`
	SentBytes int     `json:"sent_bytes"`
	JHeader   string  `json:"journal_header,omitempty"`
	Fact      string  `json:"fact,omitempty"`
	Hit       *bool   `json:"hit,omitempty"`
	Answer    string  `json:"answer,omitempty"`
}

type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8199", "serve base URL")
	condition := flag.String("condition", "journal", "label recorded with results (baseline|trim|journal)")
	distancesArg := flag.String("distances", "10,20,40", "filler distances to probe at")
	out := flag.String("out", "", "append JSONL metrics to this file")
	maxAnswer := flag.Int("max-answer", 80, "max_tokens for model replies")
	dump := flag.String("dump", "", "write a ready-to-POST request body (final history + one probe) for cold-TTFT replays")
	replay := flag.String("replay", "", "POST a dumped body once and report TTFT (cold-prefill probe); skips the conversation protocol")
	seed := flag.Int("seed", 1, "fact-set variant (1..3); also shuffles filler topic order")
	probeset := flag.String("probeset", "standard", "probe phrasing: standard | partial | zero")
	verbose := flag.Bool("v", false, "print each turn")
	flag.Parse()

	if *replay != "" {
		body, err := os.ReadFile(*replay)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: %v\n", err)
			os.Exit(1)
		}
		answer, m, err := post(*url, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("cold %s ttft=%.0fms total=%.0fms journal=%q answer=%q\n",
			*condition, m.TTFTms, m.TotalMs, m.JHeader, truncate(answer, 80))
		return
	}

	var distances []int
	for _, s := range strings.Split(*distancesArg, ",") {
		var d int
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &d); err == nil && d > 0 {
			distances = append(distances, d)
		}
	}
	var sink *os.File
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "out: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		sink = f
	}

	set := factSets[(*seed-1+len(factSets))%len(factSets)]
	var probes []string
	switch *probeset {
	case "standard":
		probes = set.standard
	case "partial":
		probes = set.partial
	case "zero":
		probes = set.zero
	default:
		fmt.Fprintf(os.Stderr, "unknown -probeset %q\n", *probeset)
		os.Exit(1)
	}
	exit := 0
	for i, d := range distances {
		dumpPath := ""
		if i == len(distances)-1 {
			dumpPath = *dump // the deepest run's history is the cold-replay fixture
		}
		if err := runConversation(*url, *condition, d, *maxAnswer, *seed, set, probes, *verbose, sink, dumpPath); err != nil {
			fmt.Fprintf(os.Stderr, "d=%d: %v\n", d, err)
			exit = 1
		}
	}
	os.Exit(exit)
}

func runConversation(url, condition string, distance, maxAnswer, seed int, set factSet, probes []string, verbose bool, sink *os.File, dumpPath string) error {
	tag := fmt.Sprintf("%s-d%d-%d", condition, distance, time.Now().UnixNano()%1_000_000)
	var history []msg
	emit := func(m turnMetric) {
		m.Condition, m.Distance = condition, distance
		if sink != nil {
			b, _ := json.Marshal(m)
			sink.Write(append(b, '\n'))
		}
	}

	ask := func(kind string, index int, text string) (string, turnMetric, error) {
		history = append(history, msg{Role: "user", Content: text})
		answer, m, err := send(url, history, maxAnswer)
		if err != nil {
			return "", m, err
		}
		history = append(history, msg{Role: "assistant", Content: answer})
		m.Kind, m.Index = kind, index
		if verbose {
			fmt.Printf("  [%s %d] ttft=%.0fms live=%s\n", kind, index, m.TTFTms, headerField(m.JHeader, "live"))
		}
		return answer, m, nil
	}

	// Turn 1: plant every fact.
	var plant strings.Builder
	plant.WriteString("Hello! For this session please remember these details about me: ")
	for i, p := range set.plants {
		if i > 0 {
			plant.WriteString("; ")
		}
		plant.WriteString(p)
	}
	fmt.Fprintf(&plant, ". Please acknowledge briefly. (session tag %s)", tag)
	if _, m, err := ask("plant", 0, plant.String()); err != nil {
		return err
	} else {
		emit(m)
	}

	// Filler exchanges: long enough that the planted turn ends up evicted well
	// before the probes at the deeper distances. The seed rotates (and, for
	// even seeds, reverses) the topic order so variants exercise different
	// conversation shapes deterministically.
	topics := append([]string{}, fillerTopics...)
	rot := (seed * 3) % len(topics)
	topics = append(topics[rot:], topics[:rot]...)
	if seed%2 == 0 {
		for i, j := 0, len(topics)-1; i < j; i, j = i+1, j-1 {
			topics[i], topics[j] = topics[j], topics[i]
		}
	}
	for i := 0; i < distance; i++ {
		topic := topics[i%len(topics)]
		text := fmt.Sprintf(
			"Filler exchange %d. Tell me, in two or three sentences, something concrete about %s. "+
				"Keep it self-contained and do not reference anything else from our conversation. "+
				"This is padding for a context experiment, so a plain factual answer is perfect.", i+1, topic)
		if _, m, err := ask("filler", i+1, text); err != nil {
			return err
		} else {
			emit(m)
		}
	}

	// Probes: one per fact, scored by substring on that fact's expected token.
	hits := 0
	for i, q := range probes {
		answer, m, err := ask("probe", i, q+" Answer in one short sentence.")
		if err != nil {
			return err
		}
		hit := strings.Contains(strings.ToLower(answer), set.expects[i])
		if hit {
			hits++
		}
		m.Fact, m.Hit, m.Answer = set.expects[i], &hit, truncate(answer, 160)
		emit(m)
		if verbose {
			fmt.Printf("  probe %q -> hit=%v (%s)\n", set.expects[i], hit, truncate(answer, 80))
		}
	}
	fmt.Printf("%s d=%d: recall %d/%d\n", condition, distance, hits, len(probes))

	if dumpPath != "" {
		replay := append(append([]msg{}, history...),
			msg{Role: "user", Content: probes[1] + " Answer in one short sentence."})
		body, err := json.Marshal(map[string]any{
			"model": "journalbench", "max_tokens": 60, "temperature": 0, "stream": true, "messages": replay,
		})
		if err == nil {
			err = os.WriteFile(dumpPath, body, 0o644)
		}
		if err != nil {
			return fmt.Errorf("dump: %w", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}

func headerField(h, key string) string {
	for _, part := range strings.Fields(h) {
		if strings.HasPrefix(part, key+"=") {
			return strings.TrimPrefix(part, key+"=")
		}
	}
	return "-"
}

// send POSTs the history to /v1/messages with streaming and temperature 0,
// returning the assistant text plus timing.
func send(url string, history []msg, maxAnswer int) (string, turnMetric, error) {
	body, err := json.Marshal(map[string]any{
		"model":       "journalbench",
		"max_tokens":  maxAnswer,
		"temperature": 0,
		"stream":      true,
		"messages":    history,
	})
	if err != nil {
		return "", turnMetric{}, err
	}
	return post(url, body)
}

// post sends a prepared body; TTFT is the first TEXT delta (llama-server sends
// the SSE preamble before prefill, so time-to-first-byte would measure nothing).
func post(url string, body []byte) (string, turnMetric, error) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(url, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", turnMetric{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", turnMetric{}, err
	}
	defer resp.Body.Close()
	m := turnMetric{SentBytes: len(body), JHeader: resp.Header.Get("X-Winc-Journal")}
	if resp.StatusCode != http.StatusOK {
		data, _ := bufio.NewReader(resp.Body).ReadString(0)
		return "", m, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(data, 300))
	}

	var text strings.Builder
	ttft := 0.0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Text != "" {
			if ttft == 0 {
				ttft = float64(time.Since(start).Microseconds()) / 1000
			}
			text.WriteString(ev.Delta.Text)
		}
	}
	if err := sc.Err(); err != nil {
		return "", m, err
	}
	m.TTFTms = ttft
	m.TotalMs = float64(time.Since(start).Microseconds()) / 1000
	if text.Len() == 0 {
		return "", m, fmt.Errorf("empty answer (no text deltas)")
	}
	return text.String(), m, nil
}
