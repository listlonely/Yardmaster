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
	"time"

	"agenthub/internal/classify"
	"agenthub/internal/store"
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
)

// IntentChain 是意图判定链。
type IntentChain struct {
	intentsPath string
	l1Model     string // provider/model；空=L1 关闭
	l1Timeout   time.Duration
	call        L1Caller // nil=L1 关闭

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

// Classify 返回消息意图 id（内置或自定义词表）；无法判定返回空串。
// nil 接收者安全（网关未装配判定链时静默跳过）。
func (c *IntentChain) Classify(message string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	strat := c.loadIntentsLocked()
	if v, ok := strat.EvaluateL0(message); ok {
		slog.Debug("意图判定", "source", "custom-l0", "intent", v.Intent)
		return v.Intent
	}
	if v, ok := classify.L0(message); ok {
		slog.Debug("意图判定", "source", "l0", "intent", v.Intent)
		return v.Intent
	}
	return c.classifyL1Locked(strat, message)
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
	var strat classify.UserStrategy
	if err := store.LoadJSON(c.intentsPath, &strat); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("intents.json 解析失败，沿用旧词表（保留原文件）", "path", c.intentsPath, "err", err)
		}
		c.intentsStamp, c.intentsSize = st.ModTime(), st.Size()
		return c.strategy
	}
	c.strategy = &strat
	c.intentsStamp, c.intentsSize = st.ModTime(), st.Size()
	slog.Info("已加载自定义意图词表", "count", len(strat.Intents), "path", c.intentsPath)
	return c.strategy
}

// classifyL1Locked 经 L1 小模型分类（缓存→冷却→调用→校验）。
func (c *IntentChain) classifyL1Locked(strat *classify.UserStrategy, message string) string {
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
	if strat != nil {
		for _, def := range strat.Intents {
			known[def.ID] = true
		}
	}

	prompt := classify.BuildL1Prompt(classify.BuiltinIntents, intentsOf(strat), message)
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

func intentsOf(strat *classify.UserStrategy) []classify.IntentDef {
	if strat == nil {
		return nil
	}
	return strat.Intents
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
