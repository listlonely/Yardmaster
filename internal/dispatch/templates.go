// 任务模板（M6-W4）：dispatch.json `templates[]`——命名任务 + {param}
// 占位 + 默认 agent/超时。模板是任务文本的参数化预制（产品易用性，
// 开发文档 §9.7-W4），不改匹配/并发/持久化语义。
//
// fail-closed 纪律：模板引用的 agent 必须存在（校验闸拦截）；占位缺失
// 或多传键均拒绝实例化（不猜参数、防手误）；形如 {xxx} 但名字不含法
// 的（如 JSON 字面量 {}）不识别为占位、原样保留——与 argv 展开的
// 「未知占位不猜语义」同款纪律。
package dispatch

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Template 是 dispatch.json 的一个任务模板条目（JSON 友好形态）。
type Template struct {
	Name    string `json:"name"`              // 唯一标识（CLI -t / 控制台下拉）
	Task    string `json:"task"`              // 任务文本，含 {param} 占位
	Agent   string `json:"agent,omitempty"`   // 默认 agent id（空=按 match/default）
	Model   string `json:"model,omitempty"`   // 默认 {model} 占位替换值
	Timeout string `json:"timeout,omitempty"` // 覆盖所选 agent 的超时（"90s"）
}

// placeholderRe 是任务文本中的合法占位名（字母/数字/下划线/连字符）。
var placeholderRe = regexp.MustCompile(`\{([A-Za-z0-9_-]+)\}`)

// PlaceholderNames 提取任务文本中的占位名（按首次出现序去重）。
func PlaceholderNames(task string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(task, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Instantiate 用 params 实例化模板任务文本。fail-closed：缺占位报错
// （列出缺失名）；多传键报错（防手误——如把 desc 写成 deesc）。
func (t Template) Instantiate(params map[string]string) (string, error) {
	names := PlaceholderNames(t.Task)
	missing := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := params[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("模板 %s 缺少参数: %s（占位 {%s}）",
			t.Name, strings.Join(missing, ", "), strings.Join(missing, "}/{"))
	}
	valid := map[string]bool{}
	for _, n := range names {
		valid[n] = true
	}
	var extra []string
	for k := range params {
		if !valid[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		return "", fmt.Errorf("模板 %s 不接受参数: %s（合法占位: %s）",
			t.Name, strings.Join(extra, ", "), orNone(names))
	}
	task := t.Task
	for _, n := range names { // 逐键替换（键已验合法，不会误伤字面量大括号）
		task = strings.ReplaceAll(task, "{"+n+"}", params[n])
	}
	return task, nil
}

// TimeoutOrDefault 返回模板超时的运行态值（0=不覆盖，沿用所选 agent 的）。
func (t Template) TimeoutOrDefault() (time.Duration, error) {
	if t.Timeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(t.Timeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("模板 %s 的 timeout 非法: %q", t.Name, t.Timeout)
	}
	return d, nil
}

// ValidateTemplates 校验模板清单（写入口与加载共用）：名非空唯一、
// 任务非空、agent 引用存在、timeout 可解析。
func ValidateTemplates(ts []Template, agents []AgentSpec) error {
	agentIDs := map[string]bool{}
	for _, a := range agents {
		agentIDs[a.ID] = true
	}
	seen := map[string]bool{}
	for _, t := range ts {
		if t.Name == "" {
			return fmt.Errorf("模板 name 不能为空（task: %q）", summary(t.Task))
		}
		if seen[t.Name] {
			return fmt.Errorf("模板 name 重复: %s", t.Name)
		}
		seen[t.Name] = true
		if strings.TrimSpace(t.Task) == "" {
			return fmt.Errorf("模板 %s 的 task 不能为空", t.Name)
		}
		if t.Agent != "" && !agentIDs[t.Agent] {
			return fmt.Errorf("模板 %s 引用不存在的 agent: %s", t.Name, t.Agent)
		}
		if _, err := t.TimeoutOrDefault(); err != nil {
			return err
		}
	}
	return nil
}

func orNone(ss []string) string {
	if len(ss) == 0 {
		return "无"
	}
	return strings.Join(ss, ", ")
}
