// M4-W5 测试：OTLP/HTTP JSON 导出（手写 envelope 形状、批量、关闭冲刷、
// 观察者联动）。全部 httptest，不碰网络。
package otlp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yardmaster/internal/gateway"
)

func sampleTrace() *gateway.Trace {
	return &gateway.Trace{
		ID: "t-1234-abcd", Time: "2026-10-30T12:00:00+08:00",
		Caller: "claude-code", RequestModel: "prov/up-m", Target: "prov/up-m",
		ClientProto: "anthropic-messages", UpstreamProto: "openai-chat",
		Intent: "code", Outcome: "ok", HTTPStatus: 200, TotalMS: 120,
		Usage: &gateway.TraceUsage{Prompt: 10, Completion: 5},
		Candidates: []gateway.TraceCandidate{
			{Provider: "prov", Account: "one", Model: "up-m", Result: gateway.CandOK, Status: 200, DurMS: 120},
		},
	}
}

// 导出的 envelope 形状：resourceSpans/scopeSpans、root+child span、
// OTel 语义约定属性（gen_ai.*）、traceId/spanId 为合法 hex。
func TestExporterEnvelopeShape(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if r.Header.Get("Authorization") != "Bearer lk-test" {
			t.Errorf("自定义头缺失")
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	exp := NewExporter(srv.URL, map[string]string{"Authorization": "Bearer lk-test"})
	exp.Export(*sampleTrace())
	exp.Close() // 关闭冲刷剩余

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("未收到导出")
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &env); err != nil {
		t.Fatalf("envelope 非法 JSON: %v", err)
	}
	raw := bodies[0]
	for _, want := range []string{
		`"service.name"`, `"yardmaster"`,
		`"gen_ai.request.model"`, `"gen_ai.usage.input_tokens"`,
		`"yardmaster.client_proto"`, `"anthropic-messages"`,
		`"parentSpanId"`, `"try prov/one up-m"`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("envelope 缺少 %q:\n%s", want, raw)
		}
	}
	// traceId/spanId 合法 hex（32/16 字符）
	var spans []map[string]any
	collectSpans(t, env, &spans)
	if len(spans) != 2 { // root + 1 候选
		t.Fatalf("span 数 = %d（root+候选应为 2）", len(spans))
	}
	for _, s := range spans {
		tid, _ := s["traceId"].(string)
		sid, _ := s["spanId"].(string)
		if len(tid) != 32 || len(sid) != 16 {
			t.Fatalf("ID 长度非法: trace=%q span=%q", tid, sid)
		}
	}
	if spans[1]["parentSpanId"] != spans[0]["spanId"] {
		t.Fatal("child 未挂到 root")
	}
}

func collectSpans(t *testing.T, env map[string]any, out *[]map[string]any) {
	t.Helper()
	rss, _ := env["resourceSpans"].([]any)
	for _, rs := range rss {
		sss, _ := rs.(map[string]any)["scopeSpans"].([]any)
		for _, ss := range sss {
			spans, _ := ss.(map[string]any)["spans"].([]any)
			for _, s := range spans {
				m, _ := s.(map[string]any)
				*out = append(*out, m)
			}
		}
	}
}

// 5 秒定时冲刷（不等到批满）。
func TestExporterFlushesOnInterval(t *testing.T) {
	got := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
	}))
	defer srv.Close()

	exp := NewExporter(srv.URL, nil)
	defer exp.Close()
	exp.Export(*sampleTrace())
	select {
	case <-got:
	case <-time.After(7 * time.Second):
		t.Fatal("定时冲刷未发生")
	}
}

// 后端 5xx：指数退避重试后成功（降级不丢批）。
func TestExporterRetriesOnServerErrors(t *testing.T) {
	var n int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur <= 2 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	exp := NewExporter(srv.URL, nil)
	exp.Export(*sampleTrace())
	exp.Close()
	mu.Lock()
	defer mu.Unlock()
	if n < 3 {
		t.Fatalf("应重试到第 3 次，实际 %d", n)
	}
}

// 观察者联动：TraceLog.Record → OnTrace → Exporter（集成链路）。
func TestTraceLogObserverFeedsExporter(t *testing.T) {
	got := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	exp := NewExporter(srv.URL, nil)
	defer exp.Close()
	tlog := gateway.NewTraceLog(t.TempDir())
	defer tlog.Close()
	tlog.OnTrace = exp.Export

	tlog.Record(*sampleTrace())
	select {
	case raw := <-got:
		if !strings.Contains(raw, "gen_ai.request.model") {
			t.Fatalf("导出内容异常:\n%s", raw)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("观察者链路未导出")
	}
}
