// 模型单价表（pricing.json）与成本报告（M2-W4）。
//
// 设计决策：成本不新增持久化——每次请求的 token 用量、服务方、上游
// 模型名已在 trace（F9）中按天落盘（保留 30 天），成本报告按
// 「单价表 × trace 聚合」在查询时计算。单价表人工维护（price set）
// + 自动同步（[2026-11-02-单价同步] M7-W1 起支持 models.dev 目录，
// 见 pricing_sync.go：同步条目带 source/synced_at 标记，人工价受
// 保护不被静默覆盖）。
//
// 计价语义：
//   - 单价单位：美元 / 1M tokens（input_per_m / output_per_m）；
//   - 价格键匹配顺序："provider/model"（精确，Served+ServedModel）
//     → 裸 "model"（ServedModel）→ 未计价（成本 0，报告计数呈现，
//     绝不静默丢弃调用次数）；
//   - 只统计 outcome=ok 且带 usage 的请求（失败请求上游未计费）；
//   - [2026-10-30-缓存扣除] M5-W2 起缓存命中单列：命中部分按
//     cached_input_per_m 计（未配置=0，即「扣除缓存命中」语义），
//     未命中部分 = prompt - cached 按全价计。
package gateway

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"yardmaster/internal/store"
)

// PriceEntry 是一个模型的单价（cached 列 M5-W2 增补，缺省 0）。
type PriceEntry struct {
	Model           string  `json:"model"`                        // "provider/model" 或裸 "model"
	InputPerM       float64 `json:"input_per_m"`                  // 美元 / 1M 输入 token
	OutputPerM      float64 `json:"output_per_m"`                 // 美元 / 1M 输出 token
	CachedInputPerM float64 `json:"cached_input_per_m,omitempty"` // 美元 / 1M 缓存命中输入 token；缺省 0（扣除语义）
	Source          string  `json:"source,omitempty"`             // 来源："models.dev"（自动同步）；空=人工（受同步保护）
	SyncedAt        string  `json:"synced_at,omitempty"`          // 最近同步时刻（RFC3339；仅同步条目）
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

// PriceUse 记录一个聚合桶内使用的价格键与单价（同一模型跨 provider
// 可能命中不同价格键——混合时逐条列出；M7-W1 增量「模型旁显示单价」）。
type PriceUse struct {
	Key   string  `json:"key"`      // 价格键（"provider/model" 或裸 "model"）
	In    float64 `json:"in_per_m"` // 输入单价 $/1M
	Out   float64 `json:"out_per_m"`
	Calls int     `json:"calls"`
}

// CostBucket 是一个聚合桶：调用次数、token 与美元成本。
type CostBucket struct {
	Calls        int        `json:"calls"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	CachedTokens int64      `json:"cached_tokens,omitempty"` // 缓存命中的输入 token（含于 InputTokens）
	Cost         float64    `json:"cost"`                    // 美元
	Prices       []PriceUse `json:"prices,omitempty"`        // 使用的价格键（首次出现序；单键=模型旁直接显示单价）
}

// CostReport 是一个时间窗的成本聚合。
type CostReport struct {
	Days      int                   `json:"days"`
	From      string                `json:"from"`                 // "2006-01-02"（含）
	To        string                `json:"to"`                   // "2006-01-02"（含）
	ByAccount map[string]CostBucket `json:"by_account"`           // key: "provider/account"
	ByModel   map[string]CostBucket `json:"by_model"`             // key: 上游模型名
	// 账号×模型二级聚合（2026-11 批3）：key "provider/account/model"——
	// 服务商页账号行「明细」弹层的数据源（按 "provider/account/" 前缀过滤）。
	ByAccountModel map[string]CostBucket `json:"by_account_model"`
	Unpriced  int                   `json:"unpriced"`             // 有用量但无单价的调用次数
	FxUsdCNY  float64               `json:"fx_usd_cny,omitempty"` // 展示换算汇率（0=仅美元；调用方自 settings 注入，不参与计费）
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
		ByAccountModel: map[string]CostBucket{},
	}
	add := func(m map[string]CostBucket, key string, calls int, in, out, cached int64, cost float64, pu PriceUse) {
		b := m[key]
		b.Calls += calls
		b.InputTokens += in
		b.OutputTokens += out
		b.CachedTokens += cached
		b.Cost += cost
		found := false
		for i := range b.Prices {
			if b.Prices[i].Key == pu.Key {
				b.Prices[i].Calls += pu.Calls
				found = true
				break
			}
		}
		if !found {
			b.Prices = append(b.Prices, pu)
		}
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
		// 缓存命中扣除：命中按 cached 单价（缺省 0），未命中按全价。
		cached := tr.Usage.Cached
		if cached > tr.Usage.Prompt { // 防御：异常上游（命中>总数）钳制
			cached = tr.Usage.Prompt
		}
		cost := float64(tr.Usage.Prompt-cached)/1e6*pe.InputPerM +
			float64(cached)/1e6*pe.CachedInputPerM +
			float64(tr.Usage.Completion)/1e6*pe.OutputPerM
		pu := PriceUse{Key: pe.Model, In: pe.InputPerM, Out: pe.OutputPerM, Calls: 1}
		add(rep.ByAccount, tr.Served, 1, tr.Usage.Prompt, tr.Usage.Completion, cached, cost, pu)
		add(rep.ByModel, tr.ServedModel, 1, tr.Usage.Prompt, tr.Usage.Completion, cached, cost, pu)
		if tr.ServedModel != "" {
			add(rep.ByAccountModel, tr.Served+"/"+tr.ServedModel, 1, tr.Usage.Prompt, tr.Usage.Completion, cached, cost, pu)
		}
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
		if p.InputPerM < 0 || p.OutputPerM < 0 || p.CachedInputPerM < 0 {
			return fmt.Errorf("单价 %s 不能为负数", p.Model)
		}
	}
	return nil
}
