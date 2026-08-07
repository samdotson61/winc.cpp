package router

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// Prefill keepalive: llama-server sends its SSE response headers immediately
// (measured: 10 ms) but the FIRST event only after prompt processing finishes
// -- 44+ seconds for a ~29k-token agent system prompt on a 35B. Claude Code
// treats that first-event silence as a network failure: it aborts the request
// mid-prefill (observed in the server log as a slot released with no eval),
// shows "Waiting for API response - check your network", and walks its backoff
// up to minutes. Anthropic's real API never trips this because its streams
// carry periodic `ping` events. So the router does the same: while the
// upstream body is still silent, ping frames flow to the client -- a real
// Anthropic `ping` event on /v1/messages, a bare SSE comment on OpenAI-compat
// paths (comments are ignored by every SSE parser; OpenAI defines no ping
// event). Pinging stops permanently at the first upstream byte.
const (
	firstPingAfter = 5 * time.Second
	pingEvery      = 10 * time.Second
)

// keepAliveSSE wraps a streaming response body so pings flow during prefill
// silence. Wired into the proxy's ModifyResponse chain; only 200 SSE responses
// are wrapped, so error paths and non-streaming requests are untouched.
func keepAliveSSE(resp *http.Response) {
	if resp.StatusCode != http.StatusOK {
		return
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return
	}
	frame := ": winc keepalive\n\n" // SSE comment -- ignored by all parsers
	if resp.Request != nil && strings.Contains(resp.Request.URL.Path, "/v1/messages") {
		frame = "event: ping\ndata: {\"type\": \"ping\"}\n\n" // the Anthropic stream ping
	}
	resp.Body = newPingBody(resp.Body, []byte(frame))
}

// pingBody pumps the upstream body through a channel so Read can wait on
// EITHER upstream data OR a ping deadline. After the first real byte it
// degrades to a plain passthrough of the already-running pump.
type pingBody struct {
	src     io.ReadCloser
	frame   []byte
	ch      chan []byte
	errc    chan error
	pending []byte
	sawData bool
	err     error
	next    time.Time // when the next ping fires
}

func newPingBody(src io.ReadCloser, frame []byte) *pingBody {
	b := &pingBody{src: src, frame: frame, ch: make(chan []byte, 4), errc: make(chan error, 1), next: time.Now().Add(firstPingAfter)}
	go func() {
		for {
			buf := make([]byte, 32<<10)
			n, err := src.Read(buf)
			if n > 0 {
				b.ch <- buf[:n]
			}
			if err != nil {
				b.errc <- err
				return
			}
		}
	}()
	return b
}

func (b *pingBody) Read(p []byte) (int, error) {
	for {
		if len(b.pending) > 0 {
			n := copy(p, b.pending)
			b.pending = b.pending[n:]
			return n, nil
		}
		// The pump goroutine queues data BEFORE it reports the terminal error,
		// so once err is recorded every buffered chunk must still be delivered
		// before the error surfaces -- draining only one chunk here dropped the
		// tail of a multi-chunk stream (message_stop included; caught by the
		// continuation test).
		if b.err != nil {
			select {
			case data := <-b.ch:
				return b.deliver(p, data)
			default:
				return 0, b.err
			}
		}
		if b.sawData {
			select {
			case data := <-b.ch:
				return b.deliver(p, data)
			case err := <-b.errc:
				b.err = err // loop: drain b.ch, then surface
			}
			continue
		}
		wait := time.Until(b.next)
		if wait <= 0 {
			b.next = time.Now().Add(pingEvery)
			return b.deliver(p, b.frame)
		}
		t := time.NewTimer(wait)
		select {
		case data := <-b.ch:
			t.Stop()
			b.sawData = true // prefill is over; never ping again
			return b.deliver(p, data)
		case err := <-b.errc:
			t.Stop()
			b.err = err // loop: drain any buffered data, then surface
		case <-t.C:
			// loop: fire the ping via the deadline check above
		}
	}
}

func (b *pingBody) deliver(p, data []byte) (int, error) {
	n := copy(p, data)
	if n < len(data) {
		b.pending = data[n:]
	}
	return n, nil
}

func (b *pingBody) Close() error { return b.src.Close() }
