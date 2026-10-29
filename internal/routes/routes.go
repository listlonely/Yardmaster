// Package routes 定义路由组策略接口与内置策略（PRD F6）。
// W5 实现 order/rotate/smart 三个内置策略并在此注册；P1 开放策略插件。
package routes

import (
	"sort"
	"time"
)

// Seat 是候选计划中的一个候选：成员模型 × 一个可用账号。
// 由 internal/gateway 的计划器构造，策略只读、排序、无副作用。
type Seat struct {
	ProviderID string
	Account    string
	Model      string
	Resting    bool      // 休眠候选排在末尾（F6）
	RestUntil  time.Time // 休眠恢复时间；零值表示未休眠
}

// Strategy 把一组 Seat 排成尝试顺序。实现必须是无副作用纯函数：
// 不读全局状态、不修改入参切片（返回新切片）。
type Strategy interface {
	Name() string
	Order(seats []Seat) []Seat
}

var registry = map[string]Strategy{}

// Register 注册策略；同名重复注册 panic（编程期错误，不做运行时处理）。
func Register(s Strategy) {
	if _, dup := registry[s.Name()]; dup {
		panic("routes: duplicate strategy " + s.Name())
	}
	registry[s.Name()] = s
}

// Find 按名取策略。未注册名（W5 之前一律如此）返回 nil，调用方必须
// 回落为「列出顺序原序处理」（PRD 规则页：未识别 routing 视为 order）。
func Find(name string) Strategy { return registry[name] }

// List 返回已注册策略名，排序输出（管理页下拉用）。
func List() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
