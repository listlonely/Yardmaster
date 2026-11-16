// 意图判定链（PRD F8）：自定义意图（intents.json，mtime 热生效）→
// 内置 L0 → L1 小模型（经注入回调调用，走网关自身转发以享受故障
// 转移与代理）。任一层失败不影响请求：绝不因分类而失败。
//
// L1 约束：仅对新的用户轮次分类一次（调用方保证传最新用户消息）；
// 判定缓存 map[消息哈希]意图；连错 ≥3 次进入 5 分钟冷却，冷却期内
// 直接返回未命中。
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yardmaster/internal/classify"
	"yardmaster/internal/plugin"
	"yardmaster/internal/store"
)

// L1Caller 是 L1 分类的注入回调：调用小模型并返回其原始文本输出。
// 超时由实现方经 ctx 遵守。
type L1Caller func(ctx context.Context, prompt string) (string, error)

// L1 决策与冷却参数（F8 规则表）。
const (
	defaultL1Timeout  = 300 * time.Millisecond
	l1Cooldown        = 5 * time.Minute
	l1ErrStreakToCool = 3
	l1CacheCap        = 2048 // 达到上限整体重置（防御性兜底）
	l1MaxOutputRunes  = 64   // 分类输出只需一个意图 id
	// pluginClassifyTimeout 是插件分类兜底的往返预算（§四·三）。文档基线
	// 300ms 面向词表型插件；LLM 驱动的 Agent 分类器（examples/agent-
	// classifier）需要真实模型往返——2026-11-16 真机实测分类脑往返
	// DeepSeek-V4.1-Flash 2.4-4.4s 波动（中位 ≈3s），3s 预算下 agent 等
	// 无种子意图常判不中，故取 4s 覆住中位+波动。插件侧已补 agent 高
	// 置信种子走快路径，LLM 只兜种子外长尾——多等的代价被快路径分流。
	// 超限回落默认组；Registry 连续 3 次失败冷却 5 分钟，熔断期间零等
	// 待直达后续层，不拖垮热路径。
	pluginClassifyTimeout = 4 * time.Second
)

// IntentChain 是意图判定链。
type IntentChain struct {
	intentsPath string
	l1Model     string // provider/model；空=L1 关闭
	l1Timeout   time.Duration
	call        L1Caller // nil=L1 关闭

	pluginIntents   func() []plugin.IntentRef                     // 启用插件声明的意图（nil=无插件注入）
	pluginClassify  func(ctx context.Context, text string, vocabulary []string) (string, error) // 插件分类兜底（nil=未接线）

	takeover atomic.Bool // 智能路由接管：分类脑先行（SetAgentTakeover 热切换）

	mu            sync.Mutex
	intentsStamp  time.Time // intents.json mtime（热生效）
	intentsSize   int64
	strategy      *classify.UserStrategy
	cache         map[string]string // 消息哈希 → 意图 id（"" 表示已判为无效输出）
	errStreak     int
	cooldownUntil time.Time
	now           func() time.Time
}

// NewIntentChain 构建判定链。l1Model 为空或 call 为 nil 时 L1 关闭；
// l1Timeout<=0 用默认 300ms。
func NewIntentChain(intentsPath, l1Model string, l1Timeout time.Duration, call L1Caller) *IntentChain {
	if l1Timeout <= 0 {
		l1Timeout = defaultL1Timeout
	}
	return &IntentChain{
		intentsPath: intentsPath,
		l1Model:     l1Model,
		l1Timeout:   l1Timeout,
		call:        call,
		cache:       map[string]string{},
		now:         time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (c *IntentChain) SetNow(f func() time.Time) { c.now = f }

// WithPluginIntents 注入插件声明意图提供者（AttachPlugins 装配）：
// 词表并入与插件分类校验共用同一来源。
func (c *IntentChain) WithPluginIntents(fn func() []plugin.IntentRef) {
	c.pluginIntents = fn
}

// WithPluginClassify 注入插件分类兜底（classify.intent，§四·三；AttachPlugins
// 装配）：L1 未配置/冷却/失败时经 Registry 请求插件。vocabulary 由链内
// 传入（锁内取，回调侧不得再拿链锁）。nil=未接线（无插件时零开销）。
func (c *IntentChain) WithPluginClassify(fn func(ctx context.Context, text string, vocabulary []string) (string, error)) {
	c.pluginClassify = fn
}

// SetAgentTakeover 热切换智能路由接管（启动装配与 settings 写钩子调用）：
// true=分类脑先行判定意图（自定义 L0 仍最优先），false=默认链。
func (c *IntentChain) SetAgentTakeover(on bool) { c.takeover.Store(on) }

// Classify 返回消息意图 id（内置或自定义词表）；无法判定返回空串。
// 判定链：自定义 L0 → 内置 L0 → L1 → 插件分类兜底（§四·三：L1 未配置/
// 冷却/失败时才落到插件）。智能路由接管开启时为：自定义 L0 → 插件
//（分类脑）→ 失败回落 内置 L0 → L1（不二次请求插件，Registry 冷却兜底）。
// nil 接收者安全（网关未装配判定链时静默跳过）。
func (c *IntentChain) Classify(message string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	strategy := c.loadIntentsLocked()
	if v, ok := strategy.EvaluateL0(message); ok {
		slog.Debug("意图判定", "source", "custom-l0", "intent", v.Intent)
		return v.Intent
	}
	if c.takeover.Load() {
		// 智能路由接管：跳过内置 L0/L1 关键词/小模型层，由分类脑按语境
		// 判意图（词表校验同规——词表外/失败视同未命中）。
		if intent := c.classifyPluginLocked(strategy, message); intent != "" {
			return intent
		}
		if v, ok := classify.L0(message); ok {
			slog.Debug("意图判定", "source", "l0-takeover-fallback", "intent", v.Intent)
			return v.Intent
		}
		return c.classifyL1Locked(strategy, message)
	}
	if v, ok := classify.L0(message); ok {
		slog.Debug("意图判定", "source", "l0", "intent", v.Intent)
		return v.Intent
	}
	if intent := c.classifyL1Locked(strategy, message); intent != "" {
		return intent
	}
	return c.classifyPluginLocked(strategy, message)
}

// Vocabulary 返回当前生效的意图词表（内置+自定义+启用插件声明；插件
// 分类兜底的校验依据，M7-W4——词表外 intent 不采纳）。nil 接收者安全。
func (c *IntentChain) Vocabulary() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	strategy := c.loadIntentsLocked()
	c.mu.Unlock()
	out := append([]string(nil), classify.BuiltinIntents...)
	for _, def := range intentsOf(strategy) {
		out = append(out, def.ID)
	}
	if c.pluginIntents != nil {
		for _, ir := range c.pluginIntents() {
			out = append(out, ir.ID)
		}
	}
	return out
}

// loadIntentsLocked 读取自定义意图（mtime 变化才重载；解析失败沿用
// 旧词表并告警——热生效不中断服务）。
func (c *IntentChain) loadIntentsLocked() *classify.UserStrategy {
	st, err := os.Stat(c.intentsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("intents.json 不可访问", "path", c.intentsPath, "err", err)
		}
		return c.strategy
	}
	if st.ModTime().Equal(c.intentsStamp) && st.Size() == c.intentsSize {
		return c.strategy
	}
	var strategy classify.UserStrategy
	if err := store.LoadJSON(c.intentsPath, &strategy); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("intents.json 解析失败，沿用旧词表（保留原文件）", "path", c.intentsPath, "err", err)
		}
		c.intentsStamp, c.intentsSize = st.ModTime(), st.Size()
		return c.strategy
	}
	c.strategy = &strategy
	c.intentsStamp, c.intentsSize = st.ModTime(), st.Size()
	slog.Info("已加载自定义意图词表", "count", len(strategy.Intents), "path", c.intentsPath)
	return c.strategy
}

// classifyL1Locked 经 L1 小模型分类（缓存→冷却→调用→校验）。
func (c *IntentChain) classifyL1Locked(strategy *classify.UserStrategy, message string) string {
	if c.call == nil || c.l1Model == "" {
		return "" // L1 未配置：仅 L0（F8 降级路径）
	}
	if c.now().Before(c.cooldownUntil) {
		return "" // 冷却期：直接未命中
	}
	hash := sha256.Sum256([]byte(c.l1Model + "\x00" + message))
	key := hex.EncodeToString(hash[:])
	if intent, ok := c.cache[key]; ok {
		if intent != "" {
			slog.Debug("意图判定", "source", "l1-cache", "intent", intent)
		}
		return intent
	}

	known := map[string]bool{}
	for _, id := range classify.BuiltinIntents {
		known[id] = true
	}
	if strategy != nil {
		for _, def := range strategy.Intents {
			known[def.ID] = true
		}
	}

	prompt := classify.BuildL1Prompt(classify.BuiltinIntents, intentsOf(strategy), message)
	ctx, cancel := context.WithTimeout(context.Background(), c.l1Timeout)
	defer cancel()
	out, err := c.call(ctx, prompt)
	if err != nil {
		c.errStreak++
		if c.errStreak >= l1ErrStreakToCool {
			c.cooldownUntil = c.now().Add(l1Cooldown)
			c.errStreak = 0
			slog.Warn("L1 分类连续失败，进入冷却", "cooldown", l1Cooldown.String())
		}
		slog.Debug("L1 分类失败", "err", err)
		return "" // 不缓存瞬时错误
	}
	c.errStreak = 0
	intent := validateL1Output(out, known)
	if len(c.cache) >= l1CacheCap {
		c.cache = map[string]string{}
	}
	c.cache[key] = intent // 无效输出也缓存，避免同一消息反复调用
	if intent == "" {
		slog.Debug("L1 输出不在词表内", "output", truncateForLog(out))
		return ""
	}
	slog.Debug("意图判定", "source", "l1", "intent", intent)
	return intent
}

func intentsOf(strategy *classify.UserStrategy) []classify.IntentDef {
	if strategy == nil {
		return nil
	}
	return strategy.Intents
}

// classifyPluginLocked 插件分类兜底（§四·三）：L1 未配置/冷却/失败时经
// Registry 发 {text, vocabulary} 请求，返回意图须命中全词表（内置+自定义+
// 插件声明），词表外视同失败回落默认组（不猜）。结果与 L1 共用缓存；
// 瞬时错误不缓存（Registry 侧自带 3 失败冷却 5 分钟）。
func (c *IntentChain) classifyPluginLocked(strategy *classify.UserStrategy, message string) string {
	if c.pluginClassify == nil {
		return "" // 未接线：链路不变（L0 → L1 → 默认组）
	}
	hash := sha256.Sum256([]byte("plugin\x00" + message))
	key := hex.EncodeToString(hash[:])
	if intent, ok := c.cache[key]; ok {
		if intent != "" {
			slog.Debug("意图判定", "source", "plugin-cache", "intent", intent)
		}
		return intent
	}
	known := map[string]bool{}
	// 词表 = 内置 + 自定义 + 插件声明（payload 全量带给插件构造提示词）
	vocabulary := append([]string(nil), classify.BuiltinIntents...)
	for _, id := range classify.BuiltinIntents {
		known[id] = true
	}
	for _, def := range intentsOf(strategy) {
		known[def.ID] = true
		vocabulary = append(vocabulary, def.ID)
	}
	if c.pluginIntents != nil {
		for _, ir := range c.pluginIntents() {
			known[ir.ID] = true
			vocabulary = append(vocabulary, ir.ID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginClassifyTimeout)
	defer cancel()
	out, err := c.pluginClassify(ctx, message, vocabulary)
	if err != nil {
		slog.Debug("插件分类失败", "err", err)
		return "" // 瞬时错误不缓存
	}
	intent := validateL1Output(out, known)
	if len(c.cache) >= l1CacheCap {
		c.cache = map[string]string{}
	}
	c.cache[key] = intent // 词表外也缓存，避免同一消息反复调用插件
	if intent == "" {
		slog.Debug("插件分类输出不在词表内", "output", truncateForLog(out))
		return ""
	}
	slog.Info("意图判定", "source", "plugin", "intent", intent)
	return intent
}

// validateL1Output 校验 L1 输出：去空白与引号后必须命中词表。
func validateL1Output(out string, known map[string]bool) string {
	s := strings.TrimSpace(out)
	s = strings.Trim(s, "\"'`，。 ")
	if !known[s] {
		return ""
	}
	return s
}

func truncateForLog(s string) string {
	r := []rune(s)
	if len(r) > l1MaxOutputRunes {
		return string(r[:l1MaxOutputRunes]) + "…"
	}
	return s
}
