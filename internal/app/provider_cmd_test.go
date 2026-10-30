// W3 CLI 测试：provider/settings 子命令的持久化与预检（testenv 沙箱）。
package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/credential"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
	"yardmaster/internal/testenv"
)

// fixtureParser 是测试专用的导入解析器（只存在于测试二进制，随
// M3-W2 内置解析器落地后仍用于管道回放）。形状依据
// testdata/credentials/claude-code.json（公开文档形状，假值）。
func fixtureParser(data []byte) (credential.OAuth, error) {
	var f struct {
		ClaudeAiOauth struct {
			AccessToken  string   `json:"accessToken"`
			RefreshToken string   `json:"refreshToken"`
			ExpiresAt    int64    `json:"expiresAt"`
			Scopes       []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return credential.OAuth{}, err
	}
	if f.ClaudeAiOauth.AccessToken == "" {
		return credential.OAuth{}, os.ErrInvalid
	}
	return credential.OAuth{
		AccessToken:  f.ClaudeAiOauth.AccessToken,
		RefreshToken: f.ClaudeAiOauth.RefreshToken,
		ExpiresAt:    f.ClaudeAiOauth.ExpiresAt / 1000, // 毫秒 → 秒
		Scopes:       f.ClaudeAiOauth.Scopes,
	}, nil
}

func init() {
	if err := credential.RegisterParser("test-fixture", fixtureParser); err != nil {
		panic(err)
	}
}

// fixture 路径（仓库级 testdata，测试运行目录为包目录）。
const claudeFixturePath = "../../testdata/credentials/claude-code.json"

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if err := Run(args, &out, &errBuf); err != nil {
		t.Fatalf("Run(%v): %v\nstdout=%s\nstderr=%s", args, err, out.String(), errBuf.String())
	}
	return out.String()
}

func TestProviderAddListRemoveLifecycle(t *testing.T) {
	home := testenv.New(t)

	out := runOK(t, "provider", "add",
		"-id", "deepseek", "-name", "DeepSeek",
		"-protocol", "openai-chat",
		"-base-url", "https://api.deepseek.com",
		"-key", "sk-test", "-account", "main")
	if !strings.Contains(out, "已添加") {
		t.Fatalf("add 输出: %s", out)
	}

	// providers.json 已落盘且 key 在内（仅存本机）
	raw, err := os.ReadFile(filepath.Join(home, ".yardmaster", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sk-test") {
		t.Fatal("key 未落盘")
	}

	out = runOK(t, "provider", "list")
	if !strings.Contains(out, "deepseek") || !strings.Contains(out, "main") {
		t.Fatalf("list 输出: %s", out)
	}

	// 重复账号拒绝
	var errBuf bytes.Buffer
	if err := Run([]string{"provider", "add", "-id", "deepseek", "-base-url", "https://x", "-key", "k2", "-account", "main"}, &bytes.Buffer{}, &errBuf); err == nil {
		t.Fatal("重复账号应报错")
	}

	// remove
	runOK(t, "provider", "remove", "deepseek")
	out = runOK(t, "provider", "list")
	if strings.Contains(out, "deepseek") {
		t.Fatalf("remove 后仍可见: %s", out)
	}
}

func TestProviderProbePersistsModels(t *testing.T) {
	home := testenv.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-ok" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
	}))
	defer srv.Close()

	runOK(t, "provider", "add",
		"-id", "ds", "-protocol", "openai-chat",
		"-base-url", srv.URL, "-key", "sk-ok")

	out := runOK(t, "provider", "probe", "ds")
	if !strings.Contains(out, "2 个模型") {
		t.Fatalf("probe 输出: %s", out)
	}

	raw, err := os.ReadFile(filepath.Join(home, ".yardmaster", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Providers []struct {
			ID     string `json:"id"`
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || len(cfg.Providers[0].Models) != 2 {
		t.Fatalf("模型清单未落盘: %s", raw)
	}
	if cfg.Providers[0].Models[1].ID != "deepseek-reasoner" {
		t.Fatalf("models = %+v", cfg.Providers[0].Models)
	}
}

func TestProviderProbeFailureMarksPrefail(t *testing.T) {
	home := testenv.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	runOK(t, "provider", "add",
		"-id", "bad", "-protocol", "openai-chat", "-base-url", srv.URL, "-key", "wrong")

	var errBuf bytes.Buffer
	if err := Run([]string{"provider", "probe", "bad"}, &bytes.Buffer{}, &errBuf); err == nil {
		t.Fatal("预检失败应返回错误")
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".yardmaster", "providers.json"))
	if !strings.Contains(string(raw), "prefail") {
		t.Fatalf("账号应标记 prefail: %s", raw)
	}
}

func TestSettingsGlobalProxy(t *testing.T) {
	home := testenv.New(t)

	out := runOK(t, "settings", "get")
	if !strings.Contains(out, "跟随系统代理") {
		t.Fatalf("默认输出: %s", out)
	}
	runOK(t, "settings", "set", "global_proxy", "socks5://user:secret@127.0.0.1:1080")
	out = runOK(t, "settings", "get")
	if strings.Contains(out, "secret") {
		t.Fatalf("设置输出泄漏凭证: %s", out)
	}
	if !strings.Contains(out, "socks5://***@127.0.0.1:1080") {
		t.Fatalf("设置输出: %s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".yardmaster", "settings.json"))
	if !strings.Contains(string(raw), "127.0.0.1:1080") {
		t.Fatalf("设置未落盘: %s", raw)
	}
	// 非法协议拒绝
	if err := Run([]string{"settings", "set", "global_proxy", "ftp://x"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("非法协议应拒绝")
	}
	// settings.json 损坏 → 按未知处理（读不到=未知）
	p := filepath.Join(home, ".yardmaster", "settings.json")
	_ = os.WriteFile(p, []byte("{bad"), 0o600)
	var errBuf bytes.Buffer
	if err := Run([]string{"settings", "get"}, &bytes.Buffer{}, &errBuf); err == nil {
		t.Fatal("损坏的 settings.json 应报错而非静默清空")
	}
	_ = store.Paths{} // 引用保持
}

func TestProviderAddOAuthPipeline(t *testing.T) {
	home := testenv.New(t)

	out := runOK(t, "provider", "add-oauth",
		"-id", "anthropic", "-name", "Anthropic",
		"-base-url", "https://api.anthropic.com",
		"-from", "test-fixture:"+claudeFixturePath)
	if !strings.Contains(out, "已添加") || !strings.Contains(out, "OAuth") {
		t.Fatalf("add-oauth 输出: %s", out)
	}

	raw, err := os.ReadFile(filepath.Join(home, ".yardmaster", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgRaw := string(raw)
	if !strings.Contains(cfgRaw, `"auth": "oauth"`) {
		t.Fatalf("providers.json 应含 auth=oauth: %s", cfgRaw)
	}
	// 铁律：token 永不进入 providers.json（及其备份栈由同一机制覆盖）
	if strings.Contains(cfgRaw, "sk-ant-oat01-EXAMPLE") {
		t.Fatal("OAuth token 泄漏进 providers.json")
	}
	// 元数据齐全（毫秒样例 → 秒）
	var f store.ProvidersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	acc := f.Providers[0].Accounts[0]
	if acc.Auth != provider.AuthOAuth || acc.OAuth == nil ||
		acc.OAuth.ExpiresAt != 1893456000 || acc.OAuth.State != provider.OAuthStateOK {
		t.Fatalf("oauth 元数据不符: %+v", acc)
	}
	// 凭证库可读回
	paths, _ := store.DefaultPaths()
	credStore := credential.Store{Path: paths.CredentialsPath(), BackupDir: paths.BackupDir()}
	got, err := credStore.Load("anthropic/main")
	if err != nil || got.AccessToken != "sk-ant-oat01-EXAMPLE000000000000000000000000000000000000AA" {
		t.Fatalf("凭证库读回: %+v, %v", got, err)
	}
	// list 展示 oauth 标记
	out = runOK(t, "provider", "list")
	if !strings.Contains(out, "oauth") || !strings.Contains(out, "过期:") {
		t.Fatalf("list 应展示 oauth 标记: %s", out)
	}
	// 重复导入需 -replace
	if err := Run([]string{"provider", "add-oauth", "-id", "anthropic", "-from", "test-fixture:" + claudeFixturePath}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("重复导入应拒绝")
	}
}

func TestProviderAddOAuthFailsClosed(t *testing.T) {
	home := testenv.New(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"未知格式", []string{"provider", "add-oauth", "-id", "x", "-from", "nope:whatever"}, "未支持的导入格式"},
		{"from 缺格式", []string{"provider", "add-oauth", "-id", "x", "-from", "just-a-path"}, "-from 须为"},
		{"源文件缺失", []string{"provider", "add-oauth", "-id", "x", "-base-url", "https://x", "-from", "test-fixture:./no-such-file.json"}, "读不懂=未知"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errBuf bytes.Buffer
			if err := Run(tc.args, &bytes.Buffer{}, &errBuf); err == nil {
				t.Fatalf("应失败: %v", tc.args)
			}
			if !strings.Contains(errBuf.String(), tc.want) {
				t.Fatalf("stderr 应含 %q: %s", tc.want, errBuf.String())
			}
		})
	}
	// 解析失败（形状不对）：整体失败，不落任何文件
	bad := filepath.Join(home, "bad-cred.json")
	if err := os.WriteFile(bad, []byte(`{"unexpected":{"shape":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	if err := Run([]string{"provider", "add-oauth", "-id", "x", "-base-url", "https://x", "-from", "test-fixture:" + bad}, &bytes.Buffer{}, &errBuf); err == nil {
		t.Fatal("解析失败应报错")
	}
	if _, err := os.Stat(filepath.Join(home, ".yardmaster", "providers.json")); !os.IsNotExist(err) {
		t.Fatalf("失败路径不应产生 providers.json: %v", err)
	}
}

func TestProviderAddOAuthReplaceConvertsAPIKey(t *testing.T) {
	home := testenv.New(t)

	runOK(t, "provider", "add",
		"-id", "anthropic", "-protocol", "anthropic-messages",
		"-base-url", "https://api.anthropic.com",
		"-key", "sk-old-key", "-account", "main")

	out := runOK(t, "provider", "add-oauth",
		"-id", "anthropic", "-account", "main", "-replace",
		"-from", "test-fixture:"+claudeFixturePath)
	if !strings.Contains(out, "已转换") {
		t.Fatalf("转换输出: %s", out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".yardmaster", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgRaw := string(raw)
	if strings.Contains(cfgRaw, "sk-old-key") {
		t.Fatal("转换后旧 api_key 应被清除（互斥铁律）")
	}
	var f store.ProvidersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	acc := f.Providers[0].Accounts[0]
	if acc.Auth != provider.AuthOAuth || acc.APIKey != "" || acc.OAuth == nil {
		t.Fatalf("转换后账号形态不符: %+v", acc)
	}
	// 凭证库已落
	paths, _ := store.DefaultPaths()
	credStore := credential.Store{Path: paths.CredentialsPath(), BackupDir: paths.BackupDir()}
	if _, err := credStore.Load("anthropic/main"); err != nil {
		t.Fatalf("凭证库应可读回: %v", err)
	}
	// 写前备份：providers.json 有快照
	snaps, _ := filepath.Glob(filepath.Join(home, ".yardmaster", "backup", "providers-*.json"))
	if len(snaps) == 0 {
		t.Fatal("providers.json 覆写前应留底")
	}
}
