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
// Intent（单值）与 Intents（多意图，2026-11-12 Agent 侧重扩展）并存，
// 命中取并集——一个 Agent 可同时承接多个意图。
type Match struct {
	Intent      string   `json:"intent,omitempty"`       // 精确等于（单值，兼容保留）
	Intents     []string `json:"intents,omitempty"`      // 多意图任一命中（Agent 侧重）
	ModelRegex  string   `json:"model_regex,omitempty"`  // 请求模型正则
	PromptRegex string   `json:"prompt_regex,omitempty"` // 任务文本正则
}

// AgentSpec 是 dispatch.json 的一个 Agent 条目（JSON 友好形态：timeout
// 为字符串时长；运行态经 Runtime() 转为 runner 的 Spec）。Models 是
// Agent 专属模型链（2026-11-12 分配策略）：有序 <provider>/<model> 列表，
// 写路径联动落地为保留前缀组 agent-<id>（order），runner 启动注入
// ANTHROPIC_MODEL=agent-<id> → Agent 的 LLM 请求按专属链分配。
type AgentSpec struct {
	ID            string   `json:"id"`
	Command       []string `json:"command"`
	WorkDir       string   `json:"work_dir,omitempty"`
	Env           []string `json:"env,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`        // "10m"；空 = 缺省
	MaxConcurrent int      `json:"max_concurrent,omitempty"` // 0 = 1
	Output        string   `json:"output,omitempty"`         // text（缺省）| jsonl
	Parser        string   `json:"parser,omitempty"`         // 输出解析插件 id（M7-W4）；不可用=该行 raw 回退
	Default       bool     `json:"default,omitempty"`        // 兜底，至多一个
	Match         *Match   `json:"match,omitempty"`
	Models        []string `json:"models,omitempty"` // 专属模型链（agent-<id> 组联动）
}

// AgentGroupPrefix 是派发专属组的保留前缀（服务商页手建组禁用，防冲突）。
const AgentGroupPrefix = "agent-"

// AgentGroupID 返回该 Agent 专属组的 id（Models 非空时有效）。
func (a AgentSpec) AgentGroupID() string { return AgentGroupPrefix + a.ID }

// DispatchFile 是 dispatch.json 的文件形态。
type DispatchFile struct {
	Agents    []AgentSpec `json:"agents"`
	Templates []Template  `json:"templates,omitempty"` // 任务模板（M6-W4）
}

// Runtime 把配置条目转为 runner 的运行态 Spec。
func (a AgentSpec) Runtime() (Spec, error) {
	spec := Spec{ID: a.ID, Command: a.Command, WorkDir: a.WorkDir, Env: a.Env,
		MaxConcurrent: a.MaxConcurrent, Parser: a.Parser}
	if len(a.Models) > 0 {
		spec.AgentModel = a.AgentGroupID() // 专属链 → runner 注入 ANTHROPIC_MODEL=agent-<id>
	}
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
			for _, it := range a.Match.Intents {
				if strings.TrimSpace(it) == "" {
					return fmt.Errorf("agent %s 的 match.intents 含空意图", a.ID)
				}
			}
		}
		for i, m := range a.Models {
			p, model, ok := strings.Cut(m, "/")
			if !ok || strings.TrimSpace(p) == "" || strings.TrimSpace(model) == "" {
				return fmt.Errorf("agent %s 的 models[%d] 须为 <provider>/<model> 两段形态: %q", a.ID, i, m)
			}
		}
	}
	// 模板校验（M6-W4）：agent 引用存在性依赖本文件 agents，故在 agents
	// 循环之后统一校验（fail-closed：引用缺失/名字重复/超时非法皆拒绝）。
	if err := ValidateTemplates(f.Templates, f.Agents); err != nil {
		return err
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
	ID         string  `json:"id"`
	Agent      string  `json:"agent"`
	Task       string  `json:"task"` // 摘要（≤512 字）
	Model      string  `json:"model,omitempty"`
	OutputMode string  `json:"output_mode,omitempty"` // text（缺省）| jsonl（M6-W3 事件解析门槛）
	Status     string  `json:"status"`                // running | done | failed | cancelled
	StartedAt  string  `json:"started_at"`
	EndedAt    string  `json:"ended_at,omitempty"`
	Result     *Result `json:"result,omitempty"`

	cancel   context.CancelFunc
	runCtx   context.Context
	finished chan struct{}
	live     *LiveBuffer
}

// Output 返回会话输出尾部（运行中实时、结束后即最终尾部；恢复的
// 历史会话无 live 缓冲，回退到 Result.Output——M6-W2 持久化）。
func (s *Session) Output() string {
	if s.live != nil {
		return s.live.Tail()
	}
	if s.Result != nil {
		return s.Result.Output
	}
	return ""
}

// Events 返回会话事件视图（M6-W3，仅 jsonl 模式）：运行中取 live 解析
// 快照；终态/恢复会话无 live 时从 Result.Output 现场解析（事件是输出
// 视图的再加工，不改留存语义）。text 模式恒 nil（不猜形状）。
func (s *Session) Events() []Event {
	if s.OutputMode != "jsonl" {
		return nil
	}
	if s.live != nil {
		return s.live.Events()
	}
	if s.Result != nil {
		return ParseEvents(s.Result.Output)
	}
	return nil
}

// LiveBuffer 是会话级输出缓冲：转发到下游（可 nil）并保留尾部，运行中
// 即可读取（控制台实时视图）。jsonl 模式（M6-W3）流式喂入事件解析器
// 留存结构化事件；text 模式不解析（事件恒 nil）。解析器与尾部各自加锁
// ——HTTP 视图读与运行 goroutine 写并发。
type LiveBuffer struct {
	tw     *tailWriter
	mu     sync.Mutex
	parser *EventParser // jsonl 模式非 nil
}

// NewLiveBuffer 构造输出缓冲（out 可为 nil：仅留存；outputMode 非
// "jsonl" 时不启用事件解析）。
func NewLiveBuffer(out io.Writer, outputMode string) *LiveBuffer {
	lb := &LiveBuffer{tw: newTailWriter(out)}
	if outputMode == "jsonl" {
		lb.parser = NewEventParser()
	}
	return lb
}

func (l *LiveBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	if l.parser != nil {
		l.parser.Feed(p)
	}
	l.mu.Unlock()
	return l.tw.Write(p)
}

// UseParser 强制启用解析器并挂插件钩子（M7-W4 agent parser；spec.Parser
// 非空时由 Submit 在首写前调用一次）。
func (l *LiveBuffer) UseParser(hook func(line string) ([]Event, bool)) {
	l.mu.Lock()
	if l.parser == nil {
		l.parser = NewEventParser()
	}
	l.parser.Hook = hook
	l.mu.Unlock()
}

// Flush 冲出事件解析器的半行残留（引擎 goroutine 在 Run 返回后调用
// 一次——此后不再有写入）。
func (l *LiveBuffer) Flush() {
	l.mu.Lock()
	if l.parser != nil {
		l.parser.Flush()
	}
	l.mu.Unlock()
}

// Tail 返回尾部留存文本。
func (l *LiveBuffer) Tail() string {
	l.tw.mu.Lock()
	defer l.tw.mu.Unlock()
	return string(l.tw.tail)
}

// Events 返回留存事件快照（text 模式恒 nil）。
func (l *LiveBuffer) Events() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.parser == nil {
		return nil
	}
	return l.parser.Events()
}

// Engine 持有派发配置与会话表；零值不可用，经 NewEngine 构造。
type Engine struct {
	mu        sync.Mutex
	agents    []AgentSpec
	templates []Template // 任务模板（M6-W4）
	sessions  []*Session
	seq       int64
	global    chan struct{}
	sems      map[string]chan struct{}
	now       func() time.Time
	store     *SessionStore // 会话终态落盘（M6-W2）；nil=不持久化

	// OnFinish 在会话终态时回调（观察者：派发 trace/OTLP 导出经此接入；
	// nil=无）。回调在会话 goroutine 内执行，应快速返回。
	OnFinish func(*Session)

	// ParseHook 是输出解析插件桥（M7-W4）：spec.Parser 非空时按行调用
	//（ok=false/错误 → 该行内置词表/raw 回退）。由网关装配注入；nil=
	// 解析插件不生效。仅在装配期赋值，运行期只读。
	ParseHook func(pluginID, line string) ([]Event, bool)
}

// NewEngine 构造引擎；f 为 nil 表示无任何 agent（显式派发报错指引）。
func NewEngine(f *DispatchFile) *Engine {
	e := &Engine{global: make(chan struct{}, GlobalMaxConcurrent), sems: map[string]chan struct{}{}, now: time.Now}
	if f != nil {
		e.agents = f.Agents
		e.templates = f.Templates
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

// Templates 返回模板清单拷贝（展示/实例化用，M6-W4）。
func (e *Engine) Templates() []Template {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Template(nil), e.templates...)
}

// FindTemplate 按名取模板（值拷贝；不存在报错——fail-closed 不猜）。
func (e *Engine) FindTemplate(name string) (Template, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.templates {
		if t.Name == name {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("未找到模板 %q（dispatch.json 现有 %d 个模板）", name, len(e.templates))
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
	if len(m.Intents) > 0 {
		hit := false
		for _, it := range m.Intents {
			if it == intent {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
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
		OutputMode: agent.Output,
		Status:     "running", StartedAt: e.now().UTC().Format(time.RFC3339),
		finished: make(chan struct{}),
		live:     NewLiveBuffer(stream, agent.Output),
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.runCtx = cancel, ctx
	e.sessions = append(e.sessions, s)
	if len(e.sessions) > SessionRingCap {
		e.sessions = e.sessions[len(e.sessions)-SessionRingCap:]
	}
	// 解析插件（M7-W4）：强制启用解析器并挂钩子（outputMode 一并改写为
	// jsonl——事件视图数据源；插件不可用行内回退 raw）。
	if spec.Parser != "" && e.ParseHook != nil {
		s.live.UseParser(func(line string) ([]Event, bool) {
			return e.ParseHook(spec.Parser, line)
		})
		s.OutputMode = "jsonl"
	}
	e.mu.Unlock()

	go func() {
		defer func() { <-sem; <-e.global; cancel() }()
		res := Run(ctx, spec, task, model, s.live)
		s.live.Flush() // 事件解析半行收口（Run 返回后无写入）
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
		// 终态落盘（M6-W2）：先于 close(finished)——Wait 返回即历史已
		// 落盘（确定性完成信号，避免 TempDir 清理与异步尾写竞态）；字段
		// 此后只读，锁外拷贝安全；Append 内部自持锁，失败只降级。
		if e.store != nil {
			e.store.Append(s.snapshot())
		}
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

// Snapshot 按 ID 返回会话的字段拷贝（锁内读取——与运行 goroutine 的
// Status/Result 写入互斥；CI linux -race 实测指针直读会报 DATA RACE）。
// live 缓冲指针一并拷贝（其内部自带锁，Output() 实时读安全）。
func (e *Engine) Snapshot(id string) (Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.sessions {
		if s.ID == id {
			return s.snapshot(), true
		}
	}
	return Session{}, false
}

// snapshot 在持锁前提下拷贝会话字段（Result 指针写入后不再变更；
// finished 一并携带——恢复会话预置为已关闭，快照 Wait 立即返回，
// 运行会话快照 Wait 等真实信号）。
func (s *Session) snapshot() Session {
	return Session{
		ID: s.ID, Agent: s.Agent, Task: s.Task, Model: s.Model,
		OutputMode: s.OutputMode,
		Status:     s.Status, StartedAt: s.StartedAt, EndedAt: s.EndedAt,
		Result: s.Result, live: s.live, finished: s.finished,
	}
}

// AttachStore 挂载会话终态落盘器（M6-W2；serve 启动装配，Submit 前
// 调用一次）。nil 传参恢复为不持久化。
func (e *Engine) AttachStore(s *SessionStore) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = s
}

// RestoreSessions 把落盘恢复的历史会话注入内存环形（旧→新追加，超出
// SessionRingCap 截断最旧）。恢复会话均为终态：finished 预置为已关闭
// （Wait 立即返回），无 live 缓冲（Output 回退 Result.Output）。
func (e *Engine) RestoreSessions(ss []Session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range ss {
		s := ss[i]
		if s.Status == "running" {
			s.Status = "failed" // 进程重启时运行中的会话已被杀：标失败（不猜结果）
		}
		closed := make(chan struct{})
		close(closed)
		s.finished = closed
		s.cancel, s.runCtx, s.live = nil, nil, nil
		e.sessions = append(e.sessions, &s)
	}
	if len(e.sessions) > SessionRingCap {
		e.sessions = e.sessions[len(e.sessions)-SessionRingCap:]
	}
}

// Reload 热替换 agent 表（PUT /api/config/dispatch 写后路径）：运行中
// 会话不受影响（各自持有旧并发槽，归还即废弃）。
func (e *Engine) Reload(f DispatchFile) error {
	if err := ValidateDispatch(f); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.agents = f.Agents
	e.templates = f.Templates
	e.sems = map[string]chan struct{}{}
	return nil
}

// Sessions 返回会话快照（新→旧，锁内拷贝）。
func (e *Engine) Sessions() []Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Session, 0, len(e.sessions))
	for i := len(e.sessions) - 1; i >= 0; i-- {
		out = append(out, e.sessions[i].snapshot())
	}
	return out
}

// ClearHistory 清除会话历史（控制台「清除历史」）：终态会话从内存环形
// 移除、落盘清空；运行中会话不受影响（终态时照常落盘并出现在列表）。
func (e *Engine) ClearHistory() {
	e.mu.Lock()
	kept := e.sessions[:0]
	for _, s := range e.sessions {
		if s.Status == "running" {
			kept = append(kept, s)
		}
	}
	e.sessions = kept
	st := e.store
	e.mu.Unlock()
	if st != nil {
		st.Clear()
	}
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

// Suggestions 返回三家 Agent 的建议条目（claude-code 形状经 M6-W1 真实
// 冒烟核实；codex/gemini 未经实测仅作文档口径模板，不自动注册——设计稿
// §五）。现为 Agent 目录（Catalog）的 spec 投影——单一条目形状与此处
// 历史形状逐字一致。
func Suggestions() []AgentSpec {
	out := make([]AgentSpec, 0, len(agentCatalog))
	for _, e := range agentCatalog {
		s := e.Spec
		if s.Match != nil { // 浅拷贝：防消费方经共享指针污染目录
			m := *s.Match
			s.Match = &m
		}
		out = append(out, s)
	}
	return out
}

// CatalogEntry 是 Agent 目录的一个条目（2026-11-11 目录化安装）：Spec 为
// 安装后的 AgentSpec 形状；DownloadURL 指向 Agent 包（zip：manifest.json
// + 包装脚本，托管仓库 examples/agent-packs/ 随代码版本化，raw 直链下载
// 安装）；Probe 为前置 CLI 名（安装面 LookPath 探测）；Desc/Prereq 为
// 目录展示文案——Prereq 注明 ToS 纪律：CLI 本体用户自装并登录，网关
// 不代装不代登录（包装脚本仅转发调用）。
type CatalogEntry struct {
	Spec        AgentSpec `json:"spec"`
	DownloadURL string    `json:"download_url"`
	Probe       string    `json:"probe"`
	Desc        string    `json:"desc"`
	Prereq      string    `json:"prereq"`
	Docs        string    `json:"docs,omitempty"`
}

// agentCatalog 是内置 Agent 目录（var 形态便于测试替换；UI 展示与
// 安装管线共用）。当前包为 Windows 形态（run.cmd 调用本机 CLI）。
var agentCatalog = []CatalogEntry{
	{
		Spec:        AgentSpec{ID: "claude-code", Command: []string{"./run.cmd"}, Output: "jsonl", Match: &Match{Intent: "coding"}},
		DownloadURL: "https://raw.githubusercontent.com/listlonely/Yardmaster/main/examples/agent-packs/claude-code.zip",
		Probe:       "claude",
		Desc:        "Claude Code headless（Anthropic 官方 CLI）：stream-json 事件流，形状经 M6-W1 真实冒烟核实",
		Prereq:      "需本机已安装 claude 并登录（claude /login）；网关不代装不代登录",
		Docs:        "https://docs.anthropic.com/en/docs/claude-code",
	},
	{
		Spec:        AgentSpec{ID: "codex", Command: []string{"./run.cmd"}, Output: "jsonl", Match: &Match{Intent: "coding"}},
		DownloadURL: "https://raw.githubusercontent.com/listlonely/Yardmaster/main/examples/agent-packs/codex.zip",
		Probe:       "codex",
		Desc:        "Codex CLI（OpenAI）：exec --json 事件流（文档口径，未经真实凭证实测）",
		Prereq:      "需本机已安装 codex 并登录；网关不代装不代登录",
	},
	{
		Spec:        AgentSpec{ID: "gemini-cli", Command: []string{"./run.cmd"}, Output: "text"},
		DownloadURL: "https://raw.githubusercontent.com/listlonely/Yardmaster/main/examples/agent-packs/gemini-cli.zip",
		Probe:       "gemini",
		Desc:        "Gemini CLI（Google）：-p 单轮 text 输出（文档口径）",
		Prereq:      "需本机已安装 gemini 并登录；网关不代装不代登录",
	},
}

// Catalog 返回内置 Agent 目录（UI 展示与安装管线共用；副本拷贝防调用
// 方污染）。
func Catalog() []CatalogEntry {
	return append([]CatalogEntry(nil), agentCatalog...)
}

// SetCatalogForTest 测试替换目录（production 永不调用——测试注入纪律
// 同 Plugin.backoff/nowFn）。
func SetCatalogForTest(entries []CatalogEntry) { agentCatalog = entries }
