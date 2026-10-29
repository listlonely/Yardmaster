// 管理 REST API（M2-W1 控制台地基）：配置读取 + 类型化写回 + 热重载。
//
//   - GET  /api/config/{name}   读配置；providers 的 api_key 脱敏输出
//   - PUT  /api/config/{name}   全文档类型化写回：严格解析（未知字段
//     拒绝）、语义校验、写前备份快照、原子写、审计日志；providers
//     脱敏回传不破坏密钥（用户值优先）
//   - POST /api/reload          重读 providers.json 热替换 Planner
//
// 生效语义：providers/rules/keys/intents 即时生效（Planner 重载 /
// 引擎按 mtime 或逐次读取热生效）；settings 需重启 serve（装配期读取）。
//
// 安全基线：仅回环监听（F10）；密钥只经内存比对，任何输出必须脱敏
// （§4.4）；当前文件「读不懂=未知」——解析失败时拒绝覆写（fail-closed），
// 绝不把损坏文件当作空配置覆盖掉。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"yardmaster/internal/classify"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// 配置文件名（/api/config/{name} 的合法取值）。
const (
	cfgProviders = "providers"
	cfgSettings  = "settings"
	cfgRules     = "rules"
	cfgKeys      = "keys"
	cfgIntents   = "intents"
	cfgPricing   = "pricing" // M2-W4：模型单价表
)

// maxConfigBody 限制配置写回请求体大小（含模型清单的 providers.json
// 实测远小于此；防误用/滥用）。
const maxConfigBody = 8 << 20

// handleConfigGet 返回 name 对应的配置文档。
func (s *Server) handleConfigGet(w http.ResponseWriter, name string) {
	switch name {
	case cfgProviders:
		var f store.ProvidersFile
		if err := s.loadCurrent(s.ProvidersPath, &f); err != nil {
			writeAPIErr(w, err)
			return
		}
		maskProviders(&f)
		writeJSON(w, f)
	case cfgSettings:
		var f store.Settings
		if err := s.loadCurrent(s.settingsPath(), &f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, f)
	case cfgRules:
		var f RulesFile
		if err := s.loadCurrent(s.rulesPath(), &f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, f)
	case cfgKeys:
		var f KeysFile
		if err := s.loadCurrent(s.keysPath(), &f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, f)
	case cfgIntents:
		var f classify.UserStrategy
		if err := s.loadCurrent(s.intentsPath(), &f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, f)
	case cfgPricing:
		var f PricingFile
		if err := s.loadCurrent(s.pricingPath(), &f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeJSON(w, f)
	default:
		writeAPIErr(w, errUnknownConfig{name})
	}
}

// handleConfigPut 校验并写回 name 对应的配置文档。
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request, name string) {
	body := io.LimitReader(r.Body, maxConfigBody)
	dec := json.NewDecoder(body)
	switch name {
	case cfgProviders:
		var f store.ProvidersFile
		if err := strictDecode(dec, &f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		var cur store.ProvidersFile
		if err := s.loadCurrentForWrite(s.ProvidersPath, &cur); err != nil {
			writeAPIErr(w, err)
			return
		}
		keepProviderKeys(&f, &cur)
		if err := validateProviders(f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := s.writeConfig(s.ProvidersPath, cfgProviders, f); err != nil {
			writeAPIErr(w, err)
			return
		}
		// 热生效：重载成功才回应成功；重载失败（理论不可达，刚写完的
		// 文件刚读过）如实报错。
		s.Planner.Reload(f.Providers, f.Groups)
		writeOK(w, effectImmediate)
	case cfgSettings:
		var f store.Settings
		if err := strictDecode(dec, &f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := s.writeConfig(s.settingsPath(), cfgSettings, f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeOK(w, effectRestart)
	case cfgRules:
		var f RulesFile
		if err := strictDecode(dec, &f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := validateRules(f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := s.writeConfig(s.rulesPath(), cfgRules, f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeOK(w, effectImmediate)
	case cfgKeys:
		var f KeysFile
		if err := strictDecode(dec, &f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := validateKeys(f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := s.writeConfig(s.keysPath(), cfgKeys, f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeOK(w, effectImmediate)
	case cfgIntents:
		var f classify.UserStrategy
		if err := strictDecode(dec, &f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := validateIntents(f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := s.writeConfig(s.intentsPath(), cfgIntents, f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeOK(w, effectImmediate)
	case cfgPricing:
		var f PricingFile
		if err := strictDecode(dec, &f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := ValidatePricing(f); err != nil {
			writeAPIErr(w, badRequest(err))
			return
		}
		if err := s.writeConfig(s.pricingPath(), cfgPricing, f); err != nil {
			writeAPIErr(w, err)
			return
		}
		writeOK(w, effectImmediate)
	default:
		writeAPIErr(w, errUnknownConfig{name})
	}
}

// handleReload 重读 providers.json 并热替换 Planner（providers.json
// 解析失败时拒绝重载，保留旧快照——读不懂=未知）。
func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	f, err := store.LoadProvidersFile(s.ProvidersPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.Planner.Reload(nil, nil)
			writeJSON(w, map[string]any{"ok": true, "effect": effectImmediate, "providers": 0})
			return
		}
		writeAPIErr(w, &apiError{status: http.StatusConflict,
			Msg: "providers.json 解析失败，拒绝重载（保留旧快照，请修复或从 backup/ 还原）: " + err.Error()})
		return
	}
	s.Planner.Reload(f.Providers, f.Groups)
	slog.Info("providers.json 已重载", "providers", len(f.Providers), "groups", len(f.Groups))
	writeJSON(w, map[string]any{"ok": true, "effect": effectImmediate, "providers": len(f.Providers)})
}

// ---------- 文件定位与读写 ----------

func (s *Server) settingsPath() string { return filepath.Join(s.AppDir, "settings.json") }
func (s *Server) rulesPath() string    { return filepath.Join(s.AppDir, "rules.json") }
func (s *Server) keysPath() string     { return filepath.Join(s.AppDir, "keys.json") }
func (s *Server) intentsPath() string  { return filepath.Join(s.AppDir, "intents.json") }
func (s *Server) pricingPath() string  { return filepath.Join(s.AppDir, "pricing.json") }

// handleCost 返回最近 N 天的成本报告（M2-W4）：单价表 × trace 聚合，
// 查询时计算、无新增持久化。查询参数：days（默认 7，上限 30=trace 保留期）。
func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	days := 7
	if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 {
		days = n
	}
	if days > 30 {
		days = 30
	}
	var prices []PriceEntry
	if f, err := LoadPricingFile(s.pricingPath()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			writeAPIErr(w, &apiError{status: http.StatusInternalServerError,
				Msg: "单价表解析失败（读不懂=未知，请修复或从 backup/ 还原）: " + err.Error()})
			return
		}
	} else {
		prices = f.Prices
	}
	traces, err := LoadTraceFiles(filepath.Join(s.AppDir, "trace"), 0)
	if err != nil {
		writeAPIErr(w, &apiError{status: http.StatusInternalServerError,
			Msg: "trace 读取失败: " + err.Error()})
		return
	}
	writeJSON(w, ComputeCost(traces, prices, days, time.Now()))
}

// loadCurrent 读当前配置到 v：文件不存在保持零值（首次创建合法），
// 解析失败返回 500（读不懂=未知，界面如实呈现）。
func (s *Server) loadCurrent(path string, v any) error {
	if err := store.LoadJSON(path, v); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return &apiError{status: http.StatusInternalServerError,
			Msg: "配置文件解析失败（读不懂=未知，请修复或从 backup/ 还原）: " + err.Error()}
	}
	return nil
}

// loadCurrentForWrite 是写路径专用的当前值读取：解析失败按 409 拒绝
// 覆写（fail-closed，宁可拒绝编辑也不覆盖可能含有用户数据的损坏文件）。
func (s *Server) loadCurrentForWrite(path string, v any) error {
	if err := store.LoadJSON(path, v); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return &apiError{status: http.StatusConflict,
			Msg: "当前配置文件解析失败，拒绝覆写（请修复或从 backup/ 还原）: " + err.Error()}
	}
	return nil
}

// writeConfig 执行写前备份 → 原子写 → 审计日志。备份失败必须中止
// （宁可拒绝编辑也不无留底覆写，铁律「覆写前留底」）。
func (s *Server) writeConfig(path, name string, v any) error {
	backupDir := filepath.Join(s.AppDir, "backup")
	if err := store.BackupConfig(path, backupDir, store.BaseName(path), 10); err != nil {
		return &apiError{status: http.StatusInternalServerError,
			Msg: "写前备份失败，已中止写入: " + err.Error()}
	}
	if err := store.SaveJSON(path, v); err != nil {
		return &apiError{status: http.StatusInternalServerError, Msg: "写入配置失败: " + err.Error()}
	}
	slog.Info("配置已通过管理 API 更新", "config", name, "path", path, "backup_dir", backupDir)
	return nil
}

// ---------- providers 密钥保留与校验 ----------

// keepProviderKeys 把脱敏回传/留空的 api_key 还原为现存密钥（用户值
// 优先：GET 脱敏 → PUT 回传的往返不得破坏密钥）。语义：
//   - api_key 为空      → 保留旧值（防止意外清空；删除账号即删除密钥）
//   - api_key == 脱敏形 → 保留旧值（未修改该 key）
//   - 其余              → 视为新密钥采纳
func keepProviderKeys(next, cur *store.ProvidersFile) {
	curKeys := map[string]string{}
	for _, p := range cur.Providers {
		for _, a := range p.Accounts {
			curKeys[p.ID+"/"+a.Name] = a.APIKey
		}
	}
	for i := range next.Providers {
		for j := range next.Providers[i].Accounts {
			acc := &next.Providers[i].Accounts[j]
			curKey := curKeys[next.Providers[i].ID+"/"+acc.Name]
			if acc.APIKey == "" || acc.APIKey == maskAPIKey(curKey) {
				acc.APIKey = curKey
			}
		}
	}
}

// maskAPIKey 把密钥脱敏为「***+末 4 位」（不足 4 位整体 ***）。
func maskAPIKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 4 {
		return "***"
	}
	return "***" + k[len(k)-4:]
}

// maskProviders 原地脱敏 f 中全部 api_key（GET 输出用）。
func maskProviders(f *store.ProvidersFile) {
	for i := range f.Providers {
		for j := range f.Providers[i].Accounts {
			f.Providers[i].Accounts[j].APIKey = maskAPIKey(f.Providers[i].Accounts[j].APIKey)
		}
	}
}

func validateProviders(f store.ProvidersFile) error {
	seenProv := map[string]bool{}
	knownGroup := map[string]bool{}
	for _, g := range f.Groups {
		knownGroup[g.ID] = true
	}
	for _, p := range f.Providers {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("provider id 不能为空")
		}
		if seenProv[p.ID] {
			return fmt.Errorf("provider id 重复: %s", p.ID)
		}
		seenProv[p.ID] = true
		switch p.Protocol {
		case provider.ProtocolChat, provider.ProtocolMessages:
		default:
			return fmt.Errorf("provider %s 协议非法: %q（openai-chat | anthropic-messages）", p.ID, p.Protocol)
		}
		if strings.TrimSpace(p.BaseURL) == "" && !p.Disabled {
			return fmt.Errorf("provider %s 缺少 base_url", p.ID)
		}
		seenAcc := map[string]bool{}
		for _, a := range p.Accounts {
			if strings.TrimSpace(a.Name) == "" {
				return fmt.Errorf("provider %s 存在未命名账号", p.ID)
			}
			if seenAcc[a.Name] {
				return fmt.Errorf("provider %s 账号名重复: %s", p.ID, a.Name)
			}
			seenAcc[a.Name] = true
		}
	}
	seenGroup := map[string]bool{}
	for _, g := range f.Groups {
		if strings.TrimSpace(g.ID) == "" {
			return fmt.Errorf("路由组 id 不能为空")
		}
		if seenGroup[g.ID] {
			return fmt.Errorf("路由组 id 重复: %s", g.ID)
		}
		seenGroup[g.ID] = true
		switch g.Strategy {
		case provider.StrategyOrder, provider.StrategyRotate, provider.StrategySmart,
			provider.StrategyUsage, provider.StrategyWeight:
		default:
			return fmt.Errorf("路由组 %s 策略非法: %q（order | rotate | smart | usage | weight）", g.ID, g.Strategy)
		}
		if len(g.Members) == 0 {
			return fmt.Errorf("路由组 %s 至少需要一个成员", g.ID)
		}
		if g.Strategy == provider.StrategyWeight {
			// 权重键不必与成员一一对应（支持账号/成员/provider 三种粒度），
			// 但值必须 ≥1（≤0 视为配置错误，缺省键由网关按 1 处理）。
			for k, v := range g.Weights {
				if v <= 0 {
					return fmt.Errorf("路由组 %s 权重 %q 必须为正整数", g.ID, k)
				}
			}
		}
		for _, m := range g.Members {
			if err := validateMember(m, f.Providers, knownGroup); err != nil {
				return fmt.Errorf("路由组 %s: %w", g.ID, err)
			}
		}
	}
	return nil
}

// validateMember 校验组成员引用："provider/model"、"provider"（裸
// provider，取其清单）或 "group/<id>"。
func validateMember(m string, provs []provider.Provider, groups map[string]bool) error {
	if strings.TrimSpace(m) == "" {
		return fmt.Errorf("成员引用为空")
	}
	if rest, ok := strings.CutPrefix(m, "group/"); ok {
		if !groups[rest] {
			return fmt.Errorf("成员 %q 引用的组不存在", m)
		}
		return nil
	}
	provID, _, _ := strings.Cut(m, "/")
	for _, p := range provs {
		if p.ID == provID {
			return nil
		}
	}
	return fmt.Errorf("成员 %q 引用的 provider 不存在", m)
}

// ---------- rules / keys / intents 校验 ----------

func validateRules(f RulesFile) error {
	seen := map[string]bool{}
	for _, r := range f.Rules {
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("规则 id 不能为空")
		}
		if seen[r.ID] {
			return fmt.Errorf("规则 id 重复: %s", r.ID)
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Target) == "" {
			return fmt.Errorf("规则 %s 缺少 target", r.ID)
		}
		if r.Regex != "" {
			if _, err := regexp.Compile(r.Regex); err != nil {
				return fmt.Errorf("规则 %s 正则非法: %v", r.ID, err)
			}
		}
	}
	return nil
}

func validateKeys(f KeysFile) error {
	seen := map[string]bool{}
	for _, b := range f.Bindings {
		if strings.TrimSpace(b.Key) == "" {
			return fmt.Errorf("绑定 key 不能为空")
		}
		if seen[b.Key] {
			return fmt.Errorf("绑定 key 重复: %s", b.Key)
		}
		seen[b.Key] = true
		if strings.TrimSpace(b.DefaultModel) == "" {
			return fmt.Errorf("绑定 %s 缺少 default_model", b.Key)
		}
	}
	return nil
}

func validateIntents(f classify.UserStrategy) error {
	seen := map[string]bool{}
	for _, d := range f.Intents {
		if strings.TrimSpace(d.ID) == "" {
			return fmt.Errorf("意图 id 不能为空")
		}
		if seen[d.ID] {
			return fmt.Errorf("意图 id 重复: %s", d.ID)
		}
		seen[d.ID] = true
		if len(d.Examples) > 5 {
			return fmt.Errorf("意图 %s 示例句最多 5 条（F8 词表约束）", d.ID)
		}
		for _, re := range d.Regexes {
			if _, err := regexp.Compile(re); err != nil {
				return fmt.Errorf("意图 %s 正则非法（%q）: %v", d.ID, re, err)
			}
		}
	}
	return nil
}

// ---------- 响应与解码助手 ----------

// 生效语义标注（写回响应的 effect 字段）。
const (
	effectImmediate = "immediate" // 已生效（含热重载/引擎热读取）
	effectRestart   = "restart"   // 需重启 serve（装配期读取的字段）
)

// apiError 携带 HTTP 状态的管理 API 错误。
type apiError struct {
	status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

type errUnknownConfig struct{ name string }

func (e errUnknownConfig) Error() string {
	return "未知配置名 " + e.name + "（可用: providers | settings | rules | keys | intents | pricing）"
}

func (e errUnknownConfig) status() int { return http.StatusNotFound }

func badRequest(err error) *apiError {
	return &apiError{status: http.StatusBadRequest, Msg: err.Error()}
}

func writeAPIErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch e := err.(type) {
	case *apiError:
		status = e.status
	case errUnknownConfig:
		status = e.status()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeOK(w http.ResponseWriter, effect string) {
	writeJSON(w, map[string]any{"ok": true, "effect": effect})
}

// strictDecode 严格解码单个 JSON 文档：未知字段拒绝（写边界宁严勿宽），
// 尾随内容拒绝（防止拼接请求被静默截断）。
func strictDecode(dec *json.Decoder, v any) error {
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("解析请求体失败: %w", err)
	}
	if dec.More() {
		return errors.New("请求体含尾随内容（应为单个 JSON 文档）")
	}
	return nil
}
