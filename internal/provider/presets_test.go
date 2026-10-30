package provider

import "testing"

func TestPresetsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets {
		if p.ID == "" || seen[p.ID] {
			t.Fatalf("预设 ID 空或重复: %q", p.ID)
		}
		seen[p.ID] = true
		switch p.Protocol {
		case ProtocolChat, ProtocolMessages, ProtocolResponses, ProtocolGemini:
		default:
			t.Fatalf("预设 %s 协议非法: %q", p.ID, p.Protocol)
		}
		if p.BaseURL == "" || p.Name == "" {
			t.Fatalf("预设 %s 缺 name/base_url", p.ID)
		}
	}
	// 关键预设存在
	for _, id := range []string{"glm", "glm-coding", "glm-coding-messages", "zai-coding", "deepseek", "deepseek-messages", "kimi", "siliconflow", "anthropic", "openai"} {
		if _, ok := PresetByID(id); !ok {
			t.Fatalf("预设 %q 应存在", id)
		}
	}
	if _, ok := PresetByID("nope"); ok {
		t.Fatal("未知预设不应命中")
	}
	// GLM Coding Plan 端点形状（对齐 Magpie）
	gc, _ := PresetByID("glm-coding")
	if gc.BaseURL != "https://open.bigmodel.cn/api/coding/paas/v4" {
		t.Fatalf("glm-coding base: %s", gc.BaseURL)
	}
	gcm, _ := PresetByID("glm-coding-messages")
	if gcm.BaseURL != "https://open.bigmodel.cn/api/anthropic" {
		t.Fatalf("glm-coding-messages base: %s", gcm.BaseURL)
	}
}
