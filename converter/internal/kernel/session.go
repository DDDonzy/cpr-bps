package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// Session 只拥有当前请求的协议副本；不会加载或刷新任何账号凭据。
type Session struct {
	source      map[string]any
	config      Config
	onCompleted func(map[string]any)
	structured  *structuredOutput
	streamStats StreamStatistics
}

func New(source map[string]any, scope string) (*Session, error) {
	if scope == "" || source == nil {
		return nil, fail(400, "invalid_scope", "trusted request scope is required")
	}
	source = cloneObject(source)
	if err := validateRequestFields(source); err != nil {
		return nil, err
	}
	source["stream"] = true
	if err := validateToolDeclarations(source["tools"]); err != nil {
		return nil, err
	}
	if items, ok := source["input"].([]any); ok {
		for _, raw := range items {
			item := objectValue(raw)
			if stringValue(item["type"]) == "additional_tools" {
				if err := validateToolDeclarations(item["tools"]); err != nil {
					return nil, err
				}
			}
		}
	}

	model := stringValue(source["model"])
	if model == "" {
		return nil, fail(400, "invalid_model", "model is required")
	}
	switch source["input"].(type) {
	case string, []any:
	default:
		return nil, fail(400, "invalid_input", "input must be text or a complete history")
	}
	if items, ok := source["input"].([]any); ok {
		for _, item := range items {
			if objectValue(item) == nil {
				return nil, fail(400, "invalid_input", "history entries must be objects")
			}
		}
	}
	for _, spec := range clientToolSpecs(source) {
		if spec.Type == "function" {
			if schema, ok := declaredSchema(spec.Spec); ok {
				if len(jsonBytes(schema)) > 1<<20 {
					return nil, fail(400, "tool_schema_too_large", "tool schema exceeds the bound")
				}
				if _, err := compileSchema(schema); err != nil {
					return nil, fail(400, "invalid_tool_schema", "tool schema is invalid or uses external resources")
				}
			}
		}
	}
	structured, err := prepareStructuredOutput(source["text"])
	if err != nil {
		return nil, err
	}
	conversation := sha256.Sum256([]byte(scope))
	effectiveCatalog := jsonBytes(clientToolSpecs(source))
	cache := sha256.Sum256(append(conversation[:], effectiveCatalog...))
	source["_cpr_tool_scope"] = hex.EncodeToString(cache[:])
	source["prompt_cache_key"] = hex.EncodeToString(conversation[:])
	return &Session{source: source, config: Config{Models: []string{model}, ModelMappings: map[string]string{model: model}, UpstreamModel: model}, structured: structured}, nil
}

func (s *Session) Prepare() (map[string]any, error) {
	body, err := prepareResponsesBody(s.source, s.config)
	if err != nil {
		return nil, err
	}
	if value, ok := s.source["max_output_tokens"]; ok {
		number, ok := value.(json.Number)
		if !ok {
			return nil, fail(400, "invalid_max_output_tokens", "max_output_tokens must be a positive integer")
		}
		n, err := number.Int64()
		if err != nil || n < 1 {
			return nil, fail(400, "invalid_max_output_tokens", "max_output_tokens must be a positive integer")
		}
		body["max_output_tokens"] = number
	}
	return body, nil
}

// OnCompleted 在真实终态已验证、但尚未向客户端发布前提交协议历史。
func (s *Session) OnCompleted(commit func(map[string]any)) { s.onCompleted = commit }

func (s *Session) Transform(raw []byte) ([]byte, error) {
	body, _, _, err := transformResponseBody(raw, s.source)
	return body, err
}

// Relay 先交付已验证文本，工具整批通过终态校验后才交付，不重发模型请求。
func (s *Session) Relay(reader io.Reader, start func(), write func([]byte) error) (result error) {
	decoder := newSSEDecoder()
	delivery := newStreamDelivery("openai", start, write)
	var lastRead time.Time
	defer func() {
		if !lastRead.IsZero() {
			delivery.stats.LastUpstreamDataAgeMS = time.Since(lastRead).Milliseconds()
		}
		s.streamStats = delivery.stats
	}()
	// 失败也通过同一序列化器交付标准错误帧，不伪造完成，不泄漏传输异常正文。
	defer func() {
		if result != nil && !delivery.disconnected {
			_ = delivery.fail(result)
		}
	}()

	completed := errors.New("terminal processed")

	chunk := make([]byte, 32<<10)
	handle := func(event, data string) error {
		delivery.stats.UpstreamEvents++
		if strings.TrimSpace(data) == "[DONE]" {
			return streamEventError()
		}
		value, reason := parseRelayObject(data)
		if reason != "" {
			return streamEventError()
		}
		kind := stringValue(value["type"])
		if kind != "" && event != "" && kind != event {
			return streamEventError()
		}
		if err := delivery.consume(event, data); err != nil {
			return err
		}
		if kind != "response.completed" && kind != "response.incomplete" {
			return nil
		}
		response, err := terminalResponse(objectValue(value["response"]))
		if err != nil {
			return err
		}
		if stringValue(response["status"]) != strings.TrimPrefix(kind, "response.") {
			return streamEventError()
		}
		// 先确认已交付正文与上游终态一致，再允许工具批次写入历史缓存。
		if err = s.structured.validate(response); err != nil {
			return err
		}
		if err = delivery.validateFinal(response); err != nil {
			return err
		}
		_, response, _, err = transformResponseBody(jsonBytes(response), s.source)
		if err != nil {
			return err
		}
		if kind == "response.completed" && s.onCompleted != nil {
			s.onCompleted(response)
		}
		if err = delivery.finish(response); err != nil {
			return err
		}
		return completed
	}
	for {
		n, err := reader.Read(chunk)

		if n > 0 {
			lastRead = time.Now()
			delivery.stats.UpstreamReadChunks++
			delivery.stats.UpstreamBytes += int64(n)
			if consumeErr := decoder.feed(chunk[:n], handle); consumeErr != nil {
				if errors.Is(consumeErr, completed) {
					return nil
				}
				return consumeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return fail(502, "incomplete_stream", "BPS stream ended without a valid terminal")
			}
			return err
		}
	}
}

func isKnownHostedTool(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26", "tool_search", "image_generation", "file_search", "code_interpreter", "computer", "computer_use_preview", "mcp", "shell", "local_shell", "apply_patch":
		return true
	default:
		return false
	}
}

func validateToolDeclarations(value any) error {
	if value == nil {
		return nil
	}
	tools, ok := value.([]any)
	if !ok {
		return fail(400, "invalid_tools", "tools must be an array")
	}
	if len(tools) > 256 {
		return fail(400, "too_many_tools", "tool catalogue exceeds the bound")
	}
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			return fail(400, "invalid_tools", "tool declaration must be an object")
		}
		kind := strings.TrimSpace(stringValue(tool["type"]))
		if isKnownHostedTool(kind) {
			// Match Sub2API: automatically ignore known hosted declarations so
			// other relayable client tools can still use BPS. Forced selection
			// remains rejected by the tool_choice validation below.
			continue
		}
		if kind != "function" && kind != "custom" && kind != "namespace" && kind != "tool_search" {
			return fail(400, "hosted_tool_not_implemented", "hosted tools require their own verified adapter")
		}
		name, ok := tool["name"].(string)
		if kind == "tool_search" && !ok {
			name = "tool_search"
			ok = true
		}
		if !ok || name == "" || len(name) > 256 || strings.IndexFunc(name, func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.')
		}) >= 0 {
			return fail(400, "invalid_tool_name", "tool name is invalid")
		}
		if kind == "namespace" {
			if err := validateToolDeclarations(tool["tools"]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRequestFields(source map[string]any) error {
	allowed := map[string]bool{"model": true, "input": true, "instructions": true, "reasoning": true, "stream": true, "stream_options": true, "store": true, "tools": true, "tool_choice": true, "service_tier": true, "text": true, "previous_response_id": true, "max_output_tokens": true, "metadata": true, "context_management": true, "prompt_cache_key": true, "client_metadata": true, "parallel_tool_calls": true, "include": true}
	for key := range source {
		if !allowed[key] {
			return fail(400, "unsupported_request_field", "request field is not implemented")
		}
	}
	if err := validateStreamOptions(source["stream_options"]); err != nil {
		return err
	}
	if value := source["instructions"]; value != nil {
		if _, ok := value.(string); !ok {
			return fail(400, "invalid_instructions", "instructions must be text")
		}
	}
	if value := source["store"]; value != nil && value != false {
		return fail(400, "unsupported_store", "persistent storage is unsupported")
	}
	if value := source["reasoning"]; value != nil {
		r, ok := value.(map[string]any)
		if !ok {
			return fail(400, "invalid_reasoning", "reasoning must be an object")
		}
		if effort := r["effort"]; effort != nil {
			name, ok := effort.(string)
			if !ok {
				return fail(400, "invalid_reasoning", "effort must be text")
			}
			if _, ok := canonicalEffort(name); !ok {
				return fail(400, "unsupported_reasoning_effort", "reasoning effort not implemented")
			}
		}
	}
	return nil
}
