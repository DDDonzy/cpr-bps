// 请求画像参考 CPA BPS v0.1.18；许可见 third_party/CPA_LICENSE。
package bps

import (
	"bytes"
	"context"
	"cpr-excel-adapter/internal/kernel"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const ResponsesURL = "https://bps.openai.com/basispoints/api/responses"
const Route = "excel-bps-v1"

// 仅接受宿主当前请求的认证，不保存 refresh token，不实现账号池。
type Envelope struct {
	Version        int            `json:"version"`
	RequestID      string         `json:"request_id"`
	TenantID       string         `json:"tenant_id"`
	AccountScope   string         `json:"account_scope"`
	ConversationID string         `json:"conversation_id"`
	Authorization  string         `json:"authorization"`
	AccountID      string         `json:"account_id"`
	ProxyURL       string         `json:"proxy_url,omitempty"`
	Request        map[string]any `json:"request"`
}

type Bridge struct {
	gate         []byte
	responsesURL string

	history *kernel.HistoryCache
}

func New(gate string) (*Bridge, error) {
	if len(gate) != 64 || strings.IndexFunc(gate, func(c rune) bool { return !strings.ContainsRune("0123456789abcdef", c) }) >= 0 {
		return nil, errors.New("gate must be 64 lowercase hexadecimal characters")
	}
	return &Bridge{gate: []byte(gate), responsesURL: ResponsesURL, history: kernel.NewHistoryCache()}, nil
}

func validText(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsAny(s, "\r\n\x00")
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/bps/responses" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Cpr-Bps-Gate")), b.gate) != 1 {
		writeError(w, 403, "bridge_authentication_failed")
		return
	}
	// Request admission belongs to CPR. Do not impose a second fixed
	// concurrency limit or a 16 MiB business payload limit in this adapter.
	var e Envelope
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err == nil {
			writeError(w, 400, "invalid_bridge_envelope")
		} else {
			writeDecodeError(w, err)
		}
		return
	}
	if (e.Version != 1 && e.Version != 2) || !validText(e.RequestID, 256) || !validText(e.TenantID, 256) || !validText(e.AccountScope, 4096) || !validText(e.ConversationID, 256) || !validText(e.AccountID, 256) || !validText(e.Authorization, 16384) || !strings.HasPrefix(e.Authorization, "Bearer ") || len(e.Authorization) <= 7 {
		writeError(w, 400, "invalid_bridge_envelope")
		return
	}
	// 只用于关联本地拒绝原因，不保存请求正文或认证信息。
	w.Header().Set("X-Cpr-Bps-Request-Id", e.RequestID)
	if e.Request["previous_response_id"] != nil && e.Version != 2 {
		writeError(w, 400, "continuation_requires_v2")
		return
	}
	scope, _ := json.Marshal([]string{e.TenantID, e.AccountScope, e.ConversationID})
	model, _ := e.Request["model"].(string)
	historyScope, _ := json.Marshal([]string{e.TenantID, e.AccountScope, e.AccountID, e.ConversationID, model})
	resolved, err := b.history.Resolve(string(historyScope), e.Request)
	if err != nil {
		var api *kernel.APIError
		if errors.As(err, &api) && api.Code() == "previous_response_not_found" {
			w.Header().Set("X-Cpr-Bps-Continuation", "missing")
		}
		writeProtocolError(w, err)
		return
	}
	session, err := kernel.New(resolved, string(scope))
	if err != nil {
		var api *kernel.APIError
		if errors.As(err, &api) {
			code := safeBridgeErrorCode(api.Code())
			if api.Code() == "unsupported_request_field" {
				log.Printf("bps_bridge_request_shape request_id=%q fields=%q", e.RequestID, safeRequestFields(resolved))
			}
			if api.Code() == "hosted_tool_not_implemented" {
				log.Printf("bps_bridge_tool_shape request_id=%q tools=%q", e.RequestID, safeRequestToolTypes(resolved))
			}
			log.Printf("bps_bridge_protocol_rejected request_id=%q code=%q", e.RequestID, code)
		}
		writeProtocolError(w, err)
		return
	}
	session.OnCompleted(func(response map[string]any) {
		id, _ := response["id"].(string)
		output, _ := response["output"].([]any)
		b.history.Remember(string(historyScope), id, resolved["input"], output)
	})
	payload, err := session.Prepare()
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	if reasoning, ok := resolved["reasoning"].(map[string]any); ok {
		requested, _ := reasoning["effort"].(string)
		effective, _ := payload["reasoning_effort"].(string)
		if requested != effective {
			// Prepare has already validated this public enum; no user text is logged.
			log.Printf("bps_reasoning_mapped request_id=%q requested=%q effective=%q", e.RequestID, strings.ToLower(strings.TrimSpace(requested)), effective)
		}
	}
	images, err := collectImages(payload)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	if e.ProxyURL != "" {
		proxy, err := url.Parse(e.ProxyURL)
		if err != nil || proxy.Hostname() == "" || proxy.Port() == "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			writeError(w, 400, "invalid_account_proxy")
			return
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if err := b.uploadImages(ctx, client, e, images); err != nil {
		writeErrorState(w, 502, err.Error(), "ambiguous")
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeError(w, 400, "invalid_request")
		return
	}

	// 不配置 GetBody 或幂等重放键，且不显式重试。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.responsesURL, io.NopCloser(bytes.NewReader(encoded)))
	if err != nil {
		writeError(w, 500, "bridge_configuration_error")
		return
	}
	req.ContentLength = int64(len(encoded))
	req.Header = authHeaders(e)
	response, err := client.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			writeErrorState(w, 502, "bps_transport_failed", "ambiguous")
		}
		return
	}
	defer response.Body.Close()
	// 与桥自身错误码分开，只记录实际收到的 BPS HTTP 状态。
	w.Header().Set("X-Cpr-Bps-Upstream-Status", strconv.Itoa(response.StatusCode))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Diagnose a bounded prefix, then forward the complete original body unchanged.
		prefix, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
		if (response.StatusCode == 400 || response.StatusCode == 422) && len(prefix) <= 65536 {
			if summary := validationSummary(bytes.NewReader(prefix)); summary != "" {
				w.Header().Set("X-Cpr-Bps-Validation", summary)
			}
		}
		for _, key := range []string{"Content-Type", "X-Request-ID", "Retry-After"} {
			if value := response.Header.Get(key); value != "" {
				w.Header().Set(key, value)
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Cpr-Bps-Route", Route)
		w.Header().Set("X-Cpr-Bps-Send-State", "sent")
		w.WriteHeader(response.StatusCode)
		secrets := []string{e.Authorization, strings.TrimPrefix(e.Authorization, "Bearer "), string(b.gate)}
		if readErr == nil {
			_ = copyErrorBody(w, io.MultiReader(bytes.NewReader(prefix), response.Body), secrets)
		} else {
			_, _ = w.Write(redactCurrentCredentials(prefix, secrets))
		}
		return
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		writeErrorState(w, 502, "bps_invalid_content_type", "sent")
		return
	}
	if id := response.Header.Get("X-Request-ID"); validText(id, 256) {
		w.Header().Set("X-Request-ID", id)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Cpr-Bps-Route", Route)
	w.Header().Set("X-Cpr-Bps-Send-State", "sent")
	// 内核统一输出有序且完整的 error 事件；此处不重复写无序的半结构帧。
	relayErr := session.Relay(response.Body, func() {}, func(data []byte) error {
		if _, err := w.Write(redactCurrentCredentials(data, []string{e.Authorization, strings.TrimPrefix(e.Authorization, "Bearer "), string(b.gate)})); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	})
	stats := session.StreamingStatistics()
	log.Printf("bps_tool_generation request_id=%q upstream_reads=%d upstream_bytes=%d upstream_events=%d last_data_age_ms=%d tool_chunks=%d tool_bytes=%d progress_events=%d relay_ok=%t caller_cancelled=%t", e.RequestID, stats.UpstreamReadChunks, stats.UpstreamBytes, stats.UpstreamEvents, stats.LastUpstreamDataAgeMS, stats.ToolArgumentDeltas, stats.ToolArgumentBytes, stats.ToolProgressEvents, relayErr == nil, r.Context().Err() != nil)
	var validation *kernel.StreamValidationError
	if errors.As(relayErr, &validation) {
		log.Printf("bps_stream_validation request_id=%q site=%q", e.RequestID, validation.Location)
	}
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		writeError(w, 413, "bridge_request_too_large")
	} else {
		writeError(w, 400, "invalid_bridge_envelope")
	}
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeErrorState(w, status, code, "not_sent")
}
func writeErrorState(w http.ResponseWriter, status int, code, state string) {
	safeCode := safeBridgeErrorCode(code)
	w.Header().Set("X-Cpr-Bps-Error-Code", safeCode)
	if state == "not_sent" {
		log.Printf("bps_bridge_rejected request_id=%q status=%d send_state=%q code=%q", w.Header().Get("X-Cpr-Bps-Request-Id"), status, state, safeCode)
	}
	w.Header().Set("X-Cpr-Bps-Route", Route)
	w.Header().Set("X-Cpr-Bps-Send-State", state)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "bps_bridge_error", "code": code, "message": code}})
}

func authHeaders(e Envelope) http.Header {
	h := http.Header{"Authorization": {e.Authorization}, "Chatgpt-Account-Id": {e.AccountID}, "X-Openai-Account-Id": {e.AccountID}, "Content-Type": {"application/json"}, "Accept": {"text/event-stream"}, "Accept-Encoding": {"identity"}, "Origin": {"https://bps.openai.com"}, "X-Basispoints-Auth-Mode": {"chatgpt"}, "User-Agent": {"cpr-excel-plugin/0.1.0-dev.1"}}
	for key, value := range map[string]string{"Client-Agent-Profile": "excel", "Client-Editor": "excel", "Client-Host": "office", "Client-Platform": "excel", "Client-Platform-Class": "PC", "Client-Product": "basispoints-excel-plugin", "Client-Runtime": "desktop", "Office-Host": "Excel", "Office-Platform": "PC"} {
		h.Set("X-OpenAI-Internal-Basispoints-"+key, value)
	}
	return h
}

// 监听地址必须是数值回环 IP，不能通过 DNS 绕过边界。
func ValidateListen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("invalid bridge listen address")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("bridge must listen on a numeric loopback address")
	}
	return nil
}

func writeProtocolError(w http.ResponseWriter, err error) {
	var api *kernel.APIError
	if errors.As(err, &api) {
		writeError(w, api.StatusCode(), api.Code())
		return
	}
	writeError(w, 400, "invalid_request")
}
