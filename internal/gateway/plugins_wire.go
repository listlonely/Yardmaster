// 插件子系统与网关的接缝（M7-W3/W5）：Server 持有 plugin.Registry，
// 管理面（状态/启停）、配置写路径（PUT /api/config/plugins 校验闸+热
// 替换）与 trace 事件扇出、意图声明并入、意图分类兜底接线。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"yardmaster/internal/plugin"
	"yardmaster/internal/store"
)

// AttachPlugins 装配插件注册表，并把决策 trace 链入插件事件扇出
// （保留既有 OnTrace 观察者——OTLP 链式不中断；无 trace 观察需求时
// no-op）。
func (s *Server) AttachPlugins(r *plugin.Registry) {
	if r == nil {
		return
	}
	s.Plugins = r
	// Agent 分配（route.pick，2026-11-13）：agent 策略组转发前经插件从
	// 候选中自主选模型（payload {message,intent,candidates}，响应 {pick}）。
	// 经 Registry 熔断（连续 3 次失败冷却 5 分钟）；失败由网关回落 smart
	// 基线。分类（classify）与挑选（pick）同 capability、异消息类型。
	// 预算 3s（2026-11-13 真机实测：分类脑经真实上游往返 ~1.9s，1.2s
	// 必超时；插件自身 fetchTimeout 2.5s 封顶，两侧口径一致）。
	s.AgentPick = func(ctx context.Context, payload map[string]any) (json.RawMessage, error) {
		return r.RequestByCap(plugin.CapClassify, "pick", payload, 3*time.Second)
	}
	// 插件声明意图并入词表（声明即同步）：路由页分配表与 GET /api/plugins
	// 视图同一口径。
	if s.Intents != nil {
		s.Intents.WithPluginIntents(r.DeclaredIntents)
		// 插件分类兜底（classify.intent，docs §四·三）：L1 未配置/冷却/
		// 失败时经 Registry 发 {text, vocabulary}（payload 带全词表供
		// Agent 分类器构造提示词），响应 {intent} 须命中词表。
		// 多插件并发归并（2026-11-16）：RequestByCapFirstHit 并发请求
		// 全部 classify 插件，首个返回「词表内非空意图」者胜出（校验
		// 前移进归并器，词表外不占赢位）——词表快插件毫秒级先于 LLM
		// 插件命中，慢插件不被跳过也不拖累（赢家即返）。全空=ErrNoHit
		// 视同未命中；全错才计熔断（连 3 败冷却 5 分钟）。
		// L1SelfHeader 防递归由插件侧自带（分类脑调用经网关推理端点）。
		s.Intents.WithPluginClassify(func(ctx context.Context, text string, vocabulary []string) (string, error) {
			vocab := make(map[string]bool, len(vocabulary))
			for _, v := range vocabulary {
				vocab[v] = true
			}
			raw, err := r.RequestByCapFirstHit(plugin.CapClassify, "classify",
				map[string]any{"text": text, "vocabulary": vocabulary},
				func(raw json.RawMessage) bool {
					var resp struct {
						Intent string `json:"intent"`
					}
					return json.Unmarshal(raw, &resp) == nil && resp.Intent != "" && vocab[resp.Intent]
				}, pluginClassifyTimeout)
			if err != nil {
				return "", err
			}
			var resp struct {
				Intent string `json:"intent"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return "", err
			}
			return resp.Intent, nil
		})
	}
	if s.Traces != nil {
		prev := s.Traces.OnTrace
		s.Traces.OnTrace = func(tr Trace) {
			if prev != nil {
				prev(tr)
			}
			r.TraceEvent(tr)
		}
	}
}

// handlePluginsView 返回全部插件状态视图（GET /api/plugins）。
func (s *Server) handlePluginsView(w http.ResponseWriter, _ *http.Request) {
	var views []plugin.StatusView
	if s.Plugins != nil {
		views = s.Plugins.Views()
	}
	if views == nil {
		views = []plugin.StatusView{}
	}
	// options 凭证脱敏（plugin_mask.go）：StatusView 原样透出会把中间件
	// 放进 options 的上游密钥（如 context-guard 的 summary.api_key）
	// 明文回显——与 api.go「任何输出必须脱敏」基线不符。
	for i := range views {
		views[i].Options = maskOptionsCreds(views[i].Options)
	}
	writeJSON(w, map[string]any{"ok": true, "plugins": views})
}

// handlePluginToggle 启用/禁用插件（POST /api/plugins/{id}/enable|disable）：
// 改 plugins.json 走同一写路径（校验+备份+审计）+ 注册表热替换。
func (s *Server) handlePluginToggle(w http.ResponseWriter, id, action string) {
	if s.Plugins == nil {
		writeAPIErr(w, badRequest(errors.New("插件未挂载（plugins.json 未配置或解析失败）")))
		return
	}
	var f plugin.File
	if err := s.loadCurrentForWrite(s.pluginsPath(), &f); err != nil {
		writeAPIErr(w, err)
		return
	}
	found := false
	for i := range f.Plugins {
		if f.Plugins[i].ID == id {
			found = true
			// 智能路由接管开启时禁止停用分类脑插件（接管必须配脑——
			// 先关开关再停用）。
			if action == "disable" && s.settingsTakeoverOn() {
				for _, c := range f.Plugins[i].Capabilities {
					if c == plugin.CapClassify {
						writeAPIErr(w, badRequest(errors.New("智能路由接管开启中，不能停用分类脑插件——先在「路由 › 智能路由」关闭接管")))
						return
					}
				}
			}
			f.Plugins[i].Enabled = action == "enable"
		}
	}
	if !found {
		writeAPIErr(w, errNotFound("插件 "+id))
		return
	}
	if err := plugin.Validate(f); err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	if err := s.writeConfig(s.pluginsPath(), cfgPlugins, f); err != nil {
		writeAPIErr(w, err)
		return
	}
	s.Plugins.Reload(f)
	writeJSON(w, map[string]any{"ok": true, "effect": effectImmediate, "id": id, "enabled": action == "enable"})
}

// agentBrainReady 报告分类脑是否就绪：启用中的 classify 插件存在且其
// options.model（provider/model）已配置。智能路由接管的开启前置校验用
//（读盘不读运行态，与写路径同源；插件进程状态由判定链自然降级兜底）。
func (s *Server) agentBrainReady() bool {
	var f plugin.File
	if err := s.loadCurrent(s.pluginsPath(), &f); err != nil {
		return false
	}
	for _, p := range f.Plugins {
		if !p.Enabled {
			continue
		}
		hasClassify := false
		for _, c := range p.Capabilities {
			if c == plugin.CapClassify {
				hasClassify = true
				break
			}
		}
		if !hasClassify {
			continue
		}
		if m, _ := p.Options["model"].(string); strings.TrimSpace(m) != "" {
			return true
		}
	}
	return false
}

// settingsTakeoverOn 读盘判断智能路由接管是否开启（解析失败按关——
// 与「读不懂=未知」铁律同规，绝不因配置损坏误拦用户操作）。
func (s *Server) settingsTakeoverOn() bool {
	st, err := store.LoadSettings(store.Paths{AppDir: s.AppDir})
	return err == nil && st.AgentTakeoverEnabled()
}

// optsHasModel 报告 options 是否带非空 model（分类脑形态）。
func optsHasModel(opts map[string]any) bool {
	m, _ := opts["model"].(string)
	return strings.TrimSpace(m) != ""
}

// handlePluginOptions 整体替换插件静态配置（PUT /api/plugins/{id}/options，
// body = 完整 options JSON 对象；置 null/空对象 = 清空）。同一写路径
//（校验+备份+审计）+ 注册表热替换；options 语义变化触发实例重建，新
// 握手把 settings.options 透传给插件（如 agent-classifier 的分类脑）。
func (s *Server) handlePluginOptions(w http.ResponseWriter, r *http.Request, id string) {
	if s.Plugins == nil {
		writeAPIErr(w, badRequest(errors.New("插件未挂载（plugins.json 未配置或解析失败）")))
		return
	}
	var opts map[string]any
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeAPIErr(w, badRequest(fmt.Errorf("body 须为 JSON 对象: %w", err)))
		return
	}
	var f plugin.File
	if err := s.loadCurrentForWrite(s.pluginsPath(), &f); err != nil {
		writeAPIErr(w, err)
		return
	}
	found := false
	var oldOpts map[string]any
	for i := range f.Plugins {
		if f.Plugins[i].ID == id {
			found = true
			oldOpts = f.Plugins[i].Options
			// 脱敏形还原（plugin_mask.go）：GET 回显是 *** 形，前端原样
			// 保存不得破坏真凭证（providers keepProviderKeys 同款）。
			restoreOptionsCreds(opts, oldOpts)
			f.Plugins[i].Options = opts
		}
	}
	if !found {
		writeAPIErr(w, errNotFound("插件 "+id))
		return
	}
	// 智能路由接管开启时禁止清空分类脑（接管必须配脑——先关开关再改）。
	if s.settingsTakeoverOn() && !optsHasModel(opts) {
		writeAPIErr(w, badRequest(errors.New("智能路由接管开启中，不能清空分类脑——先在「路由 › 智能路由」关闭接管")))
		return
	}
	if err := plugin.Validate(f); err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	if err := s.writeConfig(s.pluginsPath(), cfgPlugins, f); err != nil {
		writeAPIErr(w, err)
		return
	}
	s.Plugins.Reload(f)
	// 日志与回显走脱敏形（落盘已是 restore 后的真值）。
	masked := maskOptionsCreds(opts)
	slog.Info("插件配置已更新", "id", id, "options", masked)
	writeJSON(w, map[string]any{"ok": true, "effect": effectImmediate, "id": id, "options": masked})
}
