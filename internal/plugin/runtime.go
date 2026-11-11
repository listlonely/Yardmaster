// 插件进程运行时（M7-W3）：长驻子进程 + NDJSON 协议 + 崩溃退避重启 +
// 事件有界队列。并发模型：实例状态由 mu 保护；spec 每实例不可变
// （Reload 变更=重建实例，绝不原地改）；stdin 写由 wmu 串行；事件队列
// 满 drop-oldest（绝不阻塞推理路径）。
package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusBackoff  Status = "backoff"
	StatusFailed   Status = "failed"
	StatusStopped  Status = "stopped"
)

const (
	lineLimit       = 4 << 20 // 协议行长上限 4MB（context-guard 等改写大 body 的插件：压缩后回复仍可能 >1MB）
	queueCap        = 1024    // 事件队列（每插件）
	maxRestartsHour = 10      // 1 小时内崩溃重启上限，超出标 failed
	heartbeatEvery  = 30 * time.Second
	heartbeatDead   = 2  // 连续无 pong 的心跳周期数 → 判死
	maxLogTail      = 50 // stderr 尾部留存行数（管理面展示）
	vendorBodyCap   = 8 << 10
)

// Plugin 是一个子进程插件的运行时句柄（并发安全；spec 每实例不可变，
// 崩溃由 supervise 退避重启）。
type Plugin struct {
	spec    Spec // 不可变（Reload 变更=重建实例）
	granted []string
	appDir  string // 数据目录（runOnce 注入 YM_APP_DIR 环境变量，插件持久化用）

	mu       sync.Mutex
	status   Status
	pending  map[string]chan json.RawMessage
	logTail  []string
	failAt   []time.Time // 最近崩溃时刻（1h 窗口）
	stopping bool
	lastPong time.Time

	wmu    sync.Mutex // stdin 写串行
	stdin  io.WriteCloser
	cmd    *exec.Cmd
	reqSeq atomic.Int64
	dropN  atomic.Int64
	queue  chan []byte // 事件队列（负载为已序列化 payload），实例集共享

	started chan struct{} // 每次进程实例的握手完成信号

	// 可注入（测试）
	backoff   time.Duration
	heartbeat time.Duration
	nowFn     func() time.Time
}

func newPlugin(spec Spec, appDir string) *Plugin {
	st := StatusStarting
	if !spec.Enabled {
		st = StatusStopped // 禁用条目不启动（Reload 重建的禁用实例）
	}
	return &Plugin{
		spec: spec, appDir: appDir, pending: map[string]chan json.RawMessage{},
		status: st, queue: make(chan []byte, queueCap),
		backoff: time.Second, heartbeat: heartbeatEvery, nowFn: time.Now,
	}
}

func (p *Plugin) timeout() time.Duration {
	if p.spec.TimeoutMS > 0 {
		return time.Duration(p.spec.TimeoutMS) * time.Millisecond
	}
	return time.Second
}

func (p *Plugin) hasCap(c string) bool {
	for _, g := range p.granted {
		if g == c {
			return true
		}
	}
	return false
}

func (p *Plugin) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status == StatusRunning
}

// Status 返回实例状态。
func (p *Plugin) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// ---- 生命周期 ----

// supervise 是守护循环：启动→运行→崩溃→退避重启，直到 Stop/ctx 结束或
// 1 小时崩溃超 maxRestartsHour（标 failed，直至 Reload 重建）。
func (p *Plugin) supervise(ctx context.Context) {
	for {
		p.mu.Lock()
		if p.stopping {
			p.status = StatusStopped
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		err := p.runOnce()

		p.mu.Lock()
		if p.stopping {
			p.status = StatusStopped
			p.mu.Unlock()
			return
		}
		now := p.nowFn()
		p.failAt = append(p.failAt, now)
		var keep []time.Time
		for _, t := range p.failAt {
			if now.Sub(t) < time.Hour {
				keep = append(keep, t)
			}
		}
		p.failAt = keep
		if len(keep) > maxRestartsHour {
			p.status = StatusFailed
			p.mu.Unlock()
			slog.Warn("插件崩溃重启超限，标记 failed（重载配置恢复）",
				"plugin", p.spec.ID, "crashes_1h", len(keep), "last_err", err)
			return
		}
		p.status = StatusBackoff
		d := p.backoff << uint(min(len(keep)-1, 6)) // 1s→2s→4s…封顶 60s
		if d > 60*time.Second {
			d = 60 * time.Second
		}
		p.mu.Unlock()
		slog.Warn("插件进程退出，退避重启", "plugin", p.spec.ID, "err", err, "backoff", d)
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.status = StatusStopped
			p.mu.Unlock()
			return
		case <-time.After(d):
		}
	}
}

// runOnce 启动一次进程实例并服务至其退出（进程退出→fail 所有 pending）。
func (p *Plugin) runOnce() error {
	cmd := exec.Command(p.spec.Command[0], p.spec.Command[1:]...)
	// 最小环境（设计稿 §五）：默认不透传父进程全量环境；plugins.json
	// env 含 "INHERIT_ENV=1" 时才继承（用户显式选择）。YM_APP_DIR 为
	// 唯一自动注入项：数据目录绝对路径，插件持久化状态用（context-guard
	// 缓存等），用户配置零成本且跨设备正确。
	env := []string{}
	inherit := false
	if p.appDir != "" {
		env = append(env, "YM_APP_DIR="+p.appDir)
	}
	for _, e := range p.spec.Env {
		if e == "INHERIT_ENV=1" {
			inherit = true
			continue
		}
		env = append(env, e)
	}
	if inherit {
		env = append(os.Environ(), env...)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}
	p.mu.Lock()
	p.cmd = cmd
	p.stdin = stdin
	p.pending = map[string]chan json.RawMessage{} // 新实例；旧等待者已在退出时关闭
	p.lastPong = p.nowFn()
	p.status = StatusStarting
	p.granted = nil
	st := make(chan struct{})
	p.started = st
	p.mu.Unlock()

	hbStop := make(chan struct{})
	var hbOnce sync.Once
	stopHB := func() { hbOnce.Do(func() { close(hbStop) }) }
	go p.pumpStderr(stderr)
	go p.pumpEvents(hbStop)
	go p.heartbeatLoop(hbStop)
	go func() {
		// 读循环退出（握手失败/协议错误/进程退出）→ 杀进程走重启路径
		//（握手失败的实例不进入 running，等同降级）。
		if err := p.readLoop(stdout, st); err != nil {
			slog.Warn("插件 stdout 读循环退出", "plugin", p.spec.ID, "err", err)
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	waitErr := cmd.Wait() // 进程退出（含被杀）
	stopHB()
	p.mu.Lock()
	for id, ch := range p.pending {
		close(ch) // 等待者收到零值（进程退出）
		delete(p.pending, id)
	}
	p.stdin = nil
	p.mu.Unlock()
	return waitErr
}

// handleHandshake 处理 stdout 首行（能力声明）并回授予。
func (p *Plugin) handleHandshake(line []byte, started chan struct{}) error {
	var hs struct {
		Plugin       string   `json:"plugin"`
		Proto        int      `json:"proto"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(line, &hs); err != nil {
		return fmt.Errorf("握手行非 JSON: %w", err)
	}
	if hs.Proto != 1 {
		return fmt.Errorf("不支持的协议版本 %d", hs.Proto)
	}
	declared := map[string]bool{}
	for _, c := range hs.Capabilities {
		declared[c] = true
		if !capabilitySet[c] {
			slog.Warn("插件声明未知能力（忽略）", "plugin", p.spec.ID, "capability", c)
		}
	}
	// granted = 配置 ∩ 声明（用户值优先；越权/漏声明审计告警不中断）。
	var granted []string
	for _, c := range p.spec.Capabilities {
		if declared[c] {
			granted = append(granted, c)
		} else {
			slog.Warn("插件未声明配置授予的能力（跳过）", "plugin", p.spec.ID, "capability", c)
		}
	}
	if err := p.send(map[string]any{"ok": true, "granted": granted, "settings": map[string]any{"options": p.spec.Options}}); err != nil {
		return fmt.Errorf("握手回执写入失败: %w", err)
	}
	p.mu.Lock()
	p.granted = granted
	p.status = StatusRunning
	p.mu.Unlock()
	slog.Info("插件已连接", "plugin", p.spec.ID, "granted", granted)
	close(started)
	return nil
}

// handleLine 分发握手后的行：resp 路由等待者；pong 更新心跳；未知忽略。
func (p *Plugin) handleLine(line []byte) {
	var env struct {
		V       int             `json:"v"`
		Type    string          `json:"type"`
		ID      string          `json:"id"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return // 读不懂=忽略（协议外噪声不致命）
	}
	switch env.Type {
	case "resp":
		if env.ID == "" {
			return
		}
		p.mu.Lock()
		ch, ok := p.pending[env.ID]
		delete(p.pending, env.ID)
		p.mu.Unlock()
		if ok {
			ch <- env.Payload // chan 容量 1，不阻塞
		}
	case "pong":
		p.mu.Lock()
		p.lastPong = p.nowFn()
		p.mu.Unlock()
	}
}

// readLoop 逐行读 stdout 并分发；返回扫描错误（进程退出/行长超限）。
func (p *Plugin) readLoop(r io.Reader, started chan struct{}) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), lineLimit)
	handshook := false
	for sc.Scan() {
		line := sc.Bytes()
		if !handshook {
			if err := p.handleHandshake(line, started); err != nil {
				return err
			}
			handshook = true
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		p.handleLine(cp)
	}
	return sc.Err()
}

// pumpStderr 把插件 stderr 逐行截尾留存（管理面日志尾）并记网关日志。
func (p *Plugin) pumpStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8<<10), 2<<10)
	for sc.Scan() {
		line := sc.Text()
		p.mu.Lock()
		p.logTail = append(p.logTail, line)
		if len(p.logTail) > maxLogTail {
			p.logTail = p.logTail[len(p.logTail)-maxLogTail:]
		}
		p.mu.Unlock()
		slog.Info("插件日志", "plugin", p.spec.ID, "line", line)
	}
}

// pumpEvents 把事件队列的负载逐条写往插件 stdin（进程退出即返回）。
func (p *Plugin) pumpEvents(stop <-chan struct{}) {
	for {
		select {
		case ev := <-p.queue:
			if err := p.send(map[string]any{"v": 1, "type": "event", "payload": json.RawMessage(ev)}); err != nil {
				return
			}
		case <-stop:
			return
		}
	}
}

// heartbeatLoop 周期 ping；连续 2 周期无 pong 判死（杀进程走重启路径）。
func (p *Plugin) heartbeatLoop(stop <-chan struct{}) {
	t := time.NewTicker(p.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.mu.Lock()
			late := p.nowFn().Sub(p.lastPong) > p.heartbeat*heartbeatDead
			cmd := p.cmd
			p.mu.Unlock()
			if late {
				slog.Warn("插件心跳超时，判死重启", "plugin", p.spec.ID)
				if cmd != nil && cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				return
			}
			_ = p.send(map[string]any{"v": 1, "type": "ping"})
		}
	}
}

// send 序列化并写一行到插件 stdin（wmu 串行）。
func (p *Plugin) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.stdin == nil {
		return errors.New("插件未运行")
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// ---- 对外操作 ----

// Request 发送一次请求/响应钩子调用（须 running；超时=移除等待者）。
func (p *Plugin) Request(msgType string, payload any, timeout time.Duration) (json.RawMessage, error) {
	p.mu.Lock()
	if p.status != StatusRunning {
		p.mu.Unlock()
		return nil, ErrNoPlugin
	}
	p.reqSeq.Add(1)
	id := fmt.Sprintf("r%d", p.reqSeq.Load())
	ch := make(chan json.RawMessage, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	if err := p.send(map[string]any{"v": 1, "type": msgType, "id": id, "payload": payload}); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, err
	}
	select {
	case raw, ok := <-ch:
		if !ok {
			return nil, errors.New("插件进程退出")
		}
		return raw, nil
	case <-time.After(timeout):
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("插件响应超时（%v）", timeout)
	}
}

// EnqueueEvent 非阻塞入队一条事件（payload 已序列化）；队列满 drop-
// oldest 并计数（绝不阻塞调用方——OnTrace 内联纪律）。
func (p *Plugin) EnqueueEvent(payload []byte) {
	if !p.hasCap(CapEvents) || !p.running() {
		return
	}
	select {
	case p.queue <- payload:
	default:
		select {
		case <-p.queue:
			p.dropN.Add(1)
		default:
		}
		select {
		case p.queue <- payload:
		default:
			p.dropN.Add(1)
		}
	}
}

// Stop 停止插件（supervisor 退出；进程被杀）。
func (p *Plugin) Stop() {
	p.mu.Lock()
	p.stopping = true
	cmd := p.cmd
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// StatusView 是管理面的插件状态视图。
type StatusView struct {
	ID           string      `json:"id"`
	Command      []string    `json:"command"`
	Enabled      bool        `json:"enabled"`
	Capabilities []string    `json:"capabilities"`
	Desc         string      `json:"desc,omitempty"`
	Intents      []IntentRef `json:"intents,omitempty"`
	Granted      []string    `json:"granted,omitempty"`
	Status       Status      `json:"status"`
	Crash1H      int         `json:"crash_1h"`
	Dropped      int64       `json:"dropped_events,omitempty"`
	LogTail      []string    `json:"log_tail,omitempty"`
}

func (p *Plugin) view() StatusView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return StatusView{
		ID: p.spec.ID, Command: p.spec.Command, Enabled: p.spec.Enabled,
		Capabilities: p.spec.Capabilities, Desc: p.spec.Desc, Intents: p.spec.Intents,
		Granted: p.granted,
		Status:  p.status, Crash1H: len(p.failAt), Dropped: p.dropN.Load(),
		LogTail: append([]string(nil), p.logTail...),
	}
}

// ---- 注册表 ----

// Registry 持有全部插件实例（声明序）；能力请求入口与事件扇出。
// spec 每实例不可变：Reload 的变更（含 enabled 翻转）一律重建实例。
type Registry struct {
	mu      sync.Mutex
	order   []string
	plugins map[string]*Plugin
	appDir  string // 数据目录（启动插件时注入 YM_APP_DIR）
	ctx     context.Context
	cancel  context.CancelFunc
	backoff time.Duration // 测试注入

	cool map[string]*cooldown // capability → 熔断（连续 3 次失败 5 分钟）
}

type cooldown struct {
	fails []time.Time
	until time.Time
}

// DeclaredIntents 返回「enabled 且 granted 含 classify.intent 且运行中」
// 的插件声明的全部意图（路由页词表并入与分类校验的同一口径——声明即
// 同步，停用/崩溃即摘除）。
func (r *Registry) DeclaredIntents() []IntentRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []IntentRef
	for _, id := range r.order {
		p := r.plugins[id]
		if p == nil || !p.spec.Enabled || p.status != StatusRunning {
			continue
		}
		has := false
		for _, g := range p.granted {
			if g == CapClassify {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		out = append(out, p.spec.Intents...)
	}
	return out
}

// NewRegistry 构造空注册表（Start/Reload 装载）。appDir 为网关数据目录
// （插件进程 YM_APP_DIR 注入源；测试可传临时目录或 ""）。
func NewRegistry(appDir string) *Registry {
	return &Registry{
		plugins: map[string]*Plugin{},
		appDir:  appDir,
		cool:    map[string]*cooldown{},
		backoff: time.Second,
	}
}

// Start 装配并启动 enabled 插件（幂等：先停旧实例集）。生命周期 ctx
// 由注册表持有（Stop=cancel）。
func (r *Registry) Start(f File) {
	r.mu.Lock()
	r.stopLocked()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.plugins = map[string]*Plugin{}
	r.order = r.order[:0]
	var startList []*Plugin
	for _, spec := range f.Plugins {
		p := newPlugin(spec, r.appDir)
		p.backoff = r.backoff
		r.plugins[spec.ID] = p
		r.order = append(r.order, spec.ID)
		if spec.Enabled {
			startList = append(startList, p)
		}
	}
	ctx := r.ctx
	r.mu.Unlock()
	for _, p := range startList {
		go p.supervise(ctx)
	}
}

// Reload 热替换（PUT 写后路径）：移除/变更（含 enabled 翻转）的实例
// 重建，未变更的保留运行态；failed 标记随重建复位。
func (r *Registry) Reload(f File) {
	r.mu.Lock()
	if r.ctx == nil {
		r.mu.Unlock()
		return
	}
	ctx := r.ctx
	desired := map[string]Spec{}
	for _, s := range f.Plugins {
		desired[s.ID] = s
	}
	var stopList []*Plugin
	for id, p := range r.plugins {
		ns, ok := desired[id]
		if !ok || !sameSpec(ns, p.spec) {
			stopList = append(stopList, p)
			delete(r.plugins, id)
		}
	}
	var startList []*Plugin
	order := make([]string, 0, len(f.Plugins))
	for _, s := range f.Plugins {
		order = append(order, s.ID)
		if p, ok := r.plugins[s.ID]; ok {
			if s.Enabled && (p.Status() == StatusStopped || p.Status() == StatusFailed) {
				startList = append(startList, p)
			}
			continue
		}
		p := newPlugin(s, r.appDir)
		p.backoff = r.backoff
		r.plugins[s.ID] = p
		if s.Enabled {
			startList = append(startList, p)
		}
	}
	r.order = order
	r.mu.Unlock()
	for _, p := range stopList {
		p.Stop()
	}
	for _, p := range startList {
		go p.supervise(ctx)
	}
}

// sameSpec 判断两份配置是否语义一致（一致=实例保留运行态）。Options
// 参与 比较：改 options 需重建实例重启进程（握手 settings 才会带上
// 新值），不能原地热改。
func sameSpec(a, b Spec) bool {
	if len(a.Options) != 0 || len(b.Options) != 0 {
		ao, _ := json.Marshal(a.Options)
		bo, _ := json.Marshal(b.Options)
		if !bytes.Equal(ao, bo) {
			return false
		}
	}
	return a.Enabled == b.Enabled && a.TimeoutMS == b.TimeoutMS &&
		strings.Join(a.Command, "\x00") == strings.Join(b.Command, "\x00") &&
		strings.Join(a.Capabilities, "\x00") == strings.Join(b.Capabilities, "\x00") &&
		strings.Join(a.Env, "\x00") == strings.Join(b.Env, "\x00")
}

// Stop 停止全部插件（serve 收尾）。
func (r *Registry) Stop() {
	r.mu.Lock()
	r.stopLocked()
	r.mu.Unlock()
}

func (r *Registry) stopLocked() {
	if r.cancel != nil {
		r.cancel()
	}
	for _, p := range r.plugins {
		p.Stop()
	}
	r.plugins = map[string]*Plugin{}
	r.order = r.order[:0]
}

// Views 返回全部插件状态视图（声明序）。
func (r *Registry) Views() []StatusView {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StatusView, 0, len(r.order))
	for _, id := range r.order {
		if p, ok := r.plugins[id]; ok {
			out = append(out, p.view())
		}
	}
	return out
}

// TraceEvent 把一条决策 trace（任意可 JSON 序列化形态，通常为
// gateway.Trace 元数据）扇出给所有订阅 events.trace 的插件（序列化一次；
// 无订阅者零开销）。
func (r *Registry) TraceEvent(v any) {
	r.mu.Lock()
	var subs []*Plugin
	for _, id := range r.order {
		if p := r.plugins[id]; p.hasCap(CapEvents) {
			subs = append(subs, p)
		}
	}
	r.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	for _, p := range subs {
		p.EnqueueEvent(b)
	}
}

// RequestByCap 按能力找首个运行中插件发请求（连续 3 次失败 → 该能力
// 冷却 5 分钟——L1 同款容错）。
func (r *Registry) RequestByCap(cap, msgType string, payload any, timeout time.Duration) (json.RawMessage, error) {
	r.mu.Lock()
	if cd := r.cool[cap]; cd != nil && time.Now().Before(cd.until) {
		r.mu.Unlock()
		return nil, ErrCooldown
	}
	var p *Plugin
	for _, id := range r.order {
		pl := r.plugins[id]
		if pl.running() && pl.hasCap(cap) {
			p = pl
			break
		}
	}
	r.mu.Unlock()
	if p == nil {
		return nil, ErrNoPlugin
	}
	raw, err := p.Request(msgType, payload, timeout)
	r.record(cap, err == nil)
	return raw, err
}

// record 熔断计数：失败入 5 分钟窗口，连续 3 次 → 冷却。
func (r *Registry) record(cap string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cd := r.cool[cap]
	if cd == nil {
		cd = &cooldown{}
		r.cool[cap] = cd
	}
	if ok {
		cd.fails = nil
		return
	}
	cd.fails = append(cd.fails, now)
	var keep []time.Time
	for _, t := range cd.fails {
		if now.Sub(t) < 5*time.Minute {
			keep = append(keep, t)
		}
	}
	cd.fails = keep
	if len(keep) >= 3 {
		cd.until = now.Add(5 * time.Minute)
		slog.Warn("插件能力连续失败，冷却 5 分钟", "capability", cap, "fails", len(keep))
	}
}

// ClassifyIntent 意图分类兜底（L1 不可用/冷却时由网关调用）；intent 由
// 调用方按词表校验（读不懂=不采纳）。
func (r *Registry) ClassifyIntent(text string) (string, bool) {
	raw, err := r.RequestByCap(CapClassify, "classify", map[string]any{"text": text}, 300*time.Millisecond)
	if err != nil {
		return "", false
	}
	var out struct {
		Intent string `json:"intent"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Intent == "" {
		return "", false
	}
	return out.Intent, true
}

// ParseLine 派发输出解析钩子：返回原始 payload（{"events":[…]}），由
// 调用方（gateway）反序列化为 dispatch.Event——本包不依赖 dispatch。
func (r *Registry) ParseLine(pluginID, line string) (json.RawMessage, error) {
	r.mu.Lock()
	p := r.plugins[pluginID]
	r.mu.Unlock()
	if p == nil || !p.running() || !p.hasCap(CapDispatch) {
		return nil, ErrNoPlugin
	}
	return p.Request("parse", map[string]any{"line": line}, p.timeout())
}

// VendorVerdict 响应特判钩子：{status, body≤8KB} → verdict payload 原文
// （调用方解析 action）。
func (r *Registry) VendorVerdict(status int, body []byte) (json.RawMessage, error) {
	excerpt := body
	if len(excerpt) > vendorBodyCap {
		excerpt = excerpt[:vendorBodyCap]
	}
	return r.RequestByCap(CapVendor, "verdict", map[string]any{"status": status, "body": string(excerpt)}, 800*time.Millisecond)
}

// RewriteRequest 上游请求改写钩子（provider.rewrite，2026-11-10 magpie-
// bridge 验证）：payload = {"body","model","protocol","path","agent",
// "stream","options"}；resp payload 三态 {"changed":true,"body"} /
// {"changed":false} / {"reject":{status,reason}}。原文返回，解析归调用方
// （网关接线时决定 changed=false 放行、reject 转 4xx）。
func (r *Registry) RewriteRequest(payload any, timeout time.Duration) (json.RawMessage, error) {
	return r.RequestByCap(CapRewrite, "rewrite", payload, timeout)
}

// rewriteHookTimeout 是响应/事件钩子的调用方超时（掩码类纯文本操作，
// 从快——流式逐帧调用不能拖累转发；失败经 RequestByCap 熔断 3 次后该
// 能力冷却 5 分钟，期间帧/响应原样透传）。
const rewriteHookTimeout = 300 * time.Millisecond

// RewriteResponse 完整上游响应改写钩子（provider.rewrite.response，
// 非流式）：payload = {"body"(JSON 文本),"status","protocol","agent"}；
// resp = {"changed":true,"body"(新 JSON 文本)} / {"changed":false}。
func (r *Registry) RewriteResponse(body string, status int, protocol, agent string) (json.RawMessage, error) {
	return r.RequestByCap(CapRewriteResp, "response",
		map[string]any{"body": body, "status": status, "protocol": protocol, "agent": agent},
		rewriteHookTimeout)
}

// RewriteEventFrame 单 SSE 帧改写钩子（provider.rewrite.event，流式）：
// payload = {"frame"(data: 行 JSON 文本),"protocol","agent"}；resp =
// {"changed":true,"event"(新 JSON 文本)} / {"changed":false}。
func (r *Registry) RewriteEventFrame(event string, protocol, agent string) (json.RawMessage, error) {
	return r.RequestByCap(CapRewriteEvent, "event",
		map[string]any{"event": event, "protocol": protocol, "agent": agent},
		rewriteHookTimeout)
}

// LogTail 返回插件 stderr 尾部（管理面展示）。
func (r *Registry) LogTail(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.plugins[id]; ok {
		return p.view().LogTail
	}
	return nil
}

// RunningWithCap 报告是否存在运行中且授予该能力的插件（接线判空用）。
func (r *Registry) RunningWithCap(cap string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.order {
		if p := r.plugins[id]; p.running() && p.hasCap(cap) {
			return true
		}
	}
	return false
}

var _ = sort.Strings
