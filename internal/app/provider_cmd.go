// provider 子命令（PRD F2 的 CLI 形态）：add/list/probe/remove。
// UI 向导后续接入同一持久化与预检逻辑。
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yardmaster/internal/credential"
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
		fmt.Fprintln(stderr, "用法: yardmaster provider <add|add-oauth|presets|list|probe|quota|remove> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return providerAdd(paths, args[1:], stdout, stderr)
	case "add-oauth":
		return providerAddOAuth(paths, args[1:], stdout, stderr)
	case "presets":
		return providerPresets(stdout)
	case "list":
		return providerList(paths, stdout)
	case "probe":
		return providerProbe(paths, args[1:], stdout, stderr)
	case "quota":
		return providerQuota(paths, args[1:], stdout, stderr)
	case "remove":
		return providerRemove(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：add、add-oauth、presets、list、probe、quota、remove\n", args[0])
		return ErrUsage
	}
}

// providerPresets 列出厂商预设（M3-W4）。
func providerPresets(stdout io.Writer) error {
	fmt.Fprintln(stdout, "可用厂商预设（provider add -preset <id>）：")
	for _, p := range provider.Presets {
		fmt.Fprintf(stdout, "  %-22s %-34s %-19s %s\n", p.ID, p.Name, p.Protocol, p.Note)
		fmt.Fprintf(stdout, "  %s   base_url: %s\n", "", p.BaseURL)
	}
	return nil
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
	id := fs.String("id", "", "provider ID（如 deepseek；-preset 给出时默认同预设 ID）")
	name := fs.String("name", "", "显示名（默认同 ID）")
	protocol := fs.String("protocol", "openai-chat", "openai-chat | anthropic-messages | openai-responses | gemini（-preset 未给时必填其一；gemini 上游链路 M4-W3，W1 仅可配置）")
	baseURL := fs.String("base-url", "", "上游完整前缀（如 https://api.deepseek.com）")
	key := fs.String("key", "", "API key（必填，仅存本机）")
	account := fs.String("account", "main", "账号名")
	presetID := fs.String("preset", "", "厂商预设 ID（yardmaster provider presets 查看；自动填 id/name/protocol/base_url）")
	maxConc := fs.Int("max-concurrent", 0, "账号最大并发（0=不限，M3-W5）")
	proxy := fs.String("proxy", "", "账号级代理（http/https/socks5/socks5h/direct）")
	providerProxy := fs.String("provider-proxy", "", "Provider 级代理（账号未配置时继承）")
	replace := fs.Bool("replace", false, "账号已存在时更新其凭证与代理")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var preset provider.Preset
	if *presetID != "" {
		p, ok := provider.PresetByID(*presetID)
		if !ok {
			fmt.Fprintf(stderr, "未知预设 %q；可用见 `yardmaster provider presets`\n", *presetID)
			return ErrUsage
		}
		preset = p
		if *id == "" {
			*id = preset.ID
		}
		if *name == "" {
			*name = preset.Name
		}
		if *baseURL == "" {
			*baseURL = preset.BaseURL
		}
		if !explicit["protocol"] && preset.Protocol != "" {
			*protocol = string(preset.Protocol)
		}
	}
	if *id == "" || *baseURL == "" || *key == "" {
		fmt.Fprintln(stderr, "缺少必填参数：-key 与 -id/-base-url（或用 -preset 提供厂商预设）")
		return ErrUsage
	}
	if p := provider.Protocol(*protocol); p != provider.ProtocolChat && p != provider.ProtocolMessages &&
		p != provider.ProtocolResponses && p != provider.ProtocolGemini {
		fmt.Fprintf(stderr, "-protocol 仅支持 openai-chat | anthropic-messages | openai-responses | gemini\n")
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
			QuotaProbe: preset.QuotaProbe,
		})
		target = &cfg.Providers[len(cfg.Providers)-1]
	}
	if target.Name == "" {
		target.Name = *name
	}
	if *providerProxy != "" {
		target.Proxy = netproxy.Ref(*providerProxy)
	}
	if target.QuotaProbe == nil && preset.QuotaProbe != nil {
		target.QuotaProbe = preset.QuotaProbe
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
		Name: *account, APIKey: *key, Proxy: netproxy.Ref(*proxy),
		Status: provider.AccountAvailable, MaxConcurrent: *maxConc,
	})
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已添加 %s / 账号 %q。下一步运行 `yardmaster provider probe %s` 拉取模型清单。\n", *id, *account, *id)
	return nil
}

// expandHome 把路径前缀 ~ 展开为用户目录（-from 显式路径用）。
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), string(filepath.Separator))), nil
	}
	return p, nil
}

// oauthStateFor 依据过期时间归出账号元数据状态（W3 起复用 provider 共用实现）。
func oauthStateFor(expiresAt int64) string { return provider.OAuthStateFor(expiresAt) }

// providerAddOAuth 从 Agent 凭证文件导入 OAuth 订阅账号（M3-W1 骨架）：
// 读源文件 → 注册表解析 → 凭证入加密库 → providers.json 写入
// auth=oauth 元数据（api_key 互斥，秘密永不进 providers.json 及其备份）。
// 任一步「读不懂」即整体失败，不落半份凭证。
func providerAddOAuth(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("provider add-oauth", flag.ContinueOnError)
	id := fs.String("id", "", "provider ID（不存在时须同时给 -base-url）")
	name := fs.String("name", "", "显示名（默认同 ID）")
	protocol := fs.String("protocol", "openai-chat", "openai-chat | anthropic-messages")
	baseURL := fs.String("base-url", "", "上游完整前缀")
	account := fs.String("account", "main", "账号名")
	from := fs.String("from", "", "导入源 <格式>:<凭证文件路径>（格式见提示；文件支持 ~ 前缀）")
	proxy := fs.String("proxy", "", "账号级代理（http/https/socks5/socks5h/direct）")
	providerProxy := fs.String("provider-proxy", "", "Provider 级代理（账号未配置时继承）")
	replace := fs.Bool("replace", false, "账号已存在时转换为 OAuth（api_key 账号转换将清除其 key）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *id == "" || *from == "" {
		fmt.Fprintln(stderr, "缺少必填参数：-id、-from <格式>[:<文件路径>]")
		return ErrUsage
	}
	format, srcPath, _ := strings.Cut(*from, ":")
	format = strings.TrimSpace(format)
	if format == "" {
		fmt.Fprintln(stderr, "-from 须为 <格式>[:<文件路径>]（如 claude-code、claude-code:~/.claude/.credentials.json）")
		return ErrUsage
	}
	parse, ok := credential.Lookup(format)
	if !ok {
		fmt.Fprintf(stderr, "未支持的导入格式 %q；可用：%s\n", format, strings.Join(credential.Names(), ", "))
		return ErrUsage
	}
	srcPath = strings.TrimSpace(srcPath)
	if srcPath == "" {
		// 省略路径：用该 Agent 的默认凭证文件路径
		def, ok := credential.DefaultPath(format)
		if !ok {
			fmt.Fprintf(stderr, "格式 %q 无默认路径，请显式给出：<格式>:<文件路径>\n", format)
			return ErrUsage
		}
		srcPath = def
	}
	if p := provider.Protocol(*protocol); p != provider.ProtocolChat && p != provider.ProtocolMessages {
		fmt.Fprintln(stderr, "-protocol 仅支持 openai-chat | anthropic-messages")
		return ErrUsage
	}
	if _, err := netproxy.Parse(netproxy.Ref(*proxy)); err != nil {
		fmt.Fprintln(stderr, err)
		return ErrUsage
	}
	src, err := expandHome(srcPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		// 读不懂 = 未知：源文件读不到/读不了绝不落库
		fmt.Fprintf(stderr, "读取凭证文件失败（读不懂=未知，未做任何写入）: %v\n", err)
		return ErrUsage
	}
	oauth, err := parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "解析 %s 凭证失败（读不懂=未知，未做任何写入）: %v\n", format, err)
		return ErrUsage
	}
	if err := oauth.Validate(); err != nil {
		fmt.Fprintf(stderr, "凭证校验失败（未做任何写入）: %v\n", err)
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
		if *baseURL == "" {
			fmt.Fprintf(stderr, "provider %q 不存在；首次创建须给 -base-url\n", *id)
			return ErrUsage
		}
		cfg.Providers = append(cfg.Providers, provider.Provider{
			ID: *id, Name: *name, Protocol: provider.Protocol(*protocol), BaseURL: *baseURL,
			Proxy: netproxy.Ref(*providerProxy),
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

	now := time.Now().Unix()
	meta := &provider.OAuthMeta{Format: format, ExpiresAt: oauth.ExpiresAt, RefreshedAt: now, State: oauthStateFor(oauth.ExpiresAt)}
	converted := false
	idx := -1
	for i := range target.Accounts {
		if target.Accounts[i].Name == *account {
			idx = i
			break
		}
	}
	if idx >= 0 {
		if !*replace {
			fmt.Fprintf(stderr, "账号 %q 已存在于 %s（加 -replace 转换为 OAuth，或换名）\n", *account, *id)
			return ErrUsage
		}
		if target.Accounts[idx].APIKey != "" {
			converted = true // 用户显式 -replace：api_key 账号转 OAuth，清除旧 key
		}
	}

	ref, err := credential.NewRef(*id, *account)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ErrUsage
	}
	credStore := credential.Store{Path: paths.CredentialsPath(), BackupDir: paths.BackupDir()}
	// 顺序：凭证库先落（失败即中止，不动配置）；providers.json 随后
	// 走备份栈+原子写。反序会出现「配置指向 oauth 但库里无凭证」。
	if err := credStore.Save(ref, oauth); err != nil {
		return fmt.Errorf("凭证库写入失败（未改 providers.json）: %w", err)
	}
	if idx >= 0 {
		target.Accounts[idx].APIKey = ""
		target.Accounts[idx].Auth = provider.AuthOAuth
		target.Accounts[idx].OAuth = meta
	} else {
		target.Accounts = append(target.Accounts, provider.Account{
			Name: *account, Proxy: netproxy.Ref(*proxy),
			Status: provider.AccountAvailable,
			Auth:   provider.AuthOAuth, OAuth: meta,
		})
	}
	if err := store.BackupConfig(paths.ProvidersPath(), paths.BackupDir(), store.BaseName(paths.ProvidersPath()), 10); err != nil {
		return fmt.Errorf("写前备份失败，已中止: %w", err)
	}
	if err := saveProvidersFile(paths, cfg); err != nil {
		return err
	}
	slog.Info("OAuth 账号已导入", "provider", *id, "account", *account, "format", format)
	switch {
	case converted:
		fmt.Fprintf(stdout, "已转换 %s / 账号 %q 为 OAuth（原 api_key 已清除）。过期时间: %s\n",
			*id, *account, oauthExpiryText(meta))
	case idx >= 0:
		fmt.Fprintf(stdout, "已更新 %s / 账号 %q 为 OAuth。过期时间: %s\n", *id, *account, oauthExpiryText(meta))
	default:
		fmt.Fprintf(stdout, "已添加 %s / OAuth 账号 %q。过期时间: %s\n", *id, *account, oauthExpiryText(meta))
	}
	fmt.Fprintln(stdout, "默认按各 Agent 凭证文件导入；刷新机制见 W3。")
	return nil
}

func oauthExpiryText(m *provider.OAuthMeta) string {
	if m == nil || m.ExpiresAt <= 0 {
		return "未知"
	}
	return time.Unix(m.ExpiresAt, 0).Format("2006-01-02 15:04")
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
			var extra string
			switch {
			case a.Auth == provider.AuthOAuth:
				extra = fmt.Sprintf("|oauth|过期:%s", oauthExpiryText(a.OAuth))
			case a.MaxConcurrent > 0:
				extra = fmt.Sprintf("|并发≤%d", a.MaxConcurrent)
			}
			if a.BalanceAt > 0 {
				extra += fmt.Sprintf("|余额:%.2f", a.Balance)
			}
			info := fmt.Sprintf("%s[%s%s|代理:%s]", a.Name, a.Status, extra, netproxy.Redact(px))
			accInfo = append(accInfo, info)
		}
		qp := ""
		if p.QuotaProbe != nil {
			qp = "  余额轮询: " + netproxy.Redact(p.QuotaProbe.URL)
		}
		fmt.Fprintf(stdout, "%s  %-18s %-18s 模型 %2d  账号: %s%s\n",
			p.ID, p.Name, p.Protocol, len(p.Models), strings.Join(accInfo, " "), qp)
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
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	defer cancelProbe()
	for i := range target.Accounts {
		acc := &target.Accounts[i]
		if *account != "" && acc.Name != *account {
			continue
		}
		if acc.Auth == provider.AuthOAuth {
			// OAuth 账号凭证经网关注入（M3-W3），不走 API key 预检
			fmt.Fprintf(stdout, "△ %s/%s: OAuth 账号不参与预检（凭证经网关注入）\n", id, acc.Name)
			continue
		}
		ref, _ := gateway.ResolveProxyRef(string(acc.Proxy), string(target.Proxy), settings.GlobalProxy)
		rt, err := netproxy.SharedTransport(netproxy.Ref(ref))
		if err != nil {
			return err
		}
		models, perr := gateway.ProbeAccount(probeCtx, target, *acc, settings.GlobalProxy, rt, target.AuthStyle)
		if perr != nil && strings.Contains(perr.Error(), "HTTP 401") {
			// 认证头自动学习（M3-W4）：当前形态被 401 拒绝时尝试另一形态，
			// 成功即记住（落盘到 providers.json 的 auth_style）
			alt := "bearer"
			if target.AuthStyle == "bearer" {
				alt = "x-api-key"
			}
			if m2, e2 := gateway.ProbeAccount(probeCtx, target, *acc, settings.GlobalProxy, rt, alt); e2 == nil {
				target.AuthStyle = alt
				models, perr = m2, nil
				fmt.Fprintf(stdout, "✓ %s/%s: 认证头已学习为 %s\n", id, acc.Name, alt)
			}
		}
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
