package bps

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestReasoningMappingReachesWire(t *testing.T) {
	for _, tc := range []struct{ in, out string }{{"max", "xhigh"}, {"ultra", "xhigh"}, {"x-high", "xhigh"}, {"extra_high", "xhigh"}, {" XHIGH ", "xhigh"}, {"high", "high"}, {"medium", "medium"}, {"low", "low"}, {"minimal", "low"}, {"none", "low"}} {
		t.Run(tc.in, func(t *testing.T) {
			calls := 0
			_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["reasoning_effort"] != tc.out {
					t.Errorf("wire effort=%v want=%s", body["reasoning_effort"], tc.out)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, completed)
			})
			e := envelope()
			e.Request["reasoning"] = map[string]any{"effort": tc.in}
			resp := request(t, server.URL, e, testGate)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d", resp.StatusCode, calls)
			}
			if e.Request["reasoning"].(map[string]any)["effort"] != tc.in {
				t.Fatal("mutated client request")
			}
		})
	}
}
func TestUnknownEffortNeverSent(t *testing.T) {
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid effort sent upstream") })
	for _, value := range []any{"typo", 123} {
		e := envelope()
		e.Request["reasoning"] = map[string]any{"effort": value}
		resp := request(t, server.URL, e, testGate)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal(resp.StatusCode)
		}
	}
}
