package kernel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestReasoningCiphertextRolloverPreservesBothRealEvents(t *testing.T) {
	source := map[string]any{"model": DefaultModelID, "input": "hello"}
	s, err := New(source, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"type": "reasoning", "id": "r0", "content": []any{}, "summary": []any{}, "encrypted_content": "opaque-item-done"}
	final := cloneObject(item)
	final["encrypted_content"] = "opaque-terminal"
	var input strings.Builder
	writeSSE(&input, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp0", "status": "in_progress", "output": []any{}}})
	writeSSE(&input, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	writeSSE(&input, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	writeSSE(&input, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp0", "status": "completed", "output": []any{final}}})
	var output bytes.Buffer
	if err = s.Relay(strings.NewReader(input.String()), func() {}, func(b []byte) error { _, err := output.Write(b); return err }); err != nil {
		t.Fatal(err)
	}
	added, done, completed := 0, 0, 0
	decoder := newSSEDecoder()
	if err = decoder.feed(output.Bytes(), func(kind, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var v map[string]any
		json.Unmarshal([]byte(data), &v)
		switch kind {
		case "response.output_item.added":
			added++
		case "response.output_item.done":
			done++
			if objectValue(v["item"])["encrypted_content"] != "opaque-item-done" {
				t.Fatal("item.done rewritten")
			}
		case "response.completed":
			completed++
			r := objectValue(v["response"])
			items := r["output"].([]any)
			if objectValue(items[0])["encrypted_content"] != "opaque-terminal" {
				t.Fatal("terminal ciphertext lost")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if added != 1 || done != 1 || completed != 1 {
		t.Fatalf("duplicated or missing events: %d %d %d", added, done, completed)
	}
}

func TestReasoningRolloverStillRejectsSemanticChanges(t *testing.T) {
	original := map[string]any{"type": "reasoning", "id": "r0", "summary": []any{map[string]any{"type": "summary_text", "text": "visible summary"}}, "content": []any{}, "encrypted_content": "opaque-a"}
	for _, field := range []string{"id", "type", "summary", "content"} {
		t.Run(field, func(t *testing.T) {
			r := &streamedReasoning{id: "r0", done: true, final: original, parts: map[int]*streamedPart{}}
			changed := cloneObject(original)
			changed["encrypted_content"] = "opaque-b"
			switch field {
			case "id":
				changed[field] = "wrong"
			case "type":
				changed[field] = "message"
			default:
				changed[field] = []any{map[string]any{"type": "summary_text", "text": "changed"}}
			}
			if r.validate(changed) == nil {
				t.Fatal("accepted changed semantic field")
			}
		})
	}
}
