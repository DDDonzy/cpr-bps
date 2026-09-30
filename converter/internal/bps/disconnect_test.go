package bps

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestDownstreamCancelClosesSilentUpstream(t *testing.T) {
	for _, progress := range []bool{false, true} {
		name := "before_progress"
		if progress {
			name = "after_created"
		}
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if progress {
					io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
				}
				http.NewResponseController(w).Flush()
				close(entered)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/bps/responses", bytes.NewReader(encoded(t, envelope())))
			req.Header.Set("X-Cpr-Bps-Gate", testGate)
			done := make(chan struct{})
			go func() {
				defer close(done)
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not entered")
			}
			cancel()
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not canceled")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("client did not finish")
			}
		})
	}
}
