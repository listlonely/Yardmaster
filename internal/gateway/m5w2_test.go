// M5-W2 测试：usage 缓存细分捕获与成本扣除（httptest 经真实中继路径）、
// Responses 工具参数流式容错（jsonWhole 边界：delta 无 done / done 无参
// 数时以累积兜底）。
package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/provider"
)

// usageScanner 三协议缓存字段：openai（含于 prompt）、gemini（含于
// prompt）、anthropic（不含于 input_tokens，折叠进 prompt）。
func TestUsageScannerCachedFields(t *testing.T) {
	sc := &usageScanner{}
	_, _ = sc.Write([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":60}}}`))
	u := sc.usageOf()
	if u == nil || u.Prompt != 100 || u.Cached != 60 {
		t.Fatalf("openai 缓存提取异常: %+v", u)
	}

	sc = &usageScanner{}
	_, _ = sc.Write([]byte(`{"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":7,"cachedContentTokenCount":30}}`))
	u = sc.usageOf()
	if u == nil || u.Prompt != 50 || u.Cached != 30 {
		t.Fatalf("gemini 缓存提取异常: %+v", u)
	}

	sc = &usageScanner{}
	_, _ = sc.Write([]byte(`{"usage":{"input_tokens":40,"output_tokens":7,"cache_read_input_tokens":25}}`))
	u = sc.usageOf()
	if u == nil || u.Prompt != 65 || u.Cached != 25 {
		t.Fatalf("anthropic 缓存折叠异常（应 Prompt=input+cache_read）: %+v", u)
	}
}

// 含缓存命中的真实响应经完整中继路径进入成本报告：命中部分按 cached
// 单价计、未命中按全价计（验收：成本报告中体现扣除）。
func TestCostReportDeductsCachedTokensEndToEnd(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":500000,"prompt_tokens_details":{"cached_tokens":600000}}}`
	})
	tlog := NewTraceLog(t.TempDir())
	defer tlog.Close()
	s := &Server{Planner: NewPlanner([]provider.Provider{chatProvider(up.srv.URL)}, nil), Version: "t", Traces: tlog}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	trs := tlog.Recent(10, "", "")
	if len(trs) == 0 || trs[len(trs)-1].Usage == nil {
		t.Fatal("trace 未记录 usage")
	}
	u := trs[len(trs)-1].Usage
	if u.Prompt != 1_000_000 || u.Cached != 600_000 {
		t.Fatalf("trace usage 异常: %+v", u)
	}

	prices := []PriceEntry{{
		Model: "prov/up-m", InputPerM: 0.30, OutputPerM: 1.00, CachedInputPerM: 0.03,
	}}
	rep := ComputeCost(trs, prices, 1, time.Now())
	acct := rep.ByAccount["prov/one"]
	if acct.Calls != 1 || acct.CachedTokens != 600_000 {
		t.Fatalf("缓存桶不符: %+v", acct)
	}
	// 成本 = (1M-0.6M)*0.30 + 0.6M*0.03 + 0.5M*1.00 = 0.12+0.018+0.50
	if acct.Cost < 0.6379 || acct.Cost > 0.6381 {
		t.Fatalf("缓存扣除后成本不符: %v", acct.Cost)
	}
	// 缺省 cached 单价=0：命中即全额扣除
	rep0 := ComputeCost(trs, []PriceEntry{{Model: "prov/up-m", InputPerM: 0.30, OutputPerM: 1.00}}, 1, time.Now())
	c0 := rep0.ByAccount["prov/one"].Cost
	if c0 < 0.6199 || c0 > 0.6201 { // 0.4M*0.30 + 0.5M*1.00 = 0.62
		t.Fatalf("缺省扣除语义不符: %v", c0)
	}
}

// jsonWhole 容错一：参数 delta 到达但无 output_item.done——不产出半截
// 工具事件，流仍完整收尾（[DONE] 存在、文本保留）。
func TestResponsesToolArgsDeltaWithoutDone(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		sse := "event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","item_id":"msg_0","delta":"ok"}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"a\":"}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"1}"}` + "\n\n" +
			"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"
		return http.StatusOK, "text/event-stream", sse
	})
	gw := newGateway(t, responsesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("无 done 的参数流未正常收尾:\n%s", body)
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("半截工具调用不应外泄:\n%s", body)
	}
	text, _ := chatSSEToText(t, body)
	if text != "ok" {
		t.Fatalf("文本丢失: %q", text)
	}
}

// jsonWhole 容错二：output_item.done 的 item 缺 arguments——以 delta
// 累积兜底，工具调用仍完整（保证输出完整 JSON）。
func TestResponsesToolDoneWithoutArgsFallsBack(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		sse := "event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":"}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" + `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"北京\"}"}` + "\n\n" +
			"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather"}}` + "\n\n" +
			"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"
		return http.StatusOK, "text/event-stream", sse
	})
	gw := newGateway(t, responsesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	var args string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var cc ChatChunk
		if err := json.Unmarshal([]byte(line[6:]), &cc); err != nil {
			continue
		}
		for _, ch := range cc.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
		}
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("累积兜底参数不符: %q\n%s", args, body)
	}
}
