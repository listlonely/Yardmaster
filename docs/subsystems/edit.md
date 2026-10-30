# edit — 配置安全编辑子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 原子写 | `WriteAtomic`：临时文件 + rename；保留已有权限位，新建 0600；失败不触碰原文件 | [`edit.go`](../../internal/edit/edit.go) |
| JSONC 键级编辑 | `ParseJSONC`/`Set`/`Unset`/`Bytes` 与一次性助手 `SetJSONC`/`UnsetJSONC`：只改动目标键的原始字节区间，其余逐字节保留 | [`jsonc.go`](../../internal/edit/jsonc.go) |
| 值序列化 | `formatValue`：对象/数组按目标行缩进多行展开，标量单行；键排序保证确定性；HTML 转义关闭 | [`jsonc.go`](../../internal/edit/jsonc.go) |

普通 JSON 是 JSONC 的子集，同一实现覆盖两者。TOML/YAML/env 的键级编辑器在 M1-W4 按需补充（Codex 的 TOML、OpenCode 的 JSONC、Gemini 的 JSON 优先）。

## 实现模型 [2026-10-29-区间拼接保注释]

解析不建立完整 AST 持久层，而是记录每个成员在源文中的**原始区间**：
`[keyStart, commaEnd)`；编辑即区间拼接（splice）：

- **Set**：替换目标成员的值区间 `[valStart, valEnd)`，缩进取目标键所在行的行首空白；缺失父级先递归创建空对象成员，重解析后重走。
- **Unset**：删除区间 = 从成员键所在**行的行首**（回溯吞并前导空白与上一行的 `\n`，CRLF 时含配对 `\r`）到成员尾逗号之后；删除**最后一个**成员时，向左跨过行终止符寻找并移除前一成员的尾逗号，避免残留尾逗号破坏严格 JSON。
- **注释**位于成员之间的间隙，不在任何被拼接区间内，因此天然保留；行尾注释随其所在成员的值区间归属（值被替换时该行注释保留在行尾）。

## 约束与失效行为

- 只写名下键：编辑器只触碰目标区间；父级对象在成员清空后**保留**（不级联删除，F4）。
- BOM：解析时剥除并记录，`Bytes()` 原样还原；CRLF/LF 由字节保留自然维持。
- `Unset` 幂等：键或中间父级不存在时为空操作，字节零改动。
- `Set` 遇到中间层为标量时报错（不静默替换整棵子树）。
- 已知边界：同一物理行内的多个成员（`{ "a": 1, "b": 2 }`）删除其一可能留下合法 JSONC 的尾逗号与多余空白；插入成员时，前一行行尾 `//` 注释会随逗号归到分隔位置。两者均不破坏 JSONC 合法性，M1 接受。

## 验证

```sh
go test -tags nogui ./internal/edit
go test -tags nogui ./internal/edit -run 'TestSet|TestUnset|TestBOM'
```

无 `nogui` 标签时直接 `go test ./internal/edit`。门禁要求：还原旧代码后新测试必须失败（开发文档 §5.1）。
