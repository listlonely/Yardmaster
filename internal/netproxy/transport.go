// Transport 构建上游 HTTP Transport。W2 为最简实现：每次按需构建；
// W3 升级为按 host 缓存连接池 + HTTP/2 健康检查（设计文档 §6.4）。
package netproxy

import (
	"fmt"
	"net/http"

	"golang.org/x/net/proxy"
)

// Transport 为 Ref 构建上游 RoundTripper；返回 (nil, nil) 表示无代理
// （调用方使用共享默认 Transport）。返回接口类型是为了避免类型化 nil
// （*http.Transport)(nil) 装入接口后 `!= nil` 的经典陷阱。
// socks5h 的域名解析发生在代理端（SOCKS5 协议对非 IP 目标按域名下发，
// 语义一致）。
func Transport(ref Ref) (http.RoundTripper, error) {
	u, err := Parse(ref)
	if err != nil {
		return nil, err
	}
	if u == nil || u.Scheme == string(Direct) {
		return nil, nil
	}
	switch u.Scheme {
	case "http", "https":
		return &http.Transport{Proxy: http.ProxyURL(u)}, nil
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer %q: %w", Redact(string(ref)), err)
		}
		t := &http.Transport{}
		if cd, ok := d.(proxy.ContextDialer); ok {
			t.DialContext = cd.DialContext
		} else {
			t.Dial = d.Dial
		}
		return t, nil
	default:
		// Parse 已拒绝其他 scheme；防御分支
		return nil, fmt.Errorf("proxy %q: 不支持的协议 %q", Redact(string(ref)), u.Scheme)
	}
}
