package bps

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBridgeContinuationKeepsHistoryAndScope(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := calls.Add(1)
		wire, _ := json.Marshal(body)
		if _, ok := body["previous_response_id"]; ok {
			t.Error("parent reference leaked to stateless BPS")
		}
		if n == 2 {
			for _, needle := range []string{"historic-first", "answer-one", "fresh-second"} {
				if !strings.Contains(string(wire), needle) {
					t.Errorf("missing history item %s", needle)
				}
			}
		}
		output := []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer-one", "annotations": []any{}}}}}
		event := map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("response_%d", n), "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 7, "output_tokens": 2}}}
		data, _ := json.Marshal(event)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.completed%[1]cdata: %[2]s%[1]c%[1]c", 10, data)
	})
	e := envelope()
	e.Request["input"] = "historic-first"
	resp := request(t, server.URL, e, testGate)
	first, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(first), "response.completed") {
		t.Fatalf("first failed: %s", first)
	}
	e.Version = 2
	e.Request["previous_response_id"] = "response_1"
	e.Request["input"] = []any{map[string]any{"role": "user", "content": "fresh-second"}}
	e.RequestID = "second-request"
	resp = request(t, server.URL, e, testGate)
	second, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(second), "response.completed") {
		t.Fatalf("continuation failed: %s", second)
	}
	for _, change := range []func(*Envelope){func(e *Envelope) { e.TenantID = "another-key" }, func(e *Envelope) { e.AccountScope = "other-account-scope" }, func(e *Envelope) { e.AccountID = "other-account" }, func(e *Envelope) { e.ConversationID = "other-conversation" }} {
		other := e
		change(&other)
		resp = request(t, server.URL, other, testGate)
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || resp.Header.Get("X-Cpr-Bps-Send-State") != "not_sent" || resp.Header.Get("X-Cpr-Bps-Continuation") != "missing" || !strings.Contains(string(data), "previous_response_not_found") {
			t.Fatalf("scope rejection failed: %d %s", resp.StatusCode, data)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("cache misses made upstream calls: %d", calls.Load())
	}
}

func TestBridgeFailedResponseNeverCommitsHistory(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.failed%[1]cdata: {\"type\":\"response.failed\",\"response\":{\"id\":\"failed-response\",\"status\":\"failed\"}}%[1]c%[1]c", 10)
	})
	e := envelope()
	resp := request(t, server.URL, e, testGate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	e.Version = 2
	e.Request["previous_response_id"] = "failed-response"
	resp = request(t, server.URL, e, testGate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || calls.Load() != 1 {
		t.Fatal("failed history was accepted")
	}
}

func TestCachedToolOutputDoesNotReemitPriorAction(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 2 {
			if !strings.Contains(string(raw), "functions.run_officejs") || !strings.Contains(string(raw), "result_ok") {
				t.Error("cached native tool history not reconstructed")
			}
			fmt.Fprint(w, completed)
			return
		}
		args, _ := json.Marshal(map[string]any{"code": `{"value":7}`, "references": []any{"inspect"}})
		emitEvent(w, "response.completed", map[string]any{"response": map[string]any{"id": "r_cached_tool", "status": "completed", "output": []any{map[string]any{"id": "fc_cached_native", "call_id": "call_cached_native", "type": "function_call", "name": "functions.run_officejs", "arguments": string(args)}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 9}}})
	})
	e := envelope()
	e.Request["input"] = "inspect"
	e.Request["tools"] = []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []any{"value"}, "additionalProperties": false}}}
	response := request(t, server.URL, e, testGate)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var final map[string]any
	for _, line := range strings.Split(string(raw), string(rune(10))) {
		if strings.HasPrefix(line, "data: {") {
			var event map[string]any
			json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
			if event["type"] == "response.completed" {
				final = event["response"].(map[string]any)
			}
		}
	}
	if final == nil {
		t.Fatalf("missing initial completed tool: %s", raw)
	}
	item := final["output"].([]any)[0].(map[string]any)
	e.Version = 2
	e.Request["previous_response_id"] = final["id"]
	e.Request["input"] = []any{map[string]any{"type": "function_call_output", "call_id": item["call_id"], "output": "result_ok"}}
	response = request(t, server.URL, e, testGate)
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if calls.Load() != 2 || response.StatusCode != 200 || !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("cached continuation failed: %s", raw)
	}
	if strings.Contains(string(raw), "call_cached_native") || strings.Contains(string(raw), "function_call_arguments") {
		t.Fatal("old tool action was emitted again")
	}
}
