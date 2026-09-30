package bps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServeLongToolActivity(t *testing.T) {
	if os.Getenv("BPS_LONG_TOOL_FIXTURE") != "1" {
		t.Skip("isolated test-only fixture")
	}
	var mu sync.Mutex
	var request map[string]any
	calls, finished, toolOutputs := 0, 0, 0
	failure := ""
	prefixRoundtrip := false
	var epoch time.Time
	argumentTimes, progressTimes := []float64{}, []float64{}
	pauseStart, pauseEnd := float64(-1), float64(-1)
	pauseSeconds, _ := strconv.Atoi(os.Getenv("BPS_TOOL_PAUSE_SECONDS"))
	chunks := 22
	if pauseSeconds > 0 {
		chunks = 12
	}
	upstream := http.NewServeMux()
	upstream.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"requests": calls, "finished": finished, "toolOutputs": toolOutputs, "failure": failure, "argumentTimes": argumentTimes, "progressTimes": progressTimes, "pauseStart": pauseStart, "pauseEnd": pauseEnd, "pauseSeconds": pauseSeconds, "prefixRoundtrip": prefixRoundtrip})
	})
	upstream.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		current := request
		number := calls
		mu.Unlock()
		complete := false
		if input, ok := current["input"].([]any); ok {
			for _, raw := range input {
				if item, ok := raw.(map[string]any); ok && (item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output") {
					complete = true
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sequence := 0
		emit := func(kind string, data map[string]any) bool {
			data["type"] = kind
			data["sequence_number"] = sequence
			sequence++
			b, _ := json.Marshal(data)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
				return false
			}
			return http.NewResponseController(w).Flush() == nil
		}
		if !complete {
			mu.Lock()
			epoch = time.Now()
			mu.Unlock()
		}
		id := fmt.Sprintf("resp_tool_activity_%d", number)
		emit("response.created", map[string]any{"response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
		emit("response.in_progress", map[string]any{"response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
		if complete {
			mu.Lock()
			toolOutputs++
			prefixRoundtrip = strings.Count(string(upstreamBody), "TOOL_GENERATION_STARTED") == 1
			mu.Unlock()
			item := map[string]any{"id": "msg_fixture_final", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "LONG_TOOL_STREAM_OK", "annotations": []any{}}}}
			emit("response.completed", map[string]any{"response": map[string]any{"id": id, "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14}}})
			return
		}
		notice := map[string]any{"id": "msg_tool_notice", "type": "message", "role": "assistant", "phase": "commentary", "status": "in_progress", "content": []any{}}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": notice})
		part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
		emit("response.content_part.added", map[string]any{"output_index": 0, "item_id": "msg_tool_notice", "content_index": 0, "part": part})
		emit("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_tool_notice", "content_index": 0, "delta": "TOOL_GENERATION_STARTED"})
		emit("response.output_text.done", map[string]any{"output_index": 0, "item_id": "msg_tool_notice", "content_index": 0, "text": "TOOL_GENERATION_STARTED"})
		part["text"] = "TOOL_GENERATION_STARTED"
		emit("response.content_part.done", map[string]any{"output_index": 0, "item_id": "msg_tool_notice", "content_index": 0, "part": part})
		notice["status"], notice["content"] = "completed", []any{part}
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": notice})
		name, code, summary := fixtureShellCall(current)
		if name == "" {
			mu.Lock()
			failure = "no_supported_shell_declaration"
			mu.Unlock()
			emit("error", map[string]any{"error": map[string]any{"code": "fixture_catalog_mismatch", "message": "No supported shell declaration"}})
			return
		}
		args, _ := json.Marshal(map[string]any{"summary": summary, "extended_summary": "{}", "destructive": false, "references": []string{name}, "code": code})
		item := map[string]any{"type": "function_call", "id": "fc_long_fixture", "call_id": "call_long_fixture", "name": "run_officejs", "arguments": "", "status": "in_progress"}
		emit("response.output_item.added", map[string]any{"output_index": 1, "item": item})
		// Steady case: 330 seconds of tool arguments. Pause case: stop all
		// upstream events for 225 seconds midway, then resume the SAME response.
		for i := 0; i < chunks; i++ {
			if pauseSeconds > 0 && i == chunks/2 {
				mu.Lock()
				pauseStart = time.Since(epoch).Seconds()
				mu.Unlock()
				timer := time.NewTimer(time.Duration(pauseSeconds) * time.Second)
				select {
				case <-r.Context().Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				mu.Lock()
				pauseEnd = time.Since(epoch).Seconds()
				mu.Unlock()
			}
			timer := time.NewTimer(15 * time.Second)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			mu.Lock()
			argumentTimes = append(argumentTimes, time.Since(epoch).Seconds())
			mu.Unlock()
			begin, end := len(args)*i/chunks, len(args)*(i+1)/chunks
			if !emit("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_long_fixture", "delta": string(args[begin:end])}) {
				return
			}
		}
		item["arguments"], item["status"] = string(args), "completed"
		emit("response.function_call_arguments.done", map[string]any{"output_index": 1, "item_id": "fc_long_fixture", "arguments": string(args)})
		emit("response.output_item.done", map[string]any{"output_index": 1, "item": item})
		if emit("response.completed", map[string]any{"response": map[string]any{"id": id, "status": "completed", "output": []any{notice, item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}}) {
			mu.Lock()
			finished++
			mu.Unlock()
		}
	})
	go func() { t.Error(http.ListenAndServe("127.0.0.1:19332", upstream)) }()
	b, err := New(testGate)
	if err != nil {
		t.Fatal(err)
	}
	b.responsesURL = "http://127.0.0.1:19332/responses"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		var e Envelope
		if json.Unmarshal(body, &e) != nil {
			http.Error(w, "envelope error", 400)
			return
		}
		mu.Lock()
		request = e.Request
		shapes := []any{}
		var shape func(any, string)
		shape = func(raw any, prefix string) {
			list, _ := raw.([]any)
			for _, raw := range list {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := item["name"].(string)
				entry := map[string]any{"name": prefix + name, "type": item["type"]}
				if p, ok := item["parameters"].(map[string]any); ok {
					entry["parameters"] = p
				}
				if item["type"] == "custom" {
					entry["description"] = item["description"]
					entry["format"] = item["format"]
				}
				shapes = append(shapes, entry)
				if item["type"] == "namespace" {
					shape(item["tools"], prefix+name+".")
				}
			}
		}
		shape(e.Request["tools"], "")
		inputTypes := []any{}
		if input, ok := e.Request["input"].([]any); ok {
			for _, raw := range input {
				if item, ok := raw.(map[string]any); ok {
					inputTypes = append(inputTypes, item["type"])
					if item["type"] == "additional_tools" {
						shape(item["tools"], "")
					}
				}
			}
		}
		data, _ := json.Marshal(map[string]any{"tools": shapes, "inputTypes": inputTypes})
		os.WriteFile("tool-catalog.json", data, 0600)
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		followUp := false
		if input, ok := e.Request["input"].([]any); ok {
			for _, raw := range input {
				if item, ok := raw.(map[string]any); ok && (item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output") {
					followUp = true
				}
			}
		}
		watcher := &toolActivityWatchWriter{ResponseWriter: w, observe: func(data []byte) {
			if !followUp && bytes.Contains(data, []byte("event: response.in_progress\n")) {
				mu.Lock()
				progressTimes = append(progressTimes, time.Since(epoch).Seconds())
				mu.Unlock()
			}
		}}
		b.ServeHTTP(watcher, r)
	})
	t.Fatal(http.ListenAndServe("127.0.0.1:19331", handler))
}

func fixtureShellCall(source map[string]any) (name, code, summary string) {
	command := "printf 'BPS_TOOL_EXECUTED\n' #" + strings.Repeat("x", 20000)
	var walk func(any, string)
	walk = func(raw any, prefix string) {
		tools, _ := raw.([]any)
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			n, _ := tool["name"].(string)
			if tool["type"] == "namespace" {
				walk(tool["tools"], prefix+n+".")
				continue
			}
			if name == "" && tool["type"] == "custom" && n == "exec" {
				description, _ := tool["description"].(string)
				if strings.Contains(description, "tools.exec_command") {
					cmd, _ := json.Marshal("printf 'BPS_TOOL_EXECUTED\n'")
					name = prefix + n
					code = "/*" + strings.Repeat("x", 20000) + "*/text(await tools.exec_command({cmd:" + string(cmd) + "}));"
					summary = "Run client tool " + name
					continue
				}
			}
			if name != "" || tool["type"] != "function" {
				continue
			}
			tail := n
			if i := strings.LastIndex(n, "."); i >= 0 {
				tail = n[i+1:]
			}
			if tail != "exec_command" && tail != "shell_command" && tail != "shell" {
				continue
			}
			parameters, _ := tool["parameters"].(map[string]any)
			props, _ := parameters["properties"].(map[string]any)
			if _, ok := props["cmd"]; ok {
				name, code, summary = prefix+n, command, "cpr.function_cmd/"+prefix+n
				continue
			}
			if prop, ok := props["command"].(map[string]any); ok {
				args := map[string]any{}
				if prop["type"] == "array" {
					args["command"] = []string{"sh", "-lc", command}
				} else {
					args["command"] = command
				}
				if required, ok := parameters["required"].([]any); ok {
					for _, key := range required {
						if key == "workdir" {
							args["workdir"] = "/opt/Scripts/bps-tool-progress-20260929/client"
						}
					}
				}
				encoded, _ := json.Marshal(args)
				name, code, summary = prefix+n, string(encoded), "Run client tool "+prefix+n
			}
		}
	}
	walk(source["tools"], "")
	if input, ok := source["input"].([]any); ok {
		for _, v := range input {
			if item, ok := v.(map[string]any); ok && item["type"] == "additional_tools" {
				walk(item["tools"], "")
			}
		}
	}
	return
}

type toolActivityWatchWriter struct {
	http.ResponseWriter
	observe func([]byte)
}

func (w *toolActivityWatchWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.observe(data[:n])
	}
	return n, err
}
func (w *toolActivityWatchWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
