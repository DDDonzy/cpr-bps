package bps

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestValidationMetadataPreservesSafeReason(t *testing.T) {
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cpr-Bps-Upstream-Status", "201")
		w.WriteHeader(422)
		io.WriteString(w, `{"detail":[{"type":"greater_than_equal","loc":["body","max_output_tokens"],"ctx":{"ge":32,"pattern":"SECRET_PATTERN"},"msg":"SECRET_MSG","input":"SECRET_TOKEN"}]}`)
	})
	response := request(t, server.URL, envelope(), testGate)
	defer response.Body.Close()
	if response.StatusCode != 422 || response.Header.Get("X-Cpr-Bps-Upstream-Status") != "422" {
		t.Fatal("actual upstream status missing")
	}
	summary := response.Header.Get("X-Cpr-Bps-Validation")
	var rows []map[string]any
	if json.Unmarshal([]byte(summary), &rows) != nil || len(rows) != 1 || rows[0]["field"] != "max_output_tokens" || rows[0]["type"] != "greater_than_equal" || rows[0]["ge"] != float64(32) || strings.Contains(summary, "SECRET") {
		t.Fatal("unsafe or missing reason")
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "SECRET_MSG") || !strings.Contains(string(body), "SECRET_PATTERN") {
		t.Fatal("original upstream diagnostic was overwritten")
	}
}

func TestBridgeErrorDoesNotMasqueradeAsUpstreamStatus(t *testing.T) {
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		io.WriteString(w, "not SSE")
	})
	response := request(t, server.URL, envelope(), testGate)
	response.Body.Close()
	if response.StatusCode != 502 || response.Header.Get("X-Cpr-Bps-Upstream-Status") != "200" {
		t.Fatal("bridge and upstream status conflated")
	}
	e := envelope()
	e.Request["max_output_tokens"] = json.Number("-1")
	response = request(t, server.URL, e, testGate)
	response.Body.Close()
	if response.StatusCode != 400 || response.Header.Get("X-Cpr-Bps-Send-State") != "not_sent" || response.Header.Get("X-Cpr-Bps-Upstream-Status") != "" {
		t.Fatal("invented upstream status before send")
	}
}

func TestLocalRejectionHasSafeDiagnosticCode(t *testing.T) {
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("rejected request reached upstream") })
	e := envelope()
	e.Request["store"] = true
	response := request(t, server.URL, e, testGate)
	defer response.Body.Close()
	if response.StatusCode != 400 || response.Header.Get("X-Cpr-Bps-Error-Code") != "unsupported_store" || response.Header.Get("X-Cpr-Bps-Request-Id") != e.RequestID {
		t.Fatal("local rejection diagnostics missing")
	}
	if response.Header.Get("X-Cpr-Bps-Upstream-Status") != "" {
		t.Fatal("invented upstream status")
	}
	for _, unsafe := range []string{"SECRET_TOKEN", "unsupported_store SECRET", "input=user private text", ""} {
		if safeBridgeErrorCode(unsafe) != "unclassified_bridge_error" {
			t.Fatal("unsafe diagnostic accepted")
		}
	}
}

func TestBridgeAcceptsCodexSequentialCutoffStreamOption(t *testing.T) {
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["stream_options"]; exists {
			t.Error("transport option forwarded to model")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completed)
	})
	e := envelope()
	e.Request["stream_options"] = map[string]any{"reasoning_summary_delivery": "sequential_cutoff"}
	response := request(t, server.URL, e, testGate)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("Codex stream option rejected: %d", response.StatusCode)
	}
}
