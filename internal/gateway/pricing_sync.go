// models.dev 单价自动填充（M7-W1）[2026-11-02-单价同步]。
//
// 目录来源：models.dev 公开聚合目录（api.json），形状为
// {provider_id: {models: {model_id: {cost: {input, output, cache_read},
// …}}}}——成本单位与本项目一致（美元 / 1M tokens，实测核实）。
//
// 容错纪律（读不懂=未知）：单条模型缺成本/成本非法 → 计数留痕不填充；
// 数字字段兼容 number 与 string 两种 JSON 形态；坏 provider 条目跳过。
//
// 合并纪律（用户值优先）：价格键=裸 model id（priceOf 的通用兜底键，
// 精确键 "provider/model" 仍由人工维护且优先级更高）；已存在的
// models.dev 条目刷新；人工条目（source 空）受保护，-force 才覆盖；
// 同名键目录内多 provider 冲突 → 按字典序首个 provider 保留、余者
// 留痕（确定性，不随机取胜者）。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ModelsDevURL 是 models.dev 公开目录地址（-from 可覆盖为 URL 或本地
// 文件路径——离线/测试用途）。
const ModelsDevURL = "https://models.dev/api.json"

// flexFloat 兼容 JSON number 与 "3.5" 字符串两种形态（目录中两类都有）。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	var v float64
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return fmt.Errorf("非数字: %s", s)
	}
	*f = flexFloat(v)
	return nil
}

// modelsDevModel 是目录中一个模型的规整形态（成本已换算为 float64）。
type modelsDevModel struct {
	ID        string
	Name      string
	Input     float64 // 美元 / 1M 输入
	Output    float64 // 美元 / 1M 输出
	CacheRead float64 // 美元 / 1M 缓存命中（缺省 0=扣除语义）
	costOK    bool    // cost.input 与 output 齐备且 ≥0
}

// modelsDevCost 捕获 cost 对象（字段名与 models.dev 实测形状对齐）。
type modelsDevCost struct {
	Input     *flexFloat `json:"input"`
	Output    *flexFloat `json:"output"`
	CacheRead *flexFloat `json:"cache_read"`
}

type modelsDevRawModel struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Cost *modelsDevCost `json:"cost"`
}

type modelsDevRawProvider struct {
	ID     string                       `json:"id"`
	Models map[string]modelsDevRawModel `json:"models"`
}

// ModelsDevCatalog 是解析后的目录：provider id → 模型 id → 条目
// （仅含 costOK 的模型；无成本的在 NoPrice 计数留痕）。
type ModelsDevCatalog struct {
	Providers map[string]map[string]modelsDevModel // provider id → model id → 条目
	Notes     []string                             // 解析留痕（坏条目，读不懂=未知）
	NoPrice   int                                  // 无有效成本的模型数（未填充，留痕）
}

// ParseModelsDev 解析目录字节；顶层非对象或整体不可解析为错误（调用方
// 决定中止），单条目问题容错跳过并记 Notes。
func ParseModelsDev(data []byte) (ModelsDevCatalog, error) {
	var raw map[string]modelsDevRawProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return ModelsDevCatalog{}, fmt.Errorf("目录解析失败: %w", err)
	}
	cat := ModelsDevCatalog{Providers: map[string]map[string]modelsDevModel{}}
	for pid, p := range raw {
		if len(p.Models) == 0 {
			cat.Notes = append(cat.Notes, fmt.Sprintf("provider %s：无 models 或形状读不懂，跳过", pid))
			continue
		}
		models := map[string]modelsDevModel{}
		for mid, m := range p.Models {
			id := m.ID
			if id == "" {
				id = mid // 键即 id（目录常态）
			}
			if m.Cost == nil || m.Cost.Input == nil || m.Cost.Output == nil {
				cat.NoPrice++
				continue // 读不懂=未知：无成本数据不填充
			}
			in, out := float64(*m.Cost.Input), float64(*m.Cost.Output)
			if in < 0 || out < 0 {
				cat.NoPrice++
				continue
			}
			var cr float64
			if m.Cost.CacheRead != nil {
				cr = float64(*m.Cost.CacheRead)
				if cr < 0 {
					cr = 0
				}
			}
			models[id] = modelsDevModel{ID: id, Name: m.Name, Input: in, Output: out, CacheRead: cr, costOK: true}
		}
		if len(models) > 0 {
			cat.Providers[pid] = models
		}
	}
	return cat, nil
}

// LoadModelsDev 拉取并解析目录：from 为空用默认 URL；支持 http(s) URL
// 或本地文件路径（离线/测试）。响应上限 16MB（目录现约 2MB 量级）。
func LoadModelsDev(from string) (ModelsDevCatalog, error) {
	if from == "" {
		from = ModelsDevURL
	}
	var data []byte
	if strings.HasPrefix(from, "http://") || strings.HasPrefix(from, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(from)
		if err != nil {
			return ModelsDevCatalog{}, fmt.Errorf("目录拉取失败: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return ModelsDevCatalog{}, fmt.Errorf("目录拉取失败: HTTP %d", resp.StatusCode)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return ModelsDevCatalog{}, fmt.Errorf("目录读取失败: %w", err)
		}
	} else { // 本地文件（-from 离线/测试）
		b, err := os.ReadFile(from)
		if err != nil {
			return ModelsDevCatalog{}, fmt.Errorf("目录文件读取失败: %w", err)
		}
		data = b
	}
	return ParseModelsDev(data)
}

// SyncResult 是一次同步的报告（留痕：每一类增删改跳都可核对）。
type SyncResult struct {
	Added       []string `json:"added"`                 // 新增条目
	Updated     []string `json:"updated"`               // 刷新的 models.dev 旧条目
	Overwritten []string `json:"overwritten,omitempty"` // -force 覆盖的人工价
	Skipped     []string `json:"skipped"`               // 人工价保护 / 目录内冲突（留痕）
	NoPrice     int      `json:"no_price"`              // 目录中无有效成本的模型数
	Providers   int      `json:"providers"`             // 扫描的 provider 数
	Models      int      `json:"models"`                // 扫描的带成本模型数
}

// handlePricingSync（POST /api/pricing/sync，M7-W1）：models.dev 目录
// 同步，写路径与管理 API 同一闸门（现值 fail-closed 读取 → 合并 →
// 校验 → 备份+审计 → 写响应包携同步报告与写后单价表）。
// body 可空：{scope?, force?, provider?, from?}——scope "usage"（缺省，
// 仅同步 trace 窗口内用到的模型；同名模型按本地 provider 写精确键）
// 或 "all"（全量目录）；from 支持 URL/本地文件（离线/测试；空=默认
// 目录），与管理 API 同信任域（本机/令牌已闸）。
func (s *Server) handlePricingSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope    string `json:"scope"`
		Force    bool   `json:"force"`
		Provider string `json:"provider"`
		From     string `json:"from"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && err != io.EOF {
			writeAPIErr(w, badRequest(fmt.Errorf("请求体解析失败: %w", err)))
			return
		}
	}
	var cur PricingFile
	if err := s.loadCurrentForWrite(s.pricingPath(), &cur); err != nil {
		writeAPIErr(w, err)
		return
	}
	cat, err := LoadModelsDev(req.From)
	if err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	var merged PricingFile
	var res SyncResult
	if req.Scope == "all" {
		merged, res = SyncPricing(cat, cur, req.Force, req.Provider, time.Now())
	} else {
		merged, res = SyncPricingUsage(cat, cur, req.Force, s.usedModels(), time.Now())
	}
	if err := ValidatePricing(merged); err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	if err := s.writeConfig(s.pricingPath(), cfgPricing, merged); err != nil {
		writeAPIErr(w, err)
		return
	}
	s.writeResultExtra(w, map[string]any{"sync": res, "config": merged})
}

// usedModels 从决策 trace 收集用量对 (本地 provider, 上游模型名)——
// 用量驱动同步的目标集合（与成本报告同口径：outcome=ok 且带 usage）。
func (s *Server) usedModels() []ModelUse {
	traces, err := LoadTraceFiles(filepath.Join(s.AppDir, "trace"), 0)
	if err != nil {
		return nil // trace 读取失败=同步目标为空（空操作，不阻断配置写）
	}
	var used []ModelUse
	for _, tr := range traces {
		if tr.Outcome != "ok" || tr.Usage == nil || tr.Served == "" || tr.ServedModel == "" {
			continue
		}
		pid, _, _ := strings.Cut(tr.Served, "/")
		used = append(used, ModelUse{Provider: pid, Model: tr.ServedModel})
	}
	return used
}

// SyncPricing 用目录全量合并进现有单价表（cur 不被修改）。onlyProvider
// 非空时仅同步该 models.dev provider；force=true 时人工价可被覆盖。
// 用量驱动（只同步用到的模型）走 SyncPricingUsage。
func SyncPricing(cat ModelsDevCatalog, cur PricingFile, force bool, onlyProvider string, now time.Time) (PricingFile, SyncResult) {
	res := SyncResult{Providers: len(cat.Providers)}
	merged := PricingFile{Prices: append([]PriceEntry(nil), cur.Prices...)}
	idx := map[string]int{} // 价格键 → merged.Prices 下标
	for i, p := range merged.Prices {
		idx[p.Model] = i
	}

	// 确定性：provider/model id 字典序；同名键多 provider 冲突时首个
	// （字典序）provider 保留，余者留痕。
	pids := make([]string, 0, len(cat.Providers))
	for pid := range cat.Providers {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	seen := map[string]string{} // 价格键 → 首个来源 provider（冲突检测）

	for _, pid := range pids {
		if onlyProvider != "" && pid != onlyProvider {
			continue
		}
		mids := make([]string, 0, len(cat.Providers[pid]))
		for mid := range cat.Providers[pid] {
			mids = append(mids, mid)
		}
		sort.Strings(mids)
		for _, mid := range mids {
			m := cat.Providers[pid][mid]
			res.Models++
			if prev, dup := seen[mid]; dup {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s（目录内多 provider 冲突，保留 %s）", mid, prev))
				continue
			}
			seen[mid] = pid
			entry := catalogEntry(m, mid, now)
			upsert(&merged, idx, entry, force, &res)
		}
	}
	finalize(&res, cat)
	return merged, res
}

// ModelUse 是用量驱动同步的一个目标：本地 (provider, 上游模型名) 对
// （来自决策 trace 窗口）。Provider 为空时只做裸键兜底匹配。
type ModelUse struct {
	Provider string
	Model    string
}

// SyncPricingUsage 按用量同步（M7-W1 增量：默认不全量拉取——目录数千
// 模型绝大多数用不到）。对每个 used 对：
//   - 目录存在与本地 provider 同名的 models.dev provider 且含该模型
//     → 写 "provider/model" 精确键（供应商级单价，同名模型不同供应商
//     单价可以不同）；
//   - 否则任意 provider 含该模型 id（字典序首个）→ 裸键（通用兜底）；
//   - 都没有 → Skipped「目录未收录」留痕。
//
// 人工价保护/-force/刷新语义与全量模式一致（共用 upsert）。
func SyncPricingUsage(cat ModelsDevCatalog, cur PricingFile, force bool, used []ModelUse, now time.Time) (PricingFile, SyncResult) {
	res := SyncResult{Providers: len(cat.Providers)}
	merged := PricingFile{Prices: append([]PriceEntry(nil), cur.Prices...)}
	idx := map[string]int{}
	for i, p := range merged.Prices {
		idx[p.Model] = i
	}
	seenUse := map[[2]string]bool{}
	var ordered []ModelUse
	for _, u := range used {
		k := [2]string{u.Provider, u.Model}
		if !seenUse[k] {
			seenUse[k] = true
			ordered = append(ordered, u)
		}
	}
	for _, u := range ordered {
		res.Models++
		key := u.Provider + "/" + u.Model
		m, ok := cat.Providers[u.Provider][u.Model]
		if !ok {
			// 裸键兜底：字典序首个含该模型 id 的 provider（确定性）。
			var pids []string
			for pid, models := range cat.Providers {
				if _, has := models[u.Model]; has {
					pids = append(pids, pid)
				}
			}
			sort.Strings(pids)
			if len(pids) == 0 {
				if u.Provider == "" {
					res.Skipped = append(res.Skipped, fmt.Sprintf("%s（目录未收录）", u.Model))
				} else {
					res.Skipped = append(res.Skipped, fmt.Sprintf("%s（目录未收录，本地 provider %s）", u.Model, u.Provider))
				}
				continue
			}
			m = cat.Providers[pids[0]][u.Model]
			key = u.Model
		}
		upsert(&merged, idx, catalogEntry(m, key, now), force, &res)
	}
	finalize(&res, cat)
	return merged, res
}

// catalogEntry 由目录模型构造同步条目（来源与时刻标记）。
func catalogEntry(m modelsDevModel, key string, now time.Time) PriceEntry {
	return PriceEntry{
		Model: key, InputPerM: float64(m.Input), OutputPerM: float64(m.Output),
		CachedInputPerM: float64(m.CacheRead),
		Source:          "models.dev", SyncedAt: now.UTC().Format(time.RFC3339),
	}
}

// upsert 合并一个同步条目（人工价保护/-force/刷新判定共用）。
func upsert(merged *PricingFile, idx map[string]int, entry PriceEntry, force bool, res *SyncResult) {
	i, ok := idx[entry.Model]
	if !ok {
		merged.Prices = append(merged.Prices, entry)
		idx[entry.Model] = len(merged.Prices) - 1
		res.Added = append(res.Added, entry.Model)
		return
	}
	if merged.Prices[i].Source != "models.dev" && !force {
		res.Skipped = append(res.Skipped, fmt.Sprintf("%s（人工价保护，-force 覆盖）", entry.Model))
		return
	}
	overwritten := merged.Prices[i].Source != "models.dev"
	merged.Prices[i] = entry
	if overwritten {
		res.Overwritten = append(res.Overwritten, entry.Model)
	} else {
		res.Updated = append(res.Updated, entry.Model)
	}
}

// finalize 排序留痕清单并写入目录计数（两个同步模式共用收尾）。
func finalize(res *SyncResult, cat ModelsDevCatalog) {
	sort.Strings(res.Added)
	sort.Strings(res.Updated)
	sort.Strings(res.Overwritten)
	sort.Strings(res.Skipped)
	res.NoPrice = cat.NoPrice
}
