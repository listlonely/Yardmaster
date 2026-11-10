// 厂商特判（M3-W4）：200 伪装拒绝识别与 GLM Coding Plan 端点纠偏。
// 思路对齐 Magpie（internal/gateway/routing.go 的失败词表与
// carried-error 处理），按本项目规模裁剪。
package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// 失败词表（Magpie routing.go 裁剪版）：按错误体关键词映射等价状态码。
var (
	reVendorRate   = regexp.MustCompile(`(?i)rate.?limit|too many requests|frequency|频率|太频繁|rate_limit_exceeded`)
	reVendorCredit = regexp.MustCompile(`(?i)insufficient_quota|insufficient.?(balance|credit)|余额不足|请充值|欠费|no resource package`)
	reVendorQuota  = regexp.MustCompile(`(?i)quota|usage.?limit|额度|用量|套餐|上限`)
)

// errorReply 判定一个 200 响应体是否「整体就是一个错误」（Zhipu 等厂商
// 以 200 伪装拒绝；Magpie carried-error 同款）并映射到等价 HTTP 状态：
// 余额/额度→402、限频→429、其余→502（按上游失败切换候选）。正常回复
// （含 choices/content）即使带 error 字段也不算。
func errorReply(body []byte) (int, bool) {
	trim := bytes.TrimSpace(body)
	if len(trim) == 0 || trim[0] != '{' {
		return 0, false
	}
	var probe struct {
		Error   json.RawMessage `json:"error"`
		Choices []any           `json:"choices"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(trim, &probe); err != nil {
		return 0, false
	}
	if len(probe.Error) == 0 || string(probe.Error) == "null" {
		return 0, false
	}
	if len(probe.Choices) > 0 || (len(probe.Content) > 0 && string(probe.Content) != "null") {
		return 0, false
	}
	switch {
	case reVendorCredit.Match(probe.Error), reVendorQuota.Match(probe.Error):
		return http.StatusPaymentRequired, true
	case reVendorRate.Match(probe.Error):
		return http.StatusTooManyRequests, true
	default:
		return http.StatusBadGateway, true
	}
}

// sseErrorEvent 从 200 SSE 流的首个 data 事件提取伪装错误（错误在
// 首事件出现时可在中继前 failover；后续事件的错误响应已开始，不切换）。
// 仅识别已知厂商怪癖（限频/余额/额度）；其余错误事件交回原中继路径
// （流干净收尾，M1 契约不变）。
func sseErrorEvent(peek []byte) (int, bool) {
	for _, line := range strings.Split(string(peek), "\n") {
		line = strings.TrimSuffix(line, "\r")
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || rest == "[DONE]" {
			return 0, false
		}
		if mapped, ok := errorReply([]byte(rest)); ok && mapped != http.StatusBadGateway {
			return mapped, true
		}
		return 0, false // 首个 data 是正常事件或非怪癖错误
	}
	return 0, false
}

// glmCorrectedBase 判定 GLM 1113（key 类型与端点不匹配：余额错文案）
// 场景：仅当当前 base 含按量路径（/api/paas/v4）时，返回纠偏后的
// Coding Plan 端点（Magpie：1113 → 重试 coding 端点并迁移 provider；
// 按路径匹配，主机无关——代理/镜像同样生效）。Anthropic 协议的
// /api/anthropic 本身即 Plan 端点，无需纠偏。
func glmCorrectedBase(base string, errBody []byte) (string, bool) {
	if !strings.Contains(string(errBody), "1113") {
		return "", false
	}
	if !strings.Contains(base, "/api/paas/v4") {
		return "", false
	}
	return strings.Replace(base, "/api/paas/v4", "/api/coding/paas/v4", 1), true
}

// reGlmQuotaReset 匹配 GLM/Zhipu 429 文案中的重置时刻（M6-W1 实测样本：
// 「已达到 5 小时的使用上限。您的限额将在 2026-10-30 16:23:38 重置。」）。
// 秒可缺省；日期段固定 YYYY-MM-DD。
var reGlmQuotaReset = regexp.MustCompile(
	`将(?:于|在)\s*(\d{4}-\d{2}-\d{2})\s+(\d{1,2}:\d{2})(?::(\d{2}))?\s*重置`)

// glmQuotaReset 从 429 错误体提取上游明确给出的重置时刻（RestUntil
// 精准休眠 + SetWindow「窗口限频」徽章的数据源）。读不懂=未知：返回
// false 走默认休眠。厂商文案无时区标记，按本地时区（部署侧与厂商
// 同域时正确；否则误差为时区差，仍有兜底 5 分钟节奏可比对）。
func glmQuotaReset(body []byte) (time.Time, bool) {
	m := reGlmQuotaReset.FindSubmatch(body)
	if m == nil {
		return time.Time{}, false
	}
	sec := "00"
	if m[3] != nil {
		sec = string(m[3])
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05",
		string(m[1])+" "+string(m[2])+":"+sec, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CorrectBaseURL 端点纠偏：更新内存快照并读改写落盘 providers.json
// （写前备份；重读文件只改该 provider 的 base_url，不覆盖运行期其他
// 编辑——与 FlushUsage 同规）。base 相同或文件不存在时为空操作。
func (p *Planner) CorrectBaseURL(providerID, newBase, providersPath string) error {
	p.mu.Lock()
	found := false
	for i := range p.providers {
		if p.providers[i].ID == providerID {
			if p.providers[i].BaseURL == newBase {
				p.mu.Unlock()
				return nil
			}
			p.providers[i].BaseURL = newBase
			found = true
			break
		}
	}
	p.mu.Unlock()
	if !found {
		return fmt.Errorf("provider %q 不存在", providerID)
	}
	if providersPath == "" {
		return nil
	}
	return persistProviderEdit(providersPath, providerID, func(pf *provider.Provider) { pf.BaseURL = newBase })
}

// persistProviderEdit 把对单个 provider 的字段修改合并写回 providers.json。
// 读不懂=未知：解析失败拒绝覆写（返回错误，调用方记日志）。
func persistProviderEdit(providersPath, providerID string, mut func(*provider.Provider)) error {
	f, err := store.LoadProvidersFile(providersPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for i := range f.Providers {
		if f.Providers[i].ID != providerID {
			continue
		}
		mut(&f.Providers[i])
		backupDir := filepath.Join(filepath.Dir(providersPath), "backup")
		if err := store.BackupConfig(providersPath, backupDir, store.BaseName(providersPath), 10); err != nil {
			return err
		}
		return store.SaveProvidersFile(providersPath, f)
	}
	return nil
}

// peekBody 对 200 响应做伪装拒绝嗅探：返回（等价状态码, 嗅探片段,
// 未命中）。未命中时把已读字节接回响应体（relay 无感）。
func peekBody(resp *http.Response, stream bool) (int, []byte, bool) {
	br := bufio.NewReaderSize(resp.Body, 8192)
	peek, _ := br.Peek(8192)
	mapped, sniffed := errorReply(peek)
	if !sniffed && stream {
		mapped, sniffed = sseErrorEvent(peek)
	}
	if sniffed {
		return mapped, peek, false
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{br, resp.Body}
	return 0, peek, true
}
