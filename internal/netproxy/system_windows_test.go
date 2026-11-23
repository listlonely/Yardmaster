//go:build windows

// parseWinINETProxy 单测（2026-11-23）：ProxyServer 三形态归一。
package netproxy

import "testing"

func TestParseWinINETProxy(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
	}{
		{"127.0.0.1:7890", "http://127.0.0.1:7890"},            // 全局单值（Clash 系默认）
		{"http=1.2.3.4:8080", "http://1.2.3.4:8080"},           // 仅 http
		{"https=1.2.3.4:8443", "http://1.2.3.4:8443"},          // 仅 https（代理地址本身）
		{"http=1.2.3.4:8080;https=5.6.7.8:8443", "http://5.6.7.8:8443"}, // https 优先
		{"socks=9.9.9.9:1080", "socks5h://9.9.9.9:1080"},       // 仅 socks → socks5h
		{"=127.0.0.1:9000", "http://127.0.0.1:9000"},           // 仅缺省值
		{"", ""},                                                // 空=无代理
		{"  ; ; ", ""},                                          // 全空白=无代理
	}
	for _, c := range cases {
		if got := parseWinINETProxy(c.in); got != c.want {
			t.Errorf("parseWinINETProxy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSystemProxyDisabled(t *testing.T) {
	// 本机语义断言：ProxyEnable=0 或键不存在时返回 ("" ,false)。
	// CI/沙箱环境注册表状态未知——只验证"不 panic、bool 一致"。
	ref, ok := SystemProxy()
	if ok && ref == "" {
		t.Error("SystemProxy 报告启用但 ref 为空")
	}
	if !ok && ref != "" {
		t.Errorf("SystemProxy 报告未启用但 ref=%q", ref)
	}
}
