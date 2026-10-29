// 决策追溯（PRD F9）：每次路由决策写一条 trace——谁在调用、命中什么
// 规则、试过哪些候选、为什么最终由它服务。只记录元数据与失败原因，
// 永不落盘对话内容（§4.4）。
//
// 落盘：按天 JSONL（trace/trace-YYYYMMDD.jsonl），保留 30 天、总量
// 200MB 上限（产品文档 §6）；写入异步化，观测失败绝不影响请求。
package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yardmaster/internal/netproxy"
)

// Trace 保留与内存缓冲参数。
const (
	traceKeepDays   = 30
	traceMaxBytes   = 200 << 20 // 200MB 总量上限
	traceMemCap     = 200       // /api/trace 的内存环形缓冲
	traceChanBuf    = 256
	traceFileNameRe = `^trace-\d{8}\.jsonl$`
)

// 候选尝试结果分类（与规则页失败分类表对齐）。
const (
	CandOK     = "ok"     // 2xx
	CandNet    = "net"    // 网络层错误（含代理不可达）
	CandQuota  = "quota"  // 429/额度
	CandServer = "5xx"    // 上游服务错误
	CandCred   = "cred"   // 401/403 凭证失败
	CandClient = "client" // 其余 4xx（请求本身的问题）
)

// TraceCandidate 是候选尝试链上的一次尝试。
type TraceCandidate struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Model    string `json:"model"`
	Proxy    string `json:"proxy,omitempty"` // 实际代理出口（脱敏）
	Result   string `json:"result"`          // ok/net/quota/5xx/cred/client
	Status   int    `json:"status,omitempty"`
	Err      string `json:"err,omitempty"` // 失败原因摘要（已脱敏）
	DurMS    int64  `json:"dur_ms"`
}

// TraceUsage 是本次请求的 token 用量（上游响应可得时）。
type TraceUsage struct {
	Prompt     int64 `json:"prompt,omitempty"`
	Completion int64 `json:"completion,omitempty"`
}

// Trace 是一次路由决策的完整记录（不含对话内容）。
type Trace struct {
	ID           string           `json:"id"`
	Time         string           `json:"time"` // RFC3339
	Caller       string           `json:"caller,omitempty"`
	RequestModel string           `json:"request_model,omitempty"`
	Intent       string           `json:"intent,omitempty"`
	Rule         string           `json:"rule,omitempty"` // 命中的规则 ID
	RoutePath    string           `json:"route_path,omitempty"`
	Target       string           `json:"target,omitempty"`
	Candidates   []TraceCandidate `json:"candidates,omitempty"`
	Served       string           `json:"served,omitempty"` // provider/account（服务方）
	ServedModel  string           `json:"served_model,omitempty"`
	Outcome      string           `json:"outcome"` // ok / fail
	HTTPStatus   int              `json:"http_status,omitempty"`
	TTFBMS       int64            `json:"ttfb_ms,omitempty"`
	TotalMS      int64            `json:"total_ms,omitempty"`
	Usage        *TraceUsage      `json:"usage,omitempty"`
	Switched     bool             `json:"switched,omitempty"` // 成功前发生过候选切换
}

// TraceLog 是异步落盘的决策记录器。
type TraceLog struct {
	mu      sync.Mutex
	mem     []Trace // 最近 traceMemCap 条（环形，/api/trace 用）
	dir     string
	ch      chan Trace
	done    chan struct{}
	now     func() time.Time
	dropped atomic.Int64 // 队列满被丢弃的条数（日志可见）
	wg      sync.WaitGroup

	keepDays int   // 保留天数（默认 30；测试注入）
	maxBytes int64 // 总量上限（默认 200MB；测试注入）
}

// NewTraceLog 构建记录器并启动后台写入与清理（启动时清一次）。
func NewTraceLog(dir string) *TraceLog {
	t := &TraceLog{
		dir:      dir,
		ch:       make(chan Trace, traceChanBuf),
		done:     make(chan struct{}),
		now:      time.Now,
		keepDays: traceKeepDays,
		maxBytes: traceMaxBytes,
	}
	t.cleanup()
	t.wg.Add(1)
	go t.loop()
	return t
}

// SetNow 注入时钟（测试用）。
func (t *TraceLog) SetNow(f func() time.Time) { t.now = f }

// Record 异步记录一条 trace：内存环形缓冲立即更新（/api/trace 用），
// 落盘走后台队列；永不阻塞、永不失败（队列满丢弃并计数）。
func (t *TraceLog) Record(tr Trace) {
	t.mu.Lock()
	if len(t.mem) >= traceMemCap {
		t.mem = t.mem[len(t.mem)-traceMemCap+1:]
	}
	t.mem = append(t.mem, tr)
	t.mu.Unlock()
	select {
	case t.ch <- tr:
	default:
		t.dropped.Add(1)
	}
}

// Dropped 返回累计丢弃条数（诊断用）。
func (t *TraceLog) Dropped() int64 { return t.dropped.Load() }

// Recent 返回内存缓冲中最近的记录（limit<=0 取全部缓冲），
// agent 为 caller 前缀过滤，result 为 ok/fail/switch 过滤。
func (t *TraceLog) Recent(limit int, agent, result string) []Trace {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Trace, 0, len(t.mem))
	// t.mem 旧的在前；从新到旧输出
	for i := len(t.mem) - 1; i >= 0; i-- {
		tr := t.mem[i]
		if agent != "" && !strings.HasPrefix(tr.Caller, agent) {
			continue
		}
		switch result {
		case "ok":
			if tr.Outcome != "ok" {
				continue
			}
		case "fail":
			if tr.Outcome != "fail" {
				continue
			}
		case "switch":
			if !tr.Switched {
				continue
			}
		}
		out = append(out, tr)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Close 停止后台写入并冲刷队列。
func (t *TraceLog) Close() {
	close(t.done)
	t.wg.Wait()
}

// loop 是后台写入循环：逐条追加到当天的 JSONL 文件；退出时冲刷剩余。
func (t *TraceLog) loop() {
	defer t.wg.Done()
	var curDay string
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	write := func(tr Trace) {
		day := t.now().Format("20060102")
		if f == nil || day != curDay {
			if f != nil {
				_ = f.Close()
				f = nil
			}
			_ = os.MkdirAll(t.dir, 0o700)
			nf, err := os.OpenFile(filepath.Join(t.dir, "trace-"+day+".jsonl"),
				os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				slog.Warn("trace 落盘失败（不影响请求）", "dir", t.dir, "err", err)
				return
			}
			f, curDay = nf, day
		}
		b, err := json.Marshal(tr)
		if err != nil {
			return
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			slog.Warn("trace 写入失败", "err", err)
		}
	}
	for {
		select {
		case tr := <-t.ch:
			write(tr)
		case <-t.done:
			// 冲刷队列中剩余的记录后退出
			for {
				select {
				case tr := <-t.ch:
					write(tr)
				default:
					return
				}
			}
		}
	}
}

// cleanup 执行保留策略：删除超过 30 天的 trace 文件；总量超过 200MB
// 时按日期从旧到新删除直到达标。
func (t *TraceLog) cleanup() {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("trace 目录不可读", "dir", t.dir, "err", err)
		}
		return
	}
	re := regexp.MustCompile(traceFileNameRe)
	var files []os.DirEntry
	var total int64
	for _, e := range entries {
		if e.IsDir() || !re.MatchString(e.Name()) {
			continue
		}
		files = append(files, e)
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	cutoff := t.now().AddDate(0, 0, -t.keepDays)
	// 文件名含日期，按名排序即按时间排序（旧→新）
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	for _, e := range files {
		day, err := time.Parse("20060102", strings.TrimSuffix(strings.TrimPrefix(e.Name(), "trace-"), ".jsonl"))
		if err != nil {
			continue
		}
		dayEnd := day.AddDate(0, 0, 1)
		oversize := total > t.maxBytes
		if dayEnd.Before(cutoff) || oversize {
			path := filepath.Join(t.dir, e.Name())
			if info, err := e.Info(); err == nil {
				total -= info.Size()
			}
			if err := os.Remove(path); err == nil {
				slog.Info("已清理过期 trace", "file", e.Name())
			}
		}
	}
}

// ---------- 落盘读取（CLI trace 用） ----------

// LoadTraceFiles 读取 dir 下所有 trace JSONL，按时间从新到旧返回
// （文件名倒序 + 文件内行倒序）。maxLines<=0 表示不限。
func LoadTraceFiles(dir string, maxLines int) ([]Trace, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && regexp.MustCompile(traceFileNameRe).MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var out []Trace
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var lines []Trace
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var tr Trace
			if err := json.Unmarshal([]byte(line), &tr); err != nil {
				continue // 单行损坏跳过，不中断历史读取
			}
			lines = append(lines, tr)
		}
		_ = f.Close()
		for i := len(lines) - 1; i >= 0; i-- {
			out = append(out, lines[i])
			if maxLines > 0 && len(out) >= maxLines {
				return out, nil
			}
		}
	}
	return out, nil
}

// ---------- 流式 usage 提取 ----------

// usageRe 匹配上游流/非流响应体中的 token 用量字段（chat 与 messages
// 两种协议、流式多个 chunk 时取最后一次出现）。
var usageRe = regexp.MustCompile(
	`"(?:prompt_tokens|input_tokens)"\s*:\s*(\d+)|"(?:completion_tokens|output_tokens)"\s*:\s*(\d+)`)

// usageScanner 从上游字节流中尽力提取 token 用量。只保留尾部小窗口
// 匹配（usage JSON 很短），对流式逐块增量扫描。
type usageScanner struct {
	window []byte
	prompt int64
	compl  int64
}

const usageWindowSize = 8 << 10

func (s *usageScanner) Write(p []byte) (int, error) {
	s.window = append(s.window, p...)
	if len(s.window) > usageWindowSize {
		s.window = s.window[len(s.window)-usageWindowSize:]
	}
	for _, m := range usageRe.FindAllSubmatch(s.window, -1) {
		if m[1] != nil {
			s.prompt = parseDigits(m[1])
		} else if m[2] != nil {
			s.compl = parseDigits(m[2])
		}
	}
	return len(p), nil
}

func parseDigits(b []byte) int64 {
	var v int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return v
		}
		v = v*10 + int64(c-'0')
	}
	return v
}

// usageOf 返回提取到的用量（两者都为 0 时返回 nil）。
func (s *usageScanner) usageOf() *TraceUsage {
	if s.prompt == 0 && s.compl == 0 {
		return nil
	}
	return &TraceUsage{Prompt: s.prompt, Completion: s.compl}
}

// traceID 生成一条 trace 的 ID。
func traceID(now time.Time) string {
	return fmt.Sprintf("t-%d-%04x", now.UnixMilli(), now.Nanosecond()&0xffff)
}

// redactProxy 供 trace 记录代理出口（脱敏）。
func redactProxy(ref string) string {
	if ref == "" {
		return ""
	}
	return netproxy.Redact(ref)
}
