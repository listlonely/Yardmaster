package credential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 回放样例：testdata/credentials/ 的形状样例（假值，形状以各 Agent
// 真实凭证为准——见该目录 README）。
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "credentials", name))
	if err != nil {
		t.Fatalf("读取样例 %s: %v", name, err)
	}
	return b
}

func TestParseClaudeCodeFixture(t *testing.T) {
	o, err := parseClaudeCode(fixtureBytes(t, "claude-code.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.AccessToken != "sk-ant-oat01-EXAMPLE000000000000000000000000000000000000AA" ||
		o.RefreshToken != "sk-ant-ort01-EXAMPLE0000000000000000000000000000000000BB" {
		t.Fatalf("token 归一错误: %+v", o)
	}
	if o.ExpiresAt != 1893456000 { // 1893456000000ms → s
		t.Fatalf("毫秒应归一到秒: %d", o.ExpiresAt)
	}
	if o.Extra["subscription_type"] != "max" || o.Extra["refresh_expires_at"] != "2051222400" {
		t.Fatalf("extra 缺失: %v", o.Extra)
	}
	if len(o.Scopes) != 2 {
		t.Fatalf("scopes: %v", o.Scopes)
	}
}

func TestParseCodexFixture(t *testing.T) {
	o, err := parseCodex(fixtureBytes(t, "codex-auth.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.AccessToken != "eyJhbGciOiJQUzI1NiJ9.EXAMPLE1.PAYLOAD" || o.RefreshToken == "" {
		t.Fatalf("token 归一错误: %+v", o)
	}
	if o.Extra["account_id"] != "00000000-0000-0000-0000-000000000000" || o.Extra["auth_mode"] != "chatgpt" {
		t.Fatalf("extra 缺失: %v", o.Extra)
	}
}

func TestParseGeminiFixture(t *testing.T) {
	o, err := parseGeminiCLI(fixtureBytes(t, "gemini-oauth_creds.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.AccessToken != "ya29.EXAMPLE000000000000000000000000000000" {
		t.Fatalf("token 归一错误: %+v", o)
	}
	if o.ExpiresAt != 1893456000 || len(o.Scopes) != 2 || o.Extra["token_type"] != "Bearer" {
		t.Fatalf("归一错误: %+v", o)
	}
}

func TestParsersFailClosed(t *testing.T) {
	cases := []struct {
		name string
		data string
		fn   Parser
	}{
		{"claude 形状不符", `{"unexpected":1}`, parseClaudeCode},
		{"claude 空 token", `{"claudeAiOauth":{"accessToken":""}}`, parseClaudeCode},
		{"claude 非 JSON", `{bad`, parseClaudeCode},
		{"codex API key 登录", `{"OPENAI_API_KEY":"sk-x","tokens":{"access_token":"a"}}`, parseCodex},
		{"codex 无 tokens", `{"OPENAI_API_KEY":null}`, parseCodex},
		{"codex 空 token", `{"tokens":{"access_token":""}}`, parseCodex},
		{"gemini 空 token", `{"access_token":"","refresh_token":"r"}`, parseGeminiCLI},
		{"gemini 非 JSON", `not-json`, parseGeminiCLI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := tc.fn([]byte(tc.data))
			if err == nil {
				t.Fatalf("应拒绝，got %+v", o)
			}
			if o.AccessToken != "" {
				t.Fatalf("失败时不得返回凭证: %+v", o)
			}
		})
	}
}

func TestParsersBOMTolerant(t *testing.T) {
	for _, name := range []string{"claude-code.json", "codex-auth.json", "gemini-oauth_creds.json"} {
		raw := append([]byte("\xef\xbb\xbf"), fixtureBytes(t, name)...)
		format := map[string]Parser{
			"claude-code.json": parseClaudeCode,
			"codex-auth.json":  parseCodex,
			"gemini-oauth_creds.json": parseGeminiCLI,
		}[name]
		if _, err := format(raw); err != nil {
			t.Fatalf("%s 带 BOM 应可解析: %v", name, err)
		}
	}
}

func TestNormExpirySec(t *testing.T) {
	if got := normExpirySec(1893456000000); got != 1893456000 {
		t.Fatalf("毫秒: %d", got)
	}
	if got := normExpirySec(1893456000); got != 1893456000 {
		t.Fatalf("秒: %d", got)
	}
	if got := normExpirySec(0); got != 0 {
		t.Fatalf("零值: %d", got)
	}
}

func TestDefaultPath(t *testing.T) {
	for format, suffix := range map[string]string{
		"claude-code": filepath.Join(".claude", ".credentials.json"),
		"codex":       filepath.Join(".codex", "auth.json"),
		"gemini-cli":  filepath.Join(".gemini", "oauth_creds.json"),
	} {
		p, ok := DefaultPath(format)
		if !ok || !strings.HasSuffix(p, suffix) {
			t.Fatalf("%s 默认路径: %q ok=%v", format, p, ok)
		}
	}
	if _, ok := DefaultPath("nope"); ok {
		t.Fatal("未知格式不应有默认路径")
	}
}
