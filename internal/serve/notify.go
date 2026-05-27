package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// notifyPayload is the JSON body POSTed to the notify URL on each
// child exit. Kept small on purpose — the operator just wants to
// know which run finished and whether it succeeded; the activity
// log on the per-run page carries the detail.
type notifyPayload struct {
	ID     string `json:"id"`     // "<project>/<slug>"
	Status string `json:"status"` // "exited"
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// makeNotifier returns a function suitable for children.notify. It
// POSTs the payload as JSON with a 5-second timeout and logs (but
// doesn't propagate) any error: the run's exit must not be blocked
// by a flaky webhook.
func makeNotifier(url string, logger io.Writer) func(id string, exitErr error) {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(id string, exitErr error) {
		p := notifyPayload{ID: id, Status: "exited", OK: exitErr == nil}
		if exitErr != nil {
			p.Error = exitErr.Error()
		}
		body, err := json.Marshal(p)
		if err != nil {
			fmt.Fprintf(logger, "serve: notify marshal: %v\n", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(logger, "serve: notify build request: %v\n", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(logger, "serve: notify POST %s: %v\n", url, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			fmt.Fprintf(logger, "serve: notify POST %s: status %d\n", url, resp.StatusCode)
		}
	}
}
