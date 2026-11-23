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

	"yardmaster/internal/netproxy"
	"yardmaster/internal/store"
)

// repoAPI 是 release 查询端点（仓库更名后以 Yardmaster 为准）。
const repoAPI = "https://api.github.com/repos/listlonely/Yardmaster/releases/latest"

// updateCheck 查询 GitHub 最新 release 并与当前版本比较（升级提示）。
func updateCheck(args []string, stdout, stderr io.Writer) error {
	// 子命令分发（M11-W2/W3）：check/无参=比较提示（现状）；download=
	// 下载校验暂存；install=安装+接力重启。
	if len(args) > 0 {
		switch args[0] {
		case "download":
			return updateDownloadCmd(args[1:], stdout, stderr)
		case "install":
			return updateInstallCmd(args[1:], stdout, stderr)
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
	tag, url, err := latestRelease(globalProxyRef())
	if err != nil {
		fmt.Fprintln(stdout, "查询失败："+err.Error())
		fmt.Fprintln(stdout, "提示：三张出网卡（全局代理/系统代理/直连）全部失败。" +
			"GitHub 直连不可达的网络下，请设置全局代理（控制台 设置 → 全局代理，" +
			"如 http://127.0.0.1:7890），或开启代理工具的「系统代理」。")
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

// globalProxyRef 读全局代理设置（CLI 场景无 serveSetup：默认路径直载
// settings.json；读失败返回空=回退系统代理/直连，不打断查询）。
func globalProxyRef() string {
	if paths, err := store.DefaultPaths(); err == nil {
		if settings, serr := store.LoadSettings(paths); serr == nil {
			return settings.GlobalProxy
		}
	}
	return ""
}

// updateHTTPClient 版本检查/更新下载共用的出网客户端（2026-11-23）：
// 全局代理（settings.global_proxy）优先，空则走系统代理（WinINET——
// FlClash 等桌面代理「系统代理」开关写这里）；都没有回退直连。此前
// 版本查询用裸 http.Client 直连 api.github.com，直连不可达的网络下
// 每轮静默失败=托盘升级菜单永不出现（「版本监测失效」实证根因）。
func updateHTTPClient(globalProxy string) *http.Client {
	ref := netproxy.Ref(globalProxy)
	if ref == "" {
		if sp, ok := netproxy.SystemProxy(); ok {
			ref = sp
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if tr, err := netproxy.Transport(ref); err == nil && tr != nil {
		client.Timeout = 20 * time.Second // 代理链路首连更慢，放宽一档
		client.Transport = tr
	}
	return client
}

// releaseQueryCandidates 版本查询候选链（2026-11-23）：全局代理 →
// 系统代理（WinINET）→ 直连，首个成功者胜出。实测教训：共享代理出
// 口 IP 被 GitHub API 限流（403 rate limit）——「有代理就走代理」
// 一刀切在代理出口被限流时整链报废，必须能落回直连；反之直连不可达
// 的网络依赖前两级。与 F3 继承链「全局 → 系统 → 直连」同构。
func releaseQueryClients(globalProxy string) []*http.Client {
	var out []*http.Client
	if ref := netproxy.Ref(globalProxy); ref != "" {
		out = append(out, updateHTTPClient(globalProxy))
	}
	if sp, ok := netproxy.SystemProxy(); ok {
		out = append(out, updateHTTPClient(string(sp)))
	}
	out = append(out, &http.Client{Timeout: 10 * time.Second}) // 直连兜底
	return out
}

// fetchReleaseVia 候选链逐个查询 release，首个成功返回（全败返回末次
// 错误）。托盘升级查询与更新下载的 API 元数据获取共用。
func fetchReleaseVia(ctx context.Context, globalProxy string) (*releaseInfo, error) {
	var lastErr error
	for _, client := range releaseQueryClients(globalProxy) {
		rel, qerr := fetchRelease(ctx, client, repoAPI)
		if qerr == nil {
			return rel, nil
		}
		lastErr = qerr
	}
	return nil, lastErr
}

// latestRelease 查询 GitHub 最新 release（M9-W4 抽取：CLI update check
// 与托盘「升级到 x.y.z」动态菜单项共用；M11-W2 起委托 fetchRelease 单一
// 查询路径——含资产清单的完整元数据由下载管线直接消费。2026-11-23 起
// 走候选链：全局代理→系统代理→直连，见 releaseQueryClients）。
func latestRelease(globalProxy string) (tag, url string, err error) {
	rel, err := fetchReleaseVia(context.Background(), globalProxy)
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
