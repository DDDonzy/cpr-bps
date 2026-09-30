package kernel

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestUpstreamFailureEventPreserved(t *testing.T) {
	for _, kind := range []string{"error", "response.failed", "response.cancelled"} {
		t.Run(kind, func(t *testing.T) {
			original := map[string]any{"type": kind, "status": json.Number("429"), "request_id": "upstream-req", "error": map[string]any{"code": "rate_limit_exceeded", "type": "rate_limit_error", "message": "Original upstream diagnostic", "param": "input", "extension": "preserve"}}
			if kind != "error" {
				original["response"] = map[string]any{"id": "resp_failed", "error": original["error"], "status": "failed"}
				delete(original, "error")
			}
			var input strings.Builder
			writeSSE(&input, kind, original)
			s, err := New(scopedSource("function"), t.Name())
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err = s.Relay(strings.NewReader(input.String()), func() {}, func(b []byte) error { _, e := output.Write(b); return e })
			if err == nil {
				t.Fatal("failure became success")
			}
			events := 0
			err = newSSEDecoder().feed(output.Bytes(), func(event, data string) error {
				got, reason := parseRelayObject(data)
				if reason != "" {
					t.Fatal(reason)
				}
				delete(got, "sequence_number")
				if event != kind || !reflect.DeepEqual(got, original) {
					t.Fatalf("upstream event changed: %s", data)
				}
				events++
				return nil
			})
			if err != nil || events != 1 {
				t.Fatalf("events=%d err=%v", events, err)
			}
		})
	}
}
