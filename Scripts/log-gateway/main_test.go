package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const plain = `{"code":200,"data":{"items":[{"id":"req_test","route":"/v1/responses","clientTransport":"http_json","billing":{"total":"9007199254740993"}}]}}`
const decorated = `{"code":200,"data":{"items":[{"id":"req_test","route":"/basispoints/api/responses","clientTransport":"BPS","billing":{"total":"9007199254740993"}}]}}`

func fixture(t *testing.T, mode string) (*gateway, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	revisions := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Cookie") != "session=test" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"code":401}`)
			return
		}
		switch {
		case eligible(r.Method, r.URL.Path):
			w.Header().Set("ETag", "native")
			w.Header().Set("X-Native", "preserve")
			if mode == "oversize" {
				_, _ = io.WriteString(w, strings.Repeat("x", maximumBody+1))
				return
			}
			_, _ = io.WriteString(w, plain)
		case r.URL.Path == extensionPath:
			calls.Add(1)
			if mode == "discovery-error" {
				w.WriteHeader(503)
				return
			}
			if mode == "disabled" {
				_, _ = io.WriteString(w, `{"data":[]}`)
				return
			}
			revision := revisions.Load() + 1
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"target": target{Instance: "test", Artifact: strings.Repeat("a", 64), Revision: uint64(revision)}, "routes": []any{map[string]string{"method": "POST", "path": "decorate-logs"}}}}})
		case strings.HasSuffix(r.URL.Path, "/api/decorate-logs"):
			calls.Add(1)
			if r.Header.Get("Origin") != "https://cpr.example" || r.Host != "cpr.example" {
				t.Error("origin not preserved")
			}
			b, _ := io.ReadAll(r.Body)
			if !bytes.Equal(b, []byte(plain)) {
				t.Error("native log changed before decoration")
			}
			switch mode {
			case "conflict":
				if revisions.Add(1) == 1 {
					w.WriteHeader(409)
					return
				}
			case "plugin-error":
				w.WriteHeader(502)
				return
			case "invalid":
				_, _ = io.WriteString(w, "bad-json")
				return
			case "redirect":
				w.Header().Set("Location", "https://outside.invalid/")
				w.WriteHeader(302)
				return
			}
			_, _ = io.WriteString(w, decorated)
		default:
			t.Errorf("unexpected upstream route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(upstream.Close)
	g, err := newGateway(upstream.URL, "https://cpr.example", "test")
	if err != nil {
		t.Fatal(err)
	}
	return g, calls
}
func request(g *gateway, path, method, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}
func TestExactRoutesAndAuthentication(t *testing.T) {
	g, calls := fixture(t, "ok")
	for _, path := range []string{"/v1/responses", "/assets/app.js", "/api/admin/usage/records/summary", "/api/admin/usage/records/extra"} {
		if got := request(g, path, "GET", "session=test"); got.Code != 404 {
			t.Fatal(path, got.Code)
		}
	}
	if got := request(g, "/api/admin/usage/records", "POST", "session=test"); got.Code != 404 {
		t.Fatal(got.Code)
	}
	got := request(g, "/api/admin/usage/records", "GET", "")
	if got.Code != 401 || calls.Load() != 0 {
		t.Fatal("unauthorized response passed to plugin", got.Code, calls.Load())
	}
}
func TestDecorateAndRevisionRetry(t *testing.T) {
	for _, mode := range []string{"ok", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			g, calls := fixture(t, mode)
			got := request(g, "/api/admin/usage/records?pageSize=1", "GET", "session=test")
			if got.Code != 200 || got.Body.String() != decorated || got.Header().Get("X-Native") != "preserve" || got.Header().Get("ETag") != "" {
				t.Fatal(got.Code, got.Body.String())
			}
			expected := int32(2)
			if mode == "conflict" {
				expected = 4
			}
			if calls.Load() != expected {
				t.Fatal(calls.Load())
			}
		})
	}
}
func TestPluginFailureAlwaysReturnsOriginal(t *testing.T) {
	for _, mode := range []string{"discovery-error", "disabled", "plugin-error", "invalid", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := fixture(t, mode)
			got := request(g, "/api/admin/usage/records", "GET", "session=test")
			if got.Code != 200 || got.Body.String() != plain || got.Header().Get("ETag") != "native" {
				t.Fatal(got.Code, got.Body.String())
			}
		})
	}
}
func TestBusyDecoratorReturnsOriginalWithoutCallbacks(t *testing.T) {
	g, calls := fixture(t, "ok")
	for i := 0; i < cap(g.slots); i++ {
		g.slots <- struct{}{}
	}
	got := request(g, "/api/admin/usage/records", "GET", "session=test")
	if got.Body.String() != plain || calls.Load() != 0 {
		t.Fatal(got.Body.String(), calls.Load())
	}
}
func TestLargeBodyIsForwardedWithoutTruncation(t *testing.T) {
	g, calls := fixture(t, "oversize")
	got := request(g, "/api/admin/usage/records", "GET", "session=test")
	if got.Body.Len() != maximumBody+1 || calls.Load() != 0 {
		t.Fatal(got.Body.Len(), calls.Load())
	}
}
func TestOnlyLoopbackUpstreams(t *testing.T) {
	for _, u := range []string{"http://example.com", "https://127.0.0.1", "http://127.0.0.1/a", "http://user:pass@127.0.0.1"} {
		if _, err := newGateway(u, "https://cpr.example", "test"); err == nil {
			t.Fatal(u)
		}
	}
}
