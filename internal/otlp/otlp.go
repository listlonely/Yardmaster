// OTLP/HTTP traces 导出（M4-W5）：纯手写 JSON，无 OTel SDK 依赖
// （零依赖铁律，Magpie otlp 包同款取舍）。数据源是网关的决策 trace
// （gateway.Trace），而非侵入推理路径：每个请求一个 root span，
// 每次候选尝试一个 child span。导出失败只降级（丢弃重试有界），绝不
// 阻塞主流程；默认关闭。
package otlp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"yardmaster/internal/gateway"
)

const (
	batchSize   = 32          // 攒批上限
	flushEvery  = 5 * time.Second
	maxRetries  = 3
	maxBatchMem = 128 << 20 // 单批字节上限（防异常大批撑爆内存）
)

// Exporter 把 gateway.Trace 异步导出到 OTLP-HTTP 兼容后端
// （Jaeger/Tempo/Langfuse 等）。
type Exporter struct {
	endpoint string // 剥掉尾部 /v1/traces 后的基础端点
	headers  map[string]string
	client   *http.Client
	ch       chan *gateway.Trace
	done     chan struct{}
	wg       sync.WaitGroup
}

// NewExporter 创建并启动导出器；endpoint 为基础 URL（自动追加
// /v1/traces）。Close 停止并冲刷剩余批次。
func NewExporter(endpoint string, headers map[string]string) *Exporter {
	e := &Exporter{
		endpoint: strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1/traces"),
		headers:  headers,
		client:   &http.Client{Timeout: 10 * time.Second},
		ch:       make(chan *gateway.Trace, 256),
		done:     make(chan struct{}),
	}
	e.wg.Add(1)
	go e.loop()
	return e
}

// Export 投递一条 trace（队列满丢弃并计数——观测永不阻塞主流程）。
func (e *Exporter) Export(tr gateway.Trace) {
	select {
	case e.ch <- &tr:
	default:
		slog.Warn("otlp 导出队列满，丢弃一条 trace")
	}
}

// Close 停止后台循环并冲刷剩余。
func (e *Exporter) Close() {
	close(e.done)
	e.wg.Wait()
}

func (e *Exporter) loop() {
	defer e.wg.Done()
	batch := make([]*gateway.Trace, 0, batchSize)
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.send(batch)
		batch = batch[:0]
	}
	for {
		select {
		case tr := <-e.ch:
			batch = append(batch, tr)
			if len(batch) >= batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-e.done:
			for {
				select {
				case tr := <-e.ch:
					batch = append(batch, tr)
					if len(batch) >= batchSize {
						e.send(batch)
						batch = batch[:0]
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// send POST 一批 traces（429/5xx 按 Retry-After 指数退避，最多 3 次）。
func (e *Exporter) send(batch []*gateway.Trace) {
	body, err := json.Marshal(envelope(batch))
	if err != nil || len(body) > maxBatchMem {
		slog.Warn("otlp 批次编码失败或超限，丢弃", "size", len(body), "err", err)
		return
	}
	url := e.endpoint + "/v1/traces"
	backoff := time.Second
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, rerr := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if rerr != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range e.headers {
			req.Header.Set(k, v)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			slog.Warn("otlp 导出失败（降级重试）", "err", err, "attempt", attempt)
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return // 成功（partialSuccess 中的丢弃计数不阻塞，日志可查）
			case resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusBadGateway ||
				resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusGatewayTimeout:
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if d, perr := time.ParseDuration(ra + "s"); perr == nil {
						backoff = d
					}
				}
			default:
				slog.Warn("otlp 后端拒绝，丢弃本批", "status", resp.StatusCode)
				return
			}
		}
		if attempt == maxRetries {
			break
		}
		// 退避期间不响应 done：Close 也要把在途批次送完（重试有界，
		// 上限 1+2+4s + 请求超时，关机路径可接受）。
		time.Sleep(backoff)
		backoff *= 2
	}
	slog.Warn("otlp 导出重试耗尽，丢弃本批", "batch", len(batch))
}

// ---------- Trace → OTLP span 映射 ----------

// envelope 组装 OTLP/HTTP JSON 请求体（resourceSpans/scopeSpans/spans）。
func envelope(batch []*gateway.Trace) map[string]any {
	var spans []map[string]any
	for i := range batch {
		spans = append(spans, spansOf(batch[i])...)
	}
	if spans == nil {
		spans = []map[string]any{}
	}
	return map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{
					kv("service.name", "yardmaster"),
					kv("service.version", "v0.4.0"),
				},
			},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "yardmaster-gateway"},
				"spans": spans,
			}},
		}},
	}
}

// spansOf 把一条 trace 转为 root span + 候选尝试 child spans。
func spansOf(tr *gateway.Trace) []map[string]any {
	traceID := hexID(tr.ID, 32)
	rootID := hexID(tr.ID+"#root", 16)
	start := parseTime(tr.Time)
	attrs := []any{
		kv("gen_ai.operation.name", "chat"),
		kv("gen_ai.system", "yardmaster"),
	}
	if tr.RequestModel != "" {
		attrs = append(attrs, kv("gen_ai.request.model", tr.RequestModel))
	}
	if tr.ServedModel != "" {
		attrs = append(attrs, kv("gen_ai.response.model", tr.ServedModel))
	}
	if tr.ClientProto != "" || tr.UpstreamProto != "" {
		attrs = append(attrs, kv("yardmaster.client_proto", orEmpty(tr.ClientProto)),
			kv("yardmaster.upstream_proto", orEmpty(tr.UpstreamProto)))
	}
	if tr.Intent != "" {
		attrs = append(attrs, kv("yardmaster.intent", tr.Intent))
	}
	if tr.Rule != "" {
		attrs = append(attrs, kv("yardmaster.rule", tr.Rule))
	}
	if tr.Caller != "" {
		attrs = append(attrs, kv("yardmaster.caller", tr.Caller))
	}
	if tr.Usage != nil {
		attrs = append(attrs, kvInt("gen_ai.usage.input_tokens", tr.Usage.Prompt),
			kvInt("gen_ai.usage.output_tokens", tr.Usage.Completion))
	}
	statusCode := 2 // ERROR
	if tr.Outcome == "ok" {
		statusCode = 1 // UNSET（成功语义）
	}
	root := map[string]any{
		"traceId": traceID, "spanId": rootID,
		"name": "gateway " + orEmpty(tr.Target),
		"kind": 2, // SERVER
		"startTimeUnixNano": u64(start.UnixNano()),
		"endTimeUnixNano":   u64(start.UnixNano() + tr.TotalMS*int64(time.Millisecond)),
		"status":            map[string]any{"code": statusCode},
		"attributes":        attrs,
	}
	out := []map[string]any{root}
	for i, c := range tr.Candidates {
		child := map[string]any{
			"traceId": traceID, "spanId": hexID(fmt.Sprintf("%s#c%d", tr.ID, i), 16),
			"parentSpanId": rootID,
			"name":         "try " + c.Provider + "/" + c.Account + " " + c.Model,
			"kind":         3, // CLIENT
			"startTimeUnixNano": u64(start.UnixNano()),
			"endTimeUnixNano":   u64(start.UnixNano() + c.DurMS*int64(time.Millisecond)),
			"status": map[string]any{"code": candStatus(c.Result)},
			"attributes": []any{
				kv("yardmaster.candidate.result", c.Result),
				kv("yardmaster.provider", c.Provider),
				kv("yardmaster.account", c.Account),
			},
		}
		if c.Status != 0 {
			child["attributes"] = append(child["attributes"].([]any), kvInt("http.response.status_code", int64(c.Status)))
		}
		if c.Err != "" {
			child["status"].(map[string]any)["message"] = c.Err
		}
		out = append(out, child)
	}
	return out
}

func candStatus(result string) int {
	if result == gateway.CandOK {
		return 1
	}
	return 2
}

// ---------- 小工具 ----------

// hexID 从任意字符串派生合法 OTLP hex ID（traceId 32 字符 / spanId 16）。
func hexID(seed string, n int) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:n]
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now()
	}
	return t
}

func u64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

func orEmpty(s string) string { return s }

func kv(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}

func kvInt(k string, v int64) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"intValue": fmt.Sprint(v)}}
}
