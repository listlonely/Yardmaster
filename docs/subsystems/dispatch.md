# dispatch 子系统 —— 任务→Agent 派发（M5-W3 设计稿）

> [2026-10-30-派发设计定稿] M5-W3 出稿（开发文档 §9.6 主线可行性复核
> 点）。状态：**设计可行，主线采纳**——三家 headless 输出形状未实测是
> 主要风险，已用「text 输出模式为缺省 + 形状实测列入 W6」缓解，无需
> 切换插件体系备选。载体 `internal/dispatch`（W3：runner 原语）+
> `internal/app/run_cmd.go`（CLI run）。

## 一、产品形态（产品设计文档 §1 差异化定位）

网关从「模型流量路由」升级到「任务路由」：用户把一段任务文本交给
Yardmaster，它按派发规则选择本机**已安装**的 Agent（Claude Code /
Codex / Gemini CLI 的 headless 模式），启动子进程执行，流式回传
stdout，结果与耗时留痕。与 `internal/agent` 接线子系统协同成闭环：
接线把 Agent 的模型流量指向 Yardmaster，派发把 Yardmaster 的任务
交给 Agent——任务的模型调用仍经网关路由（意图/规则/故障转移全部生效）。

## 二、意图→Agent 映射（dispatch.json，独立于 rules.json）

- rules.json 处理**模型请求级**改写（单次 API 调用的 target 覆盖）；
  派发处理**任务级**路由（一个任务 = 一次 headless 会话）。语义不同，
  配置分文件：`dispatch.json`。
- 每个 Agent 条目：`id`、`command[]`（argv 模板，`{model}` 占位）、
  `work_dir`、`env[]`、`timeout`（缺省 10m）、`max_concurrent`（缺省 1）、
  `match`（可选：`intent` 精确 / `model_regex` / `prompt_regex`）、
  `default: true`（兜底，至多一个）。
- 匹配顺序：条目声明顺序首个命中；无 match 条目仅可显式指定；全部
  未命中且无 default → 拒绝（fail-closed，不静默选错 Agent）。
- 校验（fail-closed）：id 非空唯一、command 非空、正则可编译、
  default 至多一个、超时/并发为正。文件解析失败 → 派发整体禁用
  （报错指引修复，绝不当作空配置静默跑）。

## 三、headless 会话管理

- 会话 = 一次 headless 进程：任务文本经 **stdin** 传入（规避 argv 转义
  与长度上限）；stdout 逐块转发（CLI/控制台实时可见）+ 尾部 64KB 留存；
  stderr 截尾 2KB 作失败摘要。
- 结局归一五类：`ok`（退出 0）/ `exit`（非 0，透传退出码）/
  `timeout`（超时杀进程）/ `cancelled`（上级取消）/ `launch`（启动失败，
  如二进制不存在——按 ToS 提示「仅支持本机已安装的 Agent，不代装」）。
- 超时与取消：`exec.CommandContext` 到期 Kill **直接子进程**；孙进程
  残留为已知边界（跨平台纯 Go 约束，杀进程树需平台代码）——已知边界，
  不引入 cgo。
- 并发上限：每 Agent 信号量（max_concurrent），全局会话数上限（缺省
  8，防失控）。超限拒绝入队（不排队——headless 会话长，排队体验差）。
- 会话表：内存环形（最近 256 条：id/agent/任务摘要 512 字/状态/起止/
  Result）；重启即清（跨进程持久化不在 M5 范围）。

## 四、结果回传与派发 trace（复用 F9）

- 结果视图：`Result{agent, outcome, exit_code, output(尾 64KB), err,
  duration_ms}`；CLI `dispatch run` 直接透传 stdout + stderr 摘要行；
  控制台派发页展示会话列表 + 详情（W5）。
- trace 复用 TraceLog：派发会话记一条 `Trace{RoutePath:"dispatch",
  ClientProto:"dispatch", Served:"agent/<id>", Outcome, Candidates:[
  {Provider:agent, Result: ok|launch|timeout|...}], Usage:nil}`——
  观测页与 OTLP 导出对派发会话零改动生效（用量为 nil：headless 会话
  的 token 消耗经网关的模型请求已各自计量，不重复计）。

## 五、stdin/stdout 协议与三家形状（读不懂=未知）

- 输入协议：任务文本全文经 stdin；argv 模板仅 `{model}` 占位（其余
  原样，未知占位不猜语义）。
- 输出模式两档（spec `output` 字段，缺省 `text`）：
  - `text`：stdout 整体即最终文本（缺省，形状无关，最稳）；
  - `jsonl`：每行一个 JSON 事件，取**最后一条**含 `text`/`content`/
    `result` 字段的字符串值为结果（容错解析：单行解析失败跳过）。
- 三家建议形状（**未经真实凭证实测**，官方公开文档口径；W6 冒烟核实，
  未实测项如实标注）：

| Agent | 建议命令（用户可在 dispatch.json 覆盖） | 输出模式 | 备注 |
| :-- | :-- | :-- | :-- |
| Claude Code | `claude -p --output-format text` | text | `-p` 非交互；stdin 收任务 |
| Codex CLI | `codex exec --json` | jsonl | 事件流形状待实测 |
| Gemini CLI | `gemini -p` | text | `-p` 非交互 |

- 内置建议条目仅作 `dispatch init` 模板来源（写入前打印预览），
  **不自动注册**——命令形状由用户确认，ToS 边界（不代装、不代登录）
  由「启动失败即报错指引」兜底。

## 六、交付切分

| 周 | 交付 | 状态 |
| :-- | :-- | :-- |
| W3 | 本设计稿 + runner 原语（Run/Outcome/Result）+ CLI `run` 直通 | 已交付 |
| W4 | dispatch.json 加载校验（fail-closed）+ 匹配引擎 + 会话表 + 派发 trace | 计划 |
| W5 | 管理面 API（run/sessions/cancel）+ 控制台派发页 + CLI `dispatch` | 计划 |
| W6 | 三家真实形状冒烟（本机未装 → 如实标注未实测）+ 发布 v0.5.0 | 计划 |

## 七、边界与铁律对照

- ToS：仅启动用户已安装的 Agent（launch 失败报错指引），不代装不代登录。
- 纯 Go：进程管理仅 os/exec，无 cgo，交叉编译矩阵不变。
- fail-closed：dispatch.json 解析失败整体禁用；匹配无果拒绝；未知
  输出形状退 text 全文（不猜）。
- 五铁律不涉及（dispatch.json 为新配置，写路径走管理 API 同一闸门：
  PUT 校验 + 备份 + 审计，W5 接入）。
