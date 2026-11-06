// 插件子系统与网关的接缝（M7-W3/W4/W5）：Server 持有 plugin.Registry，
// 管理面（状态/启停）、配置写路径（PUT /api/config/plugins 校验闸+热
// 替换）与三类钩子桥（派发解析/意图兜底/厂商特判）。
package gateway

import (
	"encoding/json"
	"errors"
	"net/http"

	"yardmaster/internal/dispatch"
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
	// 插件声明意图并入词表（声明即同步）：路由页分配表、插件分类兜底
	// 校验（classifyIntentWithPlugin）与 GET /api/plugins 视图同一口径。
	if s.Intents != nil {
		s.Intents.WithPluginIntents(r.DeclaredIntents)
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
	// 派发输出解析钩子（M7-W4）：agent 条目 parser 指向插件 id；
	// 插件不可用/响应非法 → false（该行 raw 回退，M6-W3 纪律不变）。
	if s.Dispatch != nil {
		s.Dispatch.ParseHook = func(pluginID, line string) ([]dispatch.Event, bool) {
			raw, err := r.ParseLine(pluginID, line)
			if err != nil {
				return nil, false
			}
			var out struct {
				Events []dispatch.Event `json:"events"`
			}
			if json.Unmarshal(raw, &out) != nil || len(out.Events) == 0 {
				return nil, false
			}
			for _, ev := range out.Events { // 词表外 kind 拒收（读不懂=未知）
				switch ev.Kind {
				case dispatch.EventInit, dispatch.EventText, dispatch.EventTool,
					dispatch.EventToolRes, dispatch.EventResult, dispatch.EventRaw:
				default:
					return nil, false
				}
			}
			return out.Events, true
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

// classifyIntentWithPlugin 意图分类兜底（M7-W4）：内置链未命中时问插件
// （300ms+熔断在 Registry 内）；词表外 intent 不采纳；无插件/失败=空
// intent（回落默认组）。
func (s *Server) classifyIntentWithPlugin(task string) string {
	if s.Plugins == nil {
		return ""
	}
	intent, ok := s.Plugins.ClassifyIntent(task)
	if !ok {
		return ""
	}
	for _, v := range s.Intents.Vocabulary() {
		if v == intent {
			return intent
		}
	}
	return ""
}
