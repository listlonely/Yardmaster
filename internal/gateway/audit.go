// 组员可用性审计（2026-11-20）：实测被路由组引用的成员模型，把
// 「确定性不可用」的成员自动移出组。动机：模型清单拉到了 ≠ 套餐
// 能用——预检只验证凭证与清单（如智谱 1311 当前订阅套餐暂未开放
// 权限 / 1113 余额不足或无可用资源包，均发生在对话请求阶段）。
//
// 判定从严（宁可少删）：仅 403/404 与其他 4xx 中错误体命中套餐/权限
// 语义词表才移除；401（凭证）、429（限速）、5xx（上游故障）与传输层
// 错误一律保留——暂时态由既有账号休眠与 bad-credential 机制兜底。
//
// 触发点：管理 API POST /api/groups/audit（控制台「组员体检」按钮）
// 与预检成功后的自动挂钩（见 handleProviderProbe / CLI provider probe）；
// 不做周期轮询（省 token，按需触发）。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yardmaster/internal/netproxy"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// unavailableKeywords 是「确定性不可用」错误体语义词表（小写匹配）：
// 权限/套餐/资源包/余额语义。厂商报错持续出现新形态时在此扩充；
// 404 状态码本身已覆盖「模型不存在」，表内 "not found" 兜底 4xx 文本形。
var unavailableKeywords = []string{
	"未开放", "无权限", "套餐", "资源包", "余额不足",
	"not entitled", "no permission", "insufficient balance", "does not exist", "not found",
}

// memberUnavailable 判定成员模型是否「确定性不可用」：返回移除原因，
// 空串=保留。status==0 表示传输层失败（超时/连接拒绝），恒保留。
// 判定从严：401（凭证态，交既有 bad-credential 机制）、5xx（上游暂时
// 故障）与 2xx 恒保留；403/404 无条件移除；其余 4xx（**含 429**）按
// 响应体词表判定——真机实证智谱 1311「套餐未开放」曾以 400 壳返回、
// 又以 429 壳返回（2026-11-20），状态码不稳定但错误体语义确定性，
// 命中词表才移除，未命中（真限速）保留。
func memberUnavailable(status int, body string) string {
	switch {
	case status == http.StatusForbidden:
		return "HTTP 403（无权限/禁止）"
	case status == http.StatusNotFound:
		return "HTTP 404（模型不存在）"
	case status == http.StatusUnauthorized, status >= 500, status == 0, status < 400:
		return ""
	}
	low := strings.ToLower(body)
	for _, kw := range unavailableKeywords {
		if strings.Contains(low, kw) {
			return fmt.Sprintf("HTTP %d：错误体命中 %q（套餐/权限语义）", status, kw)
		}
	}
	return ""
}

// ProbeModel 对单个成员模型发一次最小对话请求（max_tokens 16、
// temperature 0、单条 "ping"），返回 (HTTP 状态码, 响应体片段, 传输层
// 错误)。状态码+片段交 memberUnavailable 判定；端点/认证头形状复用
// ProbeAccount 的四协议适配（probe.go 头注）；请求经 rt（调用方按 F3
// 链解析代理后注入）。auditProbeTimeout 独立于预检：逐成员预算。
const auditProbeTimeout = 20 * time.Second

func ProbeModel(ctx context.Context, prov *provider.Provider, acc provider.Account,
	model string, rt http.RoundTripper, authStyle string) (int, string, error) {
	var url, payload string
	switch prov.Protocol {
	case provider.ProtocolMessages:
		url = strings.TrimRight(prov.BaseURL, "/") + "/v1/messages"
		payload = fmt.Sprintf(`{"model":%q,"max_tokens":16,"temperature":0,"messages":[{"role":"user","content":"ping"}]}`, model)
	case provider.ProtocolResponses:
		// Responses 协议：input 而非 messages；max_output_tokens 下限 16
		url = strings.TrimRight(prov.BaseURL, "/") + "/responses"
		payload = fmt.Sprintf(`{"model":%q,"input":"ping","max_output_tokens":16,"temperature":0,"stream":false}`, model)
	case provider.ProtocolGemini:
		url = fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(prov.BaseURL, "/"), model)
		payload = `{"contents":[{"parts":[{"text":"ping"}]}],"generationConfig":{"maxOutputTokens":16,"temperature":0}}`
	default: // openai-chat
		url = strings.TrimRight(prov.BaseURL, "/") + "/chat/completions"
		payload = fmt.Sprintf(`{"model":%q,"max_tokens":16,"temperature":0,"stream":false,"messages":[{"role":"user","content":"ping"}]}`, model)
	}
	ctx, cancel := context.WithTimeout(ctx, auditProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	switch prov.Protocol {
	case provider.ProtocolMessages:
		if authStyle == "bearer" {
			req.Header.Set("Authorization", "Bearer "+acc.APIKey)
		} else {
			req.Header.Set("x-api-key", acc.APIKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	case provider.ProtocolGemini:
		req.Header.Set("x-goog-api-key", acc.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+acc.APIKey)
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, netproxy.Snippet(body), nil
}

// AuditItem / AuditReport 是逐成员审计结果（控制台展示形状）。
type AuditItem struct {
	Ref     string `json:"ref"`               // 成员引用原样（provider/model[@account]）
	OK      bool   `json:"ok"`                // 实测可调用（HTTP 2xx）
	Removed bool   `json:"removed"`           // 已移出组（确定性不可用）
	Reason  string `json:"reason,omitempty"`  // 移除原因；保留时的一句话结论
	Skipped string `json:"skipped,omitempty"` // 非空=跳过实测（不参与移除判定）
}

type AuditReport struct {
	Items   []AuditItem `json:"items"`
	Removed int         `json:"removed"`
	// Marked 是清单不可用标记的实际变化数（置位 + 误标清除，2026-11-20）：
	// 计入 changed——只标清单不动组（如空组保底）也要写回，否则徽标落盘。
	Marked int `json:"marked,omitempty"`
}

// AuditGroups 审计 f.Groups 中被引用的成员模型（跨组按引用去重实测）：
// 席位成员用指定账号实测，普通成员用 provider 首个非停用、非 OAuth 且
// 带密钥的账号（OAuth 凭证在凭证库，不走 API key 直测，跳过不判）。
// onlyProvider 非空时仅审计引用该 provider 的成员。probe 可注入（测试
// 假上游）。移除动作只落在 *f（f.Groups 原地过滤）与返回值：调用方
// 负责留底写回。组内成员将全部被移除时保守保留（validateProviders
// 要求组至少一个成员，宁可少删）。
func AuditGroups(f *store.ProvidersFile, onlyProvider string,
	probe func(prov *provider.Provider, acc provider.Account, model string) (int, string, error)) (*AuditReport, bool) {
	provs := map[string]*provider.Provider{}
	for i := range f.Providers {
		provs[f.Providers[i].ID] = &f.Providers[i]
	}
	// 收集被引用成员（声明序去重；范围过滤）
	var refs []string
	seen := map[string]bool{}
	for _, g := range f.Groups {
		for _, m := range g.Members {
			if seen[m] || !memberTouches(m, onlyProvider) {
				continue
			}
			seen[m] = true
			refs = append(refs, m)
		}
	}
	rep := &AuditReport{Items: make([]AuditItem, 0, len(refs))}
	removed := map[string]string{} // ref → 移除原因
	for _, ref := range refs {
		item := AuditItem{Ref: ref}
		prov, acc, skip := resolveMemberAccount(ref, provs)
		if skip != "" {
			item.Skipped = skip
			rep.Items = append(rep.Items, item)
			continue
		}
		status, body, err := probe(prov, *acc, upstreamModel(ref))
		switch {
		case err != nil:
			item.Reason = "传输层失败（保留）: " + netproxy.Redact(err.Error())
		default:
			if reason := memberUnavailable(status, body); reason != "" {
				item.Reason = reason
				removed[ref] = reason
			} else {
				item.OK = true
				item.Reason = fmt.Sprintf("HTTP %d 可调用", status)
			}
		}
		rep.Items = append(rep.Items, item)
	}
	// 应用移除：按组过滤成员；某组将因此空掉时保守保留该组成员
	guard := map[string]bool{} // ref 因「组将空」被保底保留
	for gi := range f.Groups {
		g := &f.Groups[gi]
		var keep []string
		hits := 0
		for _, m := range g.Members {
			if _, hit := removed[m]; hit {
				hits++
				continue
			}
			keep = append(keep, m)
		}
		if hits == 0 {
			continue
		}
		if len(keep) == 0 {
			for _, m := range g.Members {
				guard[m] = true
			}
			continue
		}
		g.Members = keep
	}
	for i := range rep.Items {
		if _, hit := removed[rep.Items[i].Ref]; !hit {
			continue
		}
		if guard[rep.Items[i].Ref] {
			rep.Items[i].Reason += "；所在组将无成员，保守保留"
			continue
		}
		rep.Items[i].Removed = true
		rep.Removed++
	}
	// 清单同步标记（2026-11-20）：被判定确定性不可用的成员，按
	// provider+模型 ID 给清单条目置位（席位引用剥 @seat 后同模型 ID，
	// 同一模型被多组引用也只标一次）；实测通过的成员反向清除旧标记
	//（上游暂时故障曾误标的自愈入口）。清单徽标与前端候选过滤都消费
	// 该标记，故置/清的实际变化计入 changed，确保落盘。
	marked := 0
	for ref, reason := range removed {
		if markModelUnavailable(provs, ref, reason) {
			marked++
		}
	}
	for i := range rep.Items {
		if rep.Items[i].OK && clearModelUnavailable(provs, rep.Items[i].Ref) {
			marked++
		}
	}
	rep.Marked = marked
	// changed 按「组员实际移除」或「清单标记实际变化」判定（guard 组
	// 不算成员变更，但 guard 成员的清单标记已计入 marked）。
	changed := rep.Removed > 0 || marked > 0
	return rep, changed
}

// markModelUnavailable 按成员引用（provider/model[@seat]）定位清单条目
// 置不可用标记；返回是否发生实际变化（幂等，重复置位不重复计数）。
func markModelUnavailable(provs map[string]*provider.Provider, ref, reason string) bool {
	id, _, _ := strings.Cut(ref, "/")
	p := provs[id]
	if p == nil {
		return false
	}
	model := upstreamModel(ref)
	for i := range p.Models {
		if p.Models[i].ID != model {
			continue
		}
		if p.Models[i].Unavailable && p.Models[i].UnavailReason == reason {
			return false
		}
		p.Models[i].Unavailable = true
		p.Models[i].UnavailReason = reason
		return true
	}
	return false // 模型不在清单（手动引用/清单已刷新），无处可标
}

// clearModelUnavailable 按成员引用清除清单条目的不可用标记（体检实测
// 通过 = 上游恢复或曾误标）；返回是否发生实际变化。
func clearModelUnavailable(provs map[string]*provider.Provider, ref string) bool {
	id, _, _ := strings.Cut(ref, "/")
	p := provs[id]
	if p == nil {
		return false
	}
	model := upstreamModel(ref)
	for i := range p.Models {
		if p.Models[i].ID == model && p.Models[i].Unavailable {
			p.Models[i].Unavailable = false
			p.Models[i].UnavailReason = ""
			return true
		}
	}
	return false
}

// MergeUnavailableInto 把旧清单中同 ID 条目的不可用标记继承到新清单
//（2026-11-20）：预检重拉与控制台勾选保存都会整体重建 Models，不做
// 继承则一次普通操作就抹掉体检徽标。按模型 ID 匹配（模型从上游消失
// 则条目自然不在，无从继承）；新条目已带的标记不覆盖。显式 false 与
// 缺省无法区分（omitempty），统一按零值继承——清除标记的唯一入口是
// 组员体检实测通过。API 写回（handleConfigPut）与 CLI 预检共用本函数。
func MergeUnavailableInto(dst, old []provider.Model) []provider.Model {
	oldMarks := make(map[string]string, len(old)) // id → reason（仅已标记条目）
	for _, m := range old {
		if m.Unavailable {
			oldMarks[m.ID] = m.UnavailReason
		}
	}
	for i := range dst {
		if dst[i].Unavailable {
			continue
		}
		if reason, ok := oldMarks[dst[i].ID]; ok {
			dst[i].Unavailable = true
			dst[i].UnavailReason = reason
		}
	}
	return dst
}

// memberTouches 判断成员引用是否落在 onlyProvider 范围（""=全部）。
func memberTouches(ref, onlyProvider string) bool {
	if onlyProvider == "" {
		return true
	}
	head, _, _ := strings.Cut(ref, "@")
	id, _, _ := strings.Cut(head, "/")
	return id == onlyProvider
}

// upstreamModel 取成员引用的上游模型名（@席位后缀剥除）。
func upstreamModel(ref string) string {
	head, _, _ := strings.Cut(ref, "@")
	_, model, _ := strings.Cut(head, "/")
	return model
}

// resolveMemberAccount 解析成员引用的实测对象：返回 provider、账号与
// 跳过原因（非空=无法实测，不参与移除判定）。
func resolveMemberAccount(ref string, provs map[string]*provider.Provider) (*provider.Provider, *provider.Account, string) {
	head, seat, hasSeat := strings.Cut(ref, "@")
	id, model, ok := strings.Cut(head, "/")
	if !ok || id == "" || model == "" {
		return nil, nil, "非 provider/model 形态（裸 provider/组引用不实测）"
	}
	prov := provs[id]
	if prov == nil {
		return nil, nil, "provider 不存在"
	}
	if prov.Disabled {
		return nil, nil, "provider 已停用"
	}
	if hasSeat {
		for i := range prov.Accounts {
			if prov.Accounts[i].Name != seat {
				continue
			}
			acc := &prov.Accounts[i]
			if acc.Auth == provider.AuthOAuth {
				return nil, nil, "席位账号为 OAuth（凭证在凭证库，不走 API key 实测）"
			}
			if acc.APIKey == "" {
				return nil, nil, "席位账号无 API key"
			}
			return prov, acc, ""
		}
		return nil, nil, "席位账号不存在"
	}
	for i := range prov.Accounts {
		acc := &prov.Accounts[i]
		if acc.Disabled || acc.Auth == provider.AuthOAuth || acc.APIKey == "" {
			continue
		}
		return prov, acc, ""
	}
	return nil, nil, "无可用 API key 账号（停用/OAuth 账号不实测）"
}

// ApplyAuditWithBackup 把审计后的 providers.json 写回磁盘：覆写前留底
// （铁律）→ 原子写。管理 API 与 CLI 预检挂钩共用同一写路径，勿绕过。
func ApplyAuditWithBackup(path, appDir string, f *store.ProvidersFile) error {
	backupDir := filepath.Join(appDir, "backup")
	if err := store.BackupConfig(path, backupDir, store.BaseName(path), 10); err != nil {
		return fmt.Errorf("写前备份失败，已中止写入: %w", err)
	}
	if err := store.SaveProvidersFile(path, *f); err != nil {
		return fmt.Errorf("写入 providers.json 失败: %w", err)
	}
	return nil
}

// handleGroupsAudit POST /api/groups/audit（组员体检）：逐成员发最小
// 对话请求实测被组引用的模型，确定性不可用者自动移出组并写回（覆写
// 前留底 → 热重载）。body 可选 {"provider_id": "..."} 限定审计范围；
// 逐成员预算 auditProbeTimeout，响应同步返回逐成员报告。
func (s *Server) handleGroupsAudit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProviderID string `json:"provider_id,omitempty"`
	}
	if r.Body != nil {
		// 空体/空对象均合法（全量审计）；坏 JSON 不阻断（按全量处理）
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	}
	f, err := store.LoadProvidersFile(s.ProvidersPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeAPIErr(w, badRequest(errors.New("尚无 providers.json 配置")))
			return
		}
		writeAPIErr(w, &apiError{status: http.StatusConflict,
			Msg: "providers.json 解析失败，拒绝审计（读不懂=未知）: " + err.Error()})
		return
	}
	rep, changed := s.auditFile(&f, req.ProviderID, r.Context())
	if changed {
		if err := ApplyAuditWithBackup(s.ProvidersPath, s.AppDir, &f); err != nil {
			writeAPIErr(w, &apiError{status: http.StatusInternalServerError, Msg: err.Error()})
			return
		}
		s.Planner.Reload(f.Providers, f.Groups)
		slog.Info("组员体检完成并写回", "provider", req.ProviderID, "removed", rep.Removed, "marked", rep.Marked)
	}
	s.writeResultExtra(w, map[string]any{"report": rep, "changed": changed})
}

// auditFile 组装探针闭包（按 F3 代理链解析 Transport）并执行审计。
func (s *Server) auditFile(f *store.ProvidersFile, onlyProvider string, ctx context.Context) (*AuditReport, bool) {
	probe := func(prov *provider.Provider, acc provider.Account, model string) (int, string, error) {
		ref, _ := ResolveProxyRef(string(acc.Proxy), string(prov.Proxy), s.GlobalProxy)
		rt, err := netproxy.SharedTransport(netproxy.Ref(ref))
		if err != nil {
			return 0, "", err
		}
		return ProbeModel(ctx, prov, acc, model, rt, prov.AuthStyle)
	}
	return AuditGroups(f, onlyProvider, probe)
}

// autoAuditProvider 预检成功后的自动组员体检（后台，2026-11-20）：
// 读盘 → 范围审计 → 有移除才留底写回 + 热重载。后台执行避免拉长预检
// 响应；与用户并发编辑存在小概率竞态，覆写前留底兜底可手工回滚。
func (s *Server) autoAuditProvider(id string) {
	f, err := store.LoadProvidersFile(s.ProvidersPath)
	if err != nil {
		return
	}
	rep, changed := s.auditFile(&f, id, context.Background())
	if !changed {
		return
	}
	if err := ApplyAuditWithBackup(s.ProvidersPath, s.AppDir, &f); err != nil {
		slog.Warn("预检后自动组员体检写回失败", "provider", id, "err", err)
		return
	}
	s.Planner.Reload(f.Providers, f.Groups)
	slog.Info("预检后自动组员体检：已移除确定性不可用成员", "provider", id, "removed", rep.Removed)
	for _, it := range rep.Items {
		slog.Info("组员体检明细", "member", it.Ref, "removed", it.Removed,
			"ok", it.OK, "reason", it.Reason, "skipped", it.Skipped)
	}
}
