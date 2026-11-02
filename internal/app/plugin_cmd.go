// plugin 子命令（M7-W5）：插件清单管理（list/enable/disable）。CLI 直改
// plugins.json（备份+原子写同规）；运行态启停/日志看控制台「插件」页
//（serve 在跑时 CLI 改文件不热生效——控制台或 PUT /api/config/plugins
// 才热替换；改文件后重启 serve 生效）。
package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"yardmaster/internal/plugin"
	"yardmaster/internal/store"
)

func runPlugin(args []string, stdout, stderr io.Writer) error {
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "list" {
		return pluginList(stdout, paths)
	}
	if len(args) >= 2 && (args[0] == "enable" || args[0] == "disable") {
		return pluginToggle(args[0], args[1], stdout, stderr, paths)
	}
	fmt.Fprintln(stderr, "用法: yardmaster plugin [list] | plugin <enable|disable> <插件id>")
	return ErrUsage
}

// pluginList 列出插件清单（不存在=未配置指引；损坏=fail-closed 报错
// 指引修复，不猜测内容）。
func pluginList(stdout io.Writer, paths store.Paths) error {
	f, err := plugin.Load(paths.PluginsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stdout, "未配置插件（plugins.json 不存在）。示例见仓库 examples/；写法见 docs/subsystems/plugins.md。")
			return nil
		}
		return fmt.Errorf("plugins.json 不可用（读不懂=未知，请修复或从 backup/ 还原）: %w", err)
	}
	if len(f.Plugins) == 0 {
		fmt.Fprintln(stdout, "未配置插件（plugins.json 为空清单）。")
		return nil
	}
	rows := append([]plugin.Spec(nil), f.Plugins...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	fmt.Fprintf(stdout, "%-16s %-6s %-28s %s\n", "ID", "启用", "能力", "命令")
	for _, p := range rows {
		caps := ""
		for i, c := range p.Capabilities {
			if i > 0 {
				caps += ","
			}
			caps += c
		}
		cmd := ""
		if len(p.Command) > 0 {
			cmd = p.Command[0]
		}
		fmt.Fprintf(stdout, "%-16s %-6s %-28s %s\n", p.ID, yesNo(p.Enabled), caps, cmd)
	}
	fmt.Fprintln(stdout, "\n运行态/日志：控制台「插件」页或 GET /api/plugins（serve 运行中）。")
	return nil
}

// pluginToggle 翻转插件 enabled：读现值（fail-closed：损坏拒绝改动）→
// 翻转 → 校验 → 备份 → 原子写。
func pluginToggle(action, id string, stdout, stderr io.Writer, paths store.Paths) error {
	path := paths.PluginsPath()
	f, err := plugin.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "plugins.json 不存在，无可启停的插件。")
			return ErrUsage
		}
		return fmt.Errorf("plugins.json 不可用（读不懂=未知，请修复或从 backup/ 还原）: %w", err)
	}
	found := false
	for i := range f.Plugins {
		if f.Plugins[i].ID == id {
			found = true
			f.Plugins[i].Enabled = action == "enable"
		}
	}
	if !found {
		fmt.Fprintf(stderr, "插件 %q 不在清单中（plugin list 查看）。\n", id)
		return ErrUsage
	}
	if err := plugin.Validate(f); err != nil {
		return fmt.Errorf("翻转后校验失败（未落盘）: %w", err)
	}
	if err := store.BackupConfig(path, paths.BackupDir(), store.BaseName(path), 10); err != nil {
		return err
	}
	if err := plugin.Save(path, f); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已%s插件 %s（写入 %s，已备份）。serve 运行中时请在控制台「插件」页启停（热生效），或重启 serve。\n",
		map[string]string{"enable": "启用", "disable": "停用"}[action], id, path)
	return nil
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
