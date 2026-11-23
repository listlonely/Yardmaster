//go:build windows

// 系统代理检测（F3 继承链「系统代理」环节落地，2026-11-23）：读
// WinINET 用户代理设置（HKCU Internet Settings）。FlClash/Clash 等
// 桌面代理开启「系统代理」后写这里——GitHub（api.github.com/release
// CDN）直连不可达的网络下，版本检查/更新下载借此自动走代理（此前
// 空全局代理=直连，托盘版本查询静默失败=「版本监测失效」实证根因）。
package netproxy

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

const winINETKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// SystemProxy 读 Windows 系统代理。返回 (ref, true)=系统代理已启用；
// ("", false)=未启用/未配置/读不到（调用方回退直连）。
func SystemProxy() (Ref, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, winINETKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return "", false
	}
	srv, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return "", false
	}
	ref := parseWinINETProxy(srv)
	if ref == "" {
		return "", false
	}
	return ref, true
}

// parseWinINETProxy 归一 ProxyServer 的三种形态：
//
//	"127.0.0.1:7890"                    全局单值（Clash 系默认写法）
//	"http=…;https=…;socks=…"            分协议
//	"=127.0.0.1:7890"                   分协议但仅缺省值
//
// 优先 https > http > socks；仅 socks 时用 socks5h://（域名解析在代
// 理端），其余补 http:// scheme（netproxy.Parse 要求显式 scheme）。
func parseWinINETProxy(srv string) Ref {
	var httpV, httpsV, socksV, def string
	for _, part := range strings.Split(srv, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		proto, addr, found := strings.Cut(part, "=")
		if !found {
			def = part // 无 = 的段是全局单值
			continue
		}
		switch strings.ToLower(strings.TrimSpace(proto)) {
		case "http":
			httpV = strings.TrimSpace(addr)
		case "https":
			httpsV = strings.TrimSpace(addr)
		case "socks":
			socksV = strings.TrimSpace(addr)
		default:
			def = strings.TrimSpace(addr)
		}
	}
	switch {
	case httpsV != "":
		return Ref("http://" + httpsV) // https= 指的是代理地址本身，走 http CONNECT
	case httpV != "":
		return Ref("http://" + httpV)
	case socksV != "":
		return Ref("socks5h://" + socksV)
	case def != "":
		return Ref("http://" + def)
	}
	return ""
}
