// 非 Windows 平台无原生窗口壳（M8-W2）：Linux 侧 Wails 依赖
// webkit2gtk（cgo + 系统库），破坏纯 Go 交叉编译矩阵——非目标
// （app.md §一铁律）。挂点保持 nil，runApp 走浏览器窗口路径（行为同
// v0.7.0，W4 回归验证等价）。本文件保证 !windows 构建完全不编译 wails
// （探针实证 GOOS=linux 产物 2.3MB 零 wails 链入）。
//
//go:build !windows

package app
