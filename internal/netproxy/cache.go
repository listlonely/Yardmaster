// 共享 Transport 缓存（PRD F3）：同一代理端点复用同一个 Transport，
// 从而复用其连接池；每个 Transport 启用 HTTP/2 健康检查（静默连接
// 20s 无读即 ping，10s 无 pong 判死——对齐设计文档 §6.4，防「代理
// 半死连接」挂起请求；在途流式响应不受影响）。
package netproxy

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const (
	// readIdleTimeout 是静默连接触发健康 ping 的阈值。
	readIdleTimeout = 20 * time.Second
	// pingTimeout 是 ping 无响应判死的阈值。
	pingTimeout = 10 * time.Second
)

var (
	cacheMu sync.Mutex
	cache   = map[string]http.RoundTripper{}
)

// SharedTransport 返回按 Ref 缓存的 RoundTripper；(nil, nil) 表示无代理。
// 缓存键为 scheme+host（脱敏不含凭证）——同一代理端点的多账号共享连接池。
func SharedTransport(ref Ref) (http.RoundTripper, error) {
	u, err := Parse(ref)
	if err != nil {
		return nil, err
	}
	if u == nil || u.Scheme == string(Direct) {
		return nil, nil
	}
	key := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)

	cacheMu.Lock()
	if tr, ok := cache[key]; ok {
		cacheMu.Unlock()
		return tr, nil
	}
	cacheMu.Unlock()

	tr, err := buildTransport(u, string(ref))
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if existing, ok := cache[key]; ok {
		return existing, nil // 并发下别人先建好
	}
	cache[key] = tr
	return tr, nil
}

// buildTransport 构建单个 Transport 并启用 HTTP/2 健康参数。
func buildTransport(u *url.URL, raw string) (http.RoundTripper, error) {
	switch u.Scheme {
	case "http", "https":
		tr := &http.Transport{Proxy: http.ProxyURL(u)}
		enableHTTP2Health(tr)
		return tr, nil
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer %q: %w", Redact(raw), err)
		}
		tr := &http.Transport{}
		if cd, ok := d.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		} else {
			tr.Dial = d.Dial
		}
		enableHTTP2Health(tr)
		return tr, nil
	default:
		return nil, fmt.Errorf("proxy %q: 不支持的协议 %q", Redact(raw), u.Scheme)
	}
}

// enableHTTP2Health 为 HTTPS 上游启用 HTTP/2 静默连接健康检查；
// 纯 HTTP 上游无 h2，ConfigureTransports 返回错误时忽略即可。
func enableHTTP2Health(tr *http.Transport) {
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	if h2, err := http2.ConfigureTransports(tr); err == nil && h2 != nil {
		h2.ReadIdleTimeout = readIdleTimeout
		h2.PingTimeout = pingTimeout
	}
}
