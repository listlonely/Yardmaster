// Agent 目录化安装测试（2026-11-11）：目录视图（installed/found 标注）、
// 安装全链（下载→解压→物化→注册 dispatch.json→备份→热重载）、重复安装
// 409、未知 id 404、坏包/损坏配置拒绝。假包源形态仿 plugin_store_test.go。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/dispatch"
)

// zipBytes 复用 plugin_store_test.go 的同款 helper（内存 zip 包）。

// newCatalogServer 起带派发引擎的测试网关（可选预置 dispatch.json）。
func newCatalogServer(t *testing.T, seed string) (*Server, *httptest.Server) {
	t.Helper()
	appDir := t.TempDir()
	if seed != "" {
		if err := os.WriteFile(filepath.Join(appDir, "dispatch.json"), []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	de, derr := dispatch.LoadEngine(filepath.Join(appDir, "dispatch.json"))
	if derr != nil {
		t.Fatalf("预置 dispatch.json 损坏: %v", derr)
	}
	s := &Server{Planner: NewPlanner(nil, nil), Version: "test", AppDir: appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json")}
	s.AttachDispatch(de)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

// withCatalogURL 临时替换目录条目的包源地址（SetCatalogForTest 注入，
// 测试结束恢复原目录）。
func withCatalogURL(t *testing.T, id, url string) {
	t.Helper()
	orig := dispatch.Catalog()
	mutated := append([]dispatch.CatalogEntry(nil), orig...)
	hit := false
	for i := range mutated {
		if mutated[i].Spec.ID == id {
			mutated[i].DownloadURL = url
			hit = true
		}
	}
	if !hit {
		t.Fatalf("目录缺少条目 %s", id)
	}
	dispatch.SetCatalogForTest(mutated)
	t.Cleanup(func() { dispatch.SetCatalogForTest(orig) })
}

func postCatalogInstall(t *testing.T, srv *httptest.Server, id string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/dispatch/catalog/"+id+"/install", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestDispatchCatalogView：目录 3 条 + installed 标注与 dispatch.json 联动。
// found 断言仅在本机无该 CLI 时强断言 false（CI 环境；装有 claude 的
// 开发机不做反向断言）。
func TestDispatchCatalogView(t *testing.T) {
	_, srv := newCatalogServer(t, `{"agents":[{"id":"claude-code","command":["claude","-p"]}]}`)
	resp, err := http.Get(srv.URL + "/api/dispatch/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK      bool `json:"ok"`
		Catalog []struct {
			Spec struct {
				ID string `json:"id"`
			} `json:"spec"`
			Installed bool `json:"installed"`
			Found     bool `json:"found"`
		} `json:"catalog"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(out.Catalog) != 3 {
		t.Fatalf("目录应 3 条: ok=%v n=%d", out.OK, len(out.Catalog))
	}
	seen := map[string]bool{}
	for _, e := range out.Catalog {
		seen[e.Spec.ID] = true
		if e.Spec.ID == "claude-code" {
			if !e.Installed {
				t.Fatalf("预置的 claude-code 应标 installed=true")
			}
			if _, err := exec.LookPath("claude"); err != nil && e.Found {
				t.Fatalf("本机无 claude CLI 时 found 应为 false")
			}
		}
	}
	for _, want := range []string{"claude-code", "codex", "gemini-cli"} {
		if !seen[want] {
			t.Fatalf("目录缺少条目 %s", want)
		}
	}
}

// TestDispatchCatalogInstallFullChain：假包源 → 安装 → 解压物化 → 注册
// dispatch.json（备份留底）→ 热重载可见 → 重复安装 409。
func TestDispatchCatalogInstallFullChain(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pkg := zipBytes(t, map[string]string{
			"manifest.json": `{"id":"gemini-cli","spec":{"id":"gemini-cli","command":["./run.cmd"],"output":"text"}}`,
			"run.cmd":       "@gemini -p %*",
		})
		_, _ = w.Write(pkg)
	}))
	t.Cleanup(src.Close)
	// seed 空清单：install 为非首写（BackupConfig 对缺失源 no-op，断言
	// 「写前备份留底」需要已存在的源文件）
	s, srv := newCatalogServer(t, `{"agents":[]}`)
	withCatalogURL(t, "gemini-cli", src.URL+"/gemini-cli.zip")
	code, body := postCatalogInstall(t, srv, "gemini-cli")
	if code != http.StatusOK {
		t.Fatalf("install status = %d, body = %s", code, body)
	}
	// 落盘：包文件在安装目录
	dir := s.dispatchAgentsInstallDir("gemini-cli")
	for _, f := range []string{"manifest.json", "run.cmd"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("安装包文件缺失 %s: %v", f, err)
		}
	}
	// 注册：dispatch.json 有条目，command[0] 物化为绝对路径
	raw, err := os.ReadFile(s.dispatchPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "gemini-cli") {
		t.Fatalf("dispatch.json 应含 gemini-cli: %s", raw)
	}
	if strings.Contains(string(raw), "./run.cmd") {
		t.Fatalf("command[0] 应已物化为绝对路径: %s", raw)
	}
	// 备份留底
	matches, _ := filepath.Glob(filepath.Join(s.AppDir, "backup", "dispatch-*.json"))
	if len(matches) != 1 {
		t.Fatalf("写前应留 1 份备份，实际 %d", len(matches))
	}
	// 热重载：/api/dispatch/agents 可见
	resp, err := http.Get(srv.URL + "/api/dispatch/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "gemini-cli") {
		t.Fatalf("热重载后 agents 应含 gemini-cli: %s", string(b))
	}
	// 重复安装 → 409
	if code, _ = postCatalogInstall(t, srv, "gemini-cli"); code != http.StatusConflict {
		t.Fatalf("重复安装应 409，得到 %d", code)
	}
}

// TestDispatchCatalogMatchInherit：预设侧重继承（2026-11-12）——manifest
// 未带 match 继承目录条目预置（gemini-cli=qa）；manifest 带 match 原样
// 优先（不被目录条目覆盖）。
func TestDispatchCatalogMatchInherit(t *testing.T) {
	// 场景一：manifest 无 match → 继承目录条目 qa
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes(t, map[string]string{
			"manifest.json": `{"id":"gemini-cli","spec":{"id":"gemini-cli","command":["./run.cmd"],"output":"text"}}`,
			"run.cmd":       "@gemini -p %*",
		}))
	}))
	t.Cleanup(src.Close)
	s, srv := newCatalogServer(t, `{"agents":[]}`)
	withCatalogURL(t, "gemini-cli", src.URL+"/gemini-cli.zip")
	if code, body := postCatalogInstall(t, srv, "gemini-cli"); code != http.StatusOK {
		t.Fatalf("install status = %d, body = %s", code, body)
	}
	raw, err := os.ReadFile(s.dispatchPath())
	if err != nil {
		t.Fatal(err)
	}
	var got dispatch.DispatchFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Match == nil || got.Agents[0].Match.Intent != "qa" {
		t.Fatalf("manifest 无 match 应继承目录条目预置 qa: %+v", got.Agents)
	}

	// 场景二：manifest 自带 match → 原样优先
	src2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes(t, map[string]string{
			"manifest.json": `{"id":"gemini-cli","spec":{"id":"gemini-cli","command":["./run.cmd"],"output":"text","match":{"intent":"writing"}}}`,
			"run.cmd":       "@gemini -p %*",
		}))
	}))
	t.Cleanup(src2.Close)
	s2, srv2 := newCatalogServer(t, `{"agents":[]}`)
	withCatalogURL(t, "gemini-cli", src2.URL+"/gemini-cli.zip")
	if code, body := postCatalogInstall(t, srv2, "gemini-cli"); code != http.StatusOK {
		t.Fatalf("install status = %d, body = %s", code, body)
	}
	raw2, err := os.ReadFile(s2.dispatchPath())
	if err != nil {
		t.Fatal(err)
	}
	var got2 dispatch.DispatchFile
	if err := json.Unmarshal(raw2, &got2); err != nil {
		t.Fatal(err)
	}
	if len(got2.Agents) != 1 || got2.Agents[0].Match == nil || got2.Agents[0].Match.Intent != "writing" {
		t.Fatalf("manifest 带 match 应原样优先: %+v", got2.Agents)
	}
}

// TestDispatchCatalogInstallUnknown：未知目录 id → 404。
func TestDispatchCatalogInstallUnknown(t *testing.T) {
	_, srv := newCatalogServer(t, "")
	if code, _ := postCatalogInstall(t, srv, "no-such-agent"); code != http.StatusNotFound {
		t.Fatalf("未知 id 应 404，得到 %d", code)
	}
}

// TestDispatchCatalogInstallBadPack：假源返回缺 manifest 的坏包 → 400
// 且不留半成品目录。
func TestDispatchCatalogInstallBadPack(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes(t, map[string]string{"run.cmd": "@echo off"}))
	}))
	t.Cleanup(src.Close)
	s, srv := newCatalogServer(t, "")
	withCatalogURL(t, "codex", src.URL+"/codex.zip")
	if code, _ := postCatalogInstall(t, srv, "codex"); code != http.StatusBadRequest {
		t.Fatalf("坏包应 400，得到 %d", code)
	}
	if _, err := os.Stat(s.dispatchAgentsInstallDir("codex")); !os.IsNotExist(err) {
		t.Fatalf("解压失败不得留半成品目录: %v", err)
	}
}

// TestDispatchCatalogInstallCorruptConfig：dispatch.json 损坏 → 409
//（loadCurrentForWrite fail-closed），不得落盘。
func TestDispatchCatalogInstallCorruptConfig(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes(t, map[string]string{
			"manifest.json": `{"id":"gemini-cli","spec":{"id":"gemini-cli","command":["./run.cmd"],"output":"text"}}`,
			"run.cmd":       "@gemini -p %*",
		}))
	}))
	t.Cleanup(src.Close)
	// seed 须合法（LoadEngine 对损坏文件直接 fail-closed 报错）——先合法
	// 起引擎，再把文件覆盖写坏，测 loadCurrentForWrite 的 409 路径。
	s, srv := newCatalogServer(t, `{"agents":[]}`)
	withCatalogURL(t, "gemini-cli", src.URL+"/gemini-cli.zip")
	if err := os.WriteFile(s.dispatchPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := postCatalogInstall(t, srv, "gemini-cli"); code != http.StatusConflict {
		t.Fatalf("损坏配置应 409，得到 %d", code)
	}
}
