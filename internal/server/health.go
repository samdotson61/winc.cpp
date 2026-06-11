package server

import (
	"net/http"
	"time"
)

// WaitReady polls baseURL+path until it answers 200 or the timeout elapses. If
// dead() reports the process died, it returns false immediately. Use "/health"
// for a direct llama-server (200 only once the model is fully loaded - "/v1/models"
// answers 200 while still loading, which would launch the agent too early); use
// "/v1/models" for llama-swap, which is ready when listening and loads lazily.
func WaitReady(baseURL, path string, timeout time.Duration, dead func() bool) bool {
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		if dead != nil && dead() {
			return false
		}
		resp, err := c.Get(baseURL + path)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		// A fine-grained poll: the model finishes loading at an unpredictable moment,
		// and a 1-second sleep wasted most of a second per server start -- which adds
		// up across the context ladder's rungs and team mode's head + workers.
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// HealthOK is a single non-blocking health probe: does baseURL+"/health" answer
// 200 right now? Used by the team-mode worker watchdog (WaitReady is for startup;
// this is for the steady state). A generous timeout avoids declaring a worker dead
// just because every slot is busy with slow CPU inference.
func HealthOK(baseURL string) bool {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
