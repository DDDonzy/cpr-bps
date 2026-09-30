package kernel

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func rawProtocolSource(field, name, namespace string) map[string]any {
	tool := map[string]any{"type": "function", "name": name, "parameters": map[string]any{
		"type": "object", "properties": map[string]any{field: map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}, "max_output_tokens": map[string]any{"type": "integer"}}, "required": []any{field}, "additionalProperties": false,
	}}
	if namespace != "" {
		tool = map[string]any{"type": "namespace", "name": namespace, "tools": []any{tool}}
	}
	return map[string]any{"model": "gpt-test", "input": "synthetic", "tools": []any{tool}}
}

func metadataNative(id, key, field, raw string, metadata any, references any) map[string]any {
	prefix := functionCodePrefix
	if field == "cmd" {
		prefix = functionCmdPrefix
	}
	return map[string]any{"type": "function_call", "name": transportName, "call_id": id, "arguments": string(jsonBytes(map[string]any{"summary": prefix + key, "code": raw, "extended_summary": metadata, "references": references}))}
}

func TestRawMetadataLosslessWrappersAndRawBytes(t *testing.T) {
	object := `{"workdir":"C:\\测试\\directory","max_output_tokens":9007199254740993}`
	wrapped := string(jsonBytes(object))
	raw := "  echo \"PRIVATE_RAW_VALUE\"\r\nC:\\new\\test\\file\t中文  "
	for _, field := range []string{"cmd", "code"} {
		for _, tc := range []struct{ name, metadata string }{
			{"object", object}, {"space", " \t" + object + "\r\n"}, {"bom", "\ufeff" + object}, {"json_fence", "```json\r\n" + object + "\r\n```"}, {"plain_fence", "```\n" + object + "\n```"}, {"double_encoded", wrapped},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				source := rawProtocolSource(field, "invoke", "tools")
				call, err := extractNativeClientToolCall(metadataNative(t.Name(), "tools.invoke", field, raw, tc.metadata, []any{}), clientToolSpecs(source))
				if err != nil {
					t.Fatal(err)
				}
				args, reason := parseRelayObject(call["arguments"])
				if reason != "" || args[field] != raw || args["workdir"] != `C:\测试\directory` || args["max_output_tokens"] != json.Number("9007199254740993") {
					t.Fatalf("lost data: %#v %s", args, reason)
				}
				if call["name"] != "invoke" || call["namespace"] != "tools" {
					t.Fatal("lost identity")
				}
			})
		}
	}
}

func TestRawMetadataNeverGuessesOrLeaks(t *testing.T) {
	source := rawProtocolSource("cmd", "invoke", "")
	for _, tc := range []struct {
		name     string
		metadata any
	}{
		{"empty", ""}, {"space", " \t\r\n"}, {"prose", "PRIVATE_METADATA_SENTINEL"}, {"array", "[]"}, {"number", "123"}, {"bool", "true"}, {"null", "null"}, {"json_string", `"PRIVATE_METADATA_SENTINEL"`}, {"typed_object", map[string]any{}}, {"nil", nil},
		{"trailing", "{} {}"}, {"fence_extra", "```json\n{}\n```\nPRIVATE_METADATA_SENTINEL"}, {"javascript_fence", "```javascript\n{}\n```"}, {"double_string", string(jsonBytes(string(jsonBytes("{}"))))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := extractNativeClientToolCall(metadataNative(t.Name(), "invoke", "cmd", "PRIVATE_RAW_SENTINEL", tc.metadata, []any{}), clientToolSpecs(source))
			api, ok := err.(*APIError)
			if !ok || api.Status != 422 || api.Kind != "invalid_tool_call" || call != nil {
				t.Fatalf("accepted invalid metadata: %#v %v", call, err)
			}
			if strings.Contains(api.Message, "PRIVATE_") {
				t.Fatal("error leaked metadata or raw code")
			}
		})
	}
}

func TestRawMetadataSchemaAndRouteValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		metadata   string
		references any
		want       string
	}{
		{"invalid_type", `{"max_output_tokens":"1000"}`, []any{}, "arguments_schema_mismatch"},
		{"unknown_parameter", `{"unknown":true}`, []any{}, "arguments_schema_mismatch"},
		{"duplicate_cmd", `{"cmd":"PRIVATE_DUPLICATE"}`, []any{}, "metadata_contains_raw_field"},
		{"wrong_reference", `{}`, []any{"other"}, "raw_references_conflict"},
		{"multiple_references", `{}`, []any{"invoke", "invoke"}, "raw_references_conflict"},
		{"typed_reference", `{}`, []any{map[string]any{}}, "raw_references_conflict"},
		{"null_reference", `{}`, nil, "raw_references_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := rawProtocolSource("cmd", "invoke", "")
			_, err := extractNativeClientToolCall(metadataNative(t.Name(), "invoke", "cmd", "PRIVATE_RAW", tc.metadata, tc.references), clientToolSpecs(source))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	source := rawProtocolSource("cmd", "invoke", "")
	params := objectValue(clientToolSpecs(source)["invoke"].Spec["parameters"])
	params["required"] = []any{"cmd", "workdir"}
	_, err := extractNativeClientToolCall(metadataNative(t.Name(), "invoke", "cmd", "safe", "{}", []any{}), clientToolSpecs(source))
	if err == nil || !strings.Contains(err.Error(), "arguments_schema_mismatch") {
		t.Fatal("missing required metadata accepted")
	}
	for _, refs := range []any{[]any{}, []any{"invoke"}} {
		_, err = extractNativeClientToolCall(metadataNative(t.Name(), "invoke", "cmd", "safe", `{"workdir":"/tmp"}`, refs), clientToolSpecs(source))
		if err != nil {
			t.Fatalf("unambiguous route rejected: %v", err)
		}
	}
}

func createThreadProtocolSource() map[string]any {
	return map[string]any{"tools": []any{map[string]any{"type": "namespace", "name": "codex_app", "tools": []any{map[string]any{"type": "function", "name": "create_thread", "parameters": map[string]any{"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string"}, "target": map[string]any{"type": "object"}}, "required": []any{"prompt", "target"}, "additionalProperties": false}}}}}}
}

func TestCreateThreadIsNotRawTransport(t *testing.T) {
	source := createThreadProtocolSource()
	specs := clientToolSpecs(source)
	if rawTransportKind(specs["codex_app.create_thread"]) != "" {
		t.Fatal("create_thread treated as raw")
	}
	good := rawRelayNative(t.Name(), "codex_app.create_thread", `{"prompt":"synthetic","target":{"type":"projectless"}}`, []any{"codex_app.create_thread"})
	call, err := extractNativeClientToolCall(good, specs)
	if err != nil || call["namespace"] != "codex_app" || call["name"] != "create_thread" {
		t.Fatalf("normal create_thread rejected: %v", err)
	}
	for _, field := range []string{"cmd", "code"} {
		_, err = extractNativeClientToolCall(metadataNative(t.Name()+field, "codex_app.create_thread", field, "PRIVATE_RAW", "[]", []any{}), specs)
		if err == nil || !strings.Contains(err.Error(), "raw_transport_not_declared") {
			t.Fatalf("wrong raw routing not diagnosed: %v", err)
		}
	}
	// Client result text is not a JSON function arguments object.
	bad := rawRelayNative(t.Name()+"xml", "codex_app.create_thread", "<codex_delegation>PRIVATE_RESULT</codex_delegation>", []any{"codex_app.create_thread"})
	_, err = extractNativeClientToolCall(bad, specs)
	if err == nil || strings.Contains(err.Error(), "PRIVATE_RESULT") {
		t.Fatal("XML arguments accepted or leaked")
	}
}

func TestRawCatalogueAndReminderAgree(t *testing.T) {
	source := rawProtocolSource("cmd", "exec_command", "tools")
	source["tools"] = append(source["tools"].([]any), createThreadProtocolSource()["tools"].([]any)...)
	catalog := clientToolProtocolInstructions(source)
	for _, part := range []string{"modes are mutually exclusive", "RAW FUNCTION rules override", "literal string \"{}\"", "Combined arguments JSON Schema", "no cpr.function_cmd/ or cpr.function_code/ summary marker", "all the remaining arguments", "XML delegation wrapper"} {
		if !strings.Contains(catalog, part) {
			t.Fatalf("missing %q", part)
		}
	}
	reminder := clientToolProtocolReminder(source)
	for _, part := range []string{"summary=cpr.function_cmd/tools.exec_command", "references=[]", "REFERENCE MODE codex_app.create_thread", "JSON-object string"} {
		if !strings.Contains(reminder, part) {
			t.Fatalf("reminder missing %q", part)
		}
	}
	for i := 0; i < 10; i++ {
		if clientToolProtocolReminder(source) != reminder {
			t.Fatal("nondeterministic reminder")
		}
	}
}

func TestColdRawHistoryUsesCurrentProtocol(t *testing.T) {
	for _, field := range []string{"cmd", "code"} {
		source := rawProtocolSource(field, "invoke", "tools")
		raw := "  quote \"x\"\r\nC:\\path\\中文  "
		original := map[string]any{field: raw, "workdir": "/tmp", "max_output_tokens": json.Number("9007199254740993")}
		call := map[string]any{"type": "function_call", "name": "invoke", "namespace": "tools", "id": "fc_cold", "call_id": "cold-" + field, "arguments": string(jsonBytes(original))}
		result := map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "already executed"}
		before := string(jsonBytes(call))
		history := translateInputItems([]any{call, result}, clientToolSpecs(source), t.Name())
		args := parseArguments(objectValue(history[0])["arguments"])
		prefix := functionCodePrefix
		if field == "cmd" {
			prefix = functionCmdPrefix
		}
		if args["summary"] != prefix+"tools.invoke" || args["code"] != raw || !reflect.DeepEqual(args["references"], []any{}) {
			t.Fatalf("history uses wrong format: %#v", args)
		}
		metadata, reason := parseRelayObject(args["extended_summary"])
		if reason != "" || metadata[field] != nil || metadata["workdir"] != "/tmp" {
			t.Fatal("history lost metadata")
		}
		restored, err := extractNativeClientToolCall(objectValue(history[0]), clientToolSpecs(source))
		if err != nil || restored["arguments"] != call["arguments"] {
			t.Fatalf("round trip changed arguments: %#v %v", restored, err)
		}
		if objectValue(history[1])["output"] != "already executed" || string(jsonBytes(call)) != before {
			t.Fatal("history/result mutated")
		}
	}
}

func TestInvalidRawMetadataStreamNeverDeliversToolsOrCommitsHistory(t *testing.T) {
	source := rawProtocolSource("cmd", "invoke", "")
	session, err := New(source, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	good := metadataNative("atomic-good", "invoke", "cmd", "safe", "{}", []any{})
	bad := metadataNative("atomic-bad", "invoke", "cmd", "PRIVATE_RAW", "PRIVATE_METADATA", []any{})
	response := map[string]any{"id": "resp_atomic", "status": "completed", "output": []any{good, bad}}
	var input strings.Builder
	var out bytes.Buffer
	writeSSE(&input, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_atomic", "status": "in_progress", "output": []any{}}})
	writeSSE(&input, "response.completed", map[string]any{"type": "response.completed", "response": response})
	err = session.Relay(strings.NewReader(input.String()), func() {}, func(b []byte) error { _, e := out.Write(b); return e })
	if err == nil || bytes.Contains(out.Bytes(), []byte("event: response.completed")) || bytes.Contains(out.Bytes(), []byte(`"type":"function_call"`)) || bytes.Contains(out.Bytes(), []byte("PRIVATE_")) {
		t.Fatalf("non-atomic invalid relay: %v", err)
	}
	for _, id := range []string{"atomic-good", "atomic-bad"} {
		if rememberedNativeCall(id, stringValue(session.source["_cpr_tool_scope"])) != nil {
			t.Fatal("invalid batch populated history")
		}
	}
}
