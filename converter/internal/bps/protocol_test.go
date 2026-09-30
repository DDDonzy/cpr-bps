package bps

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func emitEvent(w io.Writer, kind string, data map[string]any) {
	data["type"] = kind
	raw, _ := json.Marshal(data)
	fmt.Fprintln(w, "event: "+kind)
	fmt.Fprintln(w, "data: "+string(raw))
	fmt.Fprintln(w)
}
func textPreamble(w io.Writer) {
	emitEvent(w, "response.created", map[string]any{"response": map[string]any{"id": "mock-response", "status": "in_progress", "output": []any{}}})
	emitEvent(w, "response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{}}})
	emitEvent(w, "response.content_part.added", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_1", "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	emitEvent(w, "response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_1", "delta": "early"})
}
func textComplete(w io.Writer) {
	part := map[string]any{"type": "output_text", "text": "early", "annotations": []any{}}
	item := map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{part}}
	response := map[string]any{"id": "mock-response", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 2}}
	emitEvent(w, "response.completed", map[string]any{"response": response})
}
func TestImageUploadUsesCurrentCredentialsAndSameOrigin(t *testing.T) {
	var uploads, responses atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-token-1" {
			t.Error("lost current credential")
		}
		if r.URL.Path == "/attachments" {
			uploads.Add(1)
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			f, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
			} else {
				if header.Filename != "image.png" {
					t.Error("validated image extension was not retained")
				}
				f.Close()
			}
			fmt.Fprint(w, `{"openai_file_id":"file_mock_image"}`)
			return
		}
		responses.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("data:image")) || !bytes.Contains(raw, []byte("file_mock_image")) {
			t.Error("attachment was not rewritten")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completed)
	})
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, im)
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData.Bytes())
	e := envelope()
	e.Request["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": url}, map[string]any{"type": "input_image", "image_url": url}}}}
	r := request(t, server.URL, e, testGate)
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || uploads.Load() != 1 || responses.Load() != 1 || !bytes.Contains(raw, []byte("response.completed")) {
		t.Fatalf("image roundtrip failed %d %s", r.StatusCode, raw)
	}
}
func TestFunctionRelayAndHistoryRoundtrip(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 2 {
			if !bytes.Contains(raw, []byte("functions.run_officejs")) || !bytes.Contains(raw, []byte("result_ok")) {
				t.Error("native history not restored")
			}
			fmt.Fprint(w, completed)
			return
		}
		args, _ := json.Marshal(map[string]any{"code": `{"value":7}`, "references": []any{"inspect"}})
		emitEvent(w, "response.completed", map[string]any{"response": map[string]any{"id": "r_tool", "status": "completed", "output": []any{map[string]any{"id": "fc_native", "call_id": "call_native", "type": "function_call", "name": "functions.run_officejs", "arguments": string(args)}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 9}}})
	})
	e := envelope()
	e.Request["tools"] = []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []any{"value"}, "additionalProperties": false}}}
	r := request(t, server.URL, e, testGate)
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var final map[string]any
	for _, line := range strings.Split(string(raw), string(rune(10))) {
		if strings.HasPrefix(line, "data: {") {
			var event map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
			if event["type"] == "response.completed" {
				final = event["response"].(map[string]any)
			}
		}
	}
	if final == nil {
		t.Fatalf("tool missing: %s", raw)
	}
	item := final["output"].([]any)[0].(map[string]any)
	if item["name"] != "inspect" {
		t.Fatal("native tool escaped")
	}
	e.Request["input"] = []any{map[string]any{"role": "user", "content": "inspect"}, item, map[string]any{"type": "function_call_output", "call_id": item["call_id"], "output": "result_ok"}}
	r = request(t, server.URL, e, testGate)
	raw, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if calls.Load() != 2 || !bytes.Contains(raw, []byte("response.completed")) {
		t.Fatal("roundtrip failed")
	}
}

func TestImageContinuationRestoresAttachmentWithCurrentCredential(t *testing.T) {
	var uploads, responses atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer mock-token-1"
		if responses.Load() > 0 {
			want = "Bearer mock-token-2"
		}
		if r.Header.Get("Authorization") != want {
			t.Error("cached request reused obsolete authorization")
		}
		if r.URL.Path == "/attachments" {
			uploads.Add(1)
			fmt.Fprint(w, `{"openai_file_id":"file_cached_image"}`)
			return
		}
		n := responses.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte("file_cached_image")) || bytes.Contains(raw, []byte("mock-token")) {
			t.Error("image history or credential boundary changed")
		}
		if n == 2 && !bytes.Contains(raw, []byte("followup-image")) {
			t.Error("new image turn missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emitEvent(w, "response.completed", map[string]any{"response": map[string]any{"id": fmt.Sprintf("image_response_%d", n), "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 2}}})
	})
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		t.Fatal(err)
	}
	e := envelope()
	e.Request["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())}}}}
	response := request(t, server.URL, e, testGate)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(raw, []byte("response.completed")) {
		t.Fatalf("initial image failed: %s", raw)
	}
	e.Version = 2
	e.Authorization = "Bearer mock-token-2"
	e.Request["previous_response_id"] = "image_response_1"
	e.Request["input"] = []any{map[string]any{"role": "user", "content": "followup-image"}}
	response = request(t, server.URL, e, testGate)
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || uploads.Load() != 2 || responses.Load() != 2 || !bytes.Contains(raw, []byte("response.completed")) {
		t.Fatalf("image continuation failed: %s", raw)
	}
}
