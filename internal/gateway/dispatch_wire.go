// 派发子系统与网关的接缝（M5-W4）：Server 持有 dispatch.Engine，
// 会话终态转为 F9 Trace 记录（观测页/OTLP 对派发零改动生效）。
// 管理 API 端点与控制台页在 M5-W5（api.go / web/）。
package gateway

import (
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
