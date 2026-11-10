// M6-W4 任务模板测试：占位实例化（缺参/多参 fail-closed、字面量大括号
// 不误伤）、配置校验闸、模板超时覆盖。假 Agent 复用 runner_test 的
// TestHelperProcess。
package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTemplateInstantiate(t *testing.T) {
	tmpl := Template{Name: "greet", Task: "向 {name} 问好，主题是 {topic}，{name} 会出现两次"}

	// 正常实例化：多占位、重复占位全部替换。
	got, err := tmpl.Instantiate(map[string]string{"name": "世界", "topic": "测试"})
	if err != nil {
		t.Fatal(err)
	}
	want := "向 世界 问好，主题是 测试，世界 会出现两次"
	if got != want {
		t.Fatalf("实例化: %q ≠ %q", got, want)
	}

	// 缺参 fail-closed：列出全部缺失占位。
	if _, err := tmpl.Instantiate(map[string]string{"name": "世界"}); err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("缺参应报错且列出缺失: %v", err)
	}

	// 多传键 fail-closed（防手误）。
	if _, err := tmpl.Instantiate(map[string]string{"name": "a", "topic": "b", "deesc": "x"}); err == nil || !strings.Contains(err.Error(), "deesc") {
		t.Fatalf("多传键应报错: %v", err)
	}

	// 无占位模板：空 params 合法；字面量大括号不识别为占位、原样保留。
	lit := Template{Name: "lit", Task: `输出 JSON: {} 与 {not a name} 保持原样`}
	got, err = lit.Instantiate(nil)
	if err != nil || got != lit.Task {
		t.Fatalf("字面量大括号应原样保留: %q, %v", got, err)
	}
}

func TestValidateDispatchTemplatesFailClosed(t *testing.T) {
	okAgent := AgentSpec{ID: "fake", Command: []string{"a"}}
	cases := []struct {
		name string
		f    DispatchFile
		want string
	}{
		{"模板名空", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Task: "x"}}}, "name 不能为空"},
		{"模板名重复", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Name: "t", Task: "x"}, {Name: "t", Task: "y"}}}, "重复"},
		{"任务空", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Name: "t", Task: "  "}}}, "task 不能为空"},
		{"agent 引用缺失", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Name: "t", Task: "x", Agent: "ghost"}}}, "不存在的 agent"},
		{"超时非法", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Name: "t", Task: "x", Timeout: "abc"}}}, "timeout 非法"},
		{"超时非正", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Name: "t", Task: "x", Timeout: "0s"}}}, "timeout 非法"},
		{"合法通过", DispatchFile{Agents: []AgentSpec{okAgent}, Templates: []Template{{Name: "t", Task: "x", Agent: "fake", Timeout: "90s"}}}, ""},
	}
	for _, c := range cases {
		err := ValidateDispatch(c.f)
		if c.want == "" {
			if err != nil {
				t.Fatalf("%s: 意外失败: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want 含 %q", c.name, err, c.want)
		}
	}
}

// 模板超时覆盖所选 agent 的超时（调用方把模板 timeout 写入 AgentSpec 后
// 经 Submit 的 Runtime 生效）：slow 假 Agent（默认 10m）被 1s 覆盖杀掉。
func TestTemplateTimeoutOverride(t *testing.T) {
	spec := helperAgent(t, "slow")
	agent := AgentSpec{ID: "fake", Command: spec.Command, Env: spec.Env, Timeout: "1s"} // 模板覆盖后的形态
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{agent}, Templates: []Template{
		{Name: "t", Task: "慢任务", Agent: "fake", Timeout: "1s"},
	}})
	start := time.Now()
	sess, err := e.Submit(context.Background(), e.Agents()[0], "慢任务", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.Wait()
	snap, _ := e.Snapshot(sess.ID)
	if snap.Result == nil || snap.Result.Outcome != OutcomeTimeout {
		t.Fatalf("应为 timeout: %+v", snap.Result)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("超时覆盖未生效（耗时 %v）", time.Since(start))
	}
	// 配置校验闸：同一模板合法；engine.FindTemplate 可取回。
	tmpl, err := e.FindTemplate("t")
	if err != nil || tmpl.Task != "慢任务" {
		t.Fatalf("FindTemplate: %+v, %v", tmpl, err)
	}
	if _, err := e.FindTemplate("ghost"); err == nil {
		t.Fatal("未知模板应报错")
	}
}
