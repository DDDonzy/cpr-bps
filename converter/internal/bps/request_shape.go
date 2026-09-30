package bps

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// 只输出公开协议字段名，不保存值；未知名称只保留哈希，避免泄露自定义内容。
func safeRequestFields(body map[string]any) []string {
	fields := make([]string, 0, len(body))
	for field := range body {
		switch field {
		case "model", "input", "instructions", "reasoning", "stream", "store", "tools", "tool_choice", "service_tier", "text", "previous_response_id", "max_output_tokens", "metadata", "context_management", "prompt_cache_key", "client_metadata", "parallel_tool_calls", "include", "generate", "type", "background", "conversation", "truncation", "safety_identifier", "prompt_cache_retention", "max_tool_calls", "stream_options", "temperature", "top_p", "user", "prompt", "response_format", "reasoning_effort", "extra_body", "max_tokens", "max_completion_tokens", "logprobs", "top_logprobs", "frequency_penalty", "presence_penalty", "seed", "stop", "messages", "modalities", "audio", "prediction":
			fields = append(fields, field)
		default:
			sum := sha256.Sum256([]byte(field))
			fields = append(fields, fmt.Sprintf("unlisted_sha256_%x", sum[:8]))
		}
	}
	sort.Strings(fields)
	if len(fields) > 64 {
		fields = append(fields[:64], "additional_fields_omitted")
	}
	return fields
}

// safeRequestToolTypes reports bounded counts, never client-defined tool names.
// Unsupported types are placed first so large function catalogs cannot hide them.
func safeRequestToolTypes(body map[string]any) []string {
	counts := map[string]int{}
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 32 {
			counts["depth_limit"]++
			return
		}
		list, _ := value.([]any)
		for _, raw := range list {
			tool, ok := raw.(map[string]any)
			if !ok {
				counts["invalid_declaration"]++
				continue
			}
			typ, _ := tool["type"].(string)
			label := typ
			switch typ {
			case "function", "custom", "namespace", "tool_search", "web_search", "web_search_preview", "computer", "computer_use_preview", "file_search", "code_interpreter", "image_generation", "mcp", "shell", "local_shell", "apply_patch":
			default:
				sum := sha256.Sum256([]byte(typ))
				label = fmt.Sprintf("unlisted_sha256_%x", sum[:8])
			}
			counts[label]++
			if typ == "namespace" {
				walk(tool["tools"], depth+1)
			}
		}
	}
	walk(body["tools"], 0)
	items, _ := body["input"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "additional_tools" {
			walk(item["tools"], 0)
		}
	}
	var unsupported, supported []string
	for typ, count := range counts {
		label := fmt.Sprintf("%s:%d", typ, count)
		switch typ {
		case "function", "custom", "namespace", "tool_search":
			supported = append(supported, label)
		default:
			unsupported = append(unsupported, label)
		}
	}
	sort.Strings(unsupported)
	sort.Strings(supported)
	out := append(unsupported, supported...)
	if len(out) > 32 {
		out = append(out[:32], "additional_types_omitted")
	}
	return out
}
