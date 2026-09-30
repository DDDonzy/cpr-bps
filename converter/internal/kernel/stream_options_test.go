package kernel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexStreamOptionsAcceptedWithoutUpstreamLeak(t *testing.T) {
	source := scopedSource("function")
	source["stream_options"] = map[string]any{"reasoning_summary_delivery": "sequential_cutoff"}
	s, err := New(source, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["stream_options"]; exists {
		t.Fatal("transport setting leaked into BPS generation")
	}
	if body["stream"] != true {
		t.Fatal("stream changed")
	}
	native := rawRelayNative("stream-options-valid", "run", `{"cmd":"safe"}`, []any{"run"})
	var out bytes.Buffer
	err = s.Relay(strings.NewReader(mixedSessionStream("hello", native)), func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"name":"run"`)) || !bytes.Contains(out.Bytes(), []byte("response.completed")) {
		t.Fatal("tool or terminal output lost")
	}
	if bytes.Contains(out.Bytes(), []byte("response.reasoning_summary_text.delta")) {
		t.Fatal("unexpected concurrent summary emission")
	}
}

func TestStreamOptionsFailClosedForUnknownOrInvalidModes(t *testing.T) {
	for _, value := range []any{true, "private-value", []any{}, map[string]any{"unknown_private_field": "SECRET"}, map[string]any{"reasoning_summary_delivery": "parallel"}, map[string]any{"reasoning_summary_delivery": true}, map[string]any{"reasoning_summary_delivery": nil}, map[string]any{"reasoning_summary_delivery": map[string]any{}}} {
		s := scopedSource("function")
		s["stream_options"] = value
		_, err := New(s, t.Name())
		if err == nil {
			t.Fatal("invalid option accepted")
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("private value leaked")
		}
	}
	for _, value := range []any{nil, map[string]any{}} {
		if err := validateStreamOptions(value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamOptionsPreserveFinalReasoning(t *testing.T) {
	s, err := New(map[string]any{"model": DefaultModelID, "input": "hello", "stream_options": map[string]any{"reasoning_summary_delivery": "sequential_cutoff"}}, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	final := map[string]any{"id": "resp_reasoning", "status": "completed", "output": []any{map[string]any{"id": "reasoning_id", "type": "reasoning", "encrypted_content": "opaque-test-cipher", "summary": []any{map[string]any{"type": "summary_text", "text": "summary fixture"}}}}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}
	data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": final})
	var out bytes.Buffer
	err = s.Relay(strings.NewReader("event: response.completed\ndata: "+string(data)+"\n\n"), func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("opaque-test-cipher")) || !bytes.Contains(out.Bytes(), []byte("summary fixture")) {
		t.Fatal("final reasoning history lost")
	}
}
