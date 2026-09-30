package bps

import (
	"bytes"
	"context"
	"cpr-excel-adapter/internal/kernel"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test-only capture: credentials stay in a private ephemeral file and memory.
// Persist only generated-test event structure; opaque contents are replaced by hashes.
func TestCaptureLiveBPSValidation(t *testing.T) {
	path := os.Getenv("BPS_DIAGNOSTIC_ENVELOPE")
	if path == "" {
		t.Skip("explicit private test envelope required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	var e Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&e); err != nil {
		t.Fatal("invalid envelope")
	}
	session, err := kernel.New(e.Request, "isolated-live-diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := session.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(payload)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if e.ProxyURL != "" {
		u, err := url.Parse(e.ProxyURL)
		if err != nil {
			t.Fatal("invalid proxy")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ResponsesURL, bytes.NewReader(body))
	req.Header = authHeaders(e)
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("upstream transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("upstream status=%d", response.StatusCode)
	}
	var capture bytes.Buffer
	reader := io.TeeReader(response.Body, &capture)
	relayErr := session.Relay(reader, func() {}, func([]byte) error { return nil })
	// Continue observing this SAME request after local rejection; never resend it.
	_, drainErr := io.Copy(&capture, response.Body)
	var diagnostic *kernel.StreamValidationError
	site := ""
	if errors.As(relayErr, &diagnostic) {
		site = diagnostic.Location
	}
	events := []map[string]any{}
	var sanitized strings.Builder
	for _, line := range strings.Split(capture.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(data), &v) != nil {
			continue
		}
		if r, ok := v["response"].(map[string]any); ok {
			v["response"] = keepDiagnosticResponse(r)
		}
		scrubDiagnostic(v)
		b, _ := json.Marshal(v)
		fmt.Fprintf(&sanitized, "event: %s\ndata: %s\n\n", v["type"], b)
		entry := map[string]any{"type": v["type"], "output_index": v["output_index"], "item_id": v["item_id"]}
		if item, ok := v["item"].(map[string]any); ok {
			entry["item"] = item
		}
		if r, ok := v["response"].(map[string]any); ok {
			entry["response_id"] = r["id"]
			if strings.Contains(fmt.Sprint(v["type"]), "completed") {
				entry["output"] = r["output"]
			}
		}
		events = append(events, entry)
	}
	dir := filepath.Dir(path)
	fixture := filepath.Join(dir, "upstream-sanitized.sse")
	if err = os.WriteFile(fixture, []byte(sanitized.String()), 0600); err != nil {
		t.Fatal(err)
	}
	source, _ := json.Marshal(e.Request)
	if err = os.WriteFile(filepath.Join(dir, "request.json"), source, 0600); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"validationSite": site, "relaySucceeded": relayErr == nil, "drainSucceeded": drainErr == nil, "events": events}
	out, _ := json.MarshalIndent(result, "", "  ")
	os.WriteFile(filepath.Join(dir, "diagnostic.json"), out, 0600)
	t.Logf("relaySucceeded=%v validationSite=%s events=%d drainSucceeded=%v", relayErr == nil, site, len(events), drainErr == nil)
}
func keepDiagnosticResponse(r map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "status", "output", "usage", "error", "incomplete_details"} {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}
	return out
}
func scrubDiagnostic(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if text, ok := val.(string); ok {
				if k == "encrypted_content" {
					sum := sha256.Sum256([]byte(text))
					x[k] = "opaque_" + hex.EncodeToString(sum[:])
					continue
				}
				if k == "text" || k == "delta" {
					x[k] = strings.Repeat("x", len(text))
					continue
				}
			}
			scrubDiagnostic(val)
		}
	case []any:
		for _, item := range x {
			scrubDiagnostic(item)
		}
	}
}
func TestReplayRecordedBPSValidation(t *testing.T) {
	path := os.Getenv("BPS_REPLAY_FIXTURE")
	if path == "" {
		path = filepath.Join("testdata", "reasoning-resealed", "upstream-sanitized.sse")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&source); err != nil {
		t.Fatal(err)
	}
	s, err := kernel.New(source, "isolated-live-diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Relay(bytes.NewReader(raw), func() {}, func([]byte) error { return nil })
	if err != nil {
		var v *kernel.StreamValidationError
		if errors.As(err, &v) {
			t.Fatalf("validation at %s", v.Location)
		}
		t.Fatal(err)
	}
}
