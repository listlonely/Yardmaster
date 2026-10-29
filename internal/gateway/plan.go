// 候选计划骨架（PRD F6）：把「provider/model」解析为候选序列
// （每个可用账号一个候选），失败候选休眠（restLast 排序末尾）。
// 策略（order/rotate/smart）在 W5 接入 internal/routes；W2 默认
// 「账号声明顺序，休眠排末尾」，即 order 策略的退化形式。
package gateway

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agenthub/internal/provider"
)

// Candidate 是一次尝试的最小服务单元：成员模型 × 一个账号。
type Candidate struct {
	Provider  *provider.Provider
	Account   provider.Account
	UpstreamModel string // 去掉 "provider/" 前缀后的上游模型名
}

// Key 标识候选对应的账号（休眠表与 X-Gw 头使用）。
func (c Candidate) Key() string { return c.Provider.ID + "/" + c.Account.Name }

// Planner 持有 provider 快照与休眠表。provider 快照来自 providers.json
// （app 装配时加载）；休眠表进程内存态，重启即清（F6）。
type Planner struct {
	mu        sync.Mutex
	providers []provider.Provider
	resting   map[string]time.Time // key → 休眠截止
	now       func() time.Time
}

// NewPlanner 构建计划器。
func NewPlanner(providers []provider.Provider) *Planner {
	return &Planner{providers: providers, resting: map[string]time.Time{}, now: time.Now}
}

// SetNow 注入时钟（测试用）。
func (p *Planner) SetNow(f func() time.Time) { p.now = f }

// Providers 返回快照（只读视图；调用方不得修改）。
func (p *Planner) Providers() []provider.Provider { return p.providers }

// ResolveModel 解析 "provider/model"（W5 扩展 group/<id> 与裸 id 自动组）。
func (p *Planner) ResolveModel(model string) (*provider.Provider, string, error) {
	id, upstream, ok := strings.Cut(model, "/")
	if !ok || id == "" || upstream == "" {
		return nil, "", fmt.Errorf("模型 %q 不是 provider/model 形式", model)
	}
	for i := range p.providers {
		if p.providers[i].ID == id {
			return &p.providers[i], upstream, nil
		}
	}
	return nil, "", fmt.Errorf("未知 provider %q", id)
}

// Candidates 展开候选：每个可用账号一个，休眠中的排末尾。
func (p *Planner) Candidates(prov *provider.Provider, upstreamModel string) []Candidate {
	if prov.Disabled {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var awake, resting []Candidate
	for _, acc := range prov.Accounts {
		if acc.Status == provider.AccountBadCred {
			continue // 凭证失败：不自动重试（规则页失败分类表）
		}
		c := Candidate{Provider: prov, Account: acc, UpstreamModel: upstreamModel}
		if until, ok := p.resting[c.Key()]; ok && until.After(now) {
			resting = append(resting, c)
		} else {
			awake = append(awake, c)
		}
	}
	return append(awake, resting...)
}

// Rest 让候选休眠 d 时长（429/5xx/额度后调用，F6）。已有更晚的截止
// 时间时不缩短。
func (p *Planner) Rest(c Candidate, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.now().Add(d)
	key := c.Key()
	if old, ok := p.resting[key]; ok && old.After(until) {
		return
	}
	p.resting[key] = until
}

// RestUntil 让候选休眠到指定时刻（上游明确给出重置时间时，F6）。
func (p *Planner) RestUntil(c Candidate, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := c.Key()
	if old, ok := p.resting[key]; ok && old.After(until) {
		return
	}
	p.resting[key] = until
}

// Clear 解除休眠（成功服务后调用；对应管理页「立即恢复」）。
func (p *Planner) Clear(c Candidate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.resting, c.Key())
}

// Resting 返回当前休眠中的账号 key 与截止时刻（/api/state 用）。
func (p *Planner) Resting() map[string]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]time.Time, len(p.resting))
	for k, v := range p.resting {
		out[k] = v
	}
	return out
}

// AllModels 列出模型目录（/v1/models 用）。W2 目录来自 providers.json
// 的 provider 粒度（预检拉取的模型清单在 W3 落盘后细化）。
func (p *Planner) AllModels() []string {
	var out []string
	for _, prov := range p.providers {
		if prov.Disabled {
			continue
		}
		out = append(out, prov.ID)
	}
	sort.Strings(out)
	return out
}
