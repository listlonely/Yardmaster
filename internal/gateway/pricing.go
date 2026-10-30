// 模型单价表（pricing.json）与成本报告（M2-W4）。
//
// 设计决策：成本不新增持久化——每次请求的 token 用量、服务方、上游
// 模型名已在 trace（F9）中按天落盘（保留 30 天），成本报告按
// 「单价表 × trace 聚合」在查询时计算。单价表为用户手工维护
// （models.dev 集成为后续里程碑范围）。
//
// 计价语义：
//   - 单价单位：美元 / 1M tokens（input_per_m / output_per_m）；
//   - 价格键匹配顺序："provider/model"（精确，Served+ServedModel）
//     → 裸 "model"（ServedModel）→ 未计价（成本 0，报告计数呈现，
//     绝不静默丢弃调用次数）；
//   - 只统计 outcome=ok 且带 usage 的请求（失败请求上游未计费）。
package gateway

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"yardmaster/internal/store"
)

// PriceEntry 是一个模型的三元单价。
type PriceEntry struct {
	Model      string  `json:"model"`        // "provider/model" 或裸 "model"
	InputPerM  float64 `json:"input_per_m"`  // 美元 / 1M 输入 token
	OutputPerM float64 `json:"output_per_m"` // 美元 / 1M 输出 token
}

// PricingFile 是 pricing.json 的文件形态。
type PricingFile struct {
	Prices []PriceEntry `json:"prices"`
}

// LoadPricingFile 读取单价表；不存在返回 os.ErrNotExist。
func LoadPricingFile(path string) (PricingFile, error) {
	var f PricingFile
	if err := store.LoadJSON(path, &f); err != nil {
		return f, err
	}
	return f, nil
}

// SavePricingFile 原子写入单价表。
func SavePricingFile(path string, f PricingFile) error { return store.SaveJSON(path, f) }

// priceOf 按匹配顺序返回单价："provider/model"（provider 取 Served 的
// 账号段之前的部分）→ 裸 "model" → nil。
func priceOf(prices []PriceEntry, providerID, model string) *PriceEntry {
	for i := range prices {
		if prices[i].Model == providerID+"/"+model {
			return &prices[i]
		}
	}
	for i := range prices {
		if prices[i].Model == model {
			return &prices[i]
		}
	}
	return nil
}

// CostBucket 是一个聚合桶：调用次数、token 与美元成本。
type CostBucket struct {
	Calls        int     `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"` // 美元
}

// CostReport 是一个时间窗的成本聚合。
type CostReport struct {
	Days      int                   `json:"days"`
	From      string                `json:"from"`       // "2006-01-02"（含）
	To        string                `json:"to"`         // "2006-01-02"（含）
	ByAccount map[string]CostBucket `json:"by_account"` // key: "provider/account"
	ByModel   map[string]CostBucket `json:"by_model"`   // key: 上游模型名
	Unpriced  int                   `json:"unpriced"`   // 有用量但无单价的调用次数
}

// ComputeCost 从 trace 集合计算最近 days 天（含当日）的成本报告。
// traces 允许任意顺序（LoadTraceFiles 的倒序亦可）。
func ComputeCost(traces []Trace, prices []PriceEntry, days int, now time.Time) *CostReport {
	if days <= 0 {
		days = 7
	}
	to := now
	from := to.AddDate(0, 0, -(days - 1))
	fromDay := from.Format("2006-01-02")
	toDay := to.Format("2006-01-02")
	rep := &CostReport{
		Days:      days,
		From:      fromDay,
		To:        toDay,
		ByAccount: map[string]CostBucket{},
		ByModel:   map[string]CostBucket{},
	}
	add := func(m map[string]CostBucket, key string, calls int, in, out int64, cost float64) {
		b := m[key]
		b.Calls += calls
		b.InputTokens += in
		b.OutputTokens += out
		b.Cost += cost
		m[key] = b
	}
	for _, tr := range traces {
		if tr.Outcome != "ok" || tr.Usage == nil || tr.Served == "" {
			continue
		}
		day := tr.Time
		if t, err := time.Parse(time.RFC3339, tr.Time); err == nil {
			day = t.Format("2006-01-02")
		}
		if day < fromDay || day > toDay {
			continue
		}
		providerID, _, _ := strings.Cut(tr.Served, "/")
		pe := priceOf(prices, providerID, tr.ServedModel)
		if pe == nil {
			rep.Unpriced++
			continue
		}
		cost := float64(tr.Usage.Prompt)/1e6*pe.InputPerM +
			float64(tr.Usage.Completion)/1e6*pe.OutputPerM
		add(rep.ByAccount, tr.Served, 1, tr.Usage.Prompt, tr.Usage.Completion, cost)
		add(rep.ByModel, tr.ServedModel, 1, tr.Usage.Prompt, tr.Usage.Completion, cost)
	}
	return rep
}

// SortedCostKeys 返回按成本降序的聚合键（展示用）。
func SortedCostKeys(m map[string]CostBucket) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].Cost > m[keys[j]].Cost })
	return keys
}

// ValidatePricing 校验单价表（管理 API 写入口）：model 非空唯一、单价 ≥0。
func ValidatePricing(f PricingFile) error {
	seen := map[string]bool{}
	for _, p := range f.Prices {
		if p.Model == "" {
			return fmt.Errorf("单价条目 model 不能为空")
		}
		if seen[p.Model] {
			return fmt.Errorf("单价条目 model 重复: %s", p.Model)
		}
		seen[p.Model] = true
		if p.InputPerM < 0 || p.OutputPerM < 0 {
			return fmt.Errorf("单价 %s 不能为负数", p.Model)
		}
	}
	return nil
}
