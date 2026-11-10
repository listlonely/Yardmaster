// M6-W5 结果消费与重派——网关测试：失败会话一键重派（新会话新候选链）、
// 非 failed 拒绝、结果导出（text/jsonl 原文尾部 + 文件名后缀）。
package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 提交并轮询到指定终态（非 running 即返回详情）。
func dispatchRunToStatus(t *testing.T, srv, body string) (string, map[string]any) {
	t.Helper()
	resp := post(t, srv, "/api/dispatch/run", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d", resp.StatusCode)
	}
	var run struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &run)
	resp.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		gr, err := http.Get(srv + "/api/dispatch/sessions/" + run.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		decodeInto(t, gr.Body, &d)
		gr.Body.Close()
		if d["status"] != "running" {
			return run.Session.ID, d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("会话未在时限内终态")
	return "", nil
}

// 失败会话一键重派：新会话同 agent/同任务摘要；原会话原样留档；
// 新会话作为独立候选链最终也终态（此载体下同样 failed）。
func TestDispatchRedispatchFailed(t *testing.T) {
	srv, _ := newDispatchGateway(t, gwDispatchHelper(t, "fail"))
	oldID, oldDetail := dispatchRunToStatus(t, srv.URL, `{"task":"失败任务","agent":"fake"}`)
	if oldDetail["status"] != "failed" {
		t.Fatalf("载体会话应 failed: %v", oldDetail["status"])
	}

	resp := post(t, srv.URL, "/api/dispatch/sessions/"+oldID+"/redispatch", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redispatch status = %d", resp.StatusCode)
	}
	var r struct {
		OK              bool   `json:"ok"`
		RedispatchedFrom string `json:"redispatched_from"`
		Session         struct {
			ID    string `json:"id"`
			Agent string `json:"agent"`
			Task  string `json:"task"`
		} `json:"session"`
	}
	decodeInto(t, resp.Body, &r)
	resp.Body.Close()
	if !r.OK || r.RedispatchedFrom != oldID || r.Session.ID == oldID ||
		r.Session.Agent != "fake" || r.Session.Task != "失败任务" {
		t.Fatalf("重派响应异常: %+v", r)
	}

	// 原会话留档不变 + 新会话独立终态（两条会话并存）。
	gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + r.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var nd map[string]any
	decodeInto(t, gr.Body, &nd)
	gr.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for nd["status"] == "running" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		gr, err = http.Get(srv.URL + "/api/dispatch/sessions/" + r.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
		nd = nil
		decodeInto(t, gr.Body, &nd)
		gr.Body.Close()
	}
	if nd["status"] != "failed" {
		t.Fatalf("重派会话应独立终态: %v", nd["status"])
	}
	gr2, err := http.Get(srv.URL + "/api/dispatch/sessions/" + oldID)
	if err != nil {
		t.Fatal(err)
	}
	var od map[string]any
	decodeInto(t, gr2.Body, &od)
	gr2.Body.Close()
	if od["status"] != "failed" || od["id"] != oldID {
		t.Fatalf("原会话应原样留档: %v", od["status"])
	}
}

// 非 failed 拒绝重派：done（成功不重跑）与 running（进行中不可重派）→ 400。
func TestDispatchRedispatchRejectsNonFailed(t *testing.T) {
	srv, _ := newDispatchGateway(t, gwDispatchHelper(t, "ok"))
	doneID, d := dispatchRunToStatus(t, srv.URL, `{"task":"x","agent":"fake"}`)
	if d["status"] != "done" {
		t.Fatalf("载体会话应 done: %v", d["status"])
	}
	resp := post(t, srv.URL, "/api/dispatch/sessions/"+doneID+"/redispatch", "")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(readBody(resp), "仅失败会话") {
		t.Fatalf("done 会话应 400 拒绝: %d", resp.StatusCode)
	}

	// running：slow 假 Agent 提交后立即重派 → 400；随后取消收尾。
	slowSrv, _ := newDispatchGateway(t, gwDispatchHelper(t, "slow"))
	rresp := post(t, slowSrv.URL, "/api/dispatch/run", `{"task":"慢","agent":"fake"}`)
	var run struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decodeInto(t, rresp.Body, &run)
	rresp.Body.Close()
	resp2 := post(t, slowSrv.URL, "/api/dispatch/sessions/"+run.Session.ID+"/redispatch", "")
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("running 会话应 400 拒绝: %d", resp2.StatusCode)
	}
	post(t, slowSrv.URL, "/api/dispatch/sessions/"+run.Session.ID+"/cancel", "")
}

// 结果导出：text → .txt；jsonl → .jsonl；内容为留存原文尾部；未知会话 404。
func TestDispatchSessionExport(t *testing.T) {
	srv, _ := newDispatchGateway(t, gwDispatchHelper(t, "ok"))
	id, _ := dispatchRunToStatus(t, srv.URL, `{"task":"x","agent":"fake"}`)
	gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + id + "/export")
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	b, _ := io.ReadAll(gr.Body)
	if !strings.Contains(string(b), "派发结果 ok") {
		t.Fatalf("导出内容缺失: %q", string(b))
	}
	if cd := gr.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="dispatch-`+id+`.txt"`) {
		t.Fatalf("导出文件名: %q", cd)
	}

	// jsonl agent → .jsonl 后缀。
	jh := gwDispatchHelper(t, "jsonl")
	jh.Output = "jsonl"
	srv2, _ := newDispatchGateway(t, jh)
	jid, _ := dispatchRunToStatus(t, srv2.URL, `{"task":"x","agent":"fake"}`)
	gr2, err := http.Get(srv2.URL + "/api/dispatch/sessions/" + jid + "/export")
	if err != nil {
		t.Fatal(err)
	}
	defer gr2.Body.Close()
	if cd := gr2.Header.Get("Content-Disposition"); !strings.Contains(cd, ".jsonl") {
		t.Fatalf("jsonl 导出文件名: %q", cd)
	}
	jb, _ := io.ReadAll(gr2.Body)
	if !strings.Contains(string(jb), `"type":"result"`) {
		t.Fatalf("jsonl 导出内容: %q", string(jb))
	}

	// 未知会话 404。
	gr3, _ := http.Get(srv.URL + "/api/dispatch/sessions/d-none/export")
	if gr3.StatusCode != http.StatusNotFound {
		t.Fatalf("未知会话导出应 404: %d", gr3.StatusCode)
	}
	gr3.Body.Close()
}

// readBody 读取并关闭响应体（辅助断言错误文案）。
func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
