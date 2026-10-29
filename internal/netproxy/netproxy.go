// Package netproxy 解析代理配置并构建上游 Transport（PRD F3）。
// 支持 http/https/socks5/socks5h 与 direct；代理凭证在一切输出中脱敏
// （开发文档 §4.4）。
//
// 骨架阶段落地 Ref 解析与脱敏；Transport 构建、HTTP/2 连接健康检查、
// 启动预检在 M1-W3 实现（设计文档 §6.4）。
package netproxy

import (
	"fmt"
	"net/url"
	"strings"
)

// Ref 是代理配置引用（Provider.Proxy 与 Account.Proxy 的类型）。
// 空值表示继承上一级：账号 → Provider → 全局 → 系统代理 → 直连（F3 继承规则）。
type Ref string

// Direct 显式直连：阻断向上继承。
const Direct Ref = "direct"

// Parse 校验并解析一个 Ref。支持：
//
//	direct                                            显式直连
//	http:// | https:// | socks5:// | socks5h://host:port（可带 user:pass@）
//
// 其他 scheme（如 ftp://）在输入时即拒绝（F3 规则表）；socks5h 表示
// 域名解析发生在代理端。空 Ref 返回 (nil, nil)，语义为「继承上一级」。
func Parse(ref Ref) (*url.URL, error) {
	s := strings.TrimSpace(string(ref))
	if s == "" {
		return nil, nil
	}
	if Ref(s) == Direct {
		return &url.URL{Scheme: string(Direct)}, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", Redact(s), err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		if u.Host == "" {
			return nil, fmt.Errorf("proxy %q: missing host", Redact(s))
		}
		return u, nil
	case "":
		return nil, fmt.Errorf("proxy %q: 缺少 scheme（支持 http/https/socks5/socks5h/direct）", Redact(s))
	default:
		return nil, fmt.Errorf("proxy %q: 不支持的协议 %q（支持 http/https/socks5/socks5h/direct）", Redact(s), u.Scheme)
	}
}

// Redact 隐藏 URL 中的用户凭证：socks5://***@host:port（§4.4）。
// 无法解析时原样返回（调用方还会经过 Parse 拒绝它）。
func Redact(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return raw
	}
	// url.User("***") 会被 String() 转义成 %2A，手工拼接保证可读的 *** 形式。
	return u.Scheme + "://***@" + u.Host
}
