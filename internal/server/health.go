package server

import (
	"net/http"
	"time"
)

// WaitReady polls baseURL/v1/models until it answers 200 or the timeout elapses.
// If dead() reports the process died, it returns false immediately.
func WaitReady(baseURL string, timeout time.Duration, dead func() bool) bool {
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		if dead != nil && dead() {
			return false
		}
		resp, err := c.Get(baseURL + "/v1/models")
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
