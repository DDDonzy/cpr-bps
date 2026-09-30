package kernel

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToolArgumentActivityIsVisibleBeforeCompletion(t *testing.T) {
	var output bytes.Buffer
	d := newStreamDelivery("openai", func() {}, func(b []byte) error { _, err := output.Write(b); return err })
	consume := func(v map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(v)
		if err := d.consume(stringValue(v["type"]), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	consume(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_activity", "status": "in_progress", "output": []any{}}})
	before := output.Len()
	consume(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "native_tool", "delta": "DO_NOT_EXECUTE_OR_EXPOSE_PARTIAL_ARGUMENT"})
	if output.Len() == before {
		t.Fatal("real upstream tool generation produced no downstream activity")
	}
	progress := output.Bytes()[before:]
	if !bytes.Contains(progress, []byte("response.in_progress")) {
		t.Fatal("missing non-executing response progress")
	}
	if bytes.Contains(progress, []byte("DO_NOT_EXECUTE")) || bytes.Contains(progress, []byte("function_call")) {
		t.Fatal("unvalidated tool payload exposed")
	}
}

func TestToolProgressRequiresNewUpstreamData(t *testing.T) {
	var out bytes.Buffer
	d := newStreamDelivery("openai", func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	d.clock = func() time.Time { return now }
	d.meta = map[string]any{"id": "resp_clock", "status": "in_progress", "output": []any{}}
	d.committed = true
	if err := d.toolArgumentActivity("one"); err != nil {
		t.Fatal(err)
	}
	initial := out.Len()
	now = now.Add(10 * time.Second)
	if err := d.toolArgumentActivity("two"); err != nil {
		t.Fatal(err)
	}
	if out.Len() != initial {
		t.Fatal("progress was not throttled")
	}
	now = now.Add(10 * time.Minute)
	if out.Len() != initial {
		t.Fatal("silence generated fake progress")
	}
	if err := d.toolArgumentActivity("three"); err != nil {
		t.Fatal(err)
	}
	if d.stats.ToolProgressEvents != 2 || d.stats.ToolArgumentDeltas != 3 {
		t.Fatalf("bad counters: %+v", d.stats)
	}
}

func TestToolActivityDoesNotBypassFinalValidation(t *testing.T) {
	s, err := New(scopedSource("function"), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	native := rawRelayNative("invalid-after-progress", "run", "PRIVATE_INVALID_ARGUMENT", []any{"run"})
	input := mixedSessionStream("hello", native)
	frame := "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"native_tool\",\"delta\":\"PRIVATE_PARTIAL_ARGUMENT\"}\n\n"
	input = strings.Replace(input, "event: response.completed", frame+"event: response.completed", 1)
	var out bytes.Buffer
	err = s.Relay(strings.NewReader(input), func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	if err == nil {
		t.Fatal("invalid final tool accepted")
	}
	if bytes.Contains(out.Bytes(), []byte("PRIVATE_")) || bytes.Contains(out.Bytes(), []byte("event: response.completed")) {
		t.Fatal("unvalidated tool content or completion leaked")
	}
	if s.StreamingStatistics().ToolProgressEvents != 1 {
		t.Fatal("missing activity before rejection")
	}
}
