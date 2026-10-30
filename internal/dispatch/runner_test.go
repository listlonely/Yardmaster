// headless 原语测试：TestHelperProcess 模式（以测试二进制自身扮演假
// Agent，不碰真机 Agent、不碰网络）。
package dispatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// helperAgent 以指定行为重启测试二进制自身作为假 Agent（行为经
// GO_FAKE_BEHAVIOR 环境变量注入，避免与测试框架参数混淆）。
func helperAgent(t *testing.T, behavior string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		ID:      "fake",
		Command: []string{exe, "-test.run=TestHelperProcess", "--"},
		Env:     []string{"GO_WANT_HELPER_PROCESS=1", "GO_FAKE_BEHAVIOR=" + behavior},
	}
}

// TestHelperProcess 不是真正的测试：作为假 Agent 子进程运行。
// GO_WANT_HELPER_PROCESS 未设置时直接返回（go test 正常执行态）。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	behavior := os.Getenv("GO_FAKE_BEHAVIOR")
	task, _ := io.ReadAll(os.Stdin)
	switch behavior {
	case "ok":
		fmt.Printf("收到任务(%d字节)\n", len(task))
		fmt.Println("结果是 42")
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: 上游拒绝了")
		os.Exit(3)
	case "slow":
		fmt.Println("开始")
		time.Sleep(30 * time.Second)
		fmt.Println("结束")
	case "stream":
		for i := 0; i < 5; i++ {
			fmt.Printf("chunk-%d\n", i)
		}
	}
	os.Exit(0)
}

func TestRunOKStreamsStdout(t *testing.T) {
	var sb strings.Builder
	res := Run(context.Background(), helperAgent(t, "ok"), "算一下 6*7", "", &sb)
	if res.Outcome != OutcomeOK || res.ExitCode != 0 {
		t.Fatalf("outcome=%s exit=%d err=%s", res.Outcome, res.ExitCode, res.ErrText)
	}
	if !strings.Contains(sb.String(), "结果是 42") || !strings.Contains(res.Output, "收到任务(13字节)") {
		t.Fatalf("stdout 流式/尾部采集异常: stream=%q tail=%q", sb.String(), res.Output)
	}
	if res.DurationMS < 0 {
		t.Fatalf("duration 异常: %d", res.DurationMS)
	}
}

func TestRunNonZeroExitCarriesStderr(t *testing.T) {
	res := Run(context.Background(), helperAgent(t, "fail"), "task", "", nil)
	if res.Outcome != OutcomeExit || res.ExitCode != 3 {
		t.Fatalf("outcome=%s exit=%d", res.Outcome, res.ExitCode)
	}
	if !strings.Contains(res.ErrText, "boom") {
		t.Fatalf("stderr 摘要缺失: %q", res.ErrText)
	}
	if code := res.ExitCodeOf(); code != 3 {
		t.Fatalf("透传退出码应为 3: %d", code)
	}
}

func TestRunTimeoutKills(t *testing.T) {
	spec := helperAgent(t, "slow")
	spec.Timeout = 200 * time.Millisecond
	start := time.Now()
	res := Run(context.Background(), spec, "task", "", nil)
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("outcome=%s（应 timeout）", res.Outcome)
	}
	if code := res.ExitCodeOf(); code != ExitTimeout {
		t.Fatalf("超时约定退出码应为 124: %d", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时未及时杀进程: %v", elapsed)
	}
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	spec := helperAgent(t, "slow")
	res := Run(ctx, spec, "task", "", nil)
	if res.Outcome != OutcomeCancelled {
		t.Fatalf("outcome=%s（应 cancelled）", res.Outcome)
	}
}

func TestRunLaunchFailNotFound(t *testing.T) {
	res := Run(context.Background(), Spec{ID: "nope", Command: []string{"definitely-not-a-real-binary-xyz"}}, "t", "", nil)
	if res.Outcome != OutcomeLaunch {
		t.Fatalf("outcome=%s（应 launch）", res.Outcome)
	}
	if !strings.Contains(res.ErrText, "未找到可执行文件") {
		t.Fatalf("启动失败提示异常: %q", res.ErrText)
	}
	if res.ExitCodeOf() != ExitLaunchFail {
		t.Fatalf("启动失败约定退出码应为 127: %d", res.ExitCodeOf())
	}
}

func TestRunModelPlaceholder(t *testing.T) {
	// ok 行为回显任务字节即可验证；占位展开以 expandArgv 单测覆盖。
	got := expandArgv([]string{"a", "--model={model}", "{model}-{model}", "{}"}, "m1")
	want := []string{"a", "--model=m1", "m1-m1", "{}"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("占位展开不符: %v", got)
	}
}

func TestRunStreamPassthroughChunks(t *testing.T) {
	var sb strings.Builder
	res := Run(context.Background(), helperAgent(t, "stream"), "t", "", &sb)
	if res.Outcome != OutcomeOK {
		t.Fatalf("outcome=%s", res.Outcome)
	}
	if strings.Count(sb.String(), "chunk-") != 5 {
		t.Fatalf("流式块数不符: %q", sb.String())
	}
}
