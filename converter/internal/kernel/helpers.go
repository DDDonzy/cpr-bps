// Derived from CPA BPS v0.1.18 (MIT); see ../../third_party/CPA_LICENSE.
package kernel

import (
	"encoding/json"
	"strings"
)

const DefaultUpstreamModel = "gpt-6-astra"
const DefaultModelID = "gpt-6-astra-basispoints"

var supportedReasoningEfforts = map[string]struct{}{"low": {}, "medium": {}, "high": {}, "xhigh": {}, "ultra": {}}

type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string                 { return e.Message }
func (e *APIError) StatusCode() int               { return e.Status }
func (e *APIError) Code() string                  { return e.Kind }
func fail(status int, kind, message string) error { return &APIError{status, kind, message} }
func errorMessage([]byte) string                  { return "BPS returned a failed or invalid response" }

type Config struct {
	Models         []string
	ModelMappings  map[string]string
	UpstreamModel  string
	ToolsVersionID string
}

func defaultConfig() Config {
	return Config{Models: []string{DefaultModelID}, UpstreamModel: DefaultUpstreamModel}
}

// canonicalEffort validates aliases before preparing the BPS wire request.
// Unknown values are rejected rather than silently changing user intent.
func canonicalEffort(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "medium", true
	case "none", "minimal":
		return "low", true
	case "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high":
		return "high", true
	case "xhigh", "x-high", "extra-high", "extra_high", "max", "ultra":
		return "xhigh", true
	default:
		return "", false
	}
}
func normalizeEffort(value any) string {
	text, _ := value.(string)
	if result, ok := canonicalEffort(text); ok {
		return result
	}
	return "medium"
}

func rawObject(raw []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fail(400, "invalid_request", "request body must be a JSON object")
	}
	return object, nil
}

func jsonBytes(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func stringValue(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func numberValue(value any) int64 {
	switch n := value.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func (c Config) upstreamModelForAlias(alias string) (string, bool) {
	for _, candidate := range c.Models {
		if alias == candidate {
			if upstream, exists := c.ModelMappings[alias]; exists {
				return upstream, true
			}
			return c.UpstreamModel, true
		}
	}
	return "", false
}

// resolveUpstreamModel 同时接受客户端别名和 CPA 执行器传入的已配置上游名称。
func (c Config) resolveUpstreamModel(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" && len(c.Models) > 0 {
		model = c.Models[0]
	}
	if upstream, ok := c.upstreamModelForAlias(model); ok {
		return upstream, true
	}
	for _, alias := range c.Models {
		if upstream, ok := c.upstreamModelForAlias(alias); ok && model == upstream {
			return upstream, true
		}
	}
	return "", false
}
