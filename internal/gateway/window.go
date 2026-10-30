// 订阅窗口与余额喂路由（M3-W5，SwitchWhenSpent）：
//   - 响应头学习：Anthropic unified / OpenAI 风格的限频头 → 账号窗口
//     状态与重置时刻，restricted 未到重置的账号排候选末段；
//   - 余额轮询：Provider.QuotaProbe 端点后台轮询 → 余额耗尽的账号与
//     当日额度耗尽同等休眠（至次日）；
//   - 轮询失败只降级（保留上次值），绝不阻断主流程。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yardmaster/internal/provider"
)

// parseWindowHeaders 从成功响应头解析厂商订阅窗口：
//   - Anthropic unified：anthropic-ratelimit-unified-status
//     （allowed|allowed_limited|restricted）+ anthropic-ratelimit-unified-5m_reset（RFC3339）
//   - OpenAI 风格：x-ratelimit-remaining-requests 为 0 时 restricted，
//     重置时刻取 x-ratelimit-reset-requests（Go duration 形如 "6m0s"）
//
// 两者都缺 → ("", 0)。
func parseWindowHeaders(h http.Header) (string, int64) {
	now := time.Now()
	if st := h.Get("anthropic-ratelimit-unified-status"); st != "" {
		reset := now.Add(5 * time.Minute).Unix()
		if rs := h.Get("anthropic-ratelimit-unified-5m_reset"); rs != "" {
			if t, err := time.Parse(time.RFC3339, rs); err == nil {
				reset = t.Unix()
			}
		}
		return st, reset
	}
	if h.Get("x-ratelimit-remaining-requests") == "0" {
		reset := now.Add(time.Minute).Unix()
		if rs := h.Get("x-ratelimit-reset-requests"); rs != "" {
			if d, err := time.ParseDuration(rs); err == nil {
				reset = now.Add(d).Unix()
			}
		}
		return "restricted", reset
	}
	return "", 0
}

// balanceKeys 是余额查询响应中按序查找的数值键（Zhipu data.total_balance
// 等厂商形状；值可为数字或数字字符串）。
var balanceKeys = []string{"total_balance", "remaining_balance", "balance", "credit", "remaining"}

// extractBalance 深度优先在 JSON 中找 balanceKeys 的首个数值。
func extractBalance(body []byte) (float64, bool) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, false
	}
	return findBalance(v, 0)
}

func findBalance(v any, depth int) (float64, bool) {
	if depth > 6 {
		return 0, false
	}
	switch t := v.(type) {
	case map[string]any:
		for _, key := range balanceKeys {
			if raw, ok := t[key]; ok {
				if f, ok := numOf(raw); ok {
					return f, true
				}
			}
		}
		// 键序优先：先按 balanceKeys 找，找不到再按序递归子节点
		for _, key := range balanceKeys {
			if child, ok := t[key].(map[string]any); ok {
				if f, ok := findBalance(child, depth+1); ok {
					return f, true
				}
			}
		}
		for _, child := range t {
			if f, ok := findBalance(child, depth+1); ok {
				return f, true
			}
		}
	case []any:
		for _, item := range t {
			if f, ok := findBalance(item, depth+1); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func numOf(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// SetWindow 记录账号订阅窗口状态（响应头学习）。
func (p *Planner) SetWindow(key, status string, resetAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.providers {
		if p.providers[i].ID != providerIDOf(key) {
			continue
		}
		for j := range p.providers[i].Accounts {
			if p.providers[i].Accounts[j].Name == accountNameOf(key) {
				p.providers[i].Accounts[j].WindowStatus = status
				p.providers[i].Accounts[j].WindowResetAt = resetAt
				return
			}
		}
	}
}

// SetBalance 记录账号余额（轮询结果；失败调用方不调用本方法=保留旧值）。
func (p *Planner) SetBalance(key string, bal float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.providers {
		if p.providers[i].ID != providerIDOf(key) {
			continue
		}
		for j := range p.providers[i].Accounts {
			if p.providers[i].Accounts[j].Name == accountNameOf(key) {
				p.providers[i].Accounts[j].Balance = bal
				p.providers[i].Accounts[j].BalanceAt = time.Now().Unix()
				return
			}
		}
	}
}

// Enter/Exit 账号在途请求计数（按 key 并发上限，M3-W5）。
func (p *Planner) Enter(key string) {
	p.mu.Lock()
	p.inflight[key]++
	p.mu.Unlock()
}

func (p *Planner) Exit(key string) {
	p.mu.Lock()
	if p.inflight[key] > 0 {
		p.inflight[key]--
		if p.inflight[key] == 0 {
			delete(p.inflight, key)
		}
	}
	p.mu.Unlock()
}

// inflightOf 返回在途计数（调用方已持锁）。
func (p *Planner) inflightOf(key string) int { return p.inflight[key] }

func providerIDOf(key string) string {
	id, _, _ := strings.Cut(key, "/")
	return id
}

func accountNameOf(key string) string {
	_, name, _ := strings.Cut(key, "/")
	return name
}

// accountSpent 判断账号订阅是否已用尽（M3-W5 SwitchWhenSpent）：
// 窗口 restricted 且未到重置，或余额轮询确认耗尽。
func accountSpent(a provider.Account, now time.Time) bool {
	if a.WindowStatus == "restricted" && a.WindowResetAt > now.Unix() {
		return true
	}
	if a.BalanceAt > 0 && a.Balance <= 0 {
		return true
	}
	return false
}

// StartQuotaPoller 启动余额轮询后台任务（随 ctx 退出；30 分钟或
// QuotaProbe.Interval）。
func (s *Server) StartQuotaPoller(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollQuotas(ctx)
			}
		}
	}()
}

// pollQuotas 轮询所有配置了 QuotaProbe 的 provider 的各账号余额。
// 失败只记日志（保留上次值），不阻断、不改状态。
func (s *Server) pollQuotas(ctx context.Context) {
	for _, prov := range s.Planner.Providers() {
		if prov.QuotaProbe == nil || prov.QuotaProbe.URL == "" {
			continue
		}
		for _, acc := range prov.Accounts {
			if acc.Auth == provider.AuthOAuth {
				continue // OAuth 订阅的余额语义不同，W5 不轮询
			}
			bal, err := s.probeBalance(ctx, &prov, acc)
			if err != nil {
				slog.Warn("余额轮询失败（保留上次值）", "provider", prov.ID, "account", acc.Name, "err", err)
				continue
			}
			s.Planner.SetBalance(prov.ID+"/"+acc.Name, bal)
			if bal <= 0 {
				slog.Info("账号余额耗尽，休眠至次日重置（SwitchWhenSpent）",
					"provider", prov.ID, "account", acc.Name, "balance", bal)
			}
		}
	}
}

// probeBalance 请求余额端点并解析数值。
func (s *Server) probeBalance(ctx context.Context, prov *provider.Provider, acc provider.Account) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prov.QuotaProbe.URL, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range prov.QuotaProbe.Headers {
		req.Header.Set(k, v)
	}
	if clientProtoOf(prov.Protocol) == ProtoMessages && prov.AuthStyle != "bearer" {
		req.Header.Set("x-api-key", acc.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+acc.APIKey)
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("余额端点 HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	bal, ok := extractBalance(b)
	if !ok {
		return 0, fmt.Errorf("余额响应无数值字段")
	}
	return bal, nil
}
