package kernel

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryCacheReconstructsToolContinuation(t *testing.T) {
	c := NewHistoryCache()
	input := []any{map[string]any{"role": "user", "content": "read fixture"}}
	output := []any{map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "namespace": "functions", "input": "raw grammar input"}}
	if !c.Remember("tenant/account/conversation/model", "resp_1", input, output) {
		t.Fatal("not stored")
	}
	delta := map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "fixture result"}
	request := map[string]any{"model": "gpt-test", "previous_response_id": "resp_1", "input": []any{delta}, "instructions": "current instructions"}
	got, err := c.Resolve("tenant/account/conversation/model", request)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{input[0], output[0], delta}
	if !reflect.DeepEqual(got["input"], want) {
		t.Fatalf("history changed: %#v", got["input"])
	}
	if _, ok := got["previous_response_id"]; ok {
		t.Fatal("native parent leaked upstream")
	}
	if request["previous_response_id"] != "resp_1" {
		t.Fatal("caller mutated")
	}
	if got["instructions"] != "current instructions" {
		t.Fatal("request options lost")
	}
	for _, scope := range []string{"other tenant", "other account", "other conversation", "other model"} {
		if _, err := c.Resolve(scope, request); err == nil {
			t.Fatal("cross-scope history disclosed")
		}
	}
	if _, err := c.Resolve("tenant/account/conversation/model", map[string]any{"previous_response_id": "unknown", "input": []any{delta}}); err == nil {
		t.Fatal("missing parent treated as fresh turn")
	}
}
func TestHistoryCacheExpiryAndImmutableIds(t *testing.T) {
	now := time.Unix(1, 0)
	c := newHistoryCache(historyLimits{4096, 2048, 4, time.Minute}, func() time.Time { return now })
	if !c.Remember("s", "r", "one", nil) || !c.Remember("s", "r", "one", nil) {
		t.Fatal("identical terminal not idempotent")
	}
	if c.Remember("s", "r", "different", nil) {
		t.Fatal("response identity overwritten")
	}
	if _, err := c.Resolve("s", map[string]any{"previous_response_id": "r"}); err == nil {
		t.Fatal("conflicting response id returned stale history")
	}
	if !c.Remember("s", "fresh", "one", nil) {
		t.Fatal("fresh history rejected")
	}
	now = now.Add(time.Minute)
	if _, err := c.Resolve("s", map[string]any{"previous_response_id": "r"}); err == nil {
		t.Fatal("expired parent accepted")
	}
	if c.bytes != 0 || len(c.entries) != 0 {
		t.Fatal("expired content retained")
	}
}
func TestHistoryCacheMemoryBoundsNeverTruncate(t *testing.T) {
	c := newHistoryCache(historyLimits{200, 150, 1, time.Minute}, time.Now)
	if c.Remember("s", "oversize", strings.Repeat("x", 200), nil) {
		t.Fatal("oversize entry stored")
	}
	if !c.Remember("s", "first", []any{"one"}, nil) || !c.Remember("s", "second", []any{"two"}, nil) {
		t.Fatal("valid entry rejected")
	}
	if _, err := c.Resolve("s", map[string]any{"previous_response_id": "first"}); err == nil {
		t.Fatal("evicted parent accepted")
	}
	got, err := c.Resolve("s", map[string]any{"previous_response_id": "second", "input": []any{"tail"}})
	if err != nil || !reflect.DeepEqual(got["input"], []any{"two", "tail"}) {
		t.Fatalf("entry truncated: %#v %v", got, err)
	}
	if c.bytes > c.limits.totalBytes {
		t.Fatal("cache exceeded global byte budget")
	}
}

func TestHistoryCommitRejectsInconsistentTerminalStatus(t *testing.T) {
	session, err := New(map[string]any{"model": "gpt-test", "input": "fixture"}, "scope")
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	session.OnCompleted(func(map[string]any) { committed = true })
	data := "event: response.completed" + string(rune(10)) + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r_bad\",\"status\":\"incomplete\",\"output\":[]}}" + string([]rune{10, 10})
	err = session.Relay(strings.NewReader(data), func() {}, func([]byte) error { return nil })
	if err == nil || committed {
		t.Fatal("inconsistent terminal was cached")
	}
}

func TestOversizedReusedResponseDoesNotExposeOldHistory(t *testing.T) {
	c := newHistoryCache(historyLimits{200, 150, 2, time.Minute}, time.Now)
	if !c.Remember("scope", "same-id", []any{"old"}, nil) {
		t.Fatal("first response missing")
	}
	if c.Remember("scope", "same-id", strings.Repeat("x", 200), nil) {
		t.Fatal("oversized replacement accepted")
	}
	if _, err := c.Resolve("scope", map[string]any{"previous_response_id": "same-id"}); err == nil {
		t.Fatal("stale response remained usable")
	}
}
