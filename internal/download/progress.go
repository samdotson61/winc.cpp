package download

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// progress is an io.Writer that renders a 1-second download bar with speed + ETA.
type progress struct {
	total int64
	done  int64
	start time.Time
	last  time.Time
	label string
}

func newProgress(total int64, label string) *progress {
	now := time.Now()
	return &progress{total: total, start: now, last: now.Add(-2 * time.Second), label: label}
}

func (p *progress) Write(b []byte) (int, error) {
	n := len(b)
	p.done += int64(n)
	p.render(false)
	return n, nil
}

func gb(x int64) float64 { return float64(x) / 1e9 }

func (p *progress) render(force bool) {
	now := time.Now()
	if !force && now.Sub(p.last) < time.Second {
		return
	}
	p.last = now
	el := now.Sub(p.start).Seconds()
	var spd float64
	if el > 0 {
		spd = float64(p.done) / el
	}
	if p.total > 0 {
		pct := float64(p.done) / float64(p.total)
		if pct > 1 {
			pct = 1
		}
		const width = 28
		fill := int(float64(width) * pct)
		if fill > width {
			fill = width
		}
		bar := strings.Repeat("#", fill) + strings.Repeat("-", width-fill)
		var eta float64
		if spd > 0 {
			eta = float64(p.total-p.done) / spd
		}
		fmt.Fprintf(os.Stdout, "\r  [%s] %3d%%  %.2f/%.2f GB  %.1f MB/s  ETA %s   ",
			bar, int(pct*100), gb(p.done), gb(p.total), spd/1e6, fmtDur(eta))
	} else {
		fmt.Fprintf(os.Stdout, "\r  %.2f GB  %.1f MB/s   ", gb(p.done), spd/1e6)
	}
}

func (p *progress) finish() {
	p.render(true)
	fmt.Fprintln(os.Stdout)
}

func fmtDur(s float64) string {
	t := int(s)
	h, m, sec := t/3600, (t%3600)/60, t%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d", m, sec)
}
