// M6-W4 任务模板——网关测试：templates 端点、模板 run（默认 agent/
// 超时覆盖）、缺参/多参/未知模板 400（fail-closed）。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/provider"
)

func newTemplateGateway(t *testing.T, f dispatch.DispatchFile) *httptest.Server {
	t.Helper()
	tlog := NewTraceLog(t.TempDir())
	t.Cleanup(tlog.Close)
	engine := dispatch.NewEngine(&f)
	s := &Server{
		Planner: NewPlanner([]provider.Provider{chatProvider("http://127.0.0.1:1")}, nil),
		Version: "t", Traces: tlog,
	}
	s.AttachDispatch(engine)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// templates 端点：清单 + 占位名（控制台参数输入的数据源）。
func TestDispatchTemplatesEndpoint(t *testing.T) {
	h := gwDispatchHelper(t, "ok")
	srv := newTemplateGateway(t, dispatch.DispatchFile{
		Agents:    []dispatch.AgentSpec{h},
		Templates: []dispatch.Template{{Name: "greet", Task: "向 {name} 问好", Agent: "fake"}},
	})
	gr, err := http.Get(srv.URL + "/api/dispatch/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	var r struct {
		Templates []struct {
			Name   string   `json:"name"`
			Task   string   `json:"task"`
			Agent  string   `json:"agent"`
			Params []string `json:"params"`
		} `json:"templates"`
	}
	decodeInto(t, gr.Body, &r)
	if len(r.Templates) != 1 || r.Templates[0].Name != "greet" ||
		r.Templates[0].Agent != "fake" || len(r.Templates[0].Params) != 1 || r.Templates[0].Params[0] != "name" {
		t.Fatalf("模板视图: %+v", r.Templates)
	}
}

// 模板 run：占位实例化 + 模板默认 agent；请求显式 agent 覆盖默认。
func TestDispatchRunTemplate(t *testing.T) {
	h := gwDispatchHelper(t, "ok")
	srv := newTemplateGateway(t, dispatch.DispatchFile{
		Agents:    []dispatch.AgentSpec{h},
		Templates: []dispatch.Template{{Name: "greet", Task: "向 {name} 问好", Agent: "fake"}},
	})

	resp := post(t, srv.URL, "/api/dispatch/run", `{"template":"greet","params":{"name":"世界"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var run struct {
		Session struct {
			ID    string `json:"id"`
			Agent string `json:"agent"`
			Task  string `json:"task"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &run)
	resp.Body.Close()
	if run.Session.Agent != "fake" {
		t.Fatalf("模板默认 agent 未生效: %+v", run.Session)
	}
	if run.Session.Task != "向 世界 问好" {
		t.Fatalf("占位未实例化: %q", run.Session.Task)
	}
	// 等首个会话终态再发第二次（agent 默认并发 1，避免并发闸拒绝）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + run.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Status string `json:"status"`
		}
		decodeInto(t, gr.Body, &d)
		gr.Body.Close()
		if d.Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 请求显式 agent 覆盖模板默认（此处同 id，仅验路径不报错）。
	resp2 := post(t, srv.URL, "/api/dispatch/run", `{"template":"greet","params":{"name":"x"},"agent":"fake"}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("显式覆盖 status = %d", resp2.StatusCode)
	}
}

// 模板 run fail-closed：缺参/多参/未知模板/模板引用缺失 agent → 400。
func TestDispatchRunTemplateFailClosed(t *testing.T) {
	h := gwDispatchHelper(t, "ok")
	srv := newTemplateGateway(t, dispatch.DispatchFile{
		Agents:    []dispatch.AgentSpec{h},
		Templates: []dispatch.Template{{Name: "greet", Task: "向 {name} 问好", Agent: "fake"}},
	})
	for name, body := range map[string]string{
		"缺参":    `{"template":"greet","params":{}}`,
		"多参":    `{"template":"greet","params":{"name":"a","deesc":"b"}}`,
		"未知模板":  `{"template":"ghost","params":{}}`,
		"无参自由态": `{"template":"greet"}`,
	} {
		resp := post(t, srv.URL, "/api/dispatch/run", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d（应 400）", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// 模板超时覆盖：agent 无显式超时（默认 10m），模板 timeout=1s → 会话被杀
// 归 timeout（行为级证据，非仅字段断言）。
func TestDispatchRunTemplateTimeoutOverride(t *testing.T) {
	h := gwDispatchHelper(t, "slow")
	srv := newTemplateGateway(t, dispatch.DispatchFile{
		Agents:    []dispatch.AgentSpec{h},
		Templates: []dispatch.Template{{Name: "slow", Task: "慢任务", Agent: "fake", Timeout: "1s"}},
	})
	resp := post(t, srv.URL, "/api/dispatch/run", `{"template":"slow","params":{}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var run struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &run)
	resp.Body.Close()

	deadline := time.Now().Add(10 * time.Second)
	var d struct {
		Status string `json:"status"`
		Result *struct {
			Outcome string `json:"outcome"`
		} `json:"result"`
	}
	for time.Now().Before(deadline) {
		gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + run.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
		decodeInto(t, gr.Body, &d)
		gr.Body.Close()
		if d.Status != "running" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d.Status != "failed" || d.Result == nil || d.Result.Outcome != "timeout" {
		t.Fatalf("模板超时覆盖未生效: %+v", d)
	}
}

// PUT /api/config/dispatch 写路径：templates 同走校验闸（非法模板 400 不落盘）。
func TestDispatchConfigPutTemplatesValidation(t *testing.T) {
	dir := t.TempDir()
	s := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	body := `{"agents":[{"id":"fake","command":["a"]}],"templates":[{"name":"t","task":"x","agent":"ghost"}]}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config/dispatch", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法模板 status = %d（应 400）", resp.StatusCode)
	}
	if _, err := os.Stat(dir + string(os.PathSeparator) + "dispatch.json"); err == nil {
		t.Fatal("校验失败不应落盘")
	}
}
