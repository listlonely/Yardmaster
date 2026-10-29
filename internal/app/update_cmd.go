// update 子命令（M2-W5）：查询 GitHub 最新 release 并与当前版本比较，
// 给出升级提示。只提示不自动更新（下载与替换留给安装脚本）。
package app

import (
	"encoding/json"
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
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if Version == "dev" {
		fmt.Fprintln(stdout, "当前为开发态（dev），无版本号可比；发布版请用 yardmaster update check")
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, repoAPI, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("查询最新版本失败（网络不可达？）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("GitHub API 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		TagName string `json:"tag_name"`
		URL     string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	latest := strings.TrimPrefix(out.TagName, "v")
	fmt.Fprintf(stdout, "当前版本 %s · 最新版本 %s\n", Version, latest)
	switch compareVersions(Version, latest) {
	case 0:
		fmt.Fprintln(stdout, "已是最新版本")
	case -1:
		fmt.Fprintf(stdout, "有新版本可用：%s（下载与安装见 %s）\n", out.TagName, out.URL)
	default:
		fmt.Fprintln(stdout, "当前版本高于最新发布（开发分支？）")
	}
	return nil
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
