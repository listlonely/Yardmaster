// 派发子系统与网关的接缝（M5-W4/W5）：Server 持有 dispatch.Engine，
// 会话终态转为 F9 Trace 记录（观测页/OTLP 对派发零改动生效）；管理
// API：POST /api/dispatch/run、GET agents/sessions/{id}、POST cancel、
// GET/PUT /api/config/dispatch（五铁律写路径 + 热替换引擎）。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"yardmaster/internal/dispatch"
)

// AttachDispatch 把派发引擎挂到 Server（nil 引擎跳过）。会话终态时写
// 一条 Trace：Served="agent/<id>"、Candidates 单节点复用 F9 形状；
// Usage 恒 nil（headless 会话的模型消耗经网关请求各自计量，不重复计）。
func (s *Server) AttachDispatch(e *dispatch.Engine) {
	if e == nil {
		return
	}
	s.Dispatch = e
	e.OnFinish = func(sess *dispatch.Session) {
		if s.Traces == nil {
			return
		}
		tr := Trace{
			ID: sess.ID, Time: sess.StartedAt,
			ClientProto: "dispatch", UpstreamProto: "headless",
			RoutePath: "dispatch", Target: sess.Agent,
			Served: "agent/" + sess.Agent,
		}
		cand := TraceCandidate{Provider: sess.Agent, Model: sess.Model, DurMS: 0}
		if sess.Result != nil {
			cand.Result = dispatchResultOf(sess.Result.Outcome)
			cand.Err = sess.Result.ErrText
			tr.TotalMS = sess.Result.DurationMS
		}
		tr.Candidates = append(tr.Candidates, cand)
		tr.Outcome = "ok"
		if sess.Status != "done" {
			tr.Outcome = "fail"
		}
		s.Traces.Record(tr)
	}
}

// dispatchResultOf 把派发结局映射到 trace 结果词表。
func dispatchResultOf(o dispatch.Outcome) string {
	switch o {
	case dispatch.OutcomeOK:
		return "ok"
	case dispatch.OutcomeLaunch:
		return "net" // 启动失败归网络/环境类
	case dispatch.OutcomeTimeout:
		return "net"
	default:
		return "client" // Agent 自身退出/取消
	}
}

// ---------- 管理 API（M5-W5） ----------

// dispatchDisabled 在引擎未挂载时统一报错（fail-closed：不静默空跑）。
func dispatchDisabled(w http.ResponseWriter) {
	writeAPIErr(w, badRequest(errors.New("派发不可用：dispatch.json 损坏或 serve 未挂载引擎，请修复后重载")))
}

// handleDispatchRun 提交一次派发：{task, agent?, model?}；意图经意图链
// 尽力分类（分类失败不阻塞——intent 为空参与匹配）。响应为写后状态
// （写响应即状态精神）：会话快照。
func (s *Server) handleDispatchRun(w http.ResponseWriter, r *http.Request) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	var req struct {
		Task  string `json:"task"`
		Agent string `json:"agent"`
		Model string `json:"model"`
	}
	body := io.LimitReader(r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeAPIErr(w, badRequest(errors.New("请求体解析失败: "+err.Error())))
		return
	}
	if req.Task == "" {
		writeAPIErr(w, badRequest(errors.New("task 不能为空")))
		return
	}
	intent := ""
	if s.Intents != nil {
		intent = s.Intents.Classify(req.Task)
	}
	agent, err := s.Dispatch.Select(req.Agent, req.Task, intent, req.Model)
	if err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	// 会话生命周期独立于本次 HTTP 请求（响应返回后仍需运行）：用后台
	// ctx；取消只能经 /cancel 端点（设计稿 §三）。
	sess, err := s.Dispatch.Submit(context.Background(), agent, req.Task, req.Model, nil)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	// 视图取锁内快照（直接读 *Session 字段会与运行 goroutine 竞态，
	// CI linux -race 实测）。
	sv, _ := s.Dispatch.Snapshot(sess.ID)
	writeJSON(w, map[string]any{
		"ok": true, "effect": effectImmediate,
		"intent": intent, "session": dispatchSessionView(sv),
	})
}

// handleDispatchAgents 返回派发配置的 agent 清单。
func (s *Server) handleDispatchAgents(w http.ResponseWriter, _ *http.Request) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "agents": s.Dispatch.Agents()})
}

// handleDispatchSessions 返回会话快照（新→旧）。
func (s *Server) handleDispatchSessions(w http.ResponseWriter, _ *http.Request) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "sessions": s.Dispatch.Sessions()})
}

// handleDispatchSession 返回单个会话详情（含实时输出尾部）。
func (s *Server) handleDispatchSession(w http.ResponseWriter, id string) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	sv, ok := s.Dispatch.Snapshot(id)
	if !ok {
		writeAPIErr(w, errNotFound("会话 "+id))
		return
	}
	writeJSON(w, dispatchSessionDetail(sv))
}

// handleDispatchCancel 取消一个运行中会话。
func (s *Server) handleDispatchCancel(w http.ResponseWriter, id string) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	if !s.Dispatch.Cancel(id) {
		writeAPIErr(w, errNotFound("运行中的会话 "+id))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "effect": effectImmediate, "cancelled": id})
}

// dispatchSessionView 是会话的 JSON 视图（入参为锁内字段拷贝）。
func dispatchSessionView(s dispatch.Session) map[string]any {
	return map[string]any{
		"id": s.ID, "agent": s.Agent, "task": s.Task, "model": s.Model,
		"status": s.Status, "started_at": s.StartedAt, "ended_at": s.EndedAt,
		"result": s.Result,
	}
}

// dispatchSessionDetail 是会话详情视图（含实时输出尾部——live 缓冲自带锁）。
func dispatchSessionDetail(s dispatch.Session) map[string]any {
	v := dispatchSessionView(s)
	v["ok"] = true
	v["output"] = s.Output()
	return v
}

// errNotFound 构造 404 API 错误。
func errNotFound(what string) error {
	return &apiError{status: http.StatusNotFound, Msg: what + " 不存在"}
}
