package bps

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUpstreamHTTPFailureBodyPreserved(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"context_length_exceeded","message":"Original detail","type":"invalid_request_error","param":"input"},"extension":true}`, strings.Repeat("x", 70000)} {
		_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-ID", "upstream-id")
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(422)
			io.WriteString(w, body)
		})
		resp := request(t, server.URL, envelope(), testGate)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(raw) != body || resp.StatusCode != 422 || resp.Header.Get("X-Request-ID") != "upstream-id" || resp.Header.Get("X-Cpr-Bps-Send-State") != "sent" {
			t.Fatalf("upstream response changed: status=%d err=%v", resp.StatusCode, err)
		}
	}
}
