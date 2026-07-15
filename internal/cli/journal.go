package cli

import (
	"fmt"
	"strconv"
	"strings"

	"winc/internal/config"
	"winc/internal/journal"
	"winc/internal/paths"
	"winc/internal/router"
	"winc/internal/ui"
)

// applyJournalFlag folds a --journal[=on|off] / --no-journal flag into the
// loaded config (per-run override; winc.toml holds the default).
func applyJournalFlag(cfg *config.Config, flag string) {
	switch flag {
	case "on":
		cfg.Journal.Enabled = true
	case "off":
		cfg.Journal.Enabled = false
	}
}

// reportJournal prints the honest journal status after the router starts: on
// with real numbers, dormant with the reason, or a warning when it was
// requested but the store failed.
func reportJournal(cfg *config.Config, r *router.Router) {
	dir, n, on := r.JournalStatus()
	if on {
		ui.Info("journal: on - %d conversation(s), plaintext store at %s", n, dir)
		return
	}
	if !cfg.Journal.Enabled {
		return
	}
	if w := r.JournalDormantWindow(); w > 0 {
		ui.Dim("journal: dormant - a %d-token window needs no virtualization (set [journal] budget_tokens to force)", w)
		return
	}
	ui.Warn("journal: requested but the store failed to open - running without (see winc-journal.log)")
}

func journalStoreDir(cfg *config.Config) string {
	if cfg.Journal.Dir != "" {
		return cfg.Journal.Dir
	}
	return paths.JournalDir()
}

// cmdJournal inspects and manages the context-virtualization store. The store
// is plain files -- this command is the notebook view over them, and `rm` is
// the ONLY deletion path in the product (nothing is ever auto-deleted).
func cmdJournal(args []string) int {
	cfg := loadConfig()
	dir := journalStoreDir(cfg)
	sub := "ls"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
		args = args[1:]
	}
	switch sub {
	case "ls", "list":
		return journalLs(dir)
	case "show":
		return journalShow(dir, args)
	case "rm", "remove":
		return journalRm(dir, args)
	case "path":
		fmt.Println(dir)
		return 0
	default:
		ui.Err("unknown journal subcommand %q", sub)
		ui.Say("  winc journal [ls]               list conversations")
		ui.Say("  winc journal show <id> [--turns a-b]   dump a transcript")
		ui.Say("  winc journal rm <id>            delete a conversation")
		ui.Say("  winc journal path               print the store location")
		return 1
	}
}

func journalLs(dir string) int {
	s, err := journal.Open(dir)
	if err != nil {
		ui.Err("journal: %v", err)
		return 1
	}
	convs := s.List()
	if len(convs) == 0 {
		ui.Say("no conversations yet (store: %s)", dir)
		return 0
	}
	ui.Say("%-22s %6s %8s %9s %-16s %s", "ID", "TURNS", "EVICTED", "SIZE", "LAST ACTIVE", "TITLE")
	for _, c := range convs {
		last := "-"
		if t := c.LastActive(); !t.IsZero() {
			last = t.Format("2006-01-02 15:04")
		}
		ui.Say("%-22s %6d %8d %9s %-16s %s", c.ID(), c.Len(), c.Evicted(), humanBytes(c.Size()), last, c.Meta().Title)
	}
	return 0
}

func journalShow(dir string, args []string) int {
	var id, turns string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--turns":
			if i+1 < len(args) {
				turns = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--turns="):
			turns = strings.TrimPrefix(a, "--turns=")
		default:
			id = a
		}
	}
	if id == "" {
		ui.Err("usage: winc journal show <id> [--turns a-b]")
		return 1
	}
	s, err := journal.Open(dir)
	if err != nil {
		ui.Err("journal: %v", err)
		return 1
	}
	c := s.Get(normalizeConvID(id))
	if c == nil {
		ui.Err("no conversation %q (see 'winc journal ls')", id)
		return 1
	}
	rows, err := c.Rows()
	if err != nil {
		ui.Err("journal: %v", err)
		return 1
	}
	from, to := 1, len(rows)
	if turns != "" {
		if a, b, ok := parseRange(turns, len(rows)); ok {
			from, to = a, b
		} else {
			ui.Err("bad --turns %q (want a-b, 1-based)", turns)
			return 1
		}
	}
	m := c.Meta()
	ui.Say("%s  (%d turns, evicted through %d)  %s", c.ID(), len(rows), m.EvictedThrough, m.Title)
	if m.Summary != "" {
		ui.Dim("summary (turns 1-%d): %s", m.SummaryThrough, m.Summary)
	}
	for _, r := range rows[from-1 : to] {
		marker := " "
		if r.I < m.EvictedThrough {
			marker = "·" // out of the live prompt (recallable)
		}
		ui.Say("%s[%d] %s: %s", marker, r.I+1, r.Role, r.Text)
	}
	return 0
}

func journalRm(dir string, args []string) int {
	if len(args) < 1 {
		ui.Err("usage: winc journal rm <id>")
		return 1
	}
	s, err := journal.Open(dir)
	if err != nil {
		ui.Err("journal: %v", err)
		return 1
	}
	id := normalizeConvID(args[0])
	if err := s.Remove(id); err != nil {
		ui.Err("journal: %v", err)
		return 1
	}
	ui.Good("removed %s", id)
	return 0
}

// normalizeConvID accepts the id with or without the conv- prefix (the
// response header strips it for brevity).
func normalizeConvID(id string) string {
	if strings.HasPrefix(id, "conv-") {
		return id
	}
	return "conv-" + id
}

func parseRange(s string, n int) (from, to int, ok bool) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || a < 1 || b < a {
		return 0, 0, false
	}
	if b > n {
		b = n
	}
	if a > n {
		return 0, 0, false
	}
	return a, b, true
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
