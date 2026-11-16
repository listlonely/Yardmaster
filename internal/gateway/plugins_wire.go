// 插件子系统与网关的接缝（M7-W3/W5）：Server 持有 plugin.Registry，
// 管理面（状态/启停）、配置写路径（PUT /api/config/plugins 校验闸+热
// 替换）与 trace 事件扇出、意图声明并入、意图分类兜底接线。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"yardmaster/internal/plugin"
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
		// Agent 分类器构造提示词），响应 {intent} 须命中词表。往返预算
		// pluginClassifyTimeout；连续 3 次失败由 Registry 冷却 5 分钟。
		// L1SelfHeader 防递归由插件侧自带（分类脑调用经网关推理端点）。
		s.Intents.WithPluginClassify(func(ctx context.Context, text string, vocabulary []string) (string, error) {
			raw, err := r.RequestByCap(plugin.CapClassify, "classify",
				map[string]any{"text": text, "vocabulary": vocabulary}, pluginClassifyTimeout)
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
