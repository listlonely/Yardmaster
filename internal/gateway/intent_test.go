// W5 意图判定链测试：自定义词表热生效、内置 L0 回落、L1 缓存/冷却/
// 关闭回落。
package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yardmaster/internal/plugin"
)

// writeIntents 落一份空 intents.json（本文件全部用例同形状，去参数化）
func writeIntents(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "intents.json")
	if err := os.WriteFile(path, []byte(`{"intents":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIntentChainCustomIntentsHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intents.json")
	c := NewIntentChain(path, "", 0, nil)

	// 无文件：自定义层不命中，回落内置 L0
	if got := c.Classify("帮我修个 bug"); got != "coding" {
		t.Fatalf("内置 L0 应命中 coding, got %q", got)
	}
	// 写入自定义词表：mtime 变化热生效，优先于内置 L0
	if err := os.WriteFile(path, []byte(`{"intents":[{"id":"team/bug","label":"缺陷","keywords":["bug"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if got := c.Classify("帮我修个 bug"); got != "team/bug" {
		t.Fatalf("自定义意图应热生效, got %q", got)
	}
	// 无特征消息：自定义与 L0 都不中 → L1 关闭返回空
	if got := c.Classify("随便聊聊天气"); got != "" {
		t.Fatalf("L1 关闭时应返回空, got %q", got)
	}
}

func TestIntentChainL1CacheAndCooldown(t *testing.T) {
	path := writeIntents(t)
	calls := 0
	c := NewIntentChain(path, "prov/small", 0, func(ctx context.Context, prompt string) (string, error) {
		calls++
		return "coding", nil
	})
	base := time.Now()
	clock := base
	c.SetNow(func() time.Time { return clock })

	for i := 0; i < 3; i++ {
		if got := c.Classify("完全无特征的消息甲"); got != "coding" {
			t.Fatalf("L1 应判 coding, got %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("相同消息应命中缓存, calls = %d", calls)
	}
	// 不同消息触发真实调用
	if got := c.Classify("完全无特征的消息乙"); got != "coding" {
		t.Fatalf("got %q", got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}

	// 连错 3 次 → 冷却：冷却期内不再调用
	failClock := base
	c2 := NewIntentChain(path, "prov/small", 0, func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("upstream down")
	})
	c2.SetNow(func() time.Time { return failClock })
	for i := 0; i < 3; i++ {
		if got := c2.Classify("消息一"); got != "" {
			t.Fatalf("失败应返回空, got %q", got)
		}
		failClock = failClock.Add(time.Second) // 每次错后推进时钟
	}
	callsBefore := 0
	c2.call = func(ctx context.Context, prompt string) (string, error) {
		callsBefore++
		return "coding", nil
	}
	if got := c2.Classify("消息二"); got != "" {
		t.Fatalf("冷却期内应直接未命中, got %q", got)
	}
	if callsBefore != 0 {
		t.Fatal("冷却期内不应发起调用")
	}
	// 冷却结束恢复调用
	failClock = failClock.Add(l1Cooldown)
	if got := c2.Classify("消息三"); got != "coding" {
		t.Fatalf("冷却结束应恢复, got %q", got)
	}
	if callsBefore != 1 {
		t.Fatalf("冷却后应恢复调用, calls = %d", callsBefore)
	}
}

func TestIntentChainL1InvalidOutputNotVerdict(t *testing.T) {
	path := writeIntents(t)
	c := NewIntentChain(path, "prov/small", 0, func(ctx context.Context, prompt string) (string, error) {
		return "我不知道", nil
	})
	if got := c.Classify("无特征消息"); got != "" {
		t.Fatalf("词表外输出不应作为判定, got %q", got)
	}
}

func TestIntentChainL1Timeout(t *testing.T) {
	path := writeIntents(t)
	c := NewIntentChain(path, "prov/small", 30*time.Millisecond, func(ctx context.Context, prompt string) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return "coding", nil
		}
	})
	if got := c.Classify("无特征消息"); got != "" {
		t.Fatalf("超时应返回空, got %q", got)
	}
}

func TestIntentChainNilSafe(t *testing.T) {
	var c *IntentChain
	if got := c.Classify("anything"); got != "" {
		t.Fatalf("nil 链应返回空, got %q", got)
	}
}

// 插件分类兜底（2026-11-13 接线 docs §四·三）：L1 未配置时插件分类生效；
// 输出须命中词表（内置+自定义+插件声明），词表外/失败回落空；L1 成功时
// 不调插件（插件只在 L1 不可用/冷却/失败时兜底）。payload 携带插件声明
// 词表供 Agent 分类器构造提示词。
func TestPluginClassifyFallback(t *testing.T) {
	pluginIntents := []plugin.IntentRef{{ID: "sre/oncall"}, {ID: "sre/postmortem"}}
	newChain := func(calls *int, reply func(string) string) *IntentChain {
		c := NewIntentChain(writeIntents(t), "", 0, nil) // L1 未配置
		c.WithPluginIntents(func() []plugin.IntentRef { return pluginIntents })
		c.WithPluginClassify(func(ctx context.Context, text string, vocab []string) (string, error) {
			(*calls)++
			found := false
			for _, v := range vocab {
				if v == "sre/oncall" {
					found = true
				}
			}
			if !found {
				t.Fatalf("payload 应携带插件声明词表: %v", vocab)
			}
			return reply(text), nil
		})
		return c
	}

	// 1) L1 未配置 → 插件分类生效（词表内输出采纳）
	calls := 0
	c := newChain(&calls, func(string) string { return "sre/oncall" })
	if got := c.Classify("线上告警响了，帮我看下值班手册"); got != "sre/oncall" {
		t.Fatalf("插件分类应采纳: %q", got)
	}
	// 2) 缓存：同一消息不再调用插件
	if got := c.Classify("线上告警响了，帮我看下值班手册"); got != "sre/oncall" || calls != 1 {
		t.Fatalf("应命中缓存（calls=%d）: %q", calls, got)
	}
	// 3) 词表外输出 → 视同未命中（缓存）
	calls = 0
	c2 := newChain(&calls, func(string) string { return "不在词表里的东西" })
	if got := c2.Classify("完全无特征的话术样例"); got != "" || calls != 1 {
		t.Fatalf("词表外应回落空: %q (calls=%d)", got, calls)
	}
	// 4) 插件失败 → 回落空（不缓存瞬时错误）
	calls = 0
	c3 := NewIntentChain(writeIntents(t), "", 0, nil)
	c3.WithPluginClassify(func(context.Context, string, []string) (string, error) {
		calls++
		return "", errors.New("插件崩了")
	})
	if got := c3.Classify("无特征消息"); got != "" || calls != 1 {
		t.Fatalf("插件失败应回落空: %q (calls=%d)", got, calls)
	}
	if got := c3.Classify("无特征消息"); got != "" || calls != 2 {
		t.Fatalf("瞬时错误不应缓存（calls=%d）: %q", calls, got)
	}
	// 5) L1 成功 → 插件不被调用（文档语义：L1 不可用/冷却才兜底）
	c4 := NewIntentChain(writeIntents(t), "mock/small", time.Second,
		func(ctx context.Context, prompt string) (string, error) { return "qa", nil })
	pluginCalls := 0
	c4.WithPluginClassify(func(context.Context, string, []string) (string, error) {
		pluginCalls++
		return "sre/oncall", nil
	})
	if got := c4.Classify("帮我修个 bug"); got != "coding" || pluginCalls != 0 {
		t.Fatalf("L1 成功时不得调插件: %q (calls=%d)", got, pluginCalls)
	}
}

// 智能路由接管（2026-11-16）：自定义 L0 仍最优先（未变）；其后的请求
// 跳过内置 L0/L1 由分类脑先行判定（词表校验同规）；分类脑失败/未命中
// 回落 内置 L0 → L1，服务不因接管而降级到兜底。还原旧代码（接管不
// 改变判定顺序）即失败。
func TestIntentChainAgentTakeover(t *testing.T) {
	path := writeIntents(t)
	// 分类脑把明显 coding 的消息判成 writing——接管态证明内置 L0 被跳过
	c := NewIntentChain(path, "", 0, nil) // L1 关闭
	c.WithPluginClassify(func(_ context.Context, _ string, _ []string) (string, error) {
		return "writing", nil
	})
	// 默认链：内置 L0 先命中 coding
	if got := c.Classify("帮我修个 bug"); got != "coding" {
		t.Fatalf("默认链应内置 L0 命中 coding, got %q", got)
	}
	// 接管：分类脑先行 → writing
	c.SetAgentTakeover(true)
	if got := c.Classify("帮我修个 bug"); got != "writing" {
		t.Fatalf("接管应分类脑先行, got %q", got)
	}

	// 分类脑失败：回落 内置 L0 → L1（不二次请求插件）
	l1Calls := 0
	c2 := NewIntentChain(path, "prov/small", 0,
		func(context.Context, string) (string, error) { l1Calls++; return "qa", nil })
	pluginErrs := 0
	c2.WithPluginClassify(func(context.Context, string, []string) (string, error) {
		pluginErrs++
		return "", errors.New("分类脑不可用")
	})
	c2.SetAgentTakeover(true)
	if got := c2.Classify("帮我修个 bug"); got != "coding" {
		t.Fatalf("分类脑失败应回落内置 L0 coding, got %q", got)
	}
	if got := c2.Classify("完全无特征的独特话术样本"); got != "qa" {
		t.Fatalf("L0 未命中应回落 L1 qa, got %q", got)
	}
	if pluginErrs != 2 {
		t.Fatalf("接管态每次判定至多请求插件一次 (errs=%d)", pluginErrs)
	}
}
