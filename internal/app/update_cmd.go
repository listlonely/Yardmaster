// update 子命令（M2-W5）：查询 GitHub 最新 release 并与当前版本比较，
// 给出升级提示。只提示不自动更新（下载与替换留给安装脚本）。
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// repoAPI 是 release 查询端点（仓库更名后以 Yardmaster 为准）。
const repoAPI = "https://api.github.com/repos/listlonely/Yardmaster/releases/latest"

// updateCheck 查询 GitHub 最新 release 并与当前版本比较（升级提示）。
func updateCheck(args []string, stdout, stderr io.Writer) error {
	// 子命令分发（M11-W2）：check/无参=比较提示（现状）；download=
	// 下载校验暂存（安装与接力重启是 W3）。
	if len(args) > 0 {
		switch args[0] {
		case "download":
			return updateDownloadCmd(args[1:], stdout, stderr)
		case "check":
			args = args[1:]
		}
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr) // flag 用法错误输出到 stderr
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if Version == "dev" {
		fmt.Fprintln(stdout, "当前为开发态（dev），无版本号可比；发布版请用 yardmaster update check")
		return nil
	}
	tag, url, err := latestRelease()
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(tag, "v")
	fmt.Fprintf(stdout, "当前版本 %s · 最新版本 %s\n", Version, latest)
	switch compareVersions(Version, latest) {
	case 0:
		fmt.Fprintln(stdout, "已是最新版本")
	case -1:
		fmt.Fprintf(stdout, "有新版本可用：%s（下载与安装见 %s）\n", tag, url)
	default:
		fmt.Fprintln(stdout, "当前版本高于最新发布（开发分支？）")
	}
	return nil
}

// latestRelease 查询 GitHub 最新 release（M9-W4 抽取：CLI update check
// 与托盘「升级到 x.y.z」动态菜单项共用；M11-W2 起委托 fetchRelease 单一
// 查询路径——含资产清单的完整元数据由下载管线直接消费）。
func latestRelease() (tag, url string, err error) {
	rel, err := fetchRelease(context.Background(), &http.Client{Timeout: 10 * time.Second}, repoAPI)
	if err != nil {
		return "", "", err
	}
	return rel.Tag, rel.URL, nil
}

// compareVersions 比较 x.y.z 语义版本：a<b 返回 -1，相等 0，a>b 返回 1。
// 非数字段（预发布后缀）按小于处理不展开——本产品发布为纯 x.y.z。
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}
