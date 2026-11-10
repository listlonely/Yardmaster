// W4 接线引擎测试：接线往返、漂移、规格拒绝、备份还原（testenv 沙箱）。
package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
		// 字段形状经 Magpie codex.go 交叉校准 [2026-11-03-M9-W2]：
		// key 载体=experimental_bearer_token，协议=responses
		`experimental_bearer_token = "gw-codex"`,
		`wire_api = "responses"`,
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

	// 断开：软件创建的文件 → 逆操作删除名下键 → 变空移除（F4）
	if err := m.Unwire(opencodeDef()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("软件创建且清空后的配置应被移除")
	}
}

func TestSpecTBDRefused(t *testing.T) {
	m := newManager(t)
	// 合成 def：规格待核验机制仍由注册表数据驱动（§4.2）——真实注册表
	// 已无 TBD 条目（cline 规格已核验落地）。
	def := Def{ID: "fake", Name: "Fake", Format: FormatJSONC,
		RelPaths: []string{"~/fake.json"}, SpecNote: "写入规格待核验"}
	if _, err := m.Wire(def, "m"); !strings.Contains(err.Error(), "待核验") {
		t.Fatalf("规格待核验应拒绝接线: %v", err)
	}
	if _, err := m.Preview(def, "m"); err == nil {
		t.Fatal("preview 也应拒绝")
	}
}

// userGeminiSettings/userGeminiEnv 是 Gemini CLI 用户已有配置（含自有
// 键与注释，接线必须逐字节保留）。
const userGeminiSettings = `{
  // 用户自己的注释
  "theme": "auto",
  "model": {"name": "gemini-2.5-pro", "maxSessionTurns": 50}
}`

const userGeminiEnv = `# gemini 用户自己的注释
CUSTOM_VAR=keep
GEMINI_API_KEY=user-own-key
`

func TestWireGeminiTwoFiles(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".gemini", "settings.json")
	envPath := filepath.Join(home, ".gemini", ".env")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(userGeminiSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(userGeminiEnv), 0o600); err != nil {
		t.Fatal(err)
	}

	// 接线：settings.json 写 auth+model；.env 注入端点与 key（保留用户键）
	if _, err := m.Wire(geminiDef(), "siliconflow/zai-org/GLM-5.3-Flash"); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	sOut, _ := os.ReadFile(settingsPath)
	s := string(sOut)
	for _, want := range []string{
		`"selectedType": "gemini-api-key"`,
		`"name": "siliconflow/zai-org/GLM-5.3-Flash"`,
		`"maxSessionTurns": 50`, // 用户自有键保留
		"// 用户自己的注释",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("settings.json 缺少 %q\n%s", want, s)
		}
	}
	eOut, _ := os.ReadFile(envPath)
	e := string(eOut)
	for _, want := range []string{
		"GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:8317",
		"GEMINI_API_KEY=gw-gemini-cli",
		"CUSTOM_VAR=keep", // 用户自有键保留
		"# gemini 用户自己的注释",
	} {
		if !strings.Contains(e, want) {
			t.Fatalf(".env 缺少 %q\n%s", want, e)
		}
	}
	if strings.Contains(e, "user-own-key") {
		t.Fatalf(".env 用户 key 应被接线值替换\n%s", e)
	}

	// 漂移：覆盖两文件（.env 的 key 被外部改动）
	if err := os.WriteFile(envPath, []byte(strings.Replace(e, "GEMINI_API_KEY=gw-gemini-cli", "GEMINI_API_KEY=tampered", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := m.Drift(geminiDef())
	if err != nil {
		t.Fatal(err)
	}
	drifted := 0
	for _, r := range rows {
		if r.Drift {
			drifted++
		}
	}
	if drifted != 1 {
		t.Fatalf("应恰好 1 处漂移: %+v", rows)
	}

	// 断开：两文件均逐字节还原
	if err := m.Unwire(geminiDef()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(settingsPath); string(got) != userGeminiSettings {
		t.Fatalf("settings.json 未逐字节还原\n%s", got)
	}
	if got, _ := os.ReadFile(envPath); string(got) != userGeminiEnv {
		t.Fatalf(".env 未逐字节还原\n%s", got)
	}
}

func TestWireGeminiCreatesAndCleansUp(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".gemini", "settings.json")
	envPath := filepath.Join(home, ".gemini", ".env")

	// 两文件均不存在：接线创建
	if _, err := m.Wire(geminiDef(), "group/gem"); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	for _, p := range []string{settingsPath, envPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("应创建 %s: %v", p, err)
		}
	}
	// 断开：软件创建的文件 → 删名下键 → 变空则移除
	if err := m.Unwire(geminiDef()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{settingsPath, envPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("软件创建的文件应被移除: %s", p)
		}
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

// ---------- Cline（[2026-11-09-cline-spec] 规格核验后接线） ----------

// userClineProviders 模拟真实用户文件：cline OAuth 条目在先（接线后必须
// 原样保留），形状对齐官方测试夹具 providers.json。
const userClineProviders = `{
  "version": 1,
  "lastUsedProvider": "cline",
  "modes": {},
  "providers": {
    "cline": {
      "settings": {
        "provider": "cline",
        "auth": {"accessToken": "workos:test-token", "refreshToken": "rt-1"},
        "model": "anthropic/claude-sonnet-4.6"
      },
      "updatedAt": "2026-01-01T00:00:00.000Z",
      "tokenSource": "oauth"
    }
  }
}`

func clineDef() Def { def, _ := ByID("cline"); return def }

func TestWireClineProvidersJSON(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".cline", "data", "settings", "providers.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(userClineProviders), 0o600); err != nil {
		t.Fatal(err)
	}

	// 接线：写入 yardmaster 条目 + lastUsedProvider 翻转，cline 条目保留。
	if _, err := m.Wire(clineDef(), "group/chain-main"); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("接线后应为合法 JSON: %v\n%s", err, out)
	}
	if doc["lastUsedProvider"] != "yardmaster" {
		t.Fatalf("lastUsedProvider = %v", doc["lastUsedProvider"])
	}
	prov := doc["providers"].(map[string]any)
	ym := prov["yardmaster"].(map[string]any)
	st := ym["settings"].(map[string]any)
	if st["provider"] != "yardmaster" || st["protocol"] != "openai-chat" ||
		st["client"] != "openai-compatible" || st["apiKey"] != "gw-cline" ||
		st["model"] != "group/chain-main" {
		t.Fatalf("yardmaster.settings: %+v", st)
	}
	if st["baseUrl"] != "http://127.0.0.1:8317/v1" {
		t.Fatalf("baseUrl = %v（须为合法 URL 且指向网关 /v1）", st["baseUrl"])
	}
	if _, ok := ym["updatedAt"].(string); !ok {
		t.Fatalf("updatedAt（schema 必填时间戳）缺失: %+v", ym)
	}
	if ym["tokenSource"] != "manual" {
		t.Fatalf("tokenSource = %v", ym["tokenSource"])
	}
	// cline OAuth 条目原样保留
	cl := prov["cline"].(map[string]any)
	if cl["tokenSource"] != "oauth" {
		t.Fatalf("cline 条目应原样保留: %+v", cl)
	}

	// 状态 = 已接入；漂移 = 0（updatedAt 为 Volatile，比对跳过）
	stt, err := m.StateOf(clineDef())
	if err != nil || stt != StateWired {
		t.Fatalf("state = %v %v", stt, err)
	}
	rows, err := m.Drift(clineDef())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Drift {
			t.Fatalf("刚接线即漂移（Volatile 未跳过？）: %+v", rows)
		}
	}

	// 外部改动 → 漂移可见
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(out), "yardmaster", "other", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err = m.Drift(clineDef())
	if err != nil {
		t.Fatal(err)
	}
	drifted := false
	for _, r := range rows {
		if r.Drift {
			drifted = true
		}
	}
	if !drifted {
		t.Fatalf("外部改动应产生漂移行: %+v", rows)
	}

	// 断开：逐字节还原用户原文件
	if err := m.Unwire(clineDef()); err != nil {
		t.Fatalf("Unwire: %v", err)
	}
	restored, _ := os.ReadFile(cfgPath)
	if string(restored) != userClineProviders {
		t.Fatalf("断开应逐字节还原\n%s", restored)
	}
}

func TestWireClineCreatesThenUnsets(t *testing.T) {
	m := newManager(t)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".cline", "data", "settings", "providers.json")

	if _, err := m.Wire(clineDef(), "group/chain-main"); err != nil {
		t.Fatalf("Wire（软件创建）: %v", err)
	}
	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("配置文件应被创建: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("应为合法 JSON: %v", err)
	}
	if doc["version"] != float64(1) {
		t.Fatalf("新建文件应带 version:1（schema literal）: %v", doc["version"])
	}

	// 断开：软件创建的文件 → 名下键全删 → 级联清空 → 移除（F4）
	if err := m.Unwire(clineDef()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("软件创建且清空后的配置应被移除")
	}
}

// TestStateCLIDetection（OBS-1）：配置文件未落盘但 CLI 已在 PATH →
// 未接入（而非未安装）；CLI 不存在 → 未安装。
func TestStateCLIDetection(t *testing.T) {
	m := newManager(t)
	binDir := filepath.Join(testenv.New(t), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(binDir, "fakeagent.cmd")
	if runtime.GOOS != "windows" {
		shim = filepath.Join(binDir, "fakeagent")
	}
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	def := Def{ID: "t", Name: "T", Format: FormatJSONC, CLI: "fakeagent",
		RelPaths: []string{"~/no-such-t/settings.json"}}
	st, err := m.StateOf(def)
	if err != nil || st != StateUnwired {
		t.Fatalf("CLI 在 PATH 应报未接入: %v %v", st, err)
	}

	def2 := def
	def2.CLI = "definitely-not-a-real-cli-xyz"
	st, err = m.StateOf(def2)
	if err != nil || st != StateMissing {
		t.Fatalf("CLI 缺失应报未安装: %v %v", st, err)
	}

	// CLI="" 回退旧语义：文件缺失=未安装
	def3 := def
	def3.CLI = ""
	st, err = m.StateOf(def3)
	if err != nil || st != StateMissing {
		t.Fatalf("CLI 空应回退文件判定: %v %v", st, err)
	}
}
