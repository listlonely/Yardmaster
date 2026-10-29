# agent — Agent 接线子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 声明式注册表 | 5 个 MVP Agent 的配置位置/格式/字段，全部数据化；引擎不感知具体 Agent | [`registry.go`](../../internal/agent/registry.go) |
| 接线引擎 | `Preview/Wire/Unwire/Drift/StateOf`：预览 diff、备份栈、原子写、审计 | [`wiring.go`](../../internal/agent/wiring.go) |
| TOML 编辑 | 行级拼接：表定位、键替换/插入/删除、行尾注释保留、CRLF 保留 | [`edit/toml.go`](../../internal/edit/toml.go) |
| JSONC 编辑 | 区间拼接 + `Get`（漂移比对） | [`edit/jsonc.go`](../../internal/edit/jsonc.go) |
| CLI | `agenthub agents list/preview/wire/unwind/drift`；wire 缺省只打印预览（F4 强制预览+确认） | [`app/agents_cmd.go`](../../internal/app/agents_cmd.go) |

## 运行路径

1. **接入**：`agents wire <id> -model <模型> -yes` → 打印 diff → 备份原文件到
   `stash/<agent>/0`（**已有备份不覆盖**：二次接线时首次原始值优先）→ 按
   声明式 Op 逐键写入 → `applied.json` 记录审计（时间/addr/key/model/文件/键）。
2. **断开**：`agents unwind <id>` → 有 stash 的文件逐字节还原；软件新建的
   文件走逆操作（Unset 名下键 → 级联清理空父对象 → 仅剩空对象则移除文件）。
3. **漂移**：`agents drift <id>` → 对名下键逐个 `Get` 当前值与最后写入值
   比对；任何漂移使状态变为 `drift`（状态机见 PRD 规则页）。
4. **重新应用**：unwind（还原）→ wire（覆盖），覆盖前的值仍在首次备份栈中。

## 约束与失效行为

- **规格待核验拒绝**（§4.2）：gemini-cli（端点依赖环境变量注入）、cline
  （VS Code globalState 存储层）的写入规格未对照官方实现核验前，wire/preview
  一律拒绝并说明原因——不写猜出来的键。
- 只写名下键；断开还原后 `applied.json` 移除记录；stash 保留（供再次接入对照）。
- TOML 已知边界：多行数组/内嵌表不做行内匹配（目标键均为单行标量）；
  同名键多表按表头定位；行内多成员删除可能残留合法 JSONC 尾逗号。
- 逆操作对「软件创建文件」会重排 JSON（紧凑化）——该文件整体为本软件创建，
  格式归本软件管；来自 stash 的还原不受影响。

## 验证

```sh
go test ./internal/agent
go test ./internal/edit -run TOML
```
