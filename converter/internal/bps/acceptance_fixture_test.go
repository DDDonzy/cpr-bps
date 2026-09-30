package bps

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// This test-only executable serves a deterministic upstream and the real bridge.
// It is never included in cmd/bridge or enabled in production builds.
func TestServeAcceptanceFixture(t *testing.T) {
	if os.Getenv("CPR_STREAM_FIXTURE") != "1" {
		t.Skip("isolated acceptance fixture only")
	}
	var mu sync.Mutex
	calls := map[string]int{}
	canceled := map[string]int{}
	completedCalls := map[string]int{}
	up := http.NewServeMux()
	up.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"calls": calls, "canceled": canceled, "completed": completedCalls})
	})
	up.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		scenario := "QUICK"
		for _, s := range []string{"LONG", "CANCEL_HEADER", "CANCEL_STREAM", "HTTP_ERROR", "SSE_ERROR"} {
			if strings.Contains(string(raw), "ACCEPT_"+s) {
				scenario = s
				break
			}
		}
		mu.Lock()
		calls[scenario]++
		number := calls[scenario]
		mu.Unlock()
		ended := false
		defer func() {
			mu.Lock()
			defer mu.Unlock()
			if ended {
				completedCalls[scenario]++
			} else {
				canceled[scenario]++
			}
		}()
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Error("unexpected credential reached fixture")
			http.Error(w, "fixture credentials required", 403)
			return
		}
		if scenario == "CANCEL_HEADER" {
			<-r.Context().Done()
			return
		}
		if scenario == "HTTP_ERROR" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(504)
			io.WriteString(w, `{"error":{"code":"fixture_timeout","message":"original fixture timeout","type":"server_error"}}`)
			ended = true
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(kind string, v map[string]any) bool {
			v["type"] = kind
			b, _ := json.Marshal(v)
			_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
			if err != nil {
				return false
			}
			return http.NewResponseController(w).Flush() == nil
		}
		id := fmt.Sprintf("fixture_%s_%d", scenario, number)
		emit("response.created", map[string]any{"response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
		emit("response.in_progress", map[string]any{"response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
		if scenario == "SSE_ERROR" {
			emit("response.failed", map[string]any{"response": map[string]any{"id": id, "status": "failed", "error": map[string]any{"code": "fixture_stream_failure", "message": "original stream failure", "type": "server_error"}}})
			ended = true
			return
		}
		if scenario == "CANCEL_STREAM" {
			<-r.Context().Done()
			return
		}
		reasonID := "reason_" + id
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": reasonID, "type": "reasoning", "summary": []any{}}})
		emit("response.reasoning_summary_part.added", map[string]any{"output_index": 0, "item_id": reasonID, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
		text := "fixture progress"
		emit("response.reasoning_summary_text.delta", map[string]any{"output_index": 0, "item_id": reasonID, "summary_index": 0, "delta": text})
		if scenario == "LONG" {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for i := 0; i < 11; i++ {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					text += " tick"
					if !emit("response.reasoning_summary_text.delta", map[string]any{"output_index": 0, "item_id": reasonID, "summary_index": 0, "delta": " tick"}) {
						return
					}
				}
			}
		}
		emit("response.reasoning_summary_text.done", map[string]any{"output_index": 0, "item_id": reasonID, "summary_index": 0, "text": text})
		summary := map[string]any{"type": "summary_text", "text": text}
		emit("response.reasoning_summary_part.done", map[string]any{"output_index": 0, "item_id": reasonID, "summary_index": 0, "part": summary})
		reason := map[string]any{"id": reasonID, "type": "reasoning", "summary": []any{summary}}
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": reason})
		args, _ := json.Marshal(map[string]any{"summary": "Run client tool echo_probe", "extended_summary": "Relay the fixture payload to the declared client tool", "destructive": false, "references": []string{"echo_probe"}, "code": `{"value":"ok"}`})
		tool := map[string]any{"id": "fc_" + id, "type": "function_call", "call_id": "call_" + id, "name": "run_officejs", "arguments": string(args)}
		emit("response.completed", map[string]any{"response": map[string]any{"id": id, "status": "completed", "output": []any{reason, tool}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 2}}})
		ended = true
	})
	go func() {
		if err := http.ListenAndServe("127.0.0.1:19332", up); err != nil {
			t.Error(err)
		}
	}()
	b, err := New(testGate)
	if err != nil {
		t.Fatal(err)
	}
	b.responsesURL = "http://127.0.0.1:19332/responses"
	t.Fatal(http.ListenAndServe("127.0.0.1:19331", b))
}
