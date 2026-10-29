// provider 子命令（PRD F2 的 CLI 形态）：add/list/probe/remove。
// UI 向导后续接入同一持久化与预检逻辑。
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"yardmaster/internal/gateway"
	"yardmaster/internal/netproxy"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// providers.json 的文件形态统一在 store.ProvidersFile（gateway 用量
// 落盘共用同一 schema）。

// loadProvidersFile 供 CLI 编辑用：文件不存在视为空配置。
func loadProvidersFile(paths store.Paths) (store.ProvidersFile, error) {
	f, err := store.LoadProvidersFile(paths.ProvidersPath())
	if errors.Is(err, os.ErrNotExist) {
		return store.ProvidersFile{}, nil
	}
	return f, err
}

func saveProvidersFile(paths store.Paths, cfg store.ProvidersFile) error {
	return store.SaveProvidersFile(paths.ProvidersPath(), cfg)
}

func runProvider(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: yardmaster provider <add|list|probe|quota|remove> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return providerAdd(paths, args[1:], stdout, stderr)
	case "list":
		return providerList(paths, stdout)
	case "probe":
		return providerProbe(paths, args[1:], stdout, stderr)
	case "quota":
		return providerQuota(paths, args[1:], stdout, stderr)
	case "remove":
		return providerRemove(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：add、list、probe、quota、remove\n", args[0])
		return ErrUsage
	}
}

// providerQuota 设置账号当日额度（smart 策略的排序数据源；0=清除）。
// UsedTokens 由网关按响应 usage 累计（W6）。
func providerQuota(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "用法: yardmaster provider quota <provider-id> <账号名> <当日额度tokens|0清除>")
		return ErrUsage
	}
	quota, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || quota < 0 {
		fmt.Fprintln(stderr, "额度须为非负整数（0=清除）")
		return ErrUsage
	}
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].ID != args[0] {
			continue
		}
		for j := range cfg.Providers[i].Accounts {
			if cfg.Providers[i].Accounts[j].Name != args[1] {
				continue
			}
			acc := &cfg.Providers[i].Accounts[j]
			acc.QuotaTokensDay = quota
			if quota == 0 {
				acc.UsedTokens, acc.UsageDay = 0, ""
			}
			if err := saveProvidersFile(paths, cfg); err != nil {
				return err
			}
			if quota == 0 {
				fmt.Fprintf(stdout, "已清除 %s/%s 的当日额度\n", args[0], args[1])
			} else {
				fmt.Fprintf(stdout, "已设置 %s/%s 当日额度 %d tokens（重启 serve 生效）\n", args[0], args[1], quota)
			}
			return nil
		}
		fmt.Fprintf(stderr, "provider %q 下没有账号 %q\n", args[0], args[1])
		return ErrUsage
	}
	fmt.Fprintf(stderr, "provider %q 不存在\n", args[0])
	return ErrUsage
}

func providerAdd(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	id := fs.String("id", "", "provider ID（如 deepseek）")
	name := fs.String("name", "", "显示名（默认同 ID）")
	protocol := fs.String("protocol", "openai-chat", "openai-chat | anthropic-messages")
	baseURL := fs.String("base-url", "", "上游完整前缀（如 https://api.deepseek.com）")
	key := fs.String("key", "", "API key（必填，仅存本机）")
	account := fs.String("account", "main", "账号名")
	proxy := fs.String("proxy", "", "账号级代理（http/https/socks5/socks5h/direct）")
	providerProxy := fs.String("provider-proxy", "", "Provider 级代理（账号未配置时继承）")
	replace := fs.Bool("replace", false, "账号已存在时更新其凭证与代理")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *id == "" || *baseURL == "" || *key == "" {
		fmt.Fprintln(stderr, "缺少必填参数：-id、-base-url、-key")
		return ErrUsage
	}
	if p := provider.Protocol(*protocol); p != provider.ProtocolChat && p != provider.ProtocolMessages {
		fmt.Fprintf(stderr, "-protocol 仅支持 openai-chat | anthropic-messages\n")
		return ErrUsage
	}
	if _, err := netproxy.Parse(netproxy.Ref(*proxy)); err != nil {
		fmt.Fprintln(stderr, err)
		return ErrUsage
	}
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	var target *provider.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == *id {
			target = &cfg.Providers[i]
			break
		}
	}
	if target == nil {
		cfg.Providers = append(cfg.Providers, provider.Provider{
			ID: *id, Name: *name, Protocol: provider.Protocol(*protocol),
			BaseURL: *baseURL, Proxy: netproxy.Ref(*providerProxy),
		})
		target = &cfg.Providers[len(cfg.Providers)-1]
	}
	if target.Name == "" {
		target.Name = *name
	}
	if *providerProxy != "" {
		target.Proxy = netproxy.Ref(*providerProxy)
	}
	if *baseURL != "" && *baseURL != target.BaseURL {
		target.BaseURL = *baseURL
	}
	for i, acc := range target.Accounts {
		if acc.Name != *account {
			continue
		}
		if !*replace {
			fmt.Fprintf(stderr, "账号 %q 已存在于 %s（加 -replace 更新，或换名）\n", *account, *id)
			return ErrUsage
		}
		target.Accounts[i].APIKey = *key
		if *proxy != "" {
			target.Accounts[i].Proxy = netproxy.Ref(*proxy)
		}
		if err := saveProvidersFile(paths, cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "已更新 %s / 账号 %q。运行 `yardmaster provider probe %s` 刷新模型清单。\n", *id, *account, *id)
		return nil
	}
	target.Accounts = append(target.Accounts, provider.Account{
		Name: *account, APIKey: *key, Proxy: netproxy.Ref(*proxy), Status: provider.AccountAvailable,
	})
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已添加 %s / 账号 %q。下一步运行 `yardmaster provider probe %s` 拉取模型清单。\n", *id, *account, *id)
	return nil
}

func providerList(paths store.Paths, stdout io.Writer) error {
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	if len(cfg.Providers) == 0 {
		fmt.Fprintln(stdout, "（无 provider；用 `yardmaster provider add` 添加）")
		return nil
	}
	for _, p := range cfg.Providers {
		accInfo := make([]string, 0, len(p.Accounts))
		for _, a := range p.Accounts {
			px, _ := gateway.ResolveProxyRef(string(a.Proxy), string(p.Proxy), "")
			accInfo = append(accInfo, fmt.Sprintf("%s[%s|代理:%s]", a.Name, a.Status, netproxy.Redact(px)))
		}
		fmt.Fprintf(stdout, "%s  %-18s %-18s 模型 %2d  账号: %s\n",
			p.ID, p.Name, p.Protocol, len(p.Models), strings.Join(accInfo, " "))
	}
	return nil
}

func providerProbe(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("provider probe", flag.ContinueOnError)
	account := fs.String("account", "", "仅预检指定账号（默认全部）")
	only := fs.String("only", "", "仅保留指定模型（逗号分隔；未在上游清单中的仍保留并提示）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "用法: yardmaster provider probe [-account 名称] [-only m1,m2] <provider-id>")
		return ErrUsage
	}
	id := fs.Arg(0)
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	var target *provider.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			target = &cfg.Providers[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(stderr, "provider %q 不存在\n", id)
		return ErrUsage
	}

	settings, serr := store.LoadSettings(paths)
	if serr != nil && !errors.Is(serr, os.ErrNotExist) {
		return serr
	}

	var allModels []provider.Model
	failed := 0
	for i := range target.Accounts {
		acc := &target.Accounts[i]
		if *account != "" && acc.Name != *account {
			continue
		}
		ref, _ := gateway.ResolveProxyRef(string(acc.Proxy), string(target.Proxy), settings.GlobalProxy)
		rt, err := netproxy.SharedTransport(netproxy.Ref(ref))
		if err != nil {
			return err
		}
		models, perr := gateway.ProbeAccount(context.Background(), target, *acc, settings.GlobalProxy, rt)
		if perr != nil {
			acc.Status = provider.AccountPrefail // 预检失败仍可参与路由（F3 规则）
			failed++
			fmt.Fprintf(stdout, "✗ %s/%s: %v\n", id, acc.Name, perr)
			continue
		}
		acc.Status = provider.AccountAvailable
		if *only != "" {
			models = filterModels(models, *only, stdout)
		}
		allModels = append(allModels, models...)
		fmt.Fprintf(stdout, "✓ %s/%s: %d 个模型\n", id, acc.Name, len(models))
	}
	if *account == "" {
		target.Models = allModels
	}
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d 个账号预检失败", failed)
	}
	if target.Models != nil {
		fmt.Fprintf(stdout, "模型清单已落盘：%s\n", strings.Join(target.ModelIDs(), ", "))
	}
	return nil
}

// filterModels 把上游清单过滤为用户指定的模型集合；未出现在清单中的
// 用户指定模型仍保留（上游清单可能滞后），但给出提示。
func filterModels(models []provider.Model, only string, stdout io.Writer) []provider.Model {
	want := strings.Split(only, ",")
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ID] = true
	}
	var out []provider.Model
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if seen[w] {
			out = append(out, provider.Model{ID: w})
		} else {
			fmt.Fprintf(stdout, "△ %s 未在上游模型清单中，仍按用户指定保留\n", w)
			out = append(out, provider.Model{ID: w})
		}
	}
	return out
}

func providerRemove(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: yardmaster provider remove <provider-id>")
		return ErrUsage
	}
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	kept := cfg.Providers[:0]
	found := false
	for _, p := range cfg.Providers {
		if p.ID == args[0] {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		fmt.Fprintf(stderr, "provider %q 不存在\n", args[0])
		return ErrUsage
	}
	cfg.Providers = kept
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已删除 %s（引用其模型的组与规则将按「目标失效」处理）\n", args[0])
	return nil
}

// ---------- 路由组（PRD F6） ----------

// repeatFlags 收集可重复出现的 -flag 值（组成员列表用）。
type repeatFlags []string

func (f *repeatFlags) String() string { return strings.Join(*f, ",") }
func (f *repeatFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// runGroup 是 group 子命令：add/list/rm（管理页策略编辑后续接入同一持久化）。
func runGroup(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: yardmaster group <add|list|rm> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return groupAdd(paths, args[1:], stdout, stderr)
	case "list":
		return groupList(paths, stdout)
	case "rm":
		return groupRemove(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：add、list、rm\n", args[0])
		return ErrUsage
	}
}

func groupAdd(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("group add", flag.ContinueOnError)
	id := fs.String("id", "", "组 ID（调用方引用为 group/<id>）")
	strategy := fs.String("strategy", "order", "order | rotate | smart")
	var members repeatFlags
	fs.Var(&members, "member", "成员 provider/model（可重复）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *id == "" || len(members) == 0 {
		fmt.Fprintln(stderr, "缺少必填参数：-id 与至少一个 -member")
		return ErrUsage
	}
	switch provider.Strategy(*strategy) {
	case provider.StrategyOrder, provider.StrategyRotate, provider.StrategySmart:
	default:
		fmt.Fprintf(stderr, "-strategy 仅支持 order | rotate | smart\n")
		return ErrUsage
	}
	for _, m := range members {
		if strings.TrimSpace(m) == "" {
			fmt.Fprintf(stderr, "-member 不能为空\n")
			return ErrUsage
		}
	}
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	for _, g := range cfg.Groups {
		if g.ID == *id {
			fmt.Fprintf(stderr, "路由组 %q 已存在（先 rm 再 add）\n", *id)
			return ErrUsage
		}
	}
	cfg.Groups = append(cfg.Groups, provider.Group{
		ID: *id, Strategy: provider.Strategy(*strategy), Members: members,
	})
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已添加组 %s（%s）：\n", *id, *strategy)
	for _, m := range members {
		fmt.Fprintf(stdout, "  - %s\n", m)
	}
	fmt.Fprintf(stdout, "调用方引用名：group/%s（重启 serve 生效）\n", *id)
	return nil
}

func groupList(paths store.Paths, stdout io.Writer) error {
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	if len(cfg.Groups) == 0 {
		fmt.Fprintln(stdout, "（无路由组；用 `yardmaster group add` 创建）")
		return nil
	}
	for _, g := range cfg.Groups {
		fmt.Fprintf(stdout, "group/%s  %-7s 成员: %s\n", g.ID, g.Strategy, strings.Join(g.Members, ", "))
	}
	return nil
}

func groupRemove(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: yardmaster group rm <group-id>")
		return ErrUsage
	}
	cfg, err := loadProvidersFile(paths)
	if err != nil {
		return err
	}
	kept := cfg.Groups[:0]
	found := false
	for _, g := range cfg.Groups {
		if g.ID == args[0] {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		fmt.Fprintf(stderr, "路由组 %q 不存在\n", args[0])
		return ErrUsage
	}
	cfg.Groups = kept
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已删除组 %s（绑定它的 keys 与引用它的规则需自行调整）\n", args[0])
	return nil
}
