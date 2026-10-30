// 访问控制（M2-W5，F10 安全基线）：
//   - 监听地址默认仅回环（127.0.0.1:8317）；局域网共享 = 绑定非回环 +
//     强制令牌（ValidateSecurity 在 serve 启动与配置写回两处把关，
//     「宁可拒绝也不无鉴权暴露」）；
//   - Token 非空时 /v1/* 与 /api/* 要求匹配（Authorization: Bearer、
//     x-api-key 或 X-Gw-Token 均可——与 callerKeyOf 同源，Agent 的
//     接线 key 天然兼容）；/healthz 与控制台静态资源豁免（无敏感数据，
//     页面加载不受 token 影响，数据面由 API 401 驱动 token 输入）；
//   - Token 比对用 constant-time（防时序侧信道）。
package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// DefaultTokenExemptPaths 是免鉴权前缀：健康探针与控制台静态资源。
var tokenExemptPrefixes = []string{"/healthz", "/web/"}

// IsLoopbackAddr 判定监听地址是否仅回环可达。host 为空、localhost、
// 127.x、::1 均视为回环；解析失败按非回环处理（fail-closed）。
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 无端口（如 "localhost"）：按 host 判定
	}
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "", "localhost", "::1":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ValidateSecurity 校验监听/令牌组合：非回环监听必须设置 Token。
// serve 启动与 settings 写回（CLI/管理 API）共用同一规则。
func ValidateSecurity(listen, token string) error {
	if !IsLoopbackAddr(listen) && token == "" {
		return fmt.Errorf(
			"安全基线（F10）：监听地址 %q 非回环（局域网共享）时必须设置访问令牌（settings set token <值>），拒绝以此配置启动/保存", listen)
	}
	return nil
}

// tokenOK 常量时间比对请求携带的令牌。
func tokenOK(r *http.Request, want string) bool {
	got := r.Header.Get("X-Gw-Token")
	if got == "" {
		got = r.Header.Get("x-api-key")
	}
	if got == "" {
		if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			got = strings.TrimSpace(after)
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// authMiddleware 是令牌校验包装（Token 为空时原样放行——本机信任态）。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range tokenExemptPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if r.URL.Path == "/" {
			next.ServeHTTP(w, r) // 控制台入口页（静态壳，数据面走 API）
			return
		}
		if tokenOK(r, s.Token) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("WWW-Authenticate", `Bearer realm="yardmaster"`)
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "令牌缺失或不匹配（X-Gw-Token / Authorization: Bearer / x-api-key）"})
	})
}
