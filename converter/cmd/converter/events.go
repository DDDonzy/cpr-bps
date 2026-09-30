package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type mapper struct {
	source   map[string]any
	kinds    map[int]string
	calls    map[int]map[string]string
	started  bool
	terminal bool
}

func newMapper(source map[string]any) *mapper {
	return &mapper{source: source, kinds: map[int]string{}, calls: map[int]map[string]string{}}
}
func obj(v any) map[string]any { x, _ := v.(map[string]any); return x }
func str(v any) string         { s, _ := v.(string); return s }
func integer(v any) int {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}
func (m *mapper) content(index int, kind string, facts *[]any) {
	if _, ok := m.kinds[index]; !ok {
		m.kinds[index] = kind
		*facts = append(*facts, map[string]any{"type": "content_added", "index": index, "kind": kind})
	}
}
func (m *mapper) frames(raw []byte) ([]map[string]any, error) {
	frames := bytes.Split(raw, []byte("\n\n"))
	out := []map[string]any{}
	for _, fragment := range frames {
		if len(bytes.TrimSpace(fragment)) == 0 {
			continue
		}
		frame := append(append([]byte{}, fragment...), []byte("\n\n")...)
		data := []string{}
		for _, l := range strings.Split(string(fragment), "\n") {
			if strings.HasPrefix(l, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
			}
		}
		if len(data) == 0 {
			continue
		}
		encoded := strings.Join(data, "\n")
		if strings.TrimSpace(encoded) == "[DONE]" {
			continue
		}
		var e map[string]any
		dec := json.NewDecoder(strings.NewReader(encoded))
		dec.UseNumber()
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		facts := []any{}
		kind := str(e["type"])
		index := integer(e["output_index"])
		v := map[string]any{"wire": base64.StdEncoding.EncodeToString(frame), "facts": facts}
		switch kind {
		case "response.created":
			if m.started {
				return nil, fmt.Errorf("duplicate response start")
			}
			m.started = true
			r := obj(e["response"])
			model := str(r["model"])
			if model == "" {
				model = str(m.source["model"])
			}
			facts = append(facts, map[string]any{"type": "started", "id": r["id"], "model": model})
		case "response.output_item.added":
			item := obj(e["item"])
			t := str(item["type"])
			switch t {
			case "message":
				m.content(index, "text", &facts)
			case "reasoning":
				m.content(index, "reasoning", &facts)
			case "function_call", "custom_tool_call", "tool_search_call":
				m.content(index, "tool_call", &facts)
				id := str(item["call_id"])
				if id == "" {
					id = str(item["id"])
				}
				name := str(item["name"])
				if t == "tool_search_call" {
					name = "tool_search"
				}
				m.calls[index] = map[string]string{"id": id, "name": name}
				if id != "" {
					facts = append(facts, map[string]any{"type": "tool_call_delta", "index": index, "id": id, "name": name, "arguments": ""})
				}
			}
		case "response.output_text.delta":
			m.content(index, "text", &facts)
			facts = append(facts, map[string]any{"type": "text_delta", "index": index, "text": str(e["delta"])})
		case "response.reasoning_summary_text.delta":
			m.content(index, "reasoning", &facts)
			facts = append(facts, map[string]any{"type": "reasoning_delta", "index": index, "text": str(e["delta"])})
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			c := m.calls[index]
			if c == nil {
				return nil, fmt.Errorf("tool delta without item")
			}
			facts = append(facts, map[string]any{"type": "tool_call_delta", "index": index, "id": c["id"], "name": nil, "arguments": str(e["delta"])})
		case "response.completed", "response.incomplete":
			if m.terminal {
				return nil, fmt.Errorf("duplicate terminal")
			}
			m.terminal = true
			r := obj(e["response"])
			u := obj(r["usage"])
			if u != nil {
				usage := map[string]any{}
				for _, k := range []string{"input_tokens", "output_tokens", "total_tokens"} {
					if x := u[k]; x != nil {
						usage[k] = x
					}
				}
				if x := obj(u["input_tokens_details"]); x != nil {
					if x["cached_tokens"] != nil {
						usage["cached_tokens"] = x["cached_tokens"]
					}
					if x["cache_write_tokens"] != nil {
						usage["cache_write_tokens"] = x["cache_write_tokens"]
					}
				}
				if x := obj(u["output_tokens_details"]); x != nil && x["reasoning_tokens"] != nil {
					usage["reasoning_tokens"] = x["reasoning_tokens"]
				}
				facts = append(facts, map[string]any{"type": "usage", "usage": usage})
			}
			reason := "stop"
			if len(m.calls) > 0 {
				reason = "tool_call"
			}
			if kind == "response.incomplete" {
				reason = "length"
			}
			model := str(r["model"])
			if model == "" {
				model = str(m.source["model"])
			}
			facts = append(facts, map[string]any{"type": "completed", "id": r["id"], "model": model, "reason": reason})
			v["terminal"] = true
			v["response"] = r
			v["service_tier"] = r["service_tier"]
		case "error", "response.failed", "response.cancelled", "response.canceled":
			er := obj(e["error"])
			if er == nil {
				er = obj(obj(e["response"])["error"])
			}
			code := str(er["code"])
			msg := str(er["message"])
			if msg == "" {
				msg = "Upstream stream terminated without completion"
			}
			failure := map[string]any{"kind": failureKind(code), "status": nil, "retry_after_ms": nil, "message": msg, "code": nil}
			if code != "" {
				failure["code"] = code
			}
			v["failure"] = failure
		}
		v["facts"] = facts
		out = append(out, v)
	}
	return out, nil
}
func failureKind(code string) string {
	switch code {
	case "context_length_exceeded", "invalid_request", "invalid_request_error", "previous_response_not_found":
		return "invalid_request"
	case "rate_limit_exceeded":
		return "rate_limited"
	case "insufficient_quota", "quota_exceeded":
		return "quota_exhausted"
	case "invalid_api_key", "unauthorized":
		return "unauthorized"
	case "request_timeout", "timeout":
		return "timeout"
	}
	return "unavailable"
}
