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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"yardmaster/internal/store"
)

// DefaultTokenExemptPaths 是免鉴权前缀：健康探针与控制台静态资源。
var tokenExemptPrefixes = []string{"/healthz", "/web/"}

// SetTokenHashes 热替换命名令牌（settings.tokens）hash 集：atomic 持有
// 于 Server 实例，令牌管理写路径调用（生成/轮换/删除即时生效，无需重
// 启）。nil/空切片=清空。
func (s *Server) SetTokenHashes(hashes []string) {
	if len(hashes) == 0 {
		hashes = []string{}
	}
	s.tokenHashes.Store(hashes)
}

// HasTokenHashes 报告当前是否持有命名令牌。
func (s *Server) HasTokenHashes() bool {
	v, _ := s.tokenHashes.Load().([]string)
	return len(v) > 0
}

// TokenHashList 取当前 hash 集快照（未装载时 nil）。
func (s *Server) TokenHashList() []string {
	v, _ := s.tokenHashes.Load().([]string)
	return v
}

// TokenHashesOf 把命名令牌清单映射为 hash 集。
func TokenHashesOf(entries []store.TokenEntry) []string {
	out := make([]string, 0, len(entries))
	for _, t := range entries {
		if t.Hash != "" {
			out = append(out, t.Hash)
		}
	}
	return out
}

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
	return ValidateSecurityN(listen, token, 0)
}

// ValidateSecurityN 同上，nTokens 为命名令牌（settings.tokens）数量：
// 任一形态令牌存在即满足局域网共享的安全基线。
func ValidateSecurityN(listen, token string, nTokens int) error {
	if !IsLoopbackAddr(listen) && token == "" && nTokens == 0 {
		return fmt.Errorf(
			"安全基线（F10）：监听地址 %q 非回环（局域网共享）时必须设置访问令牌（settings set token <值> 或在控制台「设置」生成命名令牌），拒绝以此配置启动/保存", listen)
	}
	return nil
}

// tokenFrom 提取请求携带的令牌（三头同源，与 callerKeyOf 一致）。
func tokenFrom(r *http.Request) string {
	got := r.Header.Get("X-Gw-Token")
	if got == "" {
		got = r.Header.Get("x-api-key")
	}
	if got == "" {
		if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			got = strings.TrimSpace(after)
		}
	}
	return got
}

// tokenOK 常量时间比对请求携带的令牌（legacy 单令牌）。
func tokenOK(r *http.Request, want string) bool {
	got := tokenFrom(r)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// tokenHashOK 常量时间比对请求令牌的 sha256 与命名令牌 hash 集（多令
// 牌，2026-11 批2：plain 不落盘，比对走 hash 侧）。
func tokenHashOK(r *http.Request, hashes []string) bool {
	got := tokenFrom(r)
	if got == "" || len(hashes) == 0 {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	gotHash := hex.EncodeToString(sum[:])
	for _, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(gotHash), []byte(h)) == 1 {
			return true
		}
	}
	return false
}

// authMiddleware 是令牌校验包装（无任何令牌时原样放行——本机信任态）。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" && !s.HasTokenHashes() {
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
		if (s.Token != "" && tokenOK(r, s.Token)) || tokenHashOK(r, s.TokenHashList()) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("WWW-Authenticate", `Bearer realm="yardmaster"`)
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "令牌缺失或不匹配（X-Gw-Token / Authorization: Bearer / x-api-key）"})
	})
}
