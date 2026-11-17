// plugin_mask.go：插件 options 的凭证脱敏（2026-11-17）。
//
// 背景：StatusView.Options 曾原样透出，而 magpie-bridge 等中间件会把
// 上游密钥放进 options（如 context-guard 的 summary.api_key）——与
// api.go「任何输出必须脱敏」的基线不符（本机管理面回环免票，一次
// GET 即得明文上游 key）。本组函数统一全部 options 出口：
//
//   - 回显（GET /api/plugins、GET/PUT /api/config/plugins、PUT
//     /api/plugins/{id}/options 的响应与日志）：maskOptionsCreds 深拷贝
//     脱敏，凭证键值 → 「***+末 4 位」（maskAPIKey 同款形）；
//   - 写入（PUT 两路径）：restoreOptionsCreds 把「凭证键 = *** 前缀」
//     还原为现存旧值——providers keepProviderKeys 同款往返语义，前端
//     拿脱敏形保存不破坏真 key。
package gateway

import "strings"

// credOptionKeys 是 options 内视为凭证的键名（大小写不敏感）。
var credOptionKeys = map[string]bool{
	"api_key": true, "apikey": true, "token": true, "secret": true,
	"password": true, "passwd": true, "authorization": true,
}

func isCredOptionKey(k string) bool { return credOptionKeys[strings.ToLower(k)] }

func maskCredValue(v string) string {
	if len(v) <= 4 {
		return "***"
	}
	return "***" + v[len(v)-4:]
}

// maskOptionsCreds 返回脱敏深拷贝（不改入参——视图值拷贝纪律）；
// 递归处理嵌套对象，数组值原样（凭证不落在数组语义）。
func maskOptionsCreds(opts map[string]any) map[string]any {
	if opts == nil {
		return nil
	}
	out := make(map[string]any, len(opts))
	for k, v := range opts {
		switch t := v.(type) {
		case string:
			if isCredOptionKey(k) {
				out[k] = maskCredValue(t)
				continue
			}
			out[k] = t
		case map[string]any:
			out[k] = maskOptionsCreds(t)
		default:
			out[k] = v
		}
	}
	return out
}

// restoreOptionsCreds 原地把 new 中「凭证键 = *** 前缀」的值还原为 old
// 同路径原值（GET 脱敏 → PUT 回传的往返保真）。old 无该路径/类型不符
// 时保留 new 值（用户显式输入新凭证的合法路径是真值，非 *** 前缀）。
func restoreOptionsCreds(newOpts, oldOpts map[string]any) {
	if newOpts == nil || oldOpts == nil {
		return
	}
	for k, nv := range newOpts {
		ov, has := oldOpts[k]
		if nm, ok := nv.(map[string]any); ok {
			if has {
				if om, ok2 := ov.(map[string]any); ok2 {
					restoreOptionsCreds(nm, om)
				}
			}
			continue
		}
		if s, ok := nv.(string); ok && isCredOptionKey(k) &&
			strings.HasPrefix(s, "***") && has {
			if os, ok2 := ov.(string); ok2 {
				newOpts[k] = os
			}
		}
	}
}
