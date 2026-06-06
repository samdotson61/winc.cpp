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
		time.Sleep(time.Second)
	}
	return false
}
