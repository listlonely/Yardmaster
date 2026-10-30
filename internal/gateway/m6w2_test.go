// M6-W2 持久化 e2e：派发会话终态落盘 → 模拟重启（新引擎从文件恢复）
// → 控制台 API 历史跨重启可见（验收：重启 serve 后派发页仍见重启前
// 会话；httptest 断言落盘与恢复）。
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/provider"
)

// newPersistedDispatchGateway 构造挂载 SessionStore 的派发网关（app.go
// 装配顺序同构：store → AttachStore → RestoreSessions → AttachDispatch）。
func newPersistedDispatchGateway(t *testing.T, store *dispatch.SessionStore, agents ...dispatch.AgentSpec) *httptest.Server {
	t.Helper()
	engine := dispatch.NewEngine(&dispatch.DispatchFile{Agents: agents})
	if store != nil {
		engine.AttachStore(store)
		engine.RestoreSessions(store.Recent(dispatch.SessionRingCap))
	}
	s := &Server{
		Planner: NewPlanner([]provider.Provider{chatProvider("http://127.0.0.1:1")}, nil),
		Version: "t",
	}
	s.AttachDispatch(engine)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestDispatchSessionsSurviveRestart(t *testing.T) {
	agents := []dispatch.AgentSpec{gwDispatchHelper(t, "ok")}
	path := filepath.Join(t.TempDir(), "trace", "dispatch-sessions.jsonl")
	store := dispatch.NewSessionStore(path)

	// 第一台网关：派发一发并等终态（落盘发生在终态回调）。
	srv1 := newPersistedDispatchGateway(t, store, agents...)
	resp := post(t, srv1.URL, "/api/dispatch/run", `{"task":"算一下 6*7","agent":"fake"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d", resp.StatusCode)
	}
	var run struct {
		Session struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &run)
	if run.Session.ID == "" {
		t.Fatal("run 未返回会话")
	}
	// 轮询至终态（落盘先于 close(finished)，Wait 语义经 API 同样成立）。
	// 详情 API 字段为顶层平铺（dispatchSessionDetail）。
	deadline := time.Now().Add(30 * time.Second)
	for {
		var detail struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal(getBody(t, srv1.URL+"/api/dispatch/sessions/"+run.Session.ID), &detail); err != nil {
			t.Fatalf("详情解析: %v", err)
		}
		if detail.Status == "done" || detail.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("会话未在期限内终态: %q", detail.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 第二台网关（模拟重启）：新引擎从同一文件恢复 → 列表与详情可见。
	srv2 := newPersistedDispatchGateway(t, store, agents...)
	var list struct {
		Sessions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"sessions"`
	}
	decodeInto(t, bytes.NewReader(getBody(t, srv2.URL+"/api/dispatch/sessions")), &list)
	if len(list.Sessions) == 0 {
		t.Fatal("重启后派发页应可见历史会话（验收 §9.7）")
	}
	found := false
	for _, s := range list.Sessions {
		if s.ID == run.Session.ID {
			found = true
			if s.Status != "done" {
				t.Fatalf("恢复终态 = %s", s.Status)
			}
		}
	}
	if !found {
		t.Fatalf("重启后缺失会话 %s: %+v", run.Session.ID, list.Sessions)
	}
	// 详情 output 回退 Result.Output（恢复会话无 live 缓冲）。
	var detail struct {
		Output string `json:"output"`
		Status string `json:"status"`
	}
	decodeInto(t, bytes.NewReader(getBody(t, srv2.URL+"/api/dispatch/sessions/"+run.Session.ID)), &detail)
	if detail.Status != "done" || detail.Output == "" {
		t.Fatalf("重启后详情: status=%s output=%q", detail.Status, detail.Output)
	}
}
