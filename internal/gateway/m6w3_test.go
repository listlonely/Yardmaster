// M6-W3 事件级实时——网关 detail 视图测试：jsonl 会话详情携带 events
// （运行中可读、终态含终态事件），text 会话不携带（回退原文）。
package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"yardmaster/internal/dispatch"
)

// 提交并轮询至终态，返回会话 ID。
func dispatchRunToDone(t *testing.T, srv string, body string) string {
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
		var d struct {
			Status string `json:"status"`
		}
		decodeInto(t, gr.Body, &d)
		gr.Body.Close()
		if d.Status != "running" {
			return run.Session.ID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("会话未在时限内终态")
	return ""
}

// jsonl agent 详情：output_mode=events（事件视图数据源）。
func TestDispatchDetailEventsJSONL(t *testing.T) {
	h := gwDispatchHelper(t, "jsonl")
	h.Output = "jsonl"
	srv, _ := newDispatchGateway(t, h)

	id := dispatchRunToDone(t, srv.URL, `{"task":"x","agent":"fake"}`)
	gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	var d struct {
		OutputMode string           `json:"output_mode"`
		Output     string           `json:"output"`
		Events     []dispatch.Event `json:"events"`
	}
	decodeInto(t, gr.Body, &d)
	if d.OutputMode != "jsonl" {
		t.Fatalf("output_mode = %q", d.OutputMode)
	}
	if len(d.Events) != 2 {
		t.Fatalf("事件条数 = %d: %+v", len(d.Events), d.Events)
	}
	if d.Events[0].Kind != dispatch.EventInit || d.Events[0].Subtype != "init" {
		t.Fatalf("init 事件: %+v", d.Events[0])
	}
	final := d.Events[1]
	if final.Kind != dispatch.EventResult || !final.Final || final.Text != "事件终态 ok" {
		t.Fatalf("终态事件: %+v", final)
	}
}

// text agent 详情：无 events 键（视图回退原文 output）。
func TestDispatchDetailTextNoEvents(t *testing.T) {
	srv, _ := newDispatchGateway(t, gwDispatchHelper(t, "ok"))
	id := dispatchRunToDone(t, srv.URL, `{"task":"x","agent":"fake"}`)
	gr, err := http.Get(srv.URL + "/api/dispatch/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	var raw map[string]json.RawMessage
	decodeInto(t, gr.Body, &raw)
	if _, has := raw["events"]; has {
		t.Fatal("text 会话不应返回 events")
	}
}
