package kernel

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type structuredOutput struct {
	format map[string]any
	schema map[string]any
}

var structuredName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func prepareStructuredOutput(raw any) (*structuredOutput, error) {
	if raw == nil {
		return nil, nil
	}
	cfg, ok := raw.(map[string]any)
	if !ok {
		return nil, fail(400, "invalid_text_config", "text must be an object")
	}
	rawfmt, ok := cfg["format"]
	if !ok || rawfmt == nil {
		return nil, nil
	}
	format, ok := rawfmt.(map[string]any)
	if !ok {
		return nil, fail(400, "invalid_text_format", "text.format must be an object")
	}
	kind, _ := format["type"].(string)
	if kind == "text" {
		return nil, nil
	}
	if kind != "json_object" && kind != "json_schema" {
		return nil, fail(400, "invalid_text_format", "text.format.type must be text, json_object, or json_schema")
	}
	for k := range format {
		if k == "type" || (kind == "json_schema" && (k == "name" || k == "schema" || k == "strict" || k == "description")) {
			continue
		}
		return nil, fail(400, "invalid_text_format", "text.format contains an unsupported field")
	}
	out := &structuredOutput{format: cloneObject(format)}
	if kind == "json_object" {
		return out, nil
	}
	name, _ := format["name"].(string)
	if !structuredName.MatchString(name) {
		return nil, fail(400, "invalid_text_format", "json_schema requires a valid name")
	}
	schema, ok := format["schema"].(map[string]any)
	if !ok || schema == nil || len(jsonBytes(schema)) > 1<<20 {
		return nil, fail(400, "invalid_text_format", "json_schema requires a schema object")
	}
	if _, err := compileSchema(schema); err != nil {
		return nil, fail(400, "invalid_text_format", "json_schema is invalid or uses external resources")
	}
	out.schema = cloneObject(schema)
	return out, nil
}
func (s *structuredOutput) instructions() string {
	if s == nil {
		return ""
	}
	b := string(jsonBytes(s.format))
	return "The client requires a structured final answer. Return exactly one JSON value with no Markdown fences or surrounding prose. Tool calls and refusals remain separate protocol items. Requested output format: " + b
}
func (s *structuredOutput) validate(response map[string]any) error {
	if s == nil {
		return nil
	}
	output, _ := response["output"].([]any)
	var text strings.Builder
	hasTool := false
	for _, raw := range output {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		typ := stringValue(item["type"])
		if typ == "function_call" || typ == "custom_tool_call" || typ == "tool_search_call" {
			hasTool = true
			continue
		}
		if typ != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, partRaw := range content {
			part := objectValue(partRaw)
			if stringValue(part["type"]) == "output_text" {
				v, ok := part["text"].(string)
				if !ok {
					return fmt.Errorf("structured output text is invalid")
				}
				text.WriteString(v)
			}
		}
	}
	if hasTool || text.Len() == 0 {
		return nil
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(text.String()))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return fail(502, "invalid_structured_output", "BPS returned invalid structured JSON")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fail(502, "invalid_structured_output", "BPS returned trailing structured JSON")
	}
	if s.schema != nil && validateSchemaValue(s.schema, value) != nil {
		return fail(502, "invalid_structured_output", "BPS structured output does not satisfy the requested schema")
	}
	return nil
}
