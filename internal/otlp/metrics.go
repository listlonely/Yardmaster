// OTLP/HTTP metrics 导出（M9-W5，M4 砍项回收）：与 traces 同一数据源
// （决策 trace 观察流）在导出器内聚合，周期性以**累积型（cumulative）**
// 快照推送 /v1/metrics——累积型幂等，推送失败保留聚合下次重发即可，
// 无重试队列。纯手写 JSON 零 SDK（与 otlp.go 同款取舍）。
//
// 指标最小集（counter/histogram，W5 排期定稿；更细维度留给需要时）：
//   - yardmaster.requests        sum(int, monotonic)  attr: outcome
//   - yardmaster.request.duration histogram(ms)       bounds 固定七档
//   - yardmaster.tokens          sum(int, monotonic)  attr: kind=prompt|completion
package otlp

import (
	"strconv"
	"sync"
	"time"

	"yardmaster/internal/gateway"
)

// metricsFlushEvery 是 metrics 累积快照的导出周期（包级变量，测试调短）。
var metricsFlushEvery = 30 * time.Second

// metricsBounds 是时长直方图的显式边界（毫秒；N 边界 → N+1 桶）。
var metricsBounds = []float64{100, 250, 500, 1000, 2500, 5000, 10000}

// metricsAgg 是进程内指标聚合器（累积型，永不重置——start 即进程起点）。
type metricsAgg struct {
	mu       sync.Mutex
	started  time.Time
	requests map[string]int64 // outcome → 次数
	tokens   map[string]int64 // kind(prompt|completion) → token 数
	hCount   int64
	hSum     float64
	hBuckets []int64 // len(metricsBounds)+1
}

func newMetricsAgg() *metricsAgg {
	return &metricsAgg{
		started:  time.Now(),
		requests: map[string]int64{},
		tokens:   map[string]int64{},
		hBuckets: make([]int64, len(metricsBounds)+1),
	}
}

// observe 从一条决策 trace 提取指标（Export 同源：outcome/时长/usage）。
func (m *metricsAgg) observe(tr *gateway.Trace) {
	m.mu.Lock()
	defer m.mu.Unlock()
	outcome := tr.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	m.requests[outcome]++
	if tr.Usage != nil {
		m.tokens["prompt"] += tr.Usage.Prompt
		m.tokens["completion"] += tr.Usage.Completion
	}
	m.hCount++
	hSum := float64(tr.TotalMS)
	m.hSum += hSum
	bucket := len(metricsBounds) // 超界落末桶
	for i, b := range metricsBounds {
		if hSum < b {
			bucket = i
			break
		}
	}
	m.hBuckets[bucket]++
}

// envelope 组装 OTLP metrics 请求体（累积型快照）。
func (m *metricsAgg) envelope(version string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := u64(time.Now().UnixNano())
	start := u64(m.started.UnixNano())

	var reqDPs []any
	for outcome, n := range m.requests {
		reqDPs = append(reqDPs, map[string]any{
			"asInt":         fmtInt64(n),
			"attributes":    []any{kv("outcome", outcome)},
			"timeUnixNano":  now,
			"startUnixNano": start,
		})
	}
	var tokDPs []any
	for kind, n := range m.tokens {
		tokDPs = append(tokDPs, map[string]any{
			"asInt":         fmtInt64(n),
			"attributes":    []any{kv("kind", kind)},
			"timeUnixNano":  now,
			"startUnixNano": start,
		})
	}
	if reqDPs == nil {
		reqDPs = []any{}
	}
	if tokDPs == nil {
		tokDPs = []any{}
	}
	bucketCounts := make([]string, len(m.hBuckets))
	for i, c := range m.hBuckets {
		bucketCounts[i] = fmtInt64(c)
	}
	histDP := map[string]any{
		"count":          fmtInt64(m.hCount),
		"sum":            m.hSum,
		"bucketCounts":   bucketCounts,
		"explicitBounds": metricsBounds,
		"timeUnixNano":   now,
		"startUnixNano":  start,
	}
	return map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{
					kv("service.name", "yardmaster"),
					kv("service.version", version),
				},
			},
			"scopeMetrics": []any{map[string]any{
				"scope": map[string]any{"name": "yardmaster-gateway"},
				"metrics": []any{
					map[string]any{"name": "yardmaster.requests", "sum": map[string]any{
						"dataPoints": reqDPs, "aggregationTemporality": 2, "isMonotonic": true}},
					map[string]any{"name": "yardmaster.request.duration", "histogram": map[string]any{
						"dataPoints": []any{histDP}, "aggregationTemporality": 2}},
					map[string]any{"name": "yardmaster.tokens", "sum": map[string]any{
						"dataPoints": tokDPs, "aggregationTemporality": 2, "isMonotonic": true}},
				},
			}},
		}},
	}
}

func fmtInt64(v int64) string {
	if v < 0 {
		v = 0
	}
	return strconv.FormatInt(v, 10)
}
