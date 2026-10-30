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
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
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
	wrr        map[string][]int     // weight 策略平滑加权状态：策略键 → 各候选当前权重
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
		wrr:       map[string][]int{},
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
	var weights map[string]int
	var cands []Candidate
	if gid, ok := strings.CutPrefix(target, "group/"); ok {
		g := p.groupByID(gid)
		if g == nil {
			return nil, fmt.Errorf("未知路由组 %q", gid)
		}
		if g.Strategy != "" {
			strategy = g.Strategy
		}
		weights = g.Weights
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
	awake = p.applyStrategy(strategy, stratKey, weights, awake)
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

// accountExhausted 判断账号当日额度是否已用尽（M2-W4 额度感知路由）。
// QuotaTokensDay<=0 = 未设额度，永不耗尽；UsageDay 非当日时内存 used
// 视为 0（NoteUsage 跨日重置的读侧对齐）。
func accountExhausted(a provider.Account, today string) bool {
	if a.QuotaTokensDay <= 0 {
		return false
	}
	used := a.UsedTokens
	if a.UsageDay != today {
		used = 0
	}
	return used >= a.QuotaTokensDay
}

// usedToday 返回账号当日已用量（UsageDay 非当日视为 0）。
func usedToday(a provider.Account, today string) int64 {
	if a.UsageDay != today {
		return 0
	}
	if a.UsedTokens < 0 {
		return 0
	}
	return a.UsedTokens
}

// accountsOf 返回 provider 内某模型的账号候选（醒着在前，休眠在后）。
// 当日额度耗尽的账号进入休眠段（M2-W4：额度感知，休眠至次日重置）。
// 调用方必须已持有 p.mu。
func (p *Planner) accountsOf(prov *provider.Provider, upstreamModel string, now time.Time) []Candidate {
	today := now.Format("2006-01-02")
	var awake, resting, exhausted []Candidate
	for _, acc := range prov.Accounts {
		if acc.Status == provider.AccountBadCred {
			continue // 凭证失败：不自动重试（规则页失败分类表）
		}
		c := Candidate{Provider: prov, Account: acc, UpstreamModel: upstreamModel}
		until, timeResting := p.resting[c.Key()]
		switch {
		case accountExhausted(acc, today):
			exhausted = append(exhausted, c) // 额度耗尽：休眠至次日重置（读侧计算）
		case timeResting && until.After(now):
			resting = append(resting, c)
		default:
			awake = append(awake, c)
		}
	}
	return append(append(awake, resting...), exhausted...)
}

// splitAwake 把候选按「醒 / 短休眠 / 额度耗尽」稳定划分为三段（跨成员
// 合并后再分，保证不可用候选全局靠后）。调用方必须已持有 p.mu。
func (p *Planner) splitAwake(cands []Candidate, now time.Time) (awake, resting []Candidate) {
	today := now.Format("2006-01-02")
	var shortRest, exhausted []Candidate
	for _, c := range cands {
		until, timeResting := p.resting[c.Key()]
		switch {
		case accountExhausted(c.Account, today):
			exhausted = append(exhausted, c)
		case timeResting && until.After(now):
			shortRest = append(shortRest, c)
		default:
			awake = append(awake, c)
		}
	}
	return awake, append(shortRest, exhausted...)
}

// applyStrategy 对醒着的候选应用组策略（休眠候选一律排末尾，策略
// 不会把休眠候选提前）。调用方必须已持有 p.mu。
func (p *Planner) applyStrategy(strategy provider.Strategy, stratKey string,
	weights map[string]int, awake []Candidate) []Candidate {
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
		// 无用量数据（未配额度常态）：按 rotate 轮换。
		return p.rotateLocked(stratKey, awake)
	case provider.StrategyUsage:
		// 当日已用量升序（M2-W4）：用量最少的账号优先（负载均衡，
		// 不要求配置额度）；同量保持声明顺序。
		today := p.now().Format("2006-01-02")
		sort.SliceStable(awake, func(i, j int) bool {
			return usedToday(awake[i].Account, today) < usedToday(awake[j].Account, today)
		})
		return awake
	case provider.StrategyWeight:
		// 平滑加权轮询（M2-W4）：按 Group.Weights 分流，确定性顺序；
		// 权重键优先级：provider/account（账号粒度）→ provider/model
		// （成员粒度）→ provider → 缺省 1。
		return p.weightLocked(stratKey, awake, weights)
	default:
		return awake // order：声明顺序
	}
}

// weightLocked 平滑加权轮询（smooth WRR，nginx 同算法）：每轮为各候选
// 累加权重，选当前权重最大者置顶并扣减总权重。返回「选中者 + 其余按
// 声明序」的完整 failover 链。候选数变化（账号增删/休眠切换）时状态
// 重置。调用方必须已持有 p.mu。
func (p *Planner) weightLocked(stratKey string, cands []Candidate, weights map[string]int) []Candidate {
	n := len(cands)
	if n < 2 {
		return cands
	}
	cur, ok := p.wrr[stratKey]
	if !ok || len(cur) != n {
		cur = make([]int, n)
	}
	weightOf := func(c Candidate) int {
		if v, ok := weights[c.Provider.ID+"/"+c.Account.Name]; ok && v > 0 {
			return v // 账号粒度
		}
		if v, ok := weights[c.Provider.ID+"/"+c.UpstreamModel]; ok && v > 0 {
			return v // 成员粒度（provider/model）
		}
		if v, ok := weights[c.Provider.ID]; ok && v > 0 {
			return v // provider 粒度
		}
		return 1
	}
	total := 0
	for i, c := range cands {
		x := weightOf(c)
		total += x
		cur[i] += x
	}
	pick := 0
	for i := 1; i < n; i++ {
		if cur[i] > cur[pick] {
			pick = i
		}
	}
	cur[pick] -= total
	p.wrr[stratKey] = cur
	out := make([]Candidate, 0, n)
	out = append(out, cands[pick])
	out = append(out, cands[:pick]...)
	out = append(out, cands[pick+1:]...)
	return out
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
	if accountExhausted(cands[idx].Account, now.Format("2006-01-02")) {
		return cands // 亲和候选额度耗尽：维持策略顺序（醒着才提前）
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
		was := c.Provider.Accounts[i].UsedTokens
		c.Provider.Accounts[i].UsedTokens = was + tokens
		// 额度感知（M2-W4）：本次累计恰好跨过当日额度线时告警一次。
		q := c.Provider.Accounts[i].QuotaTokensDay
		if q > 0 && was < q && was+tokens >= q {
			slog.Info("账号当日额度已用尽，自动休眠至次日重置（额度感知路由）",
				"account", key, "quota", q, "used", was+tokens)
		}
		break
	}
}

// Exhausted 返回当日额度耗尽的账号 key 列表（/api/state 与控制台展示；
// 与休眠表独立——耗尽是读侧按额度计算，不占休眠表条目）。
// 账号字段被 NoteUsage 并发写入，必须持锁读（CI -race 实测）。
func (p *Planner) Exhausted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	today := p.now().Format("2006-01-02")
	var out []string
	for _, prov := range p.providers {
		for _, acc := range prov.Accounts {
			if accountExhausted(acc, today) {
				out = append(out, prov.ID+"/"+acc.Name)
			}
		}
	}
	sort.Strings(out)
	return out
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

// ClearByKey 按 "provider/account" 键解除休眠（OAuth 刷新成功后调用）。
func (p *Planner) ClearByKey(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.resting, key)
}

// MarkBadCredByKey 把账号标记为凭证失败（规则页：切走、需人工处理、
// 不自动重试——candidates 跳过该状态账号），同时清除其休眠条目。
// 账号切片与 Candidate.Provider 共享，锁内原位修改（NoteUsage 同规）。
func (p *Planner) MarkBadCredByKey(key string) {
	provID, accName, ok := strings.Cut(key, "/")
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.providers {
		if p.providers[i].ID != provID {
			continue
		}
		for j := range p.providers[i].Accounts {
			if p.providers[i].Accounts[j].Name == accName {
				p.providers[i].Accounts[j].Status = provider.AccountBadCred
				delete(p.resting, key)
				return
			}
		}
	}
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
// Reload 会整体替换快照，读取必须持锁。
func (p *Planner) AllModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
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

// StateAccount 是 /api/state 用的账号状态投影（锁内复制的值）。
type StateAccount struct {
	Key         string `json:"key"`
	Provider    string `json:"provider"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	UsedTokens  int64  `json:"used_tokens"`
	QuotaTokens int64  `json:"quota_tokens_day"`
	UsageDay    string `json:"usage_day"`
}

// StateGroup 是 /api/state 用的路由组投影。
type StateGroup struct {
	ID       string   `json:"id"`
	Strategy string   `json:"strategy"`
	Members  []string `json:"members"`
}

// StateSnapshot 一次性锁内复制 /api/state 所需的全部快照（providers
// 概要、账号实时状态、组）——避免 handleState 对内部切片的无锁遍历
// 与 NoteUsage/Reload 的并发写入竞争（CI -race 实测）。
func (p *Planner) StateSnapshot() (providers []map[string]any, accounts []StateAccount, groups []StateGroup) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, prov := range p.providers {
		providers = append(providers, map[string]any{
			"id": prov.ID, "name": prov.Name, "protocol": prov.Protocol, "accounts": len(prov.Accounts),
		})
		for _, acc := range prov.Accounts {
			accounts = append(accounts, StateAccount{
				Key:         prov.ID + "/" + acc.Name,
				Provider:    prov.ID,
				Name:        acc.Name,
				Status:      string(acc.Status),
				UsedTokens:  acc.UsedTokens,
				QuotaTokens: acc.QuotaTokensDay,
				UsageDay:    acc.UsageDay,
			})
		}
	}
	for _, g := range p.groups {
		groups = append(groups, StateGroup{ID: g.ID, Strategy: string(g.Strategy), Members: append([]string(nil), g.Members...)})
	}
	return providers, accounts, groups
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

// Reload 用磁盘快照热替换 provider/组配置（M2-W1：管理 API /api/reload
// 与 PUT providers 后调用，补 CLI 编辑「重启 serve 生效」缺口）。
//
// 语义：
//   - providers/groups 整体替换（含文件内持久化的额度字段）；
//   - 进程内运行态按「键仍存在才保留」剪枝：休眠表（编辑不解除休眠）、
//     当日用量累计（未落盘部分不丢）、会话亲和表、策略游标；
//   - 游标按策略键索引、与成员无关，直接保留。
func (p *Planner) Reload(providers []provider.Provider, groups []provider.Group) {
	p.mu.Lock()
	defer p.mu.Unlock()
	valid := map[string]bool{}
	for _, prov := range providers {
		for _, acc := range prov.Accounts {
			valid[prov.ID+"/"+acc.Name] = true
		}
	}
	// weight 策略状态按策略键（目标名）索引：目标消失即清，防止
	// 候选数变化后残留错位状态（长度不符时 weightLocked 自会重置）。
	validTargets := map[string]bool{}
	for _, prov := range providers {
		validTargets[prov.ID] = true
	}
	for _, g := range groups {
		validTargets["group/"+g.ID] = true
	}
	for k := range p.wrr {
		if !validTargets[k] {
			delete(p.wrr, k)
		}
	}
	for k := range p.resting {
		if !valid[k] {
			delete(p.resting, k)
		}
	}
	for k := range p.usage {
		if !valid[k] {
			delete(p.usage, k)
		}
	}
	for k := range p.affinity {
		if _, ok := valid[p.affinity[k]]; !ok {
			delete(p.affinity, k)
		}
	}
	p.providers, p.groups = providers, groups
}
