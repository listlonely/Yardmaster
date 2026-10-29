// Package classify 实现任务类型识别：L0 本地启发式 + L1 小模型分类，
// 两层逐级降级；任一层失败不影响请求，绝不因分类而失败（PRD F8）。
//
// 关键约束（F8 规则表）：仅对每个新的用户轮次分类一次；L1 调用自身走
// 网关（享受故障转移与代理）；缓存/冷却在本包实现。
package classify

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Verdict 是一次意图分类结果。Intent/Effort 的取值必须与任务规则
// （F7）的匹配条件使用同一词表，词表常量在 W5 与规则引擎一起定稿。
type Verdict struct {
	Intent     string  // coding / qa / writing / …
	Effort     string  // low / high；空=未判
	Confidence float64 // 0~1
}

// L0 用本地启发式判定消息意图：零成本、不外发、<5ms（F8）。
// 当前为词表种子实现；W5 补齐代码块/文件路径/diff 特征与
// 调用方工具集特征。未命中返回 (零值, false)。
func L0(message string) (Verdict, bool) {
	m := strings.ToLower(message)
	switch {
	case containsAny(m, "代码", "重构", "函数", "bug", "编译", "脚本", "code", "refactor"):
		return Verdict{Intent: "coding", Confidence: 0.6}, true
	case containsAny(m, "写作", "翻译", "润色", "文案", "改写"):
		return Verdict{Intent: "writing", Confidence: 0.6}, true
	default:
		return Verdict{}, false
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------- 用户自定义识别策略（PRD F8） ----------

// maxExamplesPerIntent 是单个自定义意图允许携带的示例句上限（F8 规则表）。
const maxExamplesPerIntent = 5

// IntentDef 是用户自定义的一个意图类别，随设置存储（intents.json），
// 下一个新轮次热生效。任务规则（F7）按 ID 引用这里的意图。
type IntentDef struct {
	ID       string   `json:"id"`       // 稳定 ID；建议命名空间化（如 team/weekly-report）
	Label    string   `json:"label"`    // 展示名
	Keywords []string `json:"keywords"` // 命中词：大小写不敏感子串匹配
	Regexes  []string `json:"regexes"`  // 正则：对原文匹配；编译失败的条目忽略并标注，不阻断其余判定
	Examples []string `json:"examples"` // 示例句：注入 L1 提示词作 few-shot，每类 ≤5 条
	Priority int      `json:"priority"` // 越大越先求值；命中即返回
}

// UserStrategy 是用户自定义识别策略：一组自定义意图定义。
// 求值顺序（设计文档 §4.2）：用户自定义 → 内置 L0 → L1 → 默认组。
type UserStrategy struct {
	Intents []IntentDef `json:"intents"`
}

// EvaluateL0 对 message 求值：意图按 Priority 降序（稳定排序），每个
// 意图先词表（大小写不敏感）后正则（保留原文大小写语义），首个命中
// 即返回。未命中返回 (零值, false)，由调用方回落内置 L0 与 L1。
func (s *UserStrategy) EvaluateL0(message string) (Verdict, bool) {
	if s == nil {
		return Verdict{}, false
	}
	m := strings.ToLower(message)
	ordered := make([]IntentDef, len(s.Intents))
	copy(ordered, s.Intents)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority > ordered[j].Priority })
	for _, def := range ordered {
		for _, kw := range def.Keywords {
			if kw != "" && strings.Contains(m, strings.ToLower(kw)) {
				return Verdict{Intent: def.ID, Confidence: 0.8}, true
			}
		}
		for _, expr := range def.Regexes {
			re, err := regexp.Compile(expr)
			if err != nil {
				continue // 无效正则：忽略该条，不影响同意图的其余特征
			}
			if re.MatchString(message) {
				return Verdict{Intent: def.ID, Confidence: 0.8}, true
			}
		}
	}
	return Verdict{}, false
}

// BuildL1Prompt 生成注入用户词表的 L1 分类提示词：小模型按
// 「内置 + 自定义」意图清单输出一个意图 id，自定义意图附带示例句
// 作为 few-shot（F8 词表注入行）。意图总数超限时由调用方在界面提示
// 准确率风险（>30）。
func BuildL1Prompt(builtin []string, intents []IntentDef, message string) string {
	var b strings.Builder
	b.WriteString("判断下面这条用户消息属于哪个意图，只输出意图 id。\n可选意图：\n")
	for _, id := range builtin {
		fmt.Fprintf(&b, "- %s\n", id)
	}
	for _, def := range intents {
		fmt.Fprintf(&b, "- %s（%s）\n", def.ID, def.Label)
		for i, ex := range def.Examples {
			if i >= maxExamplesPerIntent {
				break
			}
			fmt.Fprintf(&b, "  例：%s\n", ex)
		}
	}
	b.WriteString("\n用户消息：\n" + message + "\n意图 id：")
	return b.String()
}
