// 导入解析器注册表（M3-W1 骨架）：把各 Agent 凭证文件的原始字节
// 归一化为 OAuth。内置解析器（claude-code/codex/gemini-cli）按排期
// 于 M3-W2 落地——以各 Agent 真实数据/文档核对字段后实现，绝不凭
// 字段名猜测（§4.2）。
package credential

import (
	"fmt"
	"sort"
	"sync"
)

// Parser 把一种 Agent 凭证文件的原始内容解析为归一化 OAuth。
// 解析失败必须返回错误（读不懂=未知），不得返回零值凭证。
type Parser func(data []byte) (OAuth, error)

var (
	parserMu       sync.RWMutex
	parserRegistry = map[string]Parser{}
)

// RegisterParser 注册 format 的解析器；重复注册返回错误（避免测试与
// 将来插件静默互相覆盖）。
func RegisterParser(format string, p Parser) error {
	parserMu.Lock()
	defer parserMu.Unlock()
	if format == "" || p == nil {
		return fmt.Errorf("parser 注册需要非空格式与函数")
	}
	if _, dup := parserRegistry[format]; dup {
		return fmt.Errorf("parser %q 已注册", format)
	}
	parserRegistry[format] = p
	return nil
}

// Lookup 查找已注册解析器；未注册返回 false（调用方 fail-closed）。
func Lookup(format string) (Parser, bool) {
	parserMu.RLock()
	defer parserMu.RUnlock()
	p, ok := parserRegistry[format]
	return p, ok
}

// Names 返回全部已注册格式名（字典序，供 CLI 提示）。
func Names() []string {
	parserMu.RLock()
	defer parserMu.RUnlock()
	names := make([]string, 0, len(parserRegistry))
	for k := range parserRegistry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
