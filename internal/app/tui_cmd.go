// tui 子命令（M2-W6 最小版）：终端交互面板——状态总览 / 决策追溯 /
// 调用方默认模型绑定切换。
//
// 设计取舍（排期标注的「可砍项」按最小可用交付）：
//   - 行交互（输入命令 + 回车执行），不做全屏 raw-mode——跨平台按键
//     原始模式需引入平台特定代码或第三方依赖，违背「纯 Go 少依赖」选型；
//   - 数据全部经管理 API（/api/state、/api/trace、/api/config/keys、
//     PUT /api/config/keys）：天然支持远程网关与令牌态，与控制台同源；
//   - 自动刷新：10s 后台轮询当前视图（互斥锁防渲染竞态）。
package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"yardmaster/internal/store"
)

// tuiView 是面板的当前视图。
type tuiView struct {
	mu     sync.Mutex
	name   string // status | trace | keys
	traceN int    // trace 条数
}

func runTUI(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(stderr) // flag 用法错误输出到 stderr
	addr := fs.String("addr", "http://127.0.0.1:8317", "网关管理 API 地址")
	token := fs.String("token", "", "访问令牌（留空则读 settings.json 的 token）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *token == "" {
		if paths, err := store.DefaultPaths(); err == nil {
			if s, err := store.LoadSettings(paths); err == nil {
				*token = s.Token
			}
		}
	}
	c := &tuiClient{base: strings.TrimRight(*addr, "/"), token: *token, hc: &http.Client{Timeout: 10 * time.Second}}

	v := &tuiView{name: "status", traceN: 20}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 自动刷新（10s）
		defer wg.Done()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				v.mu.Lock()
				c.draw(stdout, v.name, v.traceN)
				v.mu.Unlock()
			case <-stop:
				return
			}
		}
	}()

	v.mu.Lock()
	c.draw(stdout, v.name, v.traceN)
	v.mu.Unlock()
	fmt.Fprintln(stdout, "命令：status | trace [n] | keys | switch <序号> <目标> | reload | refresh | help | quit")

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		v.mu.Lock()
		var err error
		switch fields[0] {
		case "q", "quit", "exit":
			v.mu.Unlock()
			close(stop)
			wg.Wait()
			fmt.Fprintln(stdout, "再见。")
			return nil
		case "s", "status":
			v.name = "status"
		case "t", "trace":
			v.name = "trace"
			if len(fields) > 1 {
				if n, e := strconv.Atoi(fields[1]); e == nil && n > 0 && n <= 200 {
					v.traceN = n
				}
			}
		case "k", "keys":
			v.name = "keys"
		case "switch":
			err = c.switchKey(stdout, fields[1:])
		case "reload":
			_, err = c.post("/api/reload")
			if err == nil {
				fmt.Fprintln(stdout, "已重载 providers.json")
			}
		case "refresh", "r":
			// 仅重绘
		case "h", "help", "?":
			fmt.Fprintln(stdout, "命令：status | trace [n] | keys | switch <序号> <目标> | reload | refresh | help | quit")
		default:
			fmt.Fprintf(stdout, "未知命令 %q（help 查看可用命令）\n", fields[0])
		}
		if err == nil {
			c.draw(stdout, v.name, v.traceN)
		} else {
			fmt.Fprintf(stdout, "错误: %v\n", err)
		}
		v.mu.Unlock()
	}
	close(stop)
	wg.Wait()
	return nil
}

// ---------- 渲染 ----------

const tuiClear = "\033[H\033[2J"

// draw 重绘当前视图（调用方持有 view 锁；单协程输出）。
func (c *tuiClient) draw(w io.Writer, view string, traceN int) {
	fmt.Fprint(w, tuiClear)
	st, err := c.state()
	if err != nil {
		fmt.Fprintf(w, "yardmaster TUI · 网关不可达（%v）\n命令：status | trace [n] | keys | switch <序号> <目标> | quit\n> ", err)
		return
	}
	fmt.Fprintf(w, "yardmaster %s · %s · 运行 %s\n", st.Version, c.base, st.Uptime)
	switch view {
	case "trace":
		c.drawTrace(w, traceN)
	case "keys":
		c.drawKeys(w)
	default:
		c.drawStatus(w, st)
	}
	fmt.Fprint(w, "> ")
}

func (c *tuiClient) drawStatus(w io.Writer, st *stateResp) {
	fmt.Fprintf(w, "\n[状态] Provider %d 个 · 路由组 %d 个 · 休眠 %d · 额度耗尽 %d\n",
		len(st.Providers), len(st.Groups), len(st.Resting), len(st.Exhausted))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROVIDER\t账号\t状态\t当日用量")
	for _, p := range st.Providers {
		fmt.Fprintf(tw, "  %s\t-\t-\t-\n", p.ID)
	}
	fmt.Fprintln(tw, "  ACCOUNT\tKEY\t状态\t用量/额度")
	for _, a := range st.Accounts {
		quota := "-"
		if a.QuotaTokens > 0 {
			quota = strconv.FormatInt(a.UsedTokens, 10) + "/" + strconv.FormatInt(a.QuotaTokens, 10)
		} else {
			quota = strconv.FormatInt(a.UsedTokens, 10)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", a.Key, "", a.Status, quota)
	}
	fmt.Fprintln(tw, "  GROUP\t策略\t成员")
	for _, g := range st.Groups {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", g.ID, g.Strategy, strings.Join(g.Members, " "))
	}
	tw.Flush()
	if len(st.Resting) > 0 {
		fmt.Fprintf(w, "  休眠中: %s\n", st.RestingKeys())
	}
	if len(st.Exhausted) > 0 {
		fmt.Fprintf(w, "  额度耗尽: %s\n", strings.Join(st.Exhausted, ", "))
	}
}

func (c *tuiClient) drawTrace(w io.Writer, n int) {
	tr, err := c.traces(n)
	if err != nil {
		fmt.Fprintf(w, "\n[追溯] 读取失败: %v\n", err)
		return
	}
	fmt.Fprintf(w, "\n[追溯] 最近 %d 条（时间降序）\n", len(tr))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  时间\t调用方\t请求模型\t路径\t目标\t结果\t候选\tms")
	for i := len(tr) - 1; i >= 0; i-- {
		t := tr[i]
		tm := strings.Replace(t.Time, "T", " ", 1)
		if len(tm) > 19 {
			tm = tm[:19]
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s/%d\t%d\t%d\n",
			tm, tuiDash(t.Caller), tuiDash(t.RequestModel), tuiDash(t.RoutePath),
			tuiDash(t.Target), t.Outcome, t.HTTPStatus, len(t.Candidates), t.TotalMS)
	}
	tw.Flush()
}

func (c *tuiClient) drawKeys(w io.Writer) {
	kb, err := c.keys()
	if err != nil {
		fmt.Fprintf(w, "\n[绑定] 读取失败: %v\n", err)
		return
	}
	fmt.Fprintf(w, "\n[调用方默认模型绑定] 用 switch <序号> <目标> 修改\n")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  #\tKEY\t默认模型")
	for i, b := range kb.Bindings {
		fmt.Fprintf(tw, "  %d\t%s\t%s\n", i+1, b.Key, b.DefaultModel)
	}
	tw.Flush()
	if len(kb.Bindings) == 0 {
		fmt.Fprintln(w, "  （无绑定；网关对未指定 model 的请求将回 404）")
	}
}

// switchKey 把第 idx 个绑定的默认模型改为 target（PUT 全文档写回，
// 服务端校验 + 备份 + 审计）。
func (c *tuiClient) switchKey(w io.Writer, args []string) error {
	if len(args) != 2 {
		return errors.New("用法: switch <序号> <目标>（如 switch 1 group/coders）")
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil || idx < 1 {
		return errors.New("序号须为正整数")
	}
	target := args[1]
	kb, err := c.keys()
	if err != nil {
		return err
	}
	if idx > len(kb.Bindings) {
		return fmt.Errorf("序号超出范围（当前 %d 条绑定）", len(kb.Bindings))
	}
	old := kb.Bindings[idx-1].DefaultModel
	kb.Bindings[idx-1].DefaultModel = target
	if err := c.putKeys(*kb); err != nil {
		return err
	}
	fmt.Fprintf(w, "已切换 %s：%s → %s（服务端备份已留底）\n", kb.Bindings[idx-1].Key, old, target)
	return nil
}

// ---------- HTTP 客户端 ----------

type tuiClient struct {
	base  string
	token string
	hc    *http.Client
}

func (c *tuiClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("X-Gw-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *tuiClient) post(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("X-Gw-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *tuiClient) put(path string, body []byte, out any) error {
	req, err := http.NewRequest(http.MethodPut, c.base+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Gw-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *tuiClient) state() (*stateResp, error) {
	var st stateResp
	if err := c.get("/api/state", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *tuiClient) traces(n int) ([]traceResp, error) {
	var out struct {
		Traces []traceResp `json:"traces"`
	}
	err := c.get("/api/trace?limit="+strconv.Itoa(n), &out)
	return out.Traces, err
}

func (c *tuiClient) keys() (*keysResp, error) {
	var kb keysResp
	err := c.get("/api/config/keys", &kb)
	return &kb, err
}

func (c *tuiClient) putKeys(kb keysResp) error {
	b, err := json.Marshal(kb)
	if err != nil {
		return err
	}
	var out map[string]any
	return c.put("/api/config/keys", b, &out)
}

// ---------- 响应形态（与 /api/state 等对齐的本地最小投影） ----------

type stateResp struct {
	Version   string `json:"version"`
	Uptime    string `json:"uptime"`
	Providers []struct {
		ID string `json:"id"`
	} `json:"providers"`
	Groups []struct {
		ID       string   `json:"id"`
		Strategy string   `json:"strategy"`
		Members  []string `json:"members"`
	} `json:"groups"`
	Accounts []struct {
		Key         string `json:"key"`
		Status      string `json:"status"`
		UsedTokens  int64  `json:"used_tokens"`
		QuotaTokens int64  `json:"quota_tokens_day"`
	} `json:"accounts"`
	Resting   map[string]string `json:"resting"`
	Exhausted []string          `json:"exhausted"`
}

// RestingKeys 返回休眠账号 key 列表（确定性顺序，展示用）。
func (s *stateResp) RestingKeys() string {
	keys := make([]string, 0, len(s.Resting))
	for k := range s.Resting {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

type traceResp struct {
	Time         string `json:"time"`
	Caller       string `json:"caller"`
	RequestModel string `json:"request_model"`
	RoutePath    string `json:"route_path"`
	Target       string `json:"target"`
	Outcome      string `json:"outcome"`
	HTTPStatus   int    `json:"http_status"`
	Candidates   []struct {
		Provider string `json:"provider"`
	} `json:"candidates"`
	TotalMS int64 `json:"total_ms"`
}

type keysResp struct {
	Bindings []struct {
		Key          string `json:"key"`
		DefaultModel string `json:"default_model"`
	} `json:"bindings"`
}

func tuiDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
