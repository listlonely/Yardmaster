// 派发引擎（M5-W4）：dispatch.json 加载与 fail-closed 校验、意图→Agent
// 匹配、headless 会话表（并发上限/取消/环形留存）、结果回传观察者。
// 设计依据 docs/subsystems/dispatch.md（M5-W3 设计稿）。
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"yardmaster/internal/store"
)

// ---------- 配置形态 ----------

// Match 是意图→Agent 的命中条件（全部条件与关系；空 match 仅可显式指定）。
type Match struct {
	Intent      string `json:"intent,omitempty"`       // 精确等于
	ModelRegex  string `json:"model_regex,omitempty"`  // 请求模型正则
	PromptRegex string `json:"prompt_regex,omitempty"` // 任务文本正则
}

// AgentSpec 是 dispatch.json 的一个 Agent 条目（JSON 友好形态：timeout
// 为字符串时长；运行态经 Runtime() 转为 runner 的 Spec）。
type AgentSpec struct {
	ID            string   `json:"id"`
	Command       []string `json:"command"`
	WorkDir       string   `json:"work_dir,omitempty"`
	Env           []string `json:"env,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`        // "10m"；空 = 缺省
	MaxConcurrent int      `json:"max_concurrent,omitempty"` // 0 = 1
	Output        string   `json:"output,omitempty"`         // text（缺省）| jsonl
	Default       bool     `json:"default,omitempty"`        // 兜底，至多一个
	Match         *Match   `json:"match,omitempty"`
}

// DispatchFile 是 dispatch.json 的文件形态。
type DispatchFile struct {
	Agents []AgentSpec `json:"agents"`
}

// Runtime 把配置条目转为 runner 的运行态 Spec。
func (a AgentSpec) Runtime() (Spec, error) {
	spec := Spec{ID: a.ID, Command: a.Command, WorkDir: a.WorkDir, Env: a.Env, MaxConcurrent: a.MaxConcurrent}
	if a.Timeout != "" {
		d, err := time.ParseDuration(a.Timeout)
		if err != nil || d <= 0 {
			return spec, fmt.Errorf("agent %s 的 timeout 非法: %q", a.ID, a.Timeout)
		}
		spec.Timeout = d
	}
	return spec, nil
}

// ValidateDispatch 校验 dispatch.json（写入口与加载共用，fail-closed）。
func ValidateDispatch(f DispatchFile) error {
	seen := map[string]bool{}
	defs := 0
	for _, a := range f.Agents {
		if a.ID == "" {
			return errors.New("agent 条目 id 不能为空")
		}
		if seen[a.ID] {
			return fmt.Errorf("agent id 重复: %s", a.ID)
		}
		seen[a.ID] = true
		if len(a.Command) == 0 {
			return fmt.Errorf("agent %s 的 command 不能为空", a.ID)
		}
		if a.Timeout != "" {
			if _, err := a.Runtime(); err != nil {
				return err
			}
		}
		if a.MaxConcurrent < 0 {
			return fmt.Errorf("agent %s 的 max_concurrent 不能为负", a.ID)
		}
		switch a.Output {
		case "", "text", "jsonl":
		default:
			return fmt.Errorf("agent %s 的 output 仅支持 text|jsonl: %q", a.ID, a.Output)
		}
		if a.Default {
			defs++
			if defs > 1 {
				return fmt.Errorf("default 兜底 agent 至多一个（%s 重复）", a.ID)
			}
		}
		if a.Match != nil {
			for name, re := range map[string]string{
				"model_regex":  a.Match.ModelRegex,
				"prompt_regex": a.Match.PromptRegex,
			} {
				if re != "" {
					if _, err := regexp.Compile(re); err != nil {
						return fmt.Errorf("agent %s 的 %s 正则非法: %w", a.ID, name, err)
					}
				}
			}
		}
	}
	return nil
}

// ---------- 引擎 ----------

// 全局并发上限（防失控，设计稿 §三）。
const GlobalMaxConcurrent = 8

// 会话环形留存上限。
const SessionRingCap = 256

// 任务摘要留存上限。
const TaskSummaryCap = 512

// Session 是一次派发会话的运行态记录。
type Session struct {
	ID        string  `json:"id"`
	Agent     string  `json:"agent"`
	Task      string  `json:"task"` // 摘要（≤512 字）
	Model     string  `json:"model,omitempty"`
	Status    string  `json:"status"` // running | done | failed | cancelled
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at,omitempty"`
	Result    *Result `json:"result,omitempty"`

	cancel   context.CancelFunc
	runCtx   context.Context
	finished chan struct{}
}

// Engine 持有派发配置与会话表；零值不可用，经 NewEngine 构造。
type Engine struct {
	mu       sync.Mutex
	agents   []AgentSpec
	sessions []*Session
	seq      int64
	global   chan struct{}
	sems     map[string]chan struct{}
	now      func() time.Time

	// OnFinish 在会话终态时回调（观察者：派发 trace/OTLP 导出经此接入；
	// nil=无）。回调在会话 goroutine 内执行，应快速返回。
	OnFinish func(*Session)
}

// NewEngine 构造引擎；f 为 nil 表示无任何 agent（显式派发报错指引）。
func NewEngine(f *DispatchFile) *Engine {
	e := &Engine{global: make(chan struct{}, GlobalMaxConcurrent), sems: map[string]chan struct{}{}, now: time.Now}
	if f != nil {
		e.agents = f.Agents
	}
	return e
}

// LoadEngine 从 dispatch.json 构造；文件不存在返回无 agent 引擎（非错误）；
// 解析失败/校验失败返回错误（fail-closed：调用方应禁用派发并指引修复）。
func LoadEngine(path string) (*Engine, error) {
	var f DispatchFile
	err := store.LoadJSON(path, &f)
	if errors.Is(err, os.ErrNotExist) {
		return NewEngine(nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("dispatch.json 解析失败（派发不可用，请修复后重试）: %w", err)
	}
	if err := ValidateDispatch(f); err != nil {
		return nil, fmt.Errorf("dispatch.json 校验失败（派发不可用，请修复后重试）: %w", err)
	}
	return NewEngine(&f), nil
}

// Agents 返回配置条目拷贝（展示用）。
func (e *Engine) Agents() []AgentSpec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AgentSpec(nil), e.agents...)
}

// Select 选择 agent：显式 id > 声明顺序首个 match 命中 > default 兜底；
// 无果返回错误（fail-closed，不静默选错）。
func (e *Engine) Select(id, task, intent, model string) (AgentSpec, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id != "" {
		for _, a := range e.agents {
			if a.ID == id {
				return a, nil
			}
		}
		return AgentSpec{}, fmt.Errorf("未找到 agent %q（dispatch.json 现有 %d 个条目）", id, len(e.agents))
	}
	for _, a := range e.agents {
		if a.Match != nil && matchHits(a.Match, task, intent, model) {
			return a, nil
		}
	}
	for _, a := range e.agents {
		if a.Default {
			return a, nil
		}
	}
	return AgentSpec{}, errors.New("无匹配 agent 且未配置 default 兜底（dispatch.json）")
}

func matchHits(m *Match, task, intent, model string) bool {
	if m.Intent != "" && m.Intent != intent {
		return false
	}
	if m.ModelRegex != "" && !regexHits(m.ModelRegex, model) {
		return false
	}
	if m.PromptRegex != "" && !regexHits(m.PromptRegex, task) {
		return false
	}
	return true
}

func regexHits(re, s string) bool {
	rx, err := regexp.Compile(re)
	if err != nil {
		return false // 校验已拦；双保险
	}
	return rx.MatchString(s)
}

// Submit 提交一次派发会话：并发满时立即拒绝（不排队——设计稿 §三）。
// stream 可为 nil（仅尾部留存）；返回的 Session 已在运行。
func (e *Engine) Submit(parent context.Context, agent AgentSpec, task, model string, stream io.Writer) (*Session, error) {
	spec, err := agent.Runtime()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	// 并发闸：全局 + 每 agent。
	if e.sems[agent.ID] == nil {
		n := agent.MaxConcurrent
		if n <= 0 {
			n = 1
		}
		e.sems[agent.ID] = make(chan struct{}, n)
	}
	sem := e.sems[agent.ID]
	if len(sem) >= cap(sem) || len(e.global) >= cap(e.global) {
		e.mu.Unlock()
		return nil, fmt.Errorf("agent %s 并发已满（agent 上限 %d / 全局 %d），请稍后再试", agent.ID, cap(sem), cap(e.global))
	}
	sem <- struct{}{}
	e.global <- struct{}{}
	e.seq++
	s := &Session{
		ID:    fmt.Sprintf("d-%d-%06d", e.now().UnixMilli(), e.seq),
		Agent: agent.ID, Task: summary(task), Model: model,
		Status: "running", StartedAt: e.now().UTC().Format(time.RFC3339),
		finished: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.runCtx = cancel, ctx
	e.sessions = append(e.sessions, s)
	if len(e.sessions) > SessionRingCap {
		e.sessions = e.sessions[len(e.sessions)-SessionRingCap:]
	}
	e.mu.Unlock()

	go func() {
		defer func() { <-sem; <-e.global; cancel() }()
		res := Run(ctx, spec, task, model, stream)
		e.mu.Lock()
		s.Result, s.EndedAt = &res, e.now().UTC().Format(time.RFC3339)
		switch res.Outcome {
		case OutcomeOK:
			s.Status = "done"
		case OutcomeCancelled:
			s.Status = "cancelled"
		default:
			s.Status = "failed"
		}
		e.mu.Unlock()
		close(s.finished)
		if e.OnFinish != nil {
			e.OnFinish(s)
		}
	}()
	return s, nil
}

// Cancel 取消一个运行中会话；返回是否有效。
func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.sessions {
		if s.ID == id && s.Status == "running" {
			s.cancel()
			return true
		}
	}
	return false
}

// Sessions 返回会话快照（新→旧）。
func (e *Engine) Sessions() []Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Session, 0, len(e.sessions))
	for i := len(e.sessions) - 1; i >= 0; i-- {
		s := e.sessions[i]
		out = append(out, Session{ID: s.ID, Agent: s.Agent, Task: s.Task, Model: s.Model,
			Status: s.Status, StartedAt: s.StartedAt, EndedAt: s.EndedAt, Result: s.Result})
	}
	return out
}

// Wait 阻塞至会话终态（测试与 CLI 同步用）。
func (s *Session) Wait() { <-s.finished }

// summary 截取任务摘要。
func summary(task string) string {
	task = strings.TrimSpace(task)
	if len(task) > TaskSummaryCap {
		return task[:TaskSummaryCap] + "…"
	}
	return task
}

// ExtractResult 按输出模式提取结果文本：text 原样；jsonl 取最后一条含
// text/content/result 字符串字段的 JSON 行（单行解析失败跳过——读不懂
// =未知，退回全文由调用方兜底）。
func ExtractResult(output, mode string) string {
	if mode != "jsonl" {
		return output
	}
	var last string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		for _, key := range []string{"result", "text", "content"} {
			if s, ok := obj[key].(string); ok && s != "" {
				last = s
			}
		}
	}
	if last != "" {
		return last
	}
	return output
}

// Suggestions 返回三家 Agent 的建议条目（形状未经真实凭证实测，仅作
// dispatch init 预览模板，不自动注册——设计稿 §五）。
func Suggestions() []AgentSpec {
	return []AgentSpec{
		{ID: "claude-code", Command: []string{"claude", "-p", "--output-format", "text"}, Output: "text", Default: false, Match: &Match{Intent: "coding"}},
		{ID: "codex", Command: []string{"codex", "exec", "--json"}, Output: "jsonl", Match: &Match{Intent: "coding"}},
		{ID: "gemini-cli", Command: []string{"gemini", "-p"}, Output: "text"},
	}
}
