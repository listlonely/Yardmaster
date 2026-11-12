// Package plugin 实现子进程插件体系（M7-W3，设计稿
// docs/subsystems/plugins.md）：长驻进程 + NDJSON 管道协议。父侧纯 Go
// （os/exec+bufio，零 cgo，交叉编译矩阵不变）；崩溃隔离靠 OS 进程边界
// ——插件死=功能降级，网关主流程不受影响。
//
// 协议（§三）：握手（插件 stdout 首行声明，5s 超时）→ 父回 granted
// （配置∩声明，用户值优先）→ 信封 {"v":1,"type","id","payload"} 往返 /
// 事件单向；心跳 30s×2 判死；行长上限 1MB。
//
// fail-closed（§五）：plugins.json 解析失败=全部禁用（调用方按空表处
// 理+报错指引）；capability 枚举外拒绝；超时 100ms~30s。
package plugin

import (
	"errors"
	"fmt"
	"os"

	"yardmaster/internal/store"
)

// 扩展点 capability（枚举外值拒绝装载）。
const (
	CapEvents   = "events.trace" // 事件订阅（fire-and-forget）
	CapClassify = "classify.intent"
	CapVendor   = "vendor.verdict"
	CapOAuth    = "provider.oauth" // 订阅厂商适配（适配本体在插件，主程序无厂商代码）
	// CapRewrite 族是请求/响应改写钩子（2026-11-10，magpie-bridge 验证），
	// 语义对齐 Magpie 网关中间件三钩子（抛错=放行，拒绝可显式）：
	//   provider.rewrite          入站请求改写（onRequest）
	//   provider.rewrite.response 完整上游响应改写（onResponse，非流式）
	//   provider.rewrite.event    流式事件改写（onEvent，SSE 帧）
	// adapter 按中间件实际导出的钩子声明，配置授予∩声明决定生效面。
	CapRewrite      = "provider.rewrite"
	CapRewriteResp  = "provider.rewrite.response"
	CapRewriteEvent = "provider.rewrite.event"
)

var capabilitySet = map[string]bool{
	CapEvents: true, CapClassify: true, CapVendor: true, CapOAuth: true,
	CapRewrite: true, CapRewriteResp: true, CapRewriteEvent: true,
}

// IntentRef 是插件声明的一个意图（classify.intent 能力）：插件自带
// 词表元数据，管理面透出后并入意图分配表与词表校验（声明即同步）。
type IntentRef struct {
	ID    string `json:"id"`              // 稳定 ID；建议命名空间化（如 sre/oncall）
	Label string `json:"label,omitempty"` // 展示名
	Desc  string `json:"desc,omitempty"`  // 一句话说明
}

// Spec 是 plugins.json 的一个插件条目。
type Spec struct {
	ID           string         `json:"id"`                     // 唯一标识
	Command      []string       `json:"command"`                // argv（外部可执行体）
	Enabled      bool           `json:"enabled,omitempty"`      // 缺省 false（显式开启）
	Capabilities []string       `json:"capabilities,omitempty"` // 声明的能力（实际授予=配置∩声明）
	Desc         string         `json:"desc,omitempty"`         // 简介（管理面插件行展示）
	Intents      []IntentRef    `json:"intents,omitempty"`      // 声明意图（须具备 classify.intent 能力）
	TimeoutMS    int            `json:"timeout_ms,omitempty"`   // 请求/响应钩子超时；0=1000ms
	Env          []string       `json:"env,omitempty"`          // 附加环境变量；含 "INHERIT_ENV=1" 则继承父进程全量环境
	Options      map[string]any `json:"options,omitempty"`      // 中间件静态配置（握手 settings.options 透传，如 param_override JSON）
}

// File 是 plugins.json 的文件形态。
type File struct {
	Plugins []Spec `json:"plugins"`
}

// ErrNoPlugin 是无可用的目标插件（未装载/未运行/无该能力）。
var ErrNoPlugin = errors.New("无可用的插件（未装载/未运行/未授予该能力）")

// ErrCooldown 是该能力处于失败冷却中（连续失败熔断）。
var ErrCooldown = errors.New("插件能力冷却中（连续失败，5 分钟后重试）")

// Validate 校验插件清单（fail-closed，写入口与加载共用）。
func Validate(f File) error {
	seen := map[string]bool{}
	for _, p := range f.Plugins {
		if p.ID == "" {
			return errors.New("插件 id 不能为空")
		}
		if seen[p.ID] {
			return fmt.Errorf("插件 id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if len(p.Command) == 0 {
			return fmt.Errorf("插件 %s 的 command 不能为空", p.ID)
		}
		for _, c := range p.Capabilities {
			if !capabilitySet[c] {
				return fmt.Errorf("插件 %s 的未知能力 %q（支持: events.trace/classify.intent/vendor.verdict/provider.oauth/provider.rewrite[.response|.event]）", p.ID, c)
			}
		}
		if len(p.Intents) > 0 {
			hasClassify := false
			for _, c := range p.Capabilities {
				if c == CapClassify {
					hasClassify = true
					break
				}
			}
			if !hasClassify {
				return fmt.Errorf("插件 %s 声明了意图但未具备 classify.intent 能力", p.ID)
			}
			for _, ir := range p.Intents {
				if ir.ID == "" {
					return fmt.Errorf("插件 %s 的声明意图 id 不能为空", p.ID)
				}
			}
		}
		if p.TimeoutMS != 0 && (p.TimeoutMS < 100 || p.TimeoutMS > 30000) {
			return fmt.Errorf("插件 %s 的 timeout_ms 须为 100~30000（0=默认 1000）", p.ID)
		}
	}
	return nil
}

// Load 读取 plugins.json；不存在返回空 File（os.ErrNotExist 语义保留给
// 调用方），解析失败返回错误（调用方 fail-closed：全部禁用+报错指引）。
func Load(path string) (File, error) {
	var f File
	if err := store.LoadJSON(path, &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, err
		}
		return File{}, err
	}
	return f, nil
}

// Save 原子写入 plugins.json（调用方负责写前备份——与管理 API writeConfig
// /CLI 备份栈同规）。
func Save(path string, f File) error { return store.SaveJSON(path, f) }
