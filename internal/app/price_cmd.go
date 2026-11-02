// price 与 cost 子命令（M2-W4）：模型单价表维护与成本报告。
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"yardmaster/internal/gateway"
	"yardmaster/internal/store"
)

func runPrice(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return fmt.Errorf("定位数据目录: %w", err)
	}
	path := paths.PricingPath()
	switch args[0] {
	case "set":
		return priceSet(path, args[1:], stdout, stderr)
	case "rm":
		return priceRm(path, args[1:], stdout, stderr)
	case "list", "":
		return priceList(path, stdout)
	case "sync":
		return priceSync(path, paths.AppDir, paths.TraceDir(), args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知 price 子命令 %q；可用：set、rm、list、sync\n", args[0])
		return ErrUsage
	}
}

// loadPricingOrDefault 读取单价表：不存在按空表处理；损坏 fail-closed
// （拒绝修改，提示先修复——绝不基于空表覆盖可能含数据的坏文件）。
func loadPricingOrDefault(path string) (gateway.PricingFile, error) {
	f, err := gateway.LoadPricingFile(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	return gateway.PricingFile{}, err
}

// priceSet 语法：price set <model> <input_per_m> <output_per_m>
// model 为 "provider/model"（精确）或裸 "model"（通用）。
func priceSet(path string, args []string, stdout, stderr io.Writer) error {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "用法: yardmaster price set <model> <input_per_m> <output_per_m>")
		return ErrUsage
	}
	in, err1 := strconv.ParseFloat(args[1], 64)
	out, err2 := strconv.ParseFloat(args[2], 64)
	if err1 != nil || err2 != nil || in < 0 || out < 0 {
		fmt.Fprintln(stderr, "单价必须为非负数字（美元 / 1M tokens）")
		return ErrUsage
	}
	f, err := loadPricingOrDefault(path)
	if err != nil {
		return fmt.Errorf("单价表读取失败（fail-closed，请先修复）: %w", err)
	}
	entry := gateway.PriceEntry{Model: args[0], InputPerM: in, OutputPerM: out}
	replaced := false
	for i := range f.Prices {
		if f.Prices[i].Model == entry.Model {
			f.Prices[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		f.Prices = append(f.Prices, entry)
	}
	if err := gateway.SavePricingFile(path, f); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已设置 %s 单价：输入 $%g / 输出 $%g（每 1M tokens）\n", entry.Model, in, out)
	return nil
}

func priceRm(path string, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: yardmaster price rm <model>")
		return ErrUsage
	}
	f, err := loadPricingOrDefault(path)
	if err != nil {
		return fmt.Errorf("单价表读取失败（fail-closed，请先修复）: %w", err)
	}
	var kept []gateway.PriceEntry
	removed := false
	for _, p := range f.Prices {
		if p.Model == args[0] {
			removed = true
			continue
		}
		kept = append(kept, p)
	}
	if !removed {
		fmt.Fprintf(stdout, "单价表无 %s（无需删除）\n", args[0])
		return nil
	}
	if err := gateway.SavePricingFile(path, gateway.PricingFile{Prices: kept}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已删除 %s 单价\n", args[0])
	return nil
}

func priceList(path string, stdout io.Writer) error {
	f, err := loadPricingOrDefault(path)
	if err != nil {
		return fmt.Errorf("单价表读取失败: %w", err)
	}
	if len(f.Prices) == 0 {
		fmt.Fprintln(stdout, "单价表为空（yardmaster price set <model> <input> <output> 或 price sync 从 models.dev 填充）")
		return nil
	}
	sort.Slice(f.Prices, func(i, j int) bool { return f.Prices[i].Model < f.Prices[j].Model })
	fmt.Fprintf(stdout, "%-36s %14s %14s %12s\n", "MODEL", "INPUT $/1M", "OUTPUT $/1M", "来源")
	for _, p := range f.Prices {
		src := "人工"
		if p.Source != "" {
			src = p.Source
		}
		fmt.Fprintf(stdout, "%-36s %14g %14g %12s\n", p.Model, p.InputPerM, p.OutputPerM, src)
	}
	return nil
}

// priceSync 从 models.dev 公开目录同步单价（M7-W1 增量）：**缺省按用
// 量同步**——只填充决策 trace 窗口内用到的 (provider, model) 对（同名
// 模型按本地 provider 写 "provider/model" 精确键，供应商级单价）；-all
// 全量目录；-models 显式追加。合并语义见 gateway.SyncPricingUsage /
// SyncPricing（人工价保护/-force）；写前备份与管理 API 写路径同款
// （backup/ 留底 + 原子写 + 审计日志）。
func priceSync(path, appDir, traceDir string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("price sync", flag.ContinueOnError)
	fs.SetOutput(stderr) // flag 用法错误输出到 stderr（unparam：参数必须被使用）
	from := fs.String("from", "", "目录来源：http(s) URL 或本地文件（空=默认 models.dev api.json）")
	force := fs.Bool("force", false, "覆盖人工维护的单价（缺省保护）")
	provider := fs.String("provider", "", "models.dev provider id（-all 模式限定范围；-models 显式模型的精确键匹配 provider）")
	all := fs.Bool("all", false, "全量同步目录所有带成本模型（缺省仅同步用到的）")
	models := fs.String("models", "", "逗号分隔的显式模型 id（追加到用量集合）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	cur, err := loadPricingOrDefault(path)
	if err != nil {
		return fmt.Errorf("单价表读取失败（fail-closed，请先修复）: %w", err)
	}
	fmt.Fprintf(stdout, "拉取目录 %s …\n", orDefaultStr(*from, gateway.ModelsDevURL))
	cat, err := gateway.LoadModelsDev(*from)
	if err != nil {
		return err
	}
	var merged gateway.PricingFile
	var res gateway.SyncResult
	var mode string
	if *all {
		mode = "全量"
		merged, res = gateway.SyncPricing(cat, cur, *force, *provider, time.Now())
	} else {
		used := usedModelsFromTrace(traceDir)
		for _, m := range strings.Split(*models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				used = append(used, gateway.ModelUse{Provider: *provider, Model: m})
			}
		}
		if len(used) == 0 {
			fmt.Fprintln(stdout, "决策 trace 内无已计费用量（无从同步）；-all 全量目录或 -model 指定模型，或先跑几次真实请求")
			return nil
		}
		mode = fmt.Sprintf("按用量（%d 个模型对）", len(used))
		merged, res = gateway.SyncPricingUsage(cat, cur, *force, used, time.Now())
	}
	if err := gateway.ValidatePricing(merged); err != nil {
		return fmt.Errorf("合并结果校验失败（不落盘）: %w", err)
	}
	// 写前备份（同管理 API writeConfig 语义：宁可拒绝也不无留底覆写）。
	backupDir := filepath.Join(appDir, "backup")
	if err := store.BackupConfig(path, backupDir, store.BaseName(path), 10); err != nil {
		return fmt.Errorf("写前备份失败，已中止写入: %w", err)
	}
	if err := gateway.SavePricingFile(path, merged); err != nil {
		return err
	}
	slog.Info("单价表已由 models.dev 同步更新", "path", path, "mode", mode,
		"added", len(res.Added), "updated", len(res.Updated),
		"overwritten", len(res.Overwritten), "skipped", len(res.Skipped))

	fmt.Fprintf(stdout, "同步完成（%s）：新增 %d、刷新 %d、覆盖人工价 %d、保护跳过 %d（目录 providers=%d 带成本模型=%d 无成本=%d 未填充）\n",
		mode, len(res.Added), len(res.Updated), len(res.Overwritten), len(res.Skipped),
		res.Providers, res.Models, res.NoPrice)
	for _, m := range res.Added {
		fmt.Fprintf(stdout, "  新增 %s\n", m)
	}
	for _, m := range res.Overwritten {
		fmt.Fprintf(stdout, "  覆盖人工价 %s\n", m)
	}
	for _, m := range res.Skipped {
		fmt.Fprintf(stdout, "  跳过 %s\n", m)
	}
	return nil
}

// usedModelsFromTrace 收集决策 trace 窗口内的用量对（与成本报告同口径：
// outcome=ok 且带 usage）；trace 不可读=空集合（不阻断，报告会如实为空）。
func usedModelsFromTrace(traceDir string) []gateway.ModelUse {
	traces, err := gateway.LoadTraceFiles(traceDir, 0)
	if err != nil {
		return nil
	}
	var used []gateway.ModelUse
	for _, tr := range traces {
		if tr.Outcome != "ok" || tr.Usage == nil || tr.Served == "" || tr.ServedModel == "" {
			continue
		}
		pid, _, _ := strings.Cut(tr.Served, "/")
		used = append(used, gateway.ModelUse{Provider: pid, Model: tr.ServedModel})
	}
	return used
}

// runCost 输出最近 N 天的成本报告（默认 7，上限 30=trace 保留期）。
func runCost(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	fs.SetOutput(stderr) // flag 用法错误输出到 stderr（unparam：参数必须被使用）
	days := fs.Int("days", 7, "统计天数（1-30）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *days < 1 {
		*days = 1
	}
	if *days > 30 {
		*days = 30
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return fmt.Errorf("定位数据目录: %w", err)
	}
	var prices []gateway.PriceEntry
	if f, err := gateway.LoadPricingFile(paths.PricingPath()); err == nil {
		prices = f.Prices
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("单价表读取失败: %w", err)
	}
	traces, err := gateway.LoadTraceFiles(paths.TraceDir(), 0)
	if err != nil {
		return fmt.Errorf("trace 读取失败: %w", err)
	}
	rep := gateway.ComputeCost(traces, prices, *days, time.Now())
	// 展示换算汇率自 settings（0=仅美元；读取失败按 0，不阻断报告）。
	if st, err := store.LoadSettings(paths); err == nil {
		rep.FxUsdCNY = st.FxUsdCNY
	}

	fmt.Fprintf(stdout, "成本报告 %s ~ %s（%d 天，单价来自 pricing.json）\n", rep.From, rep.To, rep.Days)
	if len(rep.ByAccount) == 0 && rep.Unpriced == 0 {
		fmt.Fprintln(stdout, "  窗口内无已计费请求")
		return nil
	}
	priceCell := func(b gateway.CostBucket) string {
		if len(b.Prices) == 1 {
			return fmt.Sprintf("%g/%g", b.Prices[0].In, b.Prices[0].Out)
		}
		if len(b.Prices) > 1 {
			return fmt.Sprintf("混合(%d种)", len(b.Prices))
		}
		return "-"
	}
	fmt.Fprintf(stdout, "\n按账号：\n%-28s %8s %14s %14s %12s  %-14s\n", "ACCOUNT", "CALLS", "INPUT tok", "OUTPUT tok", "COST $", "单价 in/out")
	var total float64
	for _, k := range gateway.SortedCostKeys(rep.ByAccount) {
		b := rep.ByAccount[k]
		total += b.Cost
		fmt.Fprintf(stdout, "%-28s %8d %14d %14d %12.4f  %-14s\n", k, b.Calls, b.InputTokens, b.OutputTokens, b.Cost, priceCell(b))
	}
	fmt.Fprintf(stdout, "\n按模型：\n%-28s %8s %14s %14s %12s  %-14s\n", "MODEL", "CALLS", "INPUT tok", "OUTPUT tok", "COST $", "单价 in/out")
	for _, k := range gateway.SortedCostKeys(rep.ByModel) {
		b := rep.ByModel[k]
		fmt.Fprintf(stdout, "%-28s %8d %14d %14d %12.4f  %-14s\n", k, b.Calls, b.InputTokens, b.OutputTokens, b.Cost, priceCell(b))
	}
	if rep.Unpriced > 0 {
		fmt.Fprintf(stdout, "\n未计价调用 %d 次（pricing.json 无对应单价；失败请求不计费不出现在此列）\n", rep.Unpriced)
	}
	if rep.FxUsdCNY > 0 {
		fmt.Fprintf(stdout, "\n合计 $%.4f ≈¥%.2f（汇率 %g，仅展示换算）\n", total, total*rep.FxUsdCNY, rep.FxUsdCNY)
	}
	return nil
}
