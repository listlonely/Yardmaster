// Package dispatch 实现任务→Agent 派发的进程原语与引擎（M5-W3/W4）。
//
// W3 交付 headless 启动原语（Run）：任务文本经 stdin 传入用户已安装的
// Agent 进程，stdout 流式转发与尾部采集，超时/取消杀进程，退出码归一
// 为 Outcome。ToS 边界：只启动用户已安装的 Agent，不代装、不代登录。
package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout 是 headless 会话的缺省超时。
const DefaultTimeout = 10 * time.Minute

// OutputCap 是会话详情留存的 stdout 尾部上限（CLI 透传不受限）。
const OutputCap = 64 << 10

// ErrCap 是 stderr 失败摘要上限。
const ErrCap = 2 << 10

// Spec 描述一个 headless Agent 的启动方式。
type Spec struct {
	ID            string        `json:"id"`                       // 唯一标识（trace/会话引用）
	Command       []string      `json:"command"`                  // argv 模板；占位 {model}（任务文本恒走 stdin，不进 argv）
	WorkDir       string        `json:"work_dir,omitempty"`       // 空 = 当前目录
	Env           []string      `json:"env,omitempty"`            // 附加环境变量（KEY=VAL；继承父进程环境）
	Timeout       time.Duration `json:"timeout,omitempty"`        // 0 = DefaultTimeout
	MaxConcurrent int           `json:"max_concurrent,omitempty"` // 0 = 1
}

// Outcome 是一次 headless 会话的归一结局。
type Outcome string

const (
	OutcomeOK        Outcome = "ok"        // 退出码 0
	OutcomeExit      Outcome = "exit"      // 非 0 退出（ErrText 摘要 stderr）
	OutcomeTimeout   Outcome = "timeout"   // 超时被杀
	OutcomeCancelled Outcome = "cancelled" // 上级取消
	OutcomeLaunch    Outcome = "launch"    // 启动失败（二进制不存在等）
)

// Result 是一次 headless 会话的结果（stdout 尾部 ≤OutputCap）。
type Result struct {
	Agent      string  `json:"agent"`
	Outcome    Outcome `json:"outcome"`
	ExitCode   int     `json:"exit_code,omitempty"` // launch/timeout/cancelled 为 0（以 Outcome 区分）
	Output     string  `json:"output,omitempty"`    // stdout 尾部
	ErrText    string  `json:"err,omitempty"`       // stderr 尾部摘要
	DurationMS int64   `json:"duration_ms"`
}

// exitCodeConventions 是 CLI 透传场景的约定退出码（GNU timeout 对齐）。
const (
	ExitOK         = 0
	ExitLaunchFail = 127 // 启动失败
	ExitTimeout    = 124 // 超时
	ExitCancelled  = 130 // SIGINT 取消
)

// Run 启动一次 headless 会话：stdin 写任务文本后关闭；stdout 逐块复制到
// out（可为 nil，同时截尾留存）；超时（spec.Timeout 或缺省）或 ctx 取消
// 时杀进程。进程树边界：仅杀直接子进程（跨平台纯 Go 约束，孙进程残留
// 为已知边界——设计稿 §会话管理）。
func Run(ctx context.Context, spec Spec, task, model string, out io.Writer) Result {
	started := time.Now()
	res := Result{Agent: spec.ID}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := expandArgv(spec.Command, model)
	if len(argv) == 0 {
		res.Outcome, res.ErrText, res.DurationMS = OutcomeLaunch, "命令为空", msSince(started)
		return res
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	cmd.Stdin = strings.NewReader(task)

	stdout := newTailWriter(out)
	stderr := newTailWriter(nil)
	cmd.Stdout, cmd.Stderr = stdout, stderr

	if err := cmd.Start(); err != nil {
		res.Outcome, res.ErrText, res.DurationMS = OutcomeLaunch, launchErrText(err), msSince(started)
		return res
	}
	// ctx 到期时 exec 自动 Kill；Wait 归还资源并给出退出状态。
	waitErr := cmd.Wait()

	stdout.mu.Lock()
	res.Output = string(stdout.tail)
	stdout.mu.Unlock()
	stderr.mu.Lock()
	res.ErrText = snippet(stderr.tail, ErrCap)
	stderr.mu.Unlock()
	res.DurationMS = msSince(started)

	switch {
	case ctx.Err() == nil && waitErr == nil:
		res.Outcome, res.ExitCode = OutcomeOK, 0
	case ctx.Err() == nil: // 非 0 退出
		res.Outcome, res.ExitCode = OutcomeExit, exitCodeOf(waitErr)
		res.ErrText = orDefault(res.ErrText, waitErr.Error())
	case errors.Is(ctx.Err(), context.Canceled):
		res.Outcome = OutcomeCancelled
	default: // DeadlineExceeded
		res.Outcome, res.ExitCode = OutcomeTimeout, ExitTimeout
	}
	return res
}

// ExitCodeOf 返回 CLI 透传约定的退出码（dispatch run 用）。
func (r Result) ExitCodeOf() int {
	switch r.Outcome {
	case OutcomeOK:
		return ExitOK
	case OutcomeTimeout:
		return ExitTimeout
	case OutcomeCancelled:
		return ExitCancelled
	case OutcomeLaunch:
		return ExitLaunchFail
	default: // OutcomeExit：透传 Agent 自身退出码
		return r.ExitCode
	}
}

// expandArgv 展开 argv 模板占位（{model}；未知占位原样保留——不猜语义）。
func expandArgv(argv []string, model string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, "{model}", model)
	}
	return out
}

// tailWriter 逐块转发到 out（可 nil）并保留尾部 cap 字节。
type tailWriter struct {
	mu   sync.Mutex
	tail []byte
	cap  int
	out  io.Writer
}

func newTailWriter(out io.Writer) *tailWriter {
	return &tailWriter{cap: OutputCap, out: out}
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.tail = append(t.tail, p...)
	if len(t.tail) > t.cap {
		t.tail = t.tail[len(t.tail)-t.cap:]
	}
	t.mu.Unlock()
	if t.out != nil {
		_, _ = t.out.Write(p)
	}
	return len(p), nil
}

func launchErrText(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) && errors.Is(err, exec.ErrNotFound) {
		return fmt.Sprintf("未找到可执行文件 %s（仅支持启动本机已安装的 Agent，不代装）", ee.Name)
	}
	return snippet([]byte(err.Error()), ErrCap)
}

func exitCodeOf(waitErr error) int {
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func snippet(b []byte, cap int) string {
	s := string(bytes.TrimSpace(b))
	if len(s) > cap {
		s = s[len(s)-cap:]
	}
	return s
}

func msSince(t time.Time) int64 { return time.Since(t).Milliseconds() }

func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
