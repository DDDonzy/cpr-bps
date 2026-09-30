package bps

import (
	"bufio"
	"bytes"
	"context"
	"cpr-excel-adapter/internal/kernel"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testGate = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const completed = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"mock-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"

func envelope() Envelope {
	return Envelope{Version: 1, RequestID: "mock-request", TenantID: "mock-key", AccountScope: "mock-account-scope", ConversationID: "mock-thread", Authorization: "Bearer mock-token-1", AccountID: "mock-account", Request: map[string]any{"model": "mock-model", "input": "hello"}}
}
func encoded(t *testing.T, e Envelope) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func request(t *testing.T, server string, e Envelope, gate string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server+"/v1/bps/responses", bytes.NewReader(encoded(t, e)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Cpr-Bps-Gate", gate)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func fixture(t *testing.T, h http.HandlerFunc) (*Bridge, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	b, err := New(testGate)
	if err != nil {
		t.Fatal(err)
	}
	b.responsesURL = up.URL
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	return b, s
}

func TestPrivateGateAndLoopback(t *testing.T) {
	for _, a := range []string{"0.0.0.0:1", "localhost:1", "203.0.113.1:1", ":1"} {
		if ValidateListen(a) == nil {
			t.Fatalf("accepted %s", a)
		}
	}
	for _, a := range []string{"127.0.0.1:1", "[::1]:1"} {
		if ValidateListen(a) != nil {
			t.Fatalf("rejected %s", a)
		}
	}
	if _, err := New("short"); err == nil {
		t.Fatal("accepted weak gate")
	}
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	response := request(t, server.URL, envelope(), "wrong")
	defer response.Body.Close()
	if response.StatusCode != 403 || calls.Load() != 0 {
		t.Fatal("unauthorized bridge request escaped")
	}
}

func TestCurrentCredentialPerRequest(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") != "excel" {
			t.Error("missing Excel profile")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["stream"] != true || body["store"] != false {
			t.Error("invalid BPS body")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completed)
	})
	for _, token := range []string{"Bearer mock-token-1", "Bearer mock-token-2"} {
		e := envelope()
		e.Authorization = token
		response := request(t, server.URL, e, testGate)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !bytes.Contains(body, []byte("input_tokens\":7")) {
			t.Fatalf("unexpected response %d %s", response.StatusCode, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatal("credential was cached or request repeated")
	}
}

func TestScopeSeparatesTenantsAndAccounts(t *testing.T) {
	a := envelope()
	pa, err := prepareForTest(a)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.RequestID = "another-request"
	pb, _ := prepareForTest(b)
	meta := func(p map[string]any) map[string]any { return p["metadata"].(map[string]any) }
	if meta(pa)["task_id"] != meta(pb)["task_id"] {
		t.Fatal("request id changed conversation identity")
	}
	for _, variant := range []string{"tenant", "account", "thread"} {
		b = a
		switch variant {
		case "tenant":
			b.TenantID = "other-key"
		case "account":
			b.AccountScope = "other-account"
		case "thread":
			b.ConversationID = "other-thread"
		}
		pb, _ = prepareForTest(b)
		if meta(pa)["task_id"] == meta(pb)["task_id"] {
			t.Fatalf("scope collision for %s", variant)
		}
	}
}

func TestKnownHostedFeaturesAreIgnoredWithoutExecution(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	cases := []map[string]any{
		{"model": "mock-model", "input": "hello", "tools": []any{map[string]any{"type": "web_search"}}},
		{"model": "mock-model", "input": "hello", "previous_response_id": "resp-test"},
		{"model": "mock-model", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA=="}}}}},
	}
	for index, source := range cases {
		e := envelope()
		e.Request = source
		response := request(t, server.URL, e, testGate)
		response.Body.Close()
		if index == 0 {
			if response.StatusCode != 502 {
				t.Fatalf("known hosted declaration should be ignored, got %d", response.StatusCode)
			}
		} else if response.StatusCode != 400 {
			t.Fatalf("unsupported request accepted: %d", response.StatusCode)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected only hosted declaration request to reach fixture, got %d", calls.Load())
	}
}

func TestRedirectDoesNotForwardCredential(t *testing.T) {
	var stolen atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { stolen.Add(1) }))
	defer sink.Close()
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, 307) })
	response := request(t, server.URL, envelope(), testGate)
	defer response.Body.Close()
	if response.StatusCode != 307 || stolen.Load() != 0 || response.Header.Get("Location") != "" {
		t.Fatal("redirect escaped the credential origin")
	}
}

func TestUpstreamFailureIsNotRetriedOrLeaked(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	response := request(t, server.URL, envelope(), testGate)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 503 || calls.Load() != 1 || bytes.Contains(body, []byte("mock-token")) {
		t.Fatal("upstream error retried or leaked credentials")
	}
}

func TestTextArrivesBeforeTerminal(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		textPreamble(w)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			textComplete(w)
		case <-r.Context().Done():
		}
	})
	response := request(t, server.URL, envelope(), testGate)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	seen := false
	for i := 0; i < 24; i++ {
		line, err := reader.ReadString(10)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "early") {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("text was not streamed before terminal")
	}
	once.Do(func() { close(release) })
	remaining, _ := io.ReadAll(reader)
	if !bytes.Contains(remaining, []byte("response.completed")) {
		t.Fatal("terminal response missing")
	}
}

func TestIncompleteAndUnauthorizedToolStreamsFail(t *testing.T) {
	for _, data := range []string{"event: response.created\ndata: {\"type\":\"response.created\"}\n\n", "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"dangerous-tool\"}}\n\n"} {
		recorder := httptest.NewRecorder()
		err := relayForTest(recorder, strings.NewReader(data))
		if err == nil {
			t.Fatal("invalid stream accepted")
		}
		if strings.Contains(recorder.Body.String(), "dangerous-tool") {
			t.Fatal("unauthorized tool forwarded")
		}
	}
}

func TestCancellationReachesUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	stopFixture := make(chan struct{})
	defer close(stopFixture)
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		// 上游正常解析完请求后才开始 SSE；否则 Go HTTP/1 服务端尚未启动断线监测。
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		textPreamble(w)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-stopFixture:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/bps/responses", bytes.NewReader(encoded(t, envelope())))
	req.Header.Set("X-Cpr-Bps-Gate", testGate)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
}

func TestMismatchedEventTypeIsNotForwarded(t *testing.T) {
	recorder := httptest.NewRecorder()
	frame := "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"not a tool\"}\n\n"
	if err := relayForTest(recorder, strings.NewReader(frame)); err == nil {
		t.Fatal("mismatched event type escaped validation")
	}
	if strings.Contains(recorder.Body.String(), "not a tool") || strings.Contains(recorder.Body.String(), "function_call_arguments.delta") {
		t.Fatal("invalid upstream event leaked into the client stream")
	}
	count := 0
	for _, line := range strings.Split(recorder.Body.String(), string(rune(10))) {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil || event["type"] != "error" {
			t.Fatal("missing structured rejection event")
		}
		nested, ok := event["error"].(map[string]any)
		if !ok || nested["code"] == nil {
			t.Fatal("missing standard error object")
		}
		count++
	}
	if count != 1 {
		t.Fatal("expected exactly one rejection event")
	}
}

func TestUnsupportedSemanticFieldIsNotSilentlyDropped(t *testing.T) {
	e := envelope()
	e.Request["temperature"] = json.Number("1")
	if _, err := prepareForTest(e); err == nil {
		t.Fatal("explicit output constraint was silently dropped")
	}
}

func TestRequestBeyondFormer16MiBLimitReachesUpstream(t *testing.T) {
	large := strings.Repeat("a", 17<<20)
	calls := 0
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !bytes.Contains(jsonBytesForLargeTest(body), []byte(large)) {
			t.Error("request truncated")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completed)
	})
	e := envelope()
	e.Request["input"] = large
	resp := request(t, server.URL, e, testGate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || calls != 1 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, calls)
	}
}
func jsonBytesForLargeTest(value any) []byte { b, _ := json.Marshal(value); return b }
func TestBridgeReportsActualSendState(t *testing.T) {
	var calls atomic.Int32
	_, server := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	response := request(t, server.URL, envelope(), "wrong-gate")
	response.Body.Close()
	if response.Header.Get("X-Cpr-Bps-Send-State") != "not_sent" || calls.Load() != 0 {
		t.Fatal("pre-send rejection mislabeled")
	}
	response = request(t, server.URL, envelope(), testGate)
	response.Body.Close()
	if response.Header.Get("X-Cpr-Bps-Send-State") != "sent" || response.Header.Get("X-Cpr-Bps-Route") != Route || calls.Load() != 1 {
		t.Fatal("upstream rejection lost send state")
	}
}

func prepareForTest(e Envelope) (map[string]any, error) {
	scope, _ := json.Marshal([]string{e.TenantID, e.AccountScope, e.ConversationID})
	session, err := kernel.New(e.Request, string(scope))
	if err != nil {
		return nil, err
	}
	return session.Prepare()
}
func relayForTest(w http.ResponseWriter, r io.Reader) error {
	session, err := kernel.New(envelope().Request, "test-scope")
	if err != nil {
		return err
	}
	return session.Relay(r, func() {}, func(data []byte) error { _, err := w.Write(data); return err })
}
