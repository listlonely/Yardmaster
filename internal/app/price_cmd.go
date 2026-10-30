// price 与 cost 子命令（M2-W4）：模型单价表维护与成本报告。
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
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
	default:
		fmt.Fprintf(stderr, "未知 price 子命令 %q；可用：set、rm、list\n", args[0])
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
		fmt.Fprintln(stdout, "单价表为空（yardmaster price set <model> <input> <output> 添加）")
		return nil
	}
	sort.Slice(f.Prices, func(i, j int) bool { return f.Prices[i].Model < f.Prices[j].Model })
	fmt.Fprintf(stdout, "%-32s %14s %14s\n", "MODEL", "INPUT $/1M", "OUTPUT $/1M")
	for _, p := range f.Prices {
		fmt.Fprintf(stdout, "%-32s %14g %14g\n", p.Model, p.InputPerM, p.OutputPerM)
	}
	return nil
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

	fmt.Fprintf(stdout, "成本报告 %s ~ %s（%d 天，单价来自 pricing.json）\n", rep.From, rep.To, rep.Days)
	if len(rep.ByAccount) == 0 && rep.Unpriced == 0 {
		fmt.Fprintln(stdout, "  窗口内无已计费请求")
		return nil
	}
	fmt.Fprintf(stdout, "\n按账号：\n%-28s %8s %14s %14s %12s\n", "ACCOUNT", "CALLS", "INPUT tok", "OUTPUT tok", "COST $")
	for _, k := range gateway.SortedCostKeys(rep.ByAccount) {
		b := rep.ByAccount[k]
		fmt.Fprintf(stdout, "%-28s %8d %14d %14d %12.4f\n", k, b.Calls, b.InputTokens, b.OutputTokens, b.Cost)
	}
	fmt.Fprintf(stdout, "\n按模型：\n%-28s %8s %14s %14s %12s\n", "MODEL", "CALLS", "INPUT tok", "OUTPUT tok", "COST $")
	for _, k := range gateway.SortedCostKeys(rep.ByModel) {
		b := rep.ByModel[k]
		fmt.Fprintf(stdout, "%-28s %8d %14d %14d %12.4f\n", k, b.Calls, b.InputTokens, b.OutputTokens, b.Cost)
	}
	if rep.Unpriced > 0 {
		fmt.Fprintf(stdout, "\n未计价调用 %d 次（pricing.json 无对应单价；失败请求不计费不出现在此列）\n", rep.Unpriced)
	}
	return nil
}
