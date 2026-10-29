// W4 接线引擎测试：接线往返、漂移、规格拒绝、备份还原（testenv 沙箱）。
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/store"
	"yardmaster/internal/testenv"
)

const userClaudeSettings = `{
  // 用户自己的注释
  "permissions": {"allow": ["Bash(ls)"]},
  "env": {"OTHER": "keep"}
}`

const userCodexConfig = `# codex 用户配置
model = "gpt-5.2"

[profiles.work]
model = "gpt-5.2-mini" # 用户注释
`

func newManager(t *testing.T) *Manager {
	t.Helper()
	home := testenv.New(t)
	_ = home
	paths, err := store.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Paths: paths, Addr: "http://127.0.0.1:8317"}
}

func claudeDef() Def   { def, _ := ByID("claude-code"); return def }
func codexDef() Def    { def, _ := ByID("codex"); return def }
func geminiDef() Def   { def, _ := ByID("gemini-cli"); return def }
func opencodeDef() Def { def, _ := ByID("opencode"); return def }

func TestWireUnwireClaudeCodeRoundTrip(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(userClaudeSettings), 0o600); err != nil {
		t.Fatal(err)
	}

	// 接线
	if _, err := m.Wire(claudeDef(), "group/claude-max"); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	out, _ := os.ReadFile(cfgPath)
	s := string(out)
	for _, want := range []string{
		`"ANTHROPIC_BASE_URL": "http://127.0.0.1:8317"`,
		`"ANTHROPIC_AUTH_TOKEN": "gw-claude-code"`,
		`"model": "group/claude-max"`,
		"// 用户自己的注释",
		`"OTHER": "keep"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("接线后缺少 %q\n%s", want, s)
		}
	}

	// 状态 = 已接入
	st, err := m.StateOf(claudeDef())
	if err != nil || st != StateWired {
		t.Fatalf("state = %v %v", st, err)
	}

	// 断开：逐字节还原
	if err := m.Unwire(claudeDef()); err != nil {
		t.Fatalf("Unwire: %v", err)
	}
	restored, _ := os.ReadFile(cfgPath)
	if string(restored) != userClaudeSettings {
		t.Fatalf("断开未逐字节还原\n---got---\n%s\n---want---\n%s", restored, userClaudeSettings)
	}
	st, _ = m.StateOf(claudeDef())
	if st != StateUnwired {
		t.Fatalf("断开后状态 = %v", st)
	}
}

func TestWireCodexTOMLAndDrift(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(userCodexConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Wire(codexDef(), "siliconflow/zai-org/GLM-5.3"); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	out, _ := os.ReadFile(cfgPath)
	s := string(out)
	for _, want := range []string{
		`model = "siliconflow/zai-org/GLM-5.3"`,
		"model_provider = \"yardmaster\"",
		"[model_providers.yardmaster]",
		`base_url = "http://127.0.0.1:8317/v1"`,
		`api_key = "gw-codex"`,
		"# codex 用户配置",
		`model = "gpt-5.2-mini" # 用户注释`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("接线后缺少 %q\n%s", want, s)
		}
	}

	// 无漂移
	rows, err := m.Drift(codexDef())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Drift {
			t.Fatalf("接线后不应有漂移: %+v", rows)
		}
	}

	// 模拟外部改动（F5）：model 被改回
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(s, `model = "siliconflow/zai-org/GLM-5.3"`, `model = "gpt-5.2"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := m.StateOf(codexDef())
	if err != nil || st != StateDrift {
		t.Fatalf("外部改动后状态 = %v %v，want drift", st, err)
	}
	rows, _ = m.Drift(codexDef())
	drifted := 0
	for _, r := range rows {
		if r.Drift {
			drifted++
		}
	}
	if drifted != 1 {
		t.Fatalf("应恰好 1 处漂移: %+v", rows)
	}

	// 断开还原
	if err := m.Unwire(codexDef()); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(cfgPath)
	if string(restored) != userCodexConfig {
		t.Fatalf("断开未逐字节还原\n%s", restored)
	}
}

func TestWireOpencodeCreatesAndUnsets(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".config", "opencode", "opencode.json")

	if _, err := m.Wire(opencodeDef(), "siliconflow/deepseek-ai/DeepSeek-V4.1-Flash"); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("配置文件应被创建: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "yardmaster/siliconflow/deepseek-ai/DeepSeek-V4.1-Flash") ||
		!strings.Contains(s, "http://127.0.0.1:8317/v1") {
		t.Fatalf("内容异常\n%s", s)
	}

	// 断开：软件创建的文件 → 逆操作删除名下键
	if err := m.Unwire(opencodeDef()); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restored), "yardmaster") {
		t.Fatalf("名下键未删除\n%s", restored)
	}
}

func TestSpecTBDRefused(t *testing.T) {
	m := newManager(t)
	if _, err := m.Wire(geminiDef(), "m"); !strings.Contains(err.Error(), "待核验") {
		t.Fatalf("gemini-cli 应拒绝接线: %v", err)
	}
	if _, err := m.Preview(geminiDef(), "m"); err == nil {
		t.Fatal("preview 也应拒绝")
	}
}

func TestWireIdempotentSecondPass(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	_ = os.WriteFile(cfgPath, []byte(userClaudeSettings), 0o600)

	if _, err := m.Wire(claudeDef(), "model-a"); err != nil {
		t.Fatal(err)
	}
	// 二次接线（换模型）：stash 已存在不得覆盖（首次原始值优先）
	if _, err := m.Wire(claudeDef(), "model-b"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(out), "model-b") {
		t.Fatalf("二次接线未生效\n%s", out)
	}
	// 断开仍还原到最初用户配置
	if err := m.Unwire(claudeDef()); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(cfgPath)
	if string(restored) != userClaudeSettings {
		t.Fatalf("断开应还原首次接线前的原始配置\n%s", restored)
	}
}
