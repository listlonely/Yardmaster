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
)

func writeIntents(t *testing.T, json string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "intents.json")
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
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
	path := writeIntents(t, `{"intents":[]}`)
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
	path := writeIntents(t, `{"intents":[]}`)
	c := NewIntentChain(path, "prov/small", 0, func(ctx context.Context, prompt string) (string, error) {
		return "我不知道", nil
	})
	if got := c.Classify("无特征消息"); got != "" {
		t.Fatalf("词表外输出不应作为判定, got %q", got)
	}
}

func TestIntentChainL1Timeout(t *testing.T) {
	path := writeIntents(t, `{"intents":[]}`)
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
