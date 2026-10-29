// Package testenv 提供测试沙箱：临时 HOME 与 Agent 环境变量隔离，
// 保证测试永不读写真实用户配置（开发文档 §5.1「测试不碰真机」）。
//
// 每个需要触碰文件系统或环境的包在自己的 TestMain 或用例中调用 New。
package testenv

import (
	"path/filepath"
	"testing"
)

// agentEnvVars 是接线/代理相关、测试中必须隔离的环境变量。
// 与真实用户环境的任何残留值混染都可能让测试结果因机器而异。
var agentEnvVars = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"OPENAI_BASE_URL", "OPENAI_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
}

// New 把 HOME/USERPROFILE/APPDATA 指到临时目录，并清空 Agent 相关变量，
// 返回临时 HOME 路径。内部使用 t.Setenv（测试结束自动还原）。
//
// 注意：t.Setenv 与 t.Parallel 互斥——需要并行的用例不要调用 New，
// 而是注入路径（如 store.Paths、edit.WriteAtomic 都支持显式路径）。
func New(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	for _, k := range agentEnvVars {
		// 置空而非删除：足够阻断对真实环境的读取；需要严格删除的
		// 场景在各自测试内用 os.Unsetenv + 还原处理。
		t.Setenv(k, "")
	}
	return home
}
