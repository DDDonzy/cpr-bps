package kernel

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRealProgressArrivesBeforeTerminalWithoutDuplication(t *testing.T) {
	s, err := New(map[string]any{"model": DefaultModelID, "input": "hello"}, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	delivered := make(chan []byte, 32)
	finished := make(chan error, 1)
	go func() {
		finished <- s.Relay(reader, func() {}, func(b []byte) error { delivered <- bytes.Clone(b); return nil })
	}()
	var output bytes.Buffer
	send := func(kind string, v map[string]any) {
		t.Helper()
		v["type"] = kind
		var b strings.Builder
		writeSSE(&b, kind, v)
		if _, err := io.WriteString(writer, b.String()); err != nil {
			t.Fatal(err)
		}
		select {
		case frame := <-delivered:
			output.Write(frame)
			if !bytes.Contains(frame, []byte("event: "+kind+"\n")) {
				t.Fatalf("unexpected progress %s", frame)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s buffered until completion", kind)
		}
	}
	send("response.created", map[string]any{"response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}})
	send("response.in_progress", map[string]any{"response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}})
	send("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "reason", "type": "reasoning", "summary": []any{}}})
	send("response.reasoning_summary_part.added", map[string]any{"output_index": 0, "item_id": "reason", "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	send("response.reasoning_summary_text.delta", map[string]any{"output_index": 0, "item_id": "reason", "summary_index": 0, "delta": "Still working"})
	send("response.reasoning_summary_text.done", map[string]any{"output_index": 0, "item_id": "reason", "summary_index": 0, "text": "Still working"})
	send("response.reasoning_summary_part.done", map[string]any{"output_index": 0, "item_id": "reason", "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": "Still working"}})
	item := map[string]any{"id": "reason", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "Still working"}}, "encrypted_content": "opaque-fixture"}
	send("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	terminal := map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 2}}}
	var b strings.Builder
	writeSSE(&b, "response.completed", terminal)
	io.WriteString(writer, b.String())
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal stalled")
	}
	close(delivered)
	for frame := range delivered {
		output.Write(frame)
	}
	for _, kind := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"} {
		if bytes.Count(output.Bytes(), []byte("event: "+kind+"\n")) != 1 {
			t.Fatalf("duplicate/missing %s", kind)
		}
	}
	sequence := 0
	decoder := newSSEDecoder()
	if err := decoder.feed(output.Bytes(), func(kind, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var v map[string]any
		json.Unmarshal([]byte(data), &v)
		if v["sequence_number"] != float64(sequence) {
			t.Fatal("sequence discontinuity")
		}
		sequence++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
