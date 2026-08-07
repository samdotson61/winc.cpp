package router

import (
	"io"
	"strings"
	"testing"
	"time"
)

// slowBody delivers its payload only after a delay -- the prefill-silence shape.
type slowBody struct {
	delay   time.Duration
	payload string
	started time.Time
	read    bool
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.read {
		return 0, io.EOF
	}
	if s.started.IsZero() {
		s.started = time.Now()
	}
	time.Sleep(time.Until(s.started.Add(s.delay)))
	s.read = true
	return copy(p, s.payload), nil
}
func (s *slowBody) Close() error { return nil }

// During upstream silence pings flow; the first real byte stops them for good
// and the payload arrives intact.
func TestPingBodyPingsDuringSilence(t *testing.T) {
	b := newPingBody(&slowBody{delay: 60 * time.Millisecond, payload: "event: message_start\n\n"}, []byte("PING\n\n"))
	b.next = time.Now().Add(10 * time.Millisecond) // test-speed deadlines
	out, err := readAllWithDeadline(t, b, 2*time.Second)
	if err != io.EOF {
		t.Fatalf("stream should end in EOF, got %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "PING\n\n") {
		t.Fatalf("silence must produce pings first, got %q", s)
	}
	if !strings.HasSuffix(s, "event: message_start\n\n") {
		t.Fatalf("payload must arrive intact after pings, got %q", s)
	}
}

// A fast upstream never sees a ping at all.
func TestPingBodyFastUpstreamNoPings(t *testing.T) {
	b := newPingBody(&slowBody{delay: 0, payload: "data: x\n\n"}, []byte("PING\n\n"))
	b.next = time.Now().Add(50 * time.Millisecond)
	out, err := readAllWithDeadline(t, b, 2*time.Second)
	if err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if strings.Contains(string(out), "PING") {
		t.Fatalf("fast upstream must not be pinged, got %q", out)
	}
}

// burstBody delivers many chunks back-to-back then EOF -- the shape that
// exposed the drain bug (buffered tail chunks dropped when EOF raced them,
// message_stop included; the continuation test caught it end to end).
type burstBody struct {
	chunks []string
	i      int
}

func (bb *burstBody) Read(p []byte) (int, error) {
	if bb.i >= len(bb.chunks) {
		return 0, io.EOF
	}
	n := copy(p, bb.chunks[bb.i])
	bb.i++
	return n, nil
}
func (bb *burstBody) Close() error { return nil }

func TestPingBodyDeliversFullTailAtEOF(t *testing.T) {
	b := newPingBody(&burstBody{chunks: []string{"a\n\n", "b\n\n", "c\n\n", "d\n\n", "message_stop\n\n"}}, []byte("PING\n\n"))
	b.next = time.Now().Add(time.Hour) // never ping; the tail is the test
	out, err := readAllWithDeadline(t, b, 2*time.Second)
	if err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if got, want := string(out), "a\n\nb\n\nc\n\nd\n\nmessage_stop\n\n"; got != want {
		t.Fatalf("tail dropped:\n got %q\nwant %q", got, want)
	}
}

func readAllWithDeadline(t *testing.T, r io.ReadCloser, d time.Duration) ([]byte, error) {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var all []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				ch <- res{all, err}
				return
			}
		}
	}()
	select {
	case v := <-ch:
		return v.b, v.err
	case <-time.After(d):
		t.Fatal("read deadline exceeded")
		return nil, nil
	}
}
