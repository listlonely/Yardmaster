// 批3 测试：插件商店安装器（下载/校验/落盘/注册）与成本账号×模型聚合。
package gateway

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/plugin"
)

// zipBytes 构造内存 zip 包（name→content）。
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withStoreURL 临时替换索引条目的下载地址（测试结束恢复）。
func withStoreURL(t *testing.T, id, url string) {
	t.Helper()
	for i := range PluginStoreIndex {
		if PluginStoreIndex[i].ID == id {
			old := PluginStoreIndex[i].DownloadURL
			PluginStoreIndex[i].DownloadURL = url
			t.Cleanup(func() { PluginStoreIndex[i].DownloadURL = old })
			return
		}
	}
	t.Fatalf("索引缺少条目 %s", id)
}

func postStoreInstall(t *testing.T, srv *httptest.Server, id string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/plugin-store/"+id+"/install", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestStoreInstallMagpieShape：magpie 系包（command 含多个 `./` 相对元素
// ——adapter.mjs 与 middleware.js）安装后**全部元素**物化为包目录绝对
// 路径（2026-11-11：物化从 command[0] 扩展到全部 `./` 元素，支撑六插件
// 真下载安装）。
func TestStoreInstallMagpieShape(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pkg := zipBytes(t, map[string]string{
			"manifest.json":                `{"id":"param-override","command":["node","./adapter.mjs","param-override","./param-override.middleware.js"],"capabilities":["provider.rewrite"]}`,
			"adapter.mjs":                  "// adapter",
			"param-override.middleware.js": "// mw",
		})
		_, _ = w.Write(pkg)
	}))
	t.Cleanup(src.Close)

	appDir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "test", AppDir: appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json")}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	withStoreURL(t, "param-override", src.URL+"/param-override.zip")
	code, body := postStoreInstall(t, srv, "param-override")
	if code != 200 {
		t.Fatalf("install status = %d, body = %s", code, body)
	}
	raw, err := os.ReadFile(s.pluginsPath())
	if err != nil {
		t.Fatal(err)
	}
	var f plugin.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Plugins) != 1 || f.Plugins[0].ID != "param-override" {
		t.Fatalf("注册条目不符: %+v", f.Plugins)
	}
	cmd := f.Plugins[0].Command
	if len(cmd) != 4 || cmd[0] != "node" || cmd[2] != "param-override" {
		t.Fatalf("command 形状不符: %v", cmd)
	}
	dir := s.pluginsInstallDir("param-override")
	for _, i := range []int{1, 3} {
		if !filepath.IsAbs(cmd[i]) || !strings.HasPrefix(cmd[i], dir) {
			t.Fatalf("command[%d] 应物化为包内绝对路径: %v", i, cmd)
		}
		if _, err := os.Stat(cmd[i]); err != nil {
			t.Fatalf("物化路径指向的文件缺失: %v", err)
		}
	}
}

func TestStoreInstallFullChain(t *testing.T) {
	// 假源：合法包（manifest + 入口文件）
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pkg := zipBytes(t, map[string]string{
			"manifest.json": `{"id":"incident-intents","command":["./plugin.exe"],"capabilities":["classify.intent"],"desc":"SRE 词表","intents":[{"id":"sre/oncall","label":"值班响应"}]}`,
			"plugin.exe":    "fake-binary",
		})
		_, _ = w.Write(pkg)
	}))
	t.Cleanup(src.Close)

	appDir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "test", AppDir: appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json")}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	withStoreURL(t, "incident-intents", src.URL+"/incident-intents.zip")
	code, body := postStoreInstall(t, srv, "incident-intents")
	if code != 200 {
		t.Fatalf("install status = %d, body = %s", code, body)
	}

	// 落盘：manifest + 入口都在插件目录
	dir := s.pluginsInstallDir("incident-intents")
	for _, f := range []string{"manifest.json", "plugin.exe"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("安装包文件缺失 %s: %v", f, err)
		}
	}
	// 注册：plugins.json 有条目，command[0] 物化为绝对路径，意图同步
	raw, err := os.ReadFile(s.pluginsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "./plugin.exe") {
		t.Fatal("plugins.json 的 command 应物化为绝对路径")
	}
	var pf plugin.File
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatal(err)
	}
	if len(pf.Plugins) != 1 || pf.Plugins[0].ID != "incident-intents" || !pf.Plugins[0].Enabled {
		t.Fatalf("plugins.json 注册异常: %s", raw)
	}
	if !filepath.IsAbs(pf.Plugins[0].Command[0]) || !strings.HasSuffix(pf.Plugins[0].Command[0], "plugin.exe") {
		t.Fatalf("command[0] 应为插件目录内绝对路径: %v", pf.Plugins[0].Command)
	}
	if len(pf.Plugins[0].Intents) != 1 || pf.Plugins[0].Intents[0].ID != "sre/oncall" {
		t.Fatalf("声明意图应写入: %s", raw)
	}

	// 重复安装 → 409
	code, _ = postStoreInstall(t, srv, "incident-intents")
	if code != 409 {
		t.Fatalf("重复安装应 409，got %d", code)
	}
}

func TestStoreInstallZipSlipRejected(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes(t, map[string]string{
			"manifest.json": `{"id":"cost-alert","command":["./p.exe"],"capabilities":["events.trace"]}`,
			"../evil.txt":   "boom",
		}))
	}))
	t.Cleanup(src.Close)

	appDir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "test", AppDir: appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json")}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	withStoreURL(t, "cost-alert", src.URL+"/cost-alert.zip")
	code, body := postStoreInstall(t, srv, "cost-alert")
	if code != 400 {
		t.Fatalf("zip slip 应 400，got %d: %s", code, body)
	}
	// 越界文件不存在、目录被清理、plugins.json 未写入
	if _, err := os.Stat(filepath.Join(appDir, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("越界文件不应存在")
	}
	if _, err := os.Stat(s.pluginsInstallDir("cost-alert")); !os.IsNotExist(err) {
		t.Fatal("失败安装不应残留插件目录")
	}
	if _, err := os.Stat(s.pluginsPath()); !os.IsNotExist(err) {
		t.Fatal("失败安装不应写 plugins.json")
	}
}

func TestStoreInstallManifestMismatchAndUnpublished(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes(t, map[string]string{
			"manifest.json": `{"id":"other-plugin","command":["./p.exe"],"capabilities":["events.trace"]}`,
			"p.exe":         "x",
		}))
	}))
	t.Cleanup(src.Close)

	appDir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "test", AppDir: appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json")}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// manifest.id 与条目不一致 → 400
	withStoreURL(t, "cost-alert", src.URL+"/x.zip")
	code, _ := postStoreInstall(t, srv, "cost-alert")
	if code != 400 {
		t.Fatalf("manifest 不一致应 400，got %d", code)
	}
	// 未发布（空 URL）→ 503
	withStoreURL(t, "otel-exporter", "")
	code, _ = postStoreInstall(t, srv, "otel-exporter")
	if code != 503 {
		t.Fatalf("未发布包应 503，got %d", code)
	}
	// 不存在的条目 → 404
	code, _ = postStoreInstall(t, srv, "no-such")
	if code != 404 {
		t.Fatalf("未知条目应 404，got %d", code)
	}
}

func TestComputeCostByAccountModel(t *testing.T) {
	now := time.Now()
	prices := []PriceEntry{{Model: "prov/m1", InputPerM: 1, OutputPerM: 2}, {Model: "prov/m2", InputPerM: 1, OutputPerM: 2}}
	traces := []Trace{
		{Time: now.Format(time.RFC3339), Served: "prov/main", ServedModel: "m1",
			Outcome: "ok", Usage: &TraceUsage{Prompt: 1000, Completion: 500}},
		{Time: now.Format(time.RFC3339), Served: "prov/main", ServedModel: "m2",
			Outcome: "ok", Usage: &TraceUsage{Prompt: 2000, Completion: 100}},
	}
	rep := ComputeCost(traces, prices, 7, now)
	key := "prov/main/m1"
	b, ok := rep.ByAccountModel[key]
	if !ok {
		t.Fatalf("缺账号×模型桶 %s: %+v", key, rep.ByAccountModel)
	}
	if b.Calls != 1 || b.InputTokens != 1000 {
		t.Fatalf("桶聚合异常: %+v", b)
	}
	if len(rep.ByAccountModel) != 2 {
		t.Fatalf("应有 2 个二级桶: %+v", rep.ByAccountModel)
	}
}
