package bps

import (
	"encoding/json"
	"io"
	"log"
	"strings"
)

// 只保留固定字段名及校验类型，不记录 message、input、请求正文或鉴权信息。
func validationSummary(reader io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(reader, 65537))
	if err != nil || len(raw) > 65536 {
		return ""
	}
	log.Printf("BPS validation body bytes=%d shape=%s", len(raw), validationBodyShape(raw))
	var body struct {
		Detail []struct {
			Type string         `json:"type"`
			Loc  []any          `json:"loc"`
			Ctx  map[string]any `json:"ctx"`
		} `json:"detail"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return commonValidationSummary(raw)
	}
	fields := map[string]bool{"max_output_tokens": true, "max_tokens": true, "input": true, "model": true, "reasoning_effort": true, "stream": true, "store": true, "context_management": true, "metadata": true, "prompt_cache_key": true}
	kinds := map[string]bool{"extra_forbidden": true, "missing": true, "literal_error": true, "greater_than_equal": true, "greater_than": true, "less_than_equal": true, "less_than": true, "int_parsing": true, "value_error": true}
	result := []map[string]any{}
	for _, item := range body.Detail {
		if len(result) >= 4 {
			break
		}
		kind := "validation_error"
		if kinds[item.Type] {
			kind = item.Type
		}
		field := "unlisted_field"
		for _, part := range item.Loc {
			if name, ok := part.(string); ok && fields[name] {
				field = name
				break
			}
		}
		row := map[string]any{"field": field, "type": kind}
		for _, name := range []string{"ge", "gt", "le", "lt"} {
			if value, ok := item.Ctx[name].(float64); ok {
				row[name] = value
			}
		}
		result = append(result, row)
	}
	if len(result) == 0 {
		return commonValidationSummary(raw)
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

// 非 Pydantic 错误也只提取已知参数和固定分类；消息只用于识别，绝不写入结果。
func commonValidationSummary(raw []byte) string {
	fields := []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "reasoning_effort", "model", "input", "stream", "store", "context_management", "prompt_cache_key"}
	kinds := map[string]bool{"unsupported_parameter": true, "unsupported_value": true, "invalid_value": true, "invalid_type": true, "extra_forbidden": true, "missing": true, "literal_error": true, "greater_than_equal": true, "less_than_equal": true}
	var body any
	if json.Unmarshal(raw, &body) != nil {
		body = string(raw)
	}
	result := []map[string]any{}
	var visit func(any, int)
	visit = func(node any, depth int) {
		if depth > 4 || len(result) >= 4 {
			return
		}
		field, kind, message := "", "", ""
		switch value := node.(type) {
		case string:
			message = value
		case []any:
			for _, child := range value {
				visit(child, depth+1)
			}
			return
		case map[string]any:
			for _, key := range []string{"param", "field", "parameter"} {
				if name, ok := value[key].(string); ok {
					for _, allowed := range fields {
						if name == allowed {
							field = name
						}
					}
				}
			}
			for _, key := range []string{"code", "type"} {
				if name, ok := value[key].(string); ok && kinds[name] && kind == "" {
					kind = name
				}
			}
			message, _ = value["message"].(string)
			for _, key := range []string{"error", "errors", "detail", "details"} {
				if child, ok := value[key]; ok {
					visit(child, depth+1)
				}
			}
		default:
			return
		}
		lower := strings.ToLower(message)
		if field == "" {
			for _, name := range fields {
				if strings.Contains(lower, name) {
					field = name
					break
				}
			}
		}
		if field == "" {
			return
		}
		if kind == "" {
			switch {
			case strings.Contains(lower, "unsupported value"):
				kind = "unsupported_value"
			case strings.Contains(lower, "unsupported parameter") || strings.Contains(lower, "unrecognized request argument") || strings.Contains(lower, "unexpected keyword") || strings.Contains(lower, field+" is not supported"):
				kind = "unsupported_parameter"
			default:
				kind = "value_error"
			}
		}
		if len(result) < 4 {
			result = append(result, map[string]any{"field": field, "type": kind})
		}
	}
	visit(body, 0)
	if len(result) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

// 仅输出固定键的值类型，不输出键外内容或任何响应字符串。
func validationBodyShape(raw []byte) string {
	if len(raw) == 0 {
		return "empty"
	}
	if len(raw) > 1 && raw[0] == 0x1f && raw[1] == 0x8b {
		return "gzip"
	}
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return "non_json"
	}
	shapes := []string{}
	allowed := []string{"error", "errors", "detail", "details", "message", "msg", "code", "type", "param", "field", "parameter", "path", "location", "loc", "status", "title", "description", "reason", "validation", "issues", "extensions"}
	var visit func(any, string, int)
	visit = func(node any, path string, depth int) {
		if depth > 4 || len(shapes) >= 24 {
			return
		}
		switch v := node.(type) {
		case map[string]any:
			shapes = append(shapes, path+":object")
			for _, k := range allowed {
				if child, ok := v[k]; ok {
					visit(child, path+"."+k, depth+1)
				}
			}
		case []any:
			shapes = append(shapes, path+":array")
			if len(v) > 0 {
				visit(v[0], path+"[]", depth+1)
			}
		case string:
			shapes = append(shapes, path+":string")
		case float64:
			shapes = append(shapes, path+":number")
		case nil:
			shapes = append(shapes, path+":null")
		default:
			shapes = append(shapes, path+":other")
		}
	}
	visit(root, "root", 0)
	return strings.Join(shapes, ",")
}
