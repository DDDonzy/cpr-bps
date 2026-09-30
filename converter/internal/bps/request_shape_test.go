package bps

import (
	"strings"
	"testing"
)

func TestToolTypesDoNotHideUnsupportedOrExposeNames(t *testing.T) {
	tools := []any{}
	for i := 0; i < 100; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": "PRIVATE_TOOL"})
	}
	tools = append(tools, map[string]any{"type": "web_search"})
	body := map[string]any{"tools": tools, "input": []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "PRIVATE_TYPE", "name": "PRIVATE_NAME"}}}}}
	result := strings.Join(safeRequestToolTypes(body), ",")
	if !strings.Contains(result, "web_search:1") || !strings.Contains(result, "function:100") || !strings.Contains(result, "unlisted_sha256_") {
		t.Fatal(result)
	}
	if strings.Contains(result, "PRIVATE") {
		t.Fatal("private data in diagnostic")
	}
}
