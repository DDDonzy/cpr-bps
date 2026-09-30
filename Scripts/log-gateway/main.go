// 日志显示转发只进入三个管理员 GET 接口，其他请求仍直接由 CPR 处理。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maximumBody = 4 << 20
const extensionPath = "/api/admin/plugins/extensions"

type target struct {
	Instance string `json:"instanceId"`
	Artifact string `json:"artifactSha256"`
	Revision uint64 `json:"revision"`
}
type extension struct {
	Target target `json:"target"`
	Routes []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"routes"`
}
type gateway struct {
	base     *url.URL
	origin   *url.URL
	instance string
	client   *http.Client
	proxy    *httputil.ReverseProxy
	slots    chan struct{}
}

func eligible(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	switch path {
	case "/api/admin/usage/records", "/api/admin/usage/records/detail", "/api/admin/operations/errors":
		return true
	}
	return false
}
func newGateway(upstream, origin, instance string) (*gateway, error) {
	base, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(base.Hostname())
	if base.Scheme != "http" || ip == nil || !ip.IsLoopback() || base.User != nil || base.Path != "" || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("upstream must be a fixed loopback HTTP origin")
	}
	public, err := url.Parse(origin)
	if err != nil || public == nil || public.Scheme != "https" || public.Host == "" || public.User != nil || public.Path != "" || public.RawQuery != "" || public.Fragment != "" {
		return nil, errors.New("public origin must be HTTPS")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxIdleConnsPerHost = 16
	g := &gateway{base: base, origin: public, instance: instance, slots: make(chan struct{}, 4), client: &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	g.proxy = &httputil.ReverseProxy{Transport: transport, Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(base)
		p.Out.Host = p.In.Host
		p.Out.Header.Set("Accept-Encoding", "identity")
		for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
			if v := p.In.Header.Get(k); v != "" {
				p.Out.Header.Set(k, v)
			}
		}
	}, ModifyResponse: g.modify, ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
		http.Error(w, "CPR temporarily unavailable", http.StatusBadGateway)
	}}
	return g, nil
}
func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if !eligible(r.Method, r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// 每次查询仍由 CPR 认证，不保存 Cookie/Key，也不使用服务管理员凭据。
func (g *gateway) fetch(ctx context.Context, in *http.Request, method, path string, body []byte) ([]byte, int, error) {
	u := *g.base
	u.Path = path
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Host = g.origin.Host
	for _, k := range []string{"Authorization", "Cookie"} {
		if v := in.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Origin", g.origin.String())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maximumBody+1))
	if len(b) > maximumBody {
		return nil, res.StatusCode, errors.New("metadata response too large")
	}
	return b, res.StatusCode, err
}
func (g *gateway) decorate(ctx context.Context, in *http.Request, body []byte) ([]byte, error) {
	// 版本切换发生冲突时只重试一次幂等的显示转换，不重放模型请求。
	for attempt := 0; attempt < 2; attempt++ {
		b, status, err := g.fetch(ctx, in, http.MethodGet, extensionPath, nil)
		if err != nil || status != 200 {
			return nil, errors.New("extension discovery unavailable")
		}
		var envelope struct {
			Data []extension `json:"data"`
		}
		if json.Unmarshal(b, &envelope) != nil {
			return nil, errors.New("invalid extensions")
		}
		var found *target
		for _, e := range envelope.Data {
			if e.Target.Instance == g.instance {
				for _, route := range e.Routes {
					if route.Method == http.MethodPost && route.Path == "decorate-logs" {
						t := e.Target
						found = &t
						break
					}
				}
			}
		}
		if found == nil {
			return nil, errors.New("decorator not enabled")
		}
		path := fmt.Sprintf("%s/%s/%s/%d/api/decorate-logs", extensionPath, url.PathEscape(found.Instance), url.PathEscape(found.Artifact), found.Revision)
		b, status, err = g.fetch(ctx, in, http.MethodPost, path, body)
		if status == 409 && attempt == 0 {
			continue
		}
		if err != nil || status != 200 || !json.Valid(b) {
			return nil, errors.New("decorator unavailable")
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(b, &object) != nil || object["data"] == nil {
			return nil, errors.New("invalid decorated response")
		}
		return b, nil
	}
	return nil, errors.New("decorator revision changed")
}

type restoredBody struct {
	io.Reader
	closer io.Closer
}

func (b restoredBody) Close() error { return b.closer.Close() }
func (g *gateway) modify(res *http.Response) error {
	if res.StatusCode != http.StatusOK || !eligible(res.Request.Method, res.Request.URL.Path) || !strings.Contains(res.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	if enc := res.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil
	}
	// 显示转换拥堵、插件停用或失败时返回 CPR 原始日志，不阻塞正常管理界面。
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		return nil
	}
	if res.ContentLength > maximumBody {
		return nil
	}
	old := res.Body
	b, err := io.ReadAll(io.LimitReader(old, maximumBody+1))
	res.Body = restoredBody{Reader: io.MultiReader(bytes.NewReader(b), old), closer: old}
	if err != nil || len(b) > maximumBody || !json.Valid(b) {
		return nil
	}
	ctx, cancel := context.WithTimeout(res.Request.Context(), 2*time.Second)
	defer cancel()
	transformed, err := g.decorate(ctx, res.Request, b)
	if err != nil {
		return nil
	}
	_ = old.Close()
	res.Body = io.NopCloser(bytes.NewReader(transformed))
	res.ContentLength = int64(len(transformed))
	res.Header.Set("Content-Length", strconv.Itoa(len(transformed)))
	res.Header.Del("ETag")
	res.Header.Del("Content-MD5")
	res.Header.Set("Cache-Control", "no-store")
	return nil
}
func main() {
	listen := flag.String("listen", "127.0.0.1:18901", "loopback listen address")
	upstream := flag.String("upstream", "http://127.0.0.1:8080", "CPR loopback origin")
	origin := flag.String("public-origin", "https://us.cpr.dddonzy.xyz", "public CPR origin")
	instance := flag.String("instance", "1810791f-c16b-8e48-b9d2-77b7d13315f0", "BPS instance")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("listen must use a loopback IP")
	}
	g, err := newGateway(*upstream, *origin, *instance)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Print("BPS log gateway listening on loopback; model and static routes excluded")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
