// M7-W3/W4 插件接缝测试：配置闸（cfgPlugins 严格解析/校验/热替换）、
// 管理面（状态视图/启停/404/未挂载）、trace 扇出链（prev 观察者不中
// 断、事件到达插件）、W4 三桥（dispatch.parse 词表校验回退、classify
// 词表校验、未挂载安全降级）。假插件用测试二进制自身（helper 进程，
// 与 internal/plugin 同模式）。
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/plugin"
	"yardmaster/internal/store"
)

// TestGatewayPluginHelperProcess 不是真正的测试：作为假插件子进程运行。
// 能力声明经 GO_GW_PLUGIN_CAPS（逗号分隔）；行为：
//   - event  → stderr 打 "got-event"（trace 扇出断言载体）
//   - parse  → 返回 events[{kind: GO_GW_PLUGIN_PARSE_KIND（默认 text）}]
//   - classify → 返回 {intent: GO_GW_PLUGIN_INTENT}
func TestGatewayPluginHelperProcess(t *testing.T) {
	if os.Getenv("GO_GW_PLUGIN_HELPER") != "1" {
		return
	}
	var caps []string
	for _, c := range strings.Split(os.Getenv("GO_GW_PLUGIN_CAPS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			caps = append(caps, c)
		}
	}
	out := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	out(map[string]any{"plugin": "gw", "proto": 1, "capabilities": caps})
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	in.Scan() // 父回执
	for in.Scan() {
		var env struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(in.Bytes(), &env) != nil {
			continue
		}
		switch env.Type {
		case "event":
			fmt.Fprintln(os.Stderr, "got-event")
		case "parse":
			kind := os.Getenv("GO_GW_PLUGIN_PARSE_KIND")
			if kind == "" {
				kind = "text"
			}
			out(map[string]any{"v": 1, "type": "resp", "id": env.ID,
				"payload": map[string]any{"events": []map[string]any{{"kind": kind, "text": "插件解析的文本"}}}})
		case "classify":
			out(map[string]any{"v": 1, "type": "resp", "id": env.ID,
				"payload": map[string]any{"intent": os.Getenv("GO_GW_PLUGIN_INTENT")}})
		case "ping":
			out(map[string]any{"v": 1, "type": "pong"})
		}
	}
	os.Exit(0)
}

func helperCommand() []string {
	exe, _ := os.Executable()
	return []string{exe, "-test.run=TestGatewayPluginHelperProcess", "--"}
}

// startTestPlugins 从 spec 列表构建并启动注册表（测试收尾 Stop）。
func startTestPlugins(t *testing.T, specs ...plugin.Spec) (*plugin.Registry, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	f := plugin.File{Plugins: specs}
	if err := plugin.Save(filepath.Join(dir, "plugins.json"), f); err != nil {
		t.Fatal(err)
	}
	reg := plugin.NewRegistry()
	reg.Start(f)
	return reg, reg.Stop
}

func waitRunning(t *testing.T, reg *plugin.Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range reg.Views() {
			if v.ID == id && v.Status == plugin.StatusRunning {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("插件 %s 未进入 running", id)
}

func helperSpec(id string, caps []string, extraEnv ...string) plugin.Spec {
	env := []string{"GO_GW_PLUGIN_HELPER=1", "GO_GW_PLUGIN_CAPS=" + strings.Join(caps, ",")}
	env = append(env, extraEnv...)
	return plugin.Spec{
		ID: id, Enabled: true, Capabilities: caps, TimeoutMS: 300,
		Command: helperCommand(), Env: env,
	}
}

// 配置闸：读（不存在=空）、写（校验/严格解析/落盘）、损坏拒写。
func TestPluginsConfigGate(t *testing.T) {
	dir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// 读：文件不存在 → 空清单。
	resp, err := http.Get(srv.URL + "/api/config/plugins")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Plugins []plugin.Spec `json:"plugins"`
	}
	decodeInto(t, resp.Body, &got)
	resp.Body.Close()
	if len(got.Plugins) != 0 {
		t.Fatalf("无文件应返回空清单: %+v", got)
	}

	// 写：合法文档落盘（先落旧文件——备份语义是覆写前留底；s.Plugins
	// 未挂载时不 Reload，仍成功）。
	if err := os.WriteFile(filepath.Join(dir, "plugins.json"), []byte(`{"plugins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := `{"plugins":[{"id":"p1","enabled":true,"command":["x"],"capabilities":["events.trace"]}]}`
	put := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config/plugins", strings.NewReader(body))
		r2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r2
	}
	resp = put(valid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法 PUT status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	var saved plugin.File
	if err := store.LoadJSON(filepath.Join(dir, "plugins.json"), &saved); err != nil || len(saved.Plugins) != 1 {
		t.Fatalf("落盘: %v %+v", err, saved)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup")); err != nil {
		t.Fatalf("写前备份缺失: %v", err)
	}

	// 未知能力 → 400；重复 id → 400；未知字段 → 400（strictDecode）。
	for name, body := range map[string]string{
		"未知能力": `{"plugins":[{"id":"p1","command":["x"],"capabilities":["nope"]}]}`,
		"重复id": `{"plugins":[{"id":"p","command":["x"]},{"id":"p","command":["y"]}]}`,
		"未知字段": `{"plugins":[],"extra":1}`,
	} {
		if r := put(body); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d", name, r.StatusCode)
		} else {
			r.Body.Close()
		}
	}

	// 现值损坏 → 409（fail-closed 不覆写）。
	if err := os.WriteFile(filepath.Join(dir, "plugins.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := put(valid); r.StatusCode != http.StatusConflict {
		t.Fatalf("损坏现值应 409: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

// 管理面 E2E：视图/启停/404/未挂载 + trace 扇出链。
func TestPluginsViewToggleAndTrace(t *testing.T) {
	dir := t.TempDir()
	reg, stop := startTestPlugins(t, helperSpec("tracep", []string{plugin.CapEvents}))
	t.Cleanup(stop)

	s := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	traces := NewTraceLog(filepath.Join(dir, "trace"))
	t.Cleanup(traces.Close)
	s.Traces = traces
	prevCalled := false
	traces.OnTrace = func(Trace) { prevCalled = true } // 既有观察者（OTLP 位）
	s.AttachPlugins(reg)

	// 写 plugins.json（toggle 读写现值用；AppDir 下）。
	f := plugin.File{Plugins: []plugin.Spec{helperSpec("tracep", []string{plugin.CapEvents})}}
	if err := plugin.Save(filepath.Join(dir, "plugins.json"), f); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	waitRunning(t, reg, "tracep")

	// 视图：running + granted=配置∩声明。
	resp, err := http.Get(srv.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Plugins []plugin.StatusView `json:"plugins"`
	}
	decodeInto(t, resp.Body, &view)
	resp.Body.Close()
	if len(view.Plugins) != 1 || view.Plugins[0].Status != plugin.StatusRunning {
		t.Fatalf("视图: %+v", view)
	}
	if len(view.Plugins[0].Granted) != 1 || view.Plugins[0].Granted[0] != plugin.CapEvents {
		t.Fatalf("granted: %+v", view.Plugins[0])
	}

	// trace 扇出：Record → OnTrace 链 → 插件收到事件（stderr 回执）。
	traces.Record(Trace{ID: "t1", Time: time.Now().UTC().Format(time.RFC3339), Outcome: "ok"})
	traceDeadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(reg.LogTail("tracep"), "\n"), "got-event") {
		if time.Now().After(traceDeadline) {
			t.Fatalf("事件未到达插件，日志尾: %v", reg.LogTail("tracep"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !prevCalled {
		t.Fatal("既有 OnTrace 观察者应被链式调用")
	}

	// disable → 视图 stopped + 落盘翻转。
	post := func(path string) *http.Response {
		r2, err := http.Post(srv.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r2
	}
	if r := post("/api/plugins/tracep/disable"); r.StatusCode != http.StatusOK {
		t.Fatalf("disable status = %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	var saved plugin.File
	if err := store.LoadJSON(filepath.Join(dir, "plugins.json"), &saved); err != nil || saved.Plugins[0].Enabled {
		t.Fatalf("disable 落盘: %v %+v", err, saved)
	}
	stopDeadline := time.Now().Add(5 * time.Second)
	for {
		vs := reg.Views()
		if len(vs) == 1 && vs[0].Status == plugin.StatusStopped && !vs[0].Enabled {
			break
		}
		if time.Now().After(stopDeadline) {
			t.Fatalf("disable 后视图: %+v", vs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// enable → 重新 running。
	if r := post("/api/plugins/tracep/enable"); r.StatusCode != http.StatusOK {
		t.Fatalf("enable status = %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	waitRunning(t, reg, "tracep")

	// 未知 id → 404。
	if r := post("/api/plugins/nope/enable"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 id 应 404: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 未挂载注册表 → 400（可操作指引）。
	s2 := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	r2, err := http.Post(srv2.URL+"/api/plugins/x/enable", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("未挂载应 400: %d", r2.StatusCode)
	}
	r2.Body.Close()
}

// W4：dispatch.parse 桥——合法 kind 采纳、词表外/未知插件回退 false。
func TestParseHookBridge(t *testing.T) {
	reg, stop := startTestPlugins(t,
		helperSpec("p-ok", []string{plugin.CapDispatch}),
		helperSpec("p-bad", []string{plugin.CapDispatch}, "GO_GW_PLUGIN_PARSE_KIND=nope"))
	t.Cleanup(stop)

	s := &Server{Planner: NewPlanner(nil, nil), Version: "t"}
	s.Dispatch = dispatch.NewEngine(nil)
	s.AttachPlugins(reg)
	waitRunning(t, reg, "p-ok")
	waitRunning(t, reg, "p-bad")

	evs, ok := s.Dispatch.ParseHook("p-ok", `{"type":"assistant"}`)
	if !ok || len(evs) != 1 || evs[0].Kind != dispatch.EventText || evs[0].Text != "插件解析的文本" {
		t.Fatalf("合法解析: ok=%v %+v", ok, evs)
	}
	if _, ok := s.Dispatch.ParseHook("p-bad", "x"); ok {
		t.Fatal("词表外 kind 应回退 false")
	}
	if _, ok := s.Dispatch.ParseHook("ghost", "x"); ok {
		t.Fatal("未知插件应回退 false")
	}
	// 未挂载插件注册表：ParseHook 为 nil（AttachPlugins 未调用）。
	s2 := &Server{Dispatch: dispatch.NewEngine(nil)}
	if s2.Dispatch.ParseHook != nil {
		t.Fatal("未挂载时 ParseHook 应为 nil")
	}
}

// W4：classify 桥——词表内采纳、词表外/超时/未挂载回落空（默认组）。
func TestClassifyIntentBridge(t *testing.T) {
	reg, stop := startTestPlugins(t,
		helperSpec("cls-ok", []string{plugin.CapClassify}, "GO_GW_PLUGIN_INTENT=coding"),
		helperSpec("cls-bad", []string{plugin.CapClassify}, "GO_GW_PLUGIN_INTENT=not-a-word"))
	t.Cleanup(stop)

	dir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	s.Intents = NewIntentChain(filepath.Join(dir, "intents.json"), "", 0, nil)
	s.AttachPlugins(reg)
	waitRunning(t, reg, "cls-ok")
	waitRunning(t, reg, "cls-bad")

	if got := s.classifyIntentWithPlugin("写个函数"); got != "coding" {
		t.Fatalf("词表内 intent 应采纳: %q", got)
	}
	if got := s.classifyIntentWithPlugin("写个函数"); got != "coding" {
		t.Fatalf("第二次仍应命中: %q", got)
	}
	// cls-bad 排在 cls-ok 后；把 cls-ok 不可达才能打到 cls-bad——直接用
	// 未挂载注册表的 server 验证词表外拒收的另一路径：临时构造仅含 bad。
	reg2, stop2 := startTestPlugins(t, helperSpec("cls-bad", []string{plugin.CapClassify}, "GO_GW_PLUGIN_INTENT=not-a-word"))
	t.Cleanup(stop2)
	s2 := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	s2.Intents = NewIntentChain(filepath.Join(dir, "intents.json"), "", 0, nil)
	s2.AttachPlugins(reg2)
	waitRunning(t, reg2, "cls-bad")
	if got := s2.classifyIntentWithPlugin("x"); got != "" {
		t.Fatalf("词表外 intent 应拒收: %q", got)
	}

	// 无 Intents 链（nil 接收者）不崩、词表空 → 一律拒收。
	s3 := &Server{Planner: NewPlanner(nil, nil), Version: "t"}
	s3.Plugins = reg
	if got := s3.classifyIntentWithPlugin("x"); got != "" {
		t.Fatalf("nil Intents 应拒收: %q", got)
	}
}
