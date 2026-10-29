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

	"agenthub/internal/store"
	"agenthub/internal/testenv"
)

func runOK(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	if err := Run(args, &out, &errBuf); err != nil {
		t.Fatalf("Run(%v): %v\nstdout=%s\nstderr=%s", args, err, out.String(), errBuf.String())
	}
	return out.String(), errBuf.String()
}

func TestProviderAddListRemoveLifecycle(t *testing.T) {
	home := testenv.New(t)

	out, _ := runOK(t, "provider", "add",
		"-id", "deepseek", "-name", "DeepSeek",
		"-protocol", "openai-chat",
		"-base-url", "https://api.deepseek.com",
		"-key", "sk-test", "-account", "main")
	if !strings.Contains(out, "已添加") {
		t.Fatalf("add 输出: %s", out)
	}

	// providers.json 已落盘且 key 在内（仅存本机）
	raw, err := os.ReadFile(filepath.Join(home, ".agenthub", "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sk-test") {
		t.Fatal("key 未落盘")
	}

	out, _ = runOK(t, "provider", "list")
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
	out, _ = runOK(t, "provider", "list")
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

	out, _ := runOK(t, "provider", "probe", "ds")
	if !strings.Contains(out, "2 个模型") {
		t.Fatalf("probe 输出: %s", out)
	}

	raw, err := os.ReadFile(filepath.Join(home, ".agenthub", "providers.json"))
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
	raw, _ := os.ReadFile(filepath.Join(home, ".agenthub", "providers.json"))
	if !strings.Contains(string(raw), "prefail") {
		t.Fatalf("账号应标记 prefail: %s", raw)
	}
}

func TestSettingsGlobalProxy(t *testing.T) {
	home := testenv.New(t)

	out, _ := runOK(t, "settings", "get")
	if !strings.Contains(out, "跟随系统代理") {
		t.Fatalf("默认输出: %s", out)
	}
	runOK(t, "settings", "set", "global_proxy", "socks5://user:secret@127.0.0.1:1080")
	out, _ = runOK(t, "settings", "get")
	if strings.Contains(out, "secret") {
		t.Fatalf("设置输出泄漏凭证: %s", out)
	}
	if !strings.Contains(out, "socks5://***@127.0.0.1:1080") {
		t.Fatalf("设置输出: %s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".agenthub", "settings.json"))
	if !strings.Contains(string(raw), "127.0.0.1:1080") {
		t.Fatalf("设置未落盘: %s", raw)
	}
	// 非法协议拒绝
	if err := Run([]string{"settings", "set", "global_proxy", "ftp://x"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("非法协议应拒绝")
	}
	// settings.json 损坏 → 按未知处理（读不到=未知）
	p := filepath.Join(home, ".agenthub", "settings.json")
	_ = os.WriteFile(p, []byte("{bad"), 0o600)
	var errBuf bytes.Buffer
	if err := Run([]string{"settings", "get"}, &bytes.Buffer{}, &errBuf); err == nil {
		t.Fatal("损坏的 settings.json 应报错而非静默清空")
	}
	_ = store.Paths{} // 引用保持
}
