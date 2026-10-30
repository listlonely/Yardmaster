// M5-W5 派发管理面测试：run/sessions/cancel/config 全链路（httptest +
// 测试二进制扮演假 Agent，不碰真机 Agent、不碰网络）。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/provider"
)

// decodeInto 把响应体 JSON 解码到 v（失败即 Fatal）。
func decodeInto(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
}

// gwDispatchHelper 以测试二进制自身扮演假 Agent。
func gwDispatchHelper(t *testing.T, behavior string) dispatch.AgentSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("YM_GW_HELPER_ON", "1")
	t.Setenv("YM_GW_HELPER_BEHAVIOR", behavior)
	return dispatch.AgentSpec{
		ID: "fake", Command: []string{exe, "-test.run=TestDispatchHelperProcess", "--"},
	}
}

// TestDispatchHelperProcess 不是真正的测试：作为假 Agent 子进程运行。
func TestDispatchHelperProcess(t *testing.T) {
	if os.Getenv("YM_GW_HELPER_ON") != "1" {
		return
	}
	switch os.Getenv("YM_GW_HELPER_BEHAVIOR") {
	case "ok":
		os.Stdout.WriteString("派发结果 ok\n")
	case "slow":
		os.Stdout.WriteString("开始\n")
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func newDispatchGateway(t *testing.T, agents ...dispatch.AgentSpec) (*httptest.Server, *TraceLog) {
	t.Helper()
	tlog := NewTraceLog(t.TempDir())
	t.Cleanup(tlog.Close)
	engine := dispatch.NewEngine(&dispatch.DispatchFile{Agents: agents})
	s := &Server{
		Planner: NewPlanner([]provider.Provider{chatProvider("http://127.0.0.1:1")}, nil),
		Version: "t", Traces: tlog,
	}
	s.AttachDispatch(engine)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, tlog
}

// 提交→运行→详情/列表→trace 留痕全链路。
func TestDispatchRunEndToEnd(t *testing.T) {
	srv, tlog := newDispatchGateway(t, gwDispatchHelper(t, "ok"))

	resp := post(t, srv.URL, "/api/dispatch/run",
		`{"task":"算一下 6*7","agent":"fake"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var run struct {
		OK      bool `json:"ok"`
		Session struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &run)
	if !run.OK || run.Session.ID == "" {
		t.Fatalf("run 响应异常: %+v", run)
	}

	// 轮询详情至终态（含输出尾部）。
	deadline := time.Now().Add(10 * time.Second)
	var detail struct {
		Status string `json:"status"`
		Output string `json:"output"`
	}
	for time.Now().Before(deadline) {
		gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + run.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
		decodeInto(t, gr.Body, &detail)
		gr.Body.Close()
		if detail.Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if detail.Status != "done" || !strings.Contains(detail.Output, "派发结果 ok") {
		t.Fatalf("会话终态异常: %+v", detail)
	}

	// trace 留痕（F9 复用）：RoutePath=dispatch、Served=agent/fake。
	trs := tlog.Recent(10, "", "")
	found := false
	for _, tr := range trs {
		if tr.ID == run.Session.ID && tr.RoutePath == "dispatch" && tr.Served == "agent/fake" && tr.Outcome == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("派发 trace 未留痕: %+v", trs)
	}
}

// 未挂载引擎 → 400（fail-closed，不静默空跑）。
func TestDispatchDisabledWithoutEngine(t *testing.T) {
	s := &Server{Planner: NewPlanner(nil, nil), Version: "t"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp := post(t, srv.URL, "/api/dispatch/run", `{"task":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d（应 400）", resp.StatusCode)
	}
}

// 取消运行中会话 → cancelled 终态。
func TestDispatchCancelRunningSession(t *testing.T) {
	srv, _ := newDispatchGateway(t, gwDispatchHelper(t, "slow"))
	resp := post(t, srv.URL, "/api/dispatch/run", `{"task":"慢任务","agent":"fake"}`)
	var run struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &run)
	if run.Session.ID == "" {
		t.Fatal("未取得会话 ID")
	}
	cresp, err := http.Post(srv.URL+"/api/dispatch/sessions/"+run.Session.ID+"/cancel", "", nil)
	if err != nil || cresp.StatusCode != http.StatusOK {
		t.Fatalf("取消失败: %v %d", err, cresp.StatusCode)
	}
	cresp.Body.Close()

	deadline := time.Now().Add(10 * time.Second)
	var list struct {
		Sessions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"sessions"`
	}
	for time.Now().Before(deadline) {
		gr, _ := http.Get(srv.URL + "/api/dispatch/sessions")
		decodeInto(t, gr.Body, &list)
		gr.Body.Close()
		if len(list.Sessions) > 0 && list.Sessions[0].Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(list.Sessions) == 0 || list.Sessions[0].Status != "cancelled" {
		t.Fatalf("取消终态异常: %+v", list.Sessions)
	}
}

// dispatch 配置写路径：校验 + 备份 + 热替换引擎；非法内容 400 不落盘。
func TestDispatchConfigPutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	engine := dispatch.NewEngine(nil)
	s := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir, Dispatch: engine}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	body := `{"agents":[{"id":"fake","command":["echo","hi"],"match":{"intent":"coding"}},{"id":"fb","command":["cat"],"default":true}]}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config/dispatch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 失败: %v %d", err, resp.StatusCode)
	}
	var out struct {
		OK     bool `json:"ok"`
		Config struct {
			Agents []dispatch.AgentSpec `json:"agents"`
		} `json:"config"`
	}
	decodeInto(t, resp.Body, &out)
	resp.Body.Close()
	if !out.OK || len(out.Config.Agents) != 2 {
		t.Fatalf("写响应包异常: %+v", out)
	}
	if got := engine.Agents(); len(got) != 2 || got[0].ID != "fake" {
		t.Fatalf("引擎未热替换: %+v", got)
	}
	if _, err := os.Stat(dir + string(os.PathSeparator) + "dispatch.json"); err != nil {
		t.Fatalf("配置未落盘: %v", err)
	}

	// 非法（双 default）→ 400 不落盘
	req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config/dispatch",
		strings.NewReader(`{"agents":[{"id":"a","command":["x"],"default":true},{"id":"b","command":["y"],"default":true}]}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, _ := http.DefaultClient.Do(req2)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法配置应 400: %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	if got := engine.Agents(); len(got) != 2 {
		t.Fatalf("非法写不应影响引擎: %+v", got)
	}
}
