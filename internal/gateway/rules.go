// 任务规则引擎（PRD F7）：按「意图 / 正则 / 估算 token / 调用方 key」
// 条件把请求改道到指定目标（provider/model、裸 provider id 或
// group/<id>）。文件内首条命中生效；命中即改道，改道目标再走候选
// 计划与故障转移。
//
// 热生效：mtime 变化即重载（外部/CLI 编辑后下一请求生效）；重载时
// 按 ID 保留进程内累计的 hits 计数。hits 写穿落盘（WriteAtomic）。
package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"yardmaster/internal/store"
)

// Rule 是一条任务规则。条件字段（Intent/Regex/MaxTokens/CallerKey）
// 留空表示不限；全部留空即兜底规则（建议放最后）。
type Rule struct {
	ID        string `json:"id"`
	Desc      string `json:"desc,omitempty"`
	Intent    string `json:"intent,omitempty"`     // 匹配意图 id（内置或自定义）
	Regex     string `json:"regex,omitempty"`      // 对最新用户消息原文匹配
	MaxTokens int    `json:"max_tokens,omitempty"` // 估算输入 token ≤ 此值才命中；0=不限
	CallerKey string `json:"caller_key,omitempty"` // 匹配调用方 key；空=不限
	Target    string `json:"target"`               // 命中后的路由目标（必填）
	Hits      int64  `json:"hits,omitempty"`       // 命中次数（网关写穿）
}

// RulesFile 是 rules.json 的文件形态。
type RulesFile struct {
	Rules []Rule `json:"rules"`
}

// LoadRulesFile 读取规则文件；不存在返回 os.ErrNotExist 供调用方判定。
func LoadRulesFile(path string) (RulesFile, error) {
	var f RulesFile
	if err := store.LoadJSON(path, &f); err != nil {
		return f, err
	}
	return f, nil
}

// SaveRulesFile 原子写入规则文件。
func SaveRulesFile(path string, f RulesFile) error { return store.SaveJSON(path, f) }

// RuleEngine 是规则引擎：加载、匹配、计数落盘。
type RuleEngine struct {
	mu    sync.Mutex
	path  string
	mtime time.Time
	size  int64
	rules []Rule
	res   map[string]*regexp.Regexp // 正则缓存：规则 ID → 编译结果（重载时重建）
	now   func() time.Time
}

// NewRuleEngine 构建引擎；文件不存在或损坏按空规则集运行（读不懂=
// 未知，日志告警，绝不当作「规则必然不命中」以外的语义）。
func NewRuleEngine(path string) *RuleEngine {
	e := &RuleEngine{path: path, now: time.Now}
	e.reload()
	return e
}

// SetNow 注入时钟（测试用）。
func (e *RuleEngine) SetNow(f func() time.Time) { e.now = f }

// reload 重新加载规则文件（调用方已持有 e.mu）。mtime+size 变化才
// 重载；解析失败保留旧规则并告警；按 ID 保留旧 hits。
func (e *RuleEngine) reload() {
	st, err := os.Stat(e.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("规则文件不可访问", "path", e.path, "err", err)
		}
		return
	}
	if st.ModTime().Equal(e.mtime) && st.Size() == e.size {
		return
	}
	f, err := LoadRulesFile(e.path)
	if err != nil {
		slog.Warn("规则文件解析失败，沿用内存中的规则（保留原文件）", "path", e.path, "err", err)
		e.mtime, e.size = st.ModTime(), st.Size()
		return
	}
	old := make(map[string]int64, len(e.rules))
	for _, r := range e.rules {
		old[r.ID] = r.Hits
	}
	for i := range f.Rules {
		if h, ok := old[f.Rules[i].ID]; ok {
			f.Rules[i].Hits = h
		}
	}
	e.rules = f.Rules
	e.mtime, e.size = st.ModTime(), st.Size()
	e.res = make(map[string]*regexp.Regexp, len(f.Rules))
	for i := range e.rules {
		if e.rules[i].Regex == "" {
			continue
		}
		re, err := regexp.Compile(e.rules[i].Regex)
		if err != nil {
			slog.Warn("规则正则编译失败，该条件视为不命中", "rule", e.rules[i].ID, "err", err)
			continue
		}
		e.res[e.rules[i].ID] = re
	}
	slog.Info("已加载任务规则", "count", len(e.rules), "path", e.path)
}

// Match 按文件顺序求值，返回首条命中的规则（副本）。无命中返回 false。
// 条件求值：非空字段必须全部满足；无效正则视为该条件不命中。
func (e *RuleEngine) Match(callerKey, intent, lastMsg string, estTokens int) (Rule, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reload()
	for _, r := range e.rules {
		if r.Target == "" {
			continue // 无目标=配置不完整，跳过
		}
		if r.Intent != "" && r.Intent != intent {
			continue
		}
		if r.CallerKey != "" && r.CallerKey != callerKey {
			continue
		}
		if r.MaxTokens > 0 && estTokens > r.MaxTokens {
			continue
		}
		if r.Regex != "" {
			re := e.res[r.ID]
			if re == nil || !re.MatchString(lastMsg) {
				continue
			}
		}
		r.Hits++
		e.rules[findRuleIdx(e.rules, r.ID)].Hits = r.Hits
		if err := SaveRulesFile(e.path, RulesFile{Rules: e.rules}); err != nil {
			slog.Warn("规则命中计数落盘失败（不影响本次路由）", "rule", r.ID, "err", err)
		}
		return r, true
	}
	return Rule{}, false
}

func findRuleIdx(rules []Rule, id string) int {
	for i := range rules {
		if rules[i].ID == id {
			return i
		}
	}
	return -1
}

// Snapshot 返回规则概要（/api/state 用）：id/desc/target/hits。
func (e *RuleEngine) Snapshot() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reload()
	out := make([]map[string]any, 0, len(e.rules))
	for _, r := range e.rules {
		out = append(out, map[string]any{
			"id": r.ID, "desc": r.Desc, "target": r.Target, "hits": r.Hits,
		})
	}
	return out
}

// ---------- 请求特征提取 ----------

// callerKeyOf 提取调用方标识 key：x-api-key 优先，其次 Authorization
// Bearer（F10 的 gw-<agent> 命名是约定，网关不强制）。
func callerKeyOf(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// inputStats 提取规则与分类所需的最小输入信息：最新一条用户消息文本
// 与估算输入 token（字符数 ÷2，中英混排近似值，见 W5 设计决策）。
func inputStats(clientReq any) (lastUser string, estTokens int) {
	var runes int
	switch cr := clientReq.(type) {
	case *MessagesRequest:
		runes += len([]rune(contentText(cr.System)))
		for _, m := range cr.Messages {
			t := contentText(m.Content)
			runes += len([]rune(t))
			if m.Role == "user" && strings.TrimSpace(t) != "" {
				lastUser = t // 取最后一条非空用户消息（F8：仅对新轮次分类）
			}
		}
	case *ChatRequest:
		for _, m := range cr.Messages {
			t := contentText(m.Content)
			runes += len([]rune(t))
			if m.Role == "user" && strings.TrimSpace(t) != "" {
				lastUser = t
			}
		}
	case *ResponsesRequest: // M4-W1：instructions 计入长度估计
		runes += len([]rune(cr.Instructions))
		runes += len([]rune(responsesInputText(cr.Input)))
		if t := responsesLastUser(cr.Input); strings.TrimSpace(t) != "" {
			lastUser = t
		}
	case *GeminiRequest: // M4-W3：systemInstruction 计入长度估计
		if cr.SystemInstruction != nil {
			runes += len([]rune(geminiPartsText(cr.SystemInstruction.Parts)))
		}
		for _, c := range cr.Contents {
			t := geminiPartsText(c.Parts)
			runes += len([]rune(t))
			if c.Role != "model" && strings.TrimSpace(t) != "" {
				lastUser = t
			}
		}
	}
	return lastUser, runes / 2
}

// L1SelfHeader 标记网关内部发起的 L1 分类调用：网关看到该头时跳过
// 分类与规则（防止分类调用再次触发分类的递归）。外部伪造最多使该
// 请求跳过规则覆盖，不触碰安全基线（仅回环监听）。
const L1SelfHeader = "X-Gw-Internal"

// L1SelfValue 是内部标记的取值。
const L1SelfValue = "l1"
