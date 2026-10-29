// 路由相关 CLI（W5）：rules（任务规则）、keys（调用方默认模型绑定）、
// intents（自定义意图词表）。这些文件网关运行中也会读取（热生效）；
// 但 rules.json 的 hits 由网关写穿，编辑规则建议在网关停止时进行，
// 避免计数互相覆盖。
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"agenthub/internal/classify"
	"agenthub/internal/gateway"
	"agenthub/internal/store"
)

// ---------- rules ----------

func runRules(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: agenthub rules <list|add|rm> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return rulesList(paths, stdout)
	case "add":
		return rulesAdd(paths, args[1:], stdout, stderr)
	case "rm":
		return rulesRemove(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：list、add、rm\n", args[0])
		return ErrUsage
	}
}

func rulesList(paths store.Paths, stdout io.Writer) error {
	f, err := gateway.LoadRulesFile(paths.RulesPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stdout, "（无任务规则；用 `agenthub rules add` 添加）")
			return nil
		}
		return err
	}
	if len(f.Rules) == 0 {
		fmt.Fprintln(stdout, "（无任务规则；用 `agenthub rules add` 添加）")
		return nil
	}
	for _, r := range f.Rules {
		cond := ruleCondSummary(r)
		fmt.Fprintf(stdout, "%-20s → %-40s 命中 %d 次\n", r.ID, r.Target, r.Hits)
		if r.Desc != "" {
			fmt.Fprintf(stdout, "  说明: %s\n", r.Desc)
		}
		fmt.Fprintf(stdout, "  条件: %s\n", cond)
	}
	return nil
}

func ruleCondSummary(r gateway.Rule) string {
	var parts []string
	if r.Intent != "" {
		parts = append(parts, "intent="+r.Intent)
	}
	if r.Regex != "" {
		parts = append(parts, "regex="+r.Regex)
	}
	if r.MaxTokens > 0 {
		parts = append(parts, fmt.Sprintf("max_tokens<=%d", r.MaxTokens))
	}
	if r.CallerKey != "" {
		parts = append(parts, "caller="+r.CallerKey)
	}
	if len(parts) == 0 {
		return "（无条件=兜底）"
	}
	return strings.Join(parts, " ")
}

func rulesAdd(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rules add", flag.ContinueOnError)
	id := fs.String("id", "", "规则 ID（唯一）")
	desc := fs.String("desc", "", "说明")
	intent := fs.String("intent", "", "匹配意图（内置 coding/qa/writing 或自定义意图 id）")
	regex := fs.String("regex", "", "匹配最新用户消息的正则")
	maxTokens := fs.Int("max-tokens", 0, "估算输入 token 上限（0=不限）")
	caller := fs.String("caller", "", "匹配调用方 key（如 gw-cline）")
	target := fs.String("target", "", "命中后目标：provider/model 或 group/<id>")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *id == "" || *target == "" {
		fmt.Fprintln(stderr, "缺少必填参数：-id、-target")
		return ErrUsage
	}
	if *regex != "" {
		if _, err := regexp.Compile(*regex); err != nil {
			fmt.Fprintf(stderr, "-regex 不是合法正则: %v\n", err)
			return ErrUsage
		}
	}
	f, err := gateway.LoadRulesFile(paths.RulesPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, r := range f.Rules {
		if r.ID == *id {
			fmt.Fprintf(stderr, "规则 %q 已存在（先 rm 再 add）\n", *id)
			return ErrUsage
		}
	}
	f.Rules = append(f.Rules, gateway.Rule{
		ID: *id, Desc: *desc, Intent: *intent, Regex: *regex,
		MaxTokens: *maxTokens, CallerKey: *caller, Target: *target,
	})
	if err := gateway.SaveRulesFile(paths.RulesPath(), f); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已添加规则 %s → %s（网关运行中热生效）\n", *id, *target)
	return nil
}

func rulesRemove(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: agenthub rules rm <rule-id>")
		return ErrUsage
	}
	f, err := gateway.LoadRulesFile(paths.RulesPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "规则文件不存在")
			return ErrUsage
		}
		return err
	}
	kept := f.Rules[:0]
	found := false
	for _, r := range f.Rules {
		if r.ID == args[0] {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		fmt.Fprintf(stderr, "规则 %q 不存在\n", args[0])
		return ErrUsage
	}
	f.Rules = kept
	if err := gateway.SaveRulesFile(paths.RulesPath(), f); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已删除规则 %s\n", args[0])
	return nil
}

// ---------- keys ----------

func runKeys(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: agenthub keys <list|bind|unbind> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return keysList(paths, stdout)
	case "bind":
		return keysBind(paths, args[1:], stdout, stderr)
	case "unbind":
		return keysUnbind(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：list、bind、unbind\n", args[0])
		return ErrUsage
	}
}

func keysList(paths store.Paths, stdout io.Writer) error {
	f, err := gateway.LoadKeysFile(paths.KeysPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stdout, "（无绑定；用 `agenthub keys bind` 添加）")
			return nil
		}
		return err
	}
	if len(f.Bindings) == 0 {
		fmt.Fprintln(stdout, "（无绑定；用 `agenthub keys bind` 添加）")
		return nil
	}
	for _, b := range f.Bindings {
		fmt.Fprintf(stdout, "%-28s → %s\n", b.Key, b.DefaultModel)
	}
	return nil
}

func keysBind(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys bind", flag.ContinueOnError)
	key := fs.String("key", "", "调用方 key（约定 gw-<agent> 命名）")
	model := fs.String("model", "", "默认目标：provider/model 或 group/<id>")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *key == "" || *model == "" {
		fmt.Fprintln(stderr, "缺少必填参数：-key、-model")
		return ErrUsage
	}
	f, err := gateway.LoadKeysFile(paths.KeysPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := range f.Bindings {
		if f.Bindings[i].Key == *key {
			f.Bindings[i].DefaultModel = *model // upsert
			if err := gateway.SaveKeysFile(paths.KeysPath(), f); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "已更新 %s → %s\n", *key, *model)
			return nil
		}
	}
	f.Bindings = append(f.Bindings, gateway.KeyBinding{Key: *key, DefaultModel: *model})
	if err := gateway.SaveKeysFile(paths.KeysPath(), f); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已绑定 %s → %s\n", *key, *model)
	return nil
}

func keysUnbind(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: agenthub keys unbind <key>")
		return ErrUsage
	}
	f, err := gateway.LoadKeysFile(paths.KeysPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "绑定文件不存在")
			return ErrUsage
		}
		return err
	}
	kept := f.Bindings[:0]
	found := false
	for _, b := range f.Bindings {
		if b.Key == args[0] {
			found = true
			continue
		}
		kept = append(kept, b)
	}
	if !found {
		fmt.Fprintf(stderr, "key %q 未绑定\n", args[0])
		return ErrUsage
	}
	f.Bindings = kept
	if err := gateway.SaveKeysFile(paths.KeysPath(), f); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已解绑 %s\n", args[0])
	return nil
}

// ---------- intents ----------

func runIntents(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: agenthub intents <list|add> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return intentsList(paths, stdout)
	case "add":
		return intentsAdd(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：list、add\n", args[0])
		return ErrUsage
	}
}

func loadIntents(paths store.Paths) (classify.UserStrategy, error) {
	var s classify.UserStrategy
	if err := store.LoadJSON(paths.IntentsPath(), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	return s, nil
}

func intentsList(paths store.Paths, stdout io.Writer) error {
	s, err := loadIntents(paths)
	if err != nil {
		return err
	}
	if len(s.Intents) == 0 {
		fmt.Fprintln(stdout, "（无自定义意图；用 `agenthub intents add` 添加）")
		return nil
	}
	for _, d := range s.Intents {
		fmt.Fprintf(stdout, "%-24s %-12s 优先级 %d\n", d.ID, d.Label, d.Priority)
		if len(d.Keywords) > 0 {
			fmt.Fprintf(stdout, "  词表: %s\n", strings.Join(d.Keywords, ", "))
		}
		if len(d.Regexes) > 0 {
			fmt.Fprintf(stdout, "  正则: %s\n", strings.Join(d.Regexes, ", "))
		}
		if len(d.Examples) > 0 {
			fmt.Fprintf(stdout, "  示例: %s\n", strings.Join(d.Examples, " / "))
		}
	}
	return nil
}

func intentsAdd(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("intents add", flag.ContinueOnError)
	id := fs.String("id", "", "意图 id（任务规则用 -intent 引用；建议命名空间化如 team/weekly）")
	label := fs.String("label", "", "展示名")
	keywords := fs.String("keywords", "", "命中词，逗号分隔（大小写不敏感）")
	regexes := fs.String("regexes", "", "正则，逗号分隔（对原文匹配）")
	examples := fs.String("examples", "", "示例句，逗号分隔（注入 L1 提示词，最多 5 条）")
	priority := fs.Int("priority", 0, "优先级（越大越先求值）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *id == "" {
		fmt.Fprintln(stderr, "缺少必填参数：-id")
		return ErrUsage
	}
	if *keywords == "" && *regexes == "" && *examples == "" {
		fmt.Fprintln(stderr, "至少提供一种特征：-keywords、-regexes 或 -examples")
		return ErrUsage
	}
	regexList := splitCSV(*regexes)
	for _, re := range regexList {
		if _, err := regexp.Compile(re); err != nil {
			fmt.Fprintf(stderr, "-regexes 含非法正则 %q: %v\n", re, err)
			return ErrUsage
		}
	}
	s, err := loadIntents(paths)
	if err != nil {
		return err
	}
	for _, d := range s.Intents {
		if d.ID == *id {
			fmt.Fprintf(stderr, "意图 %q 已存在（编辑 intents.json 或先移除同名定义）\n", *id)
			return ErrUsage
		}
	}
	s.Intents = append(s.Intents, classify.IntentDef{
		ID:       *id,
		Label:    *label,
		Keywords: splitCSV(*keywords),
		Regexes:  regexList,
		Examples: splitCSV(*examples),
		Priority: *priority,
	})
	if err := store.SaveJSON(paths.IntentsPath(), s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已添加意图 %s（intents.json 热生效，下一新轮次起作用）\n", *id)
	return nil
}

// splitCSV 拆分逗号分隔参数（去空白、去空项）。W5 简化：正则含逗号
// 时请改用词表特征。
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
