package kernel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func mixedSessionStream(finalText string, native map[string]any) string {
	var b strings.Builder
	emit := func(kind string, value map[string]any) { value["type"] = kind; writeSSE(&b, kind, value) }
	emit("response.created", map[string]any{"response": map[string]any{"id": "resp_mixed", "status": "in_progress", "output": []any{}}})
	emit("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "msg_mixed", "role": "assistant", "status": "in_progress", "content": []any{}}})
	emit("response.content_part.added", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_mixed", "part": map[string]any{"type": "output_text", "text": ""}})
	emit("response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_mixed", "delta": "hello"})
	message := map[string]any{"type": "message", "id": "msg_mixed", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": finalText}}}
	emit("response.completed", map[string]any{"response": map[string]any{"id": "resp_mixed", "status": "completed", "output": []any{message, native}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 2}}})
	return b.String()
}

func TestRelayMixedTextAndToolCompletes(t *testing.T) {
	s, err := New(scopedSource("function"), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	native := rawRelayNative("mixed-valid", "run", `{"cmd":"safe"}`, []any{"run"})
	var out bytes.Buffer
	err = s.Relay(strings.NewReader(mixedSessionStream("hello", native)), func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"name":"run"`)) || !bytes.Contains(out.Bytes(), []byte("event: response.completed")) {
		t.Fatal("mixed stream lost tool or completion")
	}
}

func TestRelayToolFailureEmitsStructuredSequencedError(t *testing.T) {
	s, err := New(scopedSource("function"), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	native := rawRelayNative("mixed-invalid", "run", "DO_NOT_LEAK_PAYLOAD", []any{"run"})
	var out bytes.Buffer
	err = s.Relay(strings.NewReader(mixedSessionStream("hello", native)), func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	if err == nil {
		t.Fatal("invalid tool accepted")
	}
	if bytes.Contains(out.Bytes(), []byte("DO_NOT_LEAK_PAYLOAD")) || bytes.Contains(out.Bytes(), []byte("event: response.completed")) {
		t.Fatal("leaked payload or fabricated completion")
	}
	count := 0
	seq := 0
	decoder := newSSEDecoder()
	err = decoder.feed(out.Bytes(), func(kind, data string) error {
		var event map[string]any
		if e := json.Unmarshal([]byte(data), &event); e != nil {
			return e
		}
		if event["sequence_number"] != float64(seq) {
			t.Fatal("nonsequential error framing")
		}
		seq++
		if kind == "error" {
			count++
			nested := objectValue(event["error"])
			if nested["code"] != "invalid_tool_call" || nested["type"] != "invalid_request_error" {
				t.Fatal("missing standard error payload")
			}
		}
		return nil
	})
	if err != nil || count != 1 {
		t.Fatalf("error count=%d err=%v", count, err)
	}
}

func TestInvalidFinalTextCannotPopulateToolHistory(t *testing.T) {
	s, err := New(scopedSource("function"), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	id := "uncommitted-native-call"
	native := rawRelayNative(id, "run", `{"cmd":"safe"}`, []any{"run"})
	err = s.Relay(strings.NewReader(mixedSessionStream("tampered", native)), func() {}, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("mismatched final text accepted")
	}
	if got := rememberedNativeCall(id, stringValue(s.source["_cpr_tool_scope"])); len(got) != 0 {
		t.Fatal("invalid final response polluted history cache")
	}
}

func TestCodeModeCatalogueDoesNotInventOuterShellTools(t *testing.T) {
	source := map[string]any{"input": []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{
		map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}},
		map[string]any{"type": "function", "name": "wait", "parameters": map[string]any{"type": "object"}},
	}}}}}}
	instructions := clientToolProtocolInstructions(source)
	for _, name := range []string{"functions.exec", "functions.wait", "INNER inventory", "Input format"} {
		if !strings.Contains(instructions, name) {
			t.Fatalf("missing catalogue contract %s", name)
		}
	}
	for _, phantom := range []string{"Run client tool exec_command", "Run client tool apply_patch", "Begin Patch"} {
		if strings.Contains(instructions, phantom) {
			t.Fatalf("invented example %s", phantom)
		}
	}
}
