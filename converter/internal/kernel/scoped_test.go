package kernel

import (
	"bytes"
	"encoding/json"
	"testing"
)

func scopedSource(kind string) map[string]any {
	return map[string]any{"model": "gpt-test", "input": "test", "tools": []any{map[string]any{"type": kind, "name": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}, "additionalProperties": false}}}}
}

func TestTrustedScopeAndCataloguePreventCrossTenantReplay(t *testing.T) {
	source := scopedSource("function")
	a, err := New(source, "key-a/account/thread")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(source, "key-b/account/thread")
	if err != nil {
		t.Fatal(err)
	}
	native := rawRelayNative("shared-call-id", "run", "{\"cmd\":\"private-a\"}", []any{"run"})
	native["id"] = "fc_native_a"
	encoded, err := a.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if json.Unmarshal(encoded, &response) != nil {
		t.Fatal("bad transformed response")
	}
	history := []any{map[string]any{"type": "function_call", "id": "fc_native_b", "call_id": "shared-call-id", "name": "run", "arguments": "{\"cmd\":\"public-b\"}"}}
	b.source["input"] = history
	request, err := b.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(jsonBytes(request), []byte("private-a")) || !bytes.Contains(jsonBytes(request), []byte("public-b")) {
		t.Fatal("cross-tenant call cache leaked")
	}
}

func TestColdCustomReplayPreservesRawInputAndNativeID(t *testing.T) {
	source := scopedSource("custom")
	session, err := New(source, "custom-scope")
	if err != nil {
		t.Fatal(err)
	}
	raw := "*** Begin Patch\n*** Add File: sample.txt\n+quote \" and tab\t\n*** End Patch\n"
	native := rawRelayNative("custom-cold-id", "run", raw, []any{"run"})
	native["id"] = "fc_original_native"
	encoded, err := session.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	_ = json.Unmarshal(encoded, &response)
	call := objectValue(response["output"].([]any)[0])
	if call["id"] != "ctc_original_native" || call["input"] != raw {
		t.Fatal("custom identity or bytes changed")
	}
	nativeCallCache.Lock()
	delete(nativeCallCache.items, cacheKey("custom-cold-id", []string{stringValue(session.source["_cpr_tool_scope"])}))
	nativeCallCache.Unlock()
	session.source["input"] = []any{call, map[string]any{"type": "custom_tool_call_output", "call_id": "custom-cold-id", "output": "done"}}
	body, err := session.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	var replay map[string]any
	for _, item := range items {
		if m := objectValue(item); m["type"] == "function_call" {
			replay = m
		}
	}
	if replay == nil || replay["id"] != "fc_original_native" {
		t.Fatal("cold replay lost original native item id")
	}
	inner, err := transportEnvelope(replay)
	if err != nil || inner["args"] != raw {
		t.Fatal("cold replay changed patch input")
	}
}

func TestFullSchemaValidationAndClosedReferences(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"$ref": "#/$defs/count"}}, "required": []any{"n"}, "additionalProperties": false, "$defs": map[string]any{"count": map[string]any{"type": "integer", "minimum": json.Number("2")}}}
	if validateSchemaValue(schema, map[string]any{"n": json.Number("3")}) != nil {
		t.Fatal("valid local reference rejected")
	}
	for _, v := range []map[string]any{{"n": json.Number("2.5")}, {"n": json.Number("1")}, {"n": json.Number("3"), "extra": true}} {
		if validateSchemaValue(schema, v) == nil {
			t.Fatal("invalid arguments accepted")
		}
	}
	if _, err := compileSchema(map[string]any{"$ref": "file:///etc/passwd"}); err == nil {
		t.Fatal("external schema resource allowed")
	}
	if validateSchemaValue(false, map[string]any{}) == nil {
		t.Fatal("boolean false schema ignored")
	}
}

func TestMaxOutputTokensIsValidatedAndForwarded(t *testing.T) {
	source := scopedSource("function")
	source["max_output_tokens"] = json.Number("123")
	session, err := New(source, "max-scope")
	if err != nil {
		t.Fatal(err)
	}
	body, err := session.Prepare()
	if err != nil || body["max_output_tokens"] != json.Number("123") {
		t.Fatal("output cap was not forwarded")
	}
	for _, value := range []json.Number{"0", "-1", "1.5"} {
		session.source["max_output_tokens"] = value
		if _, err = session.Prepare(); err == nil {
			t.Fatal("invalid token cap accepted")
		}
	}
}

func TestKnownHostedSearchIsIgnoredInAutomaticMode(t *testing.T) {
	source := scopedSource("function")
	source["tools"] = []any{map[string]any{"type": "web_search"}}
	session, err := New(source, "search-scope")
	if err != nil {
		t.Fatal(err)
	}
	body, err := session.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["tools"]; ok {
		t.Fatal("hosted declaration leaked upstream")
	}
}
