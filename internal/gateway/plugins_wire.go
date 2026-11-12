// 插件子系统与网关的接缝（M7-W3/W5）：Server 持有 plugin.Registry，
// 管理面（状态/启停）、配置写路径（PUT /api/config/plugins 校验闸+热
// 替换）与 trace 事件扇出、意图声明并入。
package gateway

import (
	"errors"
	"net/http"

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
	// 插件声明意图并入词表（声明即同步）：路由页分配表与 GET /api/plugins
	// 视图同一口径。
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
