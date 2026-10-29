// 候选计划（PRD F6）：把目标（"provider/model"、裸 provider id 或
// "group/<id>"）解析为候选序列（每个可用账号一个候选），并应用
// 路由组策略与会话粘性。
//
// 策略说明：order/rotate/smart 三个内置策略在本包实现（rotate/smart
// 需要游标等进程态，不适合无副作用接口）；internal/routes 的
// Strategy 接口保留给 P1 无状态策略插件。裸 provider id 视为「自动
// 组」：按预检模型清单逐模型展开（order）。
package gateway

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"agenthub/internal/provider"
	"agenthub/internal/store"
)

// Candidate 是一次尝试的最小服务单元：成员模型 × 一个账号。
type Candidate struct {
	Provider      *provider.Provider
	Account       provider.Account
	UpstreamModel string // 去掉 "provider/" 前缀后的上游模型名
}

// Key 标识候选对应的账号（休眠表、亲和表与 X-Gw 头使用）。
func (c Candidate) Key() string { return c.Provider.ID + "/" + c.Account.Name }

// affinityCap 是亲和表条目上限：达到后整体重置（会话重新学习），
// 防止伪造 X-Gw-Session 无限增长（仅回环监听，属防御性兜底）。
const affinityCap = 4096

// Planner 持有 provider/组快照与休眠表、策略游标、会话亲和表。
// 快照来自 providers.json（app 装配时加载）；其余进程内存态，重启即清。
type Planner struct {
	mu         sync.Mutex
	providers  []provider.Provider
	groups     []provider.Group
	resting    map[string]time.Time // key → 休眠截止
	cursors    map[string]int       // rotate/smart 游标：策略键 → 下一候选下标
	affinity   map[string]string    // stickyKey → 候选 key
	usage      map[string]usageEntry
	usageDirty bool
	now        func() time.Time
}

// usageEntry 是一个账号的当日用量累计（内存态，FlushUsage 落盘）。
type usageEntry struct {
	day  string // "2006-01-02"，跨日重置
	used int64
}

// NewPlanner 构建计划器。
func NewPlanner(providers []provider.Provider, groups []provider.Group) *Planner {
	return &Planner{
		providers: providers,
		groups:    groups,
		resting:   map[string]time.Time{},
		cursors:   map[string]int{},
		affinity:  map[string]string{},
		usage:     map[string]usageEntry{},
		now:       time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (p *Planner) SetNow(f func() time.Time) { p.now = f }

// Providers 返回快照（只读视图；调用方不得修改）。
func (p *Planner) Providers() []provider.Provider { return p.providers }

// Groups 返回组快照（只读视图）。
func (p *Planner) Groups() []provider.Group { return p.groups }

// ResolveModel 解析 "provider/model"（探针与测试使用；请求路径走 Plan）。
func (p *Planner) ResolveModel(model string) (*provider.Provider, string, error) {
	id, upstream, ok := strings.Cut(model, "/")
	if !ok || id == "" || upstream == "" {
		return nil, "", fmt.Errorf("模型 %q 不是 provider/model 形式", model)
	}
	prov := p.providerByID(id)
	if prov == nil {
		return nil, "", fmt.Errorf("未知 provider %q", id)
	}
	return prov, upstream, nil
}

// Candidates 展开单个 "provider/model" 的账号候选：休眠中的排末尾
// （W2 行为，探针与测试使用；请求路径经 Plan 应用策略）。
func (p *Planner) Candidates(prov *provider.Provider, upstreamModel string) []Candidate {
	if prov == nil || prov.Disabled {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accountsOf(prov, upstreamModel, p.now())
}

// Plan 生成一次请求的候选序列：解析目标 → 展开成员候选（组可跨
// provider）→ 全局醒/休两段划分 → 应用组策略 → 会话亲和提前。
// 返回空切片且无错误表示「目标存在但无可用候选」（调用方回 503）。
func (p *Planner) Plan(target, stickyKey string) ([]Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()

	strategy := provider.StrategyOrder
	stratKey := target // 游标键：直连目标按目标名，组按组名
	var cands []Candidate
	if gid, ok := strings.CutPrefix(target, "group/"); ok {
		g := p.groupByID(gid)
		if g == nil {
			return nil, fmt.Errorf("未知路由组 %q", gid)
		}
		if g.Strategy != "" {
			strategy = g.Strategy
		}
		stratKey = target
		for _, m := range g.Members {
			cs, err := p.expandMember(m, now)
			if err != nil {
				return nil, err
			}
			cands = append(cands, cs...)
		}
	} else {
		cs, err := p.expandMember(target, now)
		if err != nil {
			return nil, err
		}
		cands = cs
	}

	awake, resting := p.splitAwake(cands, now)
	awake = p.applyStrategy(strategy, stratKey, awake)
	return p.applyAffinityLocked(append(awake, resting...), stickyKey, now), nil
}

// expandMember 展开一个成员引用为候选：
//   - "provider/model"：该模型的每个可用账号一个候选；
//   - 裸 provider id（自动组）：按预检模型清单逐模型 × 账号展开
//     （未预检的 provider 无模型清单，无法确定上游模型名，报错）。
//
// provider 未知或裸 id 无模型清单返回错误（配置错误应可见）；
// provider 停用/无可用账号返回空切片（动态状态，调用方合并后判定）。
func (p *Planner) expandMember(ref string, now time.Time) ([]Candidate, error) {
	if id, model, ok := strings.Cut(ref, "/"); ok && id != "" && model != "" {
		prov := p.providerByID(id)
		if prov == nil {
			return nil, fmt.Errorf("未知 provider %q", id)
		}
		if prov.Disabled {
			return nil, nil
		}
		return p.accountsOf(prov, model, now), nil
	}
	prov := p.providerByID(ref)
	if prov == nil {
		return nil, fmt.Errorf("未知 provider %q", ref)
	}
	if prov.Disabled {
		return nil, nil
	}
	if len(prov.Models) == 0 {
		return nil, fmt.Errorf("provider %q 未预检模型清单，不能按裸 id 路由（请先 probe 或显式指定 provider/model）", ref)
	}
	var cands []Candidate
	for _, m := range prov.Models {
		cands = append(cands, p.accountsOf(prov, m.ID, now)...)
	}
	return cands, nil
}

// accountsOf 返回 provider 内某模型的账号候选（醒着在前，休眠在后）。
// 调用方必须已持有 p.mu。
func (p *Planner) accountsOf(prov *provider.Provider, upstreamModel string, now time.Time) []Candidate {
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

// splitAwake 把候选按是否休眠稳定划分为两段（跨成员合并后再分，
// 保证休眠候选全局靠后）。调用方必须已持有 p.mu。
func (p *Planner) splitAwake(cands []Candidate, now time.Time) (awake, resting []Candidate) {
	for _, c := range cands {
		if until, ok := p.resting[c.Key()]; ok && until.After(now) {
			resting = append(resting, c)
		} else {
			awake = append(awake, c)
		}
	}
	return awake, resting
}

// applyStrategy 对醒着的候选应用组策略（休眠候选一律排末尾，策略
// 不会把休眠候选提前）。调用方必须已持有 p.mu。
func (p *Planner) applyStrategy(strategy provider.Strategy, stratKey string, awake []Candidate) []Candidate {
	switch strategy {
	case provider.StrategyRotate:
		return p.rotateLocked(stratKey, awake)
	case provider.StrategySmart:
		if hasUsageData(awake) {
			sort.SliceStable(awake, func(i, j int) bool {
				return remainingRatio(awake[i].Account) > remainingRatio(awake[j].Account)
			})
			return awake
		}
		// 无用量数据（W6 接入前常态）：按 rotate 轮换。
		return p.rotateLocked(stratKey, awake)
	default:
		return awake // order：声明顺序
	}
}

// rotateLocked 以 stratKey 为游标轮换候选（每请求前进一格）。
func (p *Planner) rotateLocked(stratKey string, cands []Candidate) []Candidate {
	n := len(cands)
	if n < 2 {
		return cands
	}
	off := p.cursors[stratKey] % n
	p.cursors[stratKey] = (p.cursors[stratKey] + 1) % n
	out := make([]Candidate, 0, n)
	out = append(out, cands[off:]...)
	return append(out, cands[:off]...)
}

// hasUsageData 判断候选集中是否任一账号携带当日用量（W6 填充）。
func hasUsageData(cands []Candidate) bool {
	for _, c := range cands {
		if c.Account.QuotaTokensDay > 0 {
			return true
		}
	}
	return false
}

// remainingRatio 返回账号当日剩余额度比例（0~1）。
func remainingRatio(a provider.Account) float64 {
	if a.QuotaTokensDay <= 0 {
		return 0
	}
	used := a.UsedTokens
	if used < 0 {
		used = 0
	}
	r := float64(a.QuotaTokensDay-used) / float64(a.QuotaTokensDay)
	if r < 0 {
		return 0
	}
	return r
}

// NoteAffinity 记录会话粘性：stickyKey 下次优先使用该候选。
func (p *Planner) NoteAffinity(stickyKey, candKey string) {
	if stickyKey == "" || candKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.affinity) >= affinityCap {
		p.affinity = map[string]string{}
	}
	p.affinity[stickyKey] = candKey
}

// applyAffinityLocked 把亲和候选（醒着的）稳定提前到队首；休眠中的
// 亲和候选不提前（「醒着才提前」，避免优先打到休眠账号）。
// 调用方必须已持有 p.mu。
func (p *Planner) applyAffinityLocked(cands []Candidate, stickyKey string, now time.Time) []Candidate {
	want, ok := p.affinity[stickyKey]
	if !ok {
		return cands
	}
	idx := -1
	for i, c := range cands {
		if c.Key() == want {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return cands
	}
	if until, resting := p.resting[want]; resting && until.After(now) {
		return cands // 亲和候选休眠中：维持策略顺序
	}
	c := cands[idx]
	out := append([]Candidate{c}, cands[:idx]...)
	return append(out, cands[idx+1:]...)
}

// ---------- 当日用量（W6） ----------

// NoteUsage 累计候选账号当日用量（响应 usage 提取值）：更新内存累计、
// 内存快照中的账号字段（smart 策略实时读取），并标记待落盘。
func (p *Planner) NoteUsage(c Candidate, tokens int64) {
	if tokens <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	day := p.now().Format("2006-01-02")
	key := c.Key()
	e := p.usage[key]
	if e.day != day {
		e = usageEntry{day: day}
	}
	e.used += tokens
	p.usage[key] = e
	p.usageDirty = true
	for i := range c.Provider.Accounts {
		if c.Provider.Accounts[i].Name != c.Account.Name {
			continue
		}
		if c.Provider.Accounts[i].UsageDay != day {
			c.Provider.Accounts[i].UsageDay = day
			c.Provider.Accounts[i].UsedTokens = 0
		}
		c.Provider.Accounts[i].UsedTokens += tokens
		break
	}
}

// FlushUsage 把内存累计合并写回 providers.json：重读文件、只更新本
// 进程跟踪的账号字段（不覆盖运行期间 CLI 的其他编辑）；文件不存在
// 或损坏时放弃本次并保留 dirty（读不懂=未知，绝不覆盖原文件）。
// 调用方定时调用（serve 期间每分钟 + 退出前一次）。
func (p *Planner) FlushUsage(path string) error {
	p.mu.Lock()
	if !p.usageDirty {
		p.mu.Unlock()
		return nil
	}
	p.usageDirty = false // 先清；失败路径恢复，避免覆盖期间新累计被跳过
	day := p.now().Format("2006-01-02")
	snap := make(map[string]usageEntry, len(p.usage))
	for k, e := range p.usage {
		if e.day == day {
			snap[k] = e
		}
	}
	p.mu.Unlock()

	f, err := store.LoadProvidersFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 无配置文件：无处可写
		}
		p.markDirty() // 损坏：恢复 dirty，下次重试
		return err
	}
	changed := false
	for i := range f.Providers {
		for j := range f.Providers[i].Accounts {
			acc := &f.Providers[i].Accounts[j]
			e, ok := snap[f.Providers[i].ID+"/"+acc.Name]
			if !ok {
				continue
			}
			if acc.UsageDay != e.day {
				acc.UsedTokens = 0
			}
			acc.UsageDay = e.day
			acc.UsedTokens = e.used
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := store.SaveProvidersFile(path, f); err != nil {
		p.markDirty()
		return err
	}
	return nil
}

// markDirty 恢复待落盘标记（锁内写，race 安全）。
func (p *Planner) markDirty() {
	p.mu.Lock()
	p.usageDirty = true
	p.mu.Unlock()
}

// ---------- 休眠表 ----------

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

// AllModels 列出模型目录（/v1/models 用）：有模型清单的 provider
// 展开为 provider/model；未预检的以 provider 自身占位。
func (p *Planner) AllModels() []string {
	var out []string
	for _, prov := range p.providers {
		if prov.Disabled {
			continue
		}
		if len(prov.Models) == 0 {
			out = append(out, prov.ID)
			continue
		}
		for _, m := range prov.Models {
			out = append(out, prov.ID+"/"+m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- 查找助手（调用方已持有 p.mu 的锁内使用） ----------

func (p *Planner) providerByID(id string) *provider.Provider {
	for i := range p.providers {
		if p.providers[i].ID == id {
			return &p.providers[i]
		}
	}
	return nil
}

func (p *Planner) groupByID(id string) *provider.Group {
	for i := range p.groups {
		if p.groups[i].ID == id {
			return &p.groups[i]
		}
	}
	return nil
}
