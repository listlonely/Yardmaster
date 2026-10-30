# dispatch 子系统 —— 任务→Agent 派发（M5-W3 设计稿）

> [2026-10-30-派发设计定稿] M5-W3 出稿（开发文档 §9.6 主线可行性复核
> 点）。状态：**设计可行，主线采纳**——三家 headless 输出形状未实测是
> 主要风险，已用「text 输出模式为缺省 + 形状实测列入 W6」缓解，无需
> 切换插件体系备选。载体 `internal/dispatch`（W3：runner 原语）+
> `internal/app/run_cmd.go`（CLI run）。
> [2026-10-30-真实校准] M6-W1 已完成 Claude Code 真实冒烟（形状/事件
> 词表/缓存对账闭环，见 §九）；codex/gemini 降级留档。

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
- 三家建议形状（官方公开文档口径；用户可在 dispatch.json 覆盖）：

| Agent | 建议命令 | 输出模式 | 备注 |
| :-- | :-- | :-- | :-- |
| Claude Code | `claude -p --output-format stream-json --verbose` | jsonl | **已实测**（[2026-10-30-claude真实冒烟]）：`-p` 非交互、stdin 收任务；stream-json 须配 `--verbose` |
| Codex CLI | `codex exec --json` | jsonl | 事件流形状待实测（本机无 OpenAI 订阅，降级留档） |
| Gemini CLI | `gemini -p` | text | 形状待实测（本机无 Google 登录，降级留档） |

- Claude Code 实测事件词表（stream-json，W3 事件解析词表依据）：
  `{"type":"system","subtype":"init",…}` → `{"type":"assistant","message":{content:[{type:"text",…}]}}`
  →（工具轮次 `{"type":"user",…}`）→ `{"type":"result","subtype":"success","result":"…","usage":{…}}`。
  终态 `result` 字段与 ExtractResult 的键表命中；`subtype` 取 success/error。
- 接线模板（经网关回环，[2026-10-30-claude真实冒烟] 验证）：env 三件套
  `ANTHROPIC_BASE_URL=http://127.0.0.1:8317`、`ANTHROPIC_AUTH_TOKEN=<网关key>`、
  `ANTHROPIC_MODEL=<provider/model 形式>`（planner 只认 provider/model；
  claude 对带前缀模型名报 unrecognized_model 警告但不影响调用）。

- 内置建议条目仅作 `dispatch init` 模板来源（写入前打印预览），
  **不自动注册**——命令形状由用户确认，ToS 边界（不代装、不代登录）
  由「启动失败即报错指引」兜底。

## 六、交付切分

| 周 | 交付 | 状态 |
| :-- | :-- | :-- |
| W3 | 本设计稿 + runner 原语（Run/Outcome/Result）+ CLI `run` 直通 | 已交付 |
| W4 | dispatch.json 加载校验（fail-closed）+ 匹配引擎 + 会话表 + 派发 trace | 已交付 |
| W5 | 管理面 API（run/agents/sessions/cancel + config/dispatch 写路径热替换）+ 控制台派发页 + CLI `dispatch` | 已交付 |
| W6 | 双平台冒烟 + 发布 v0.5.0 | 已交付（见下「冒烟记录与未实测项」） |

## 八、冒烟记录与未实测项（W6，如实标注）

已实测（2026-10-30，Windows 本机）：

- 管理面全链路：`POST /api/dispatch/run`（后台 ctx 会话）→ 会话 done（44ms）→
  详情含输出尾部 → trace 落盘 `route_path=dispatch`（与真实模型请求 trace
  同构并存）；取消路径与配置 PUT 热替换由 httptest 全覆盖。
- 控制台派发页浏览器验证（7/7）：tab 切换、agent 下拉、任务提交、
  会话表 5s 自动刷新至终态、输出面板回显。
- CLI：`run`（stdin→stdout 透传、退出码透传/124/127/130）、
  `dispatch agents`、`dispatch run`（匹配选择 + 流式输出 + 摘要行）。
- 假 Agent 冒烟以 `cmd /c more`（stdin 回显）为载体，验证
  stdin→Agent→stdout→会话/trace 的字节保真链路。

未实测项（如实标注）：

- ~~三家真实 Agent headless 形状~~ → **Claude Code 已实测**（2026-10-30
  M6-W1，见下「真实校准记录」）；codex/gemini 本机无对应订阅/登录，
  降级留档（形状沿用官方文档口径）。
- Linux 侧真实派发冒烟（无 WSL/实机；降级为 CI fake-agent 冒烟）。

## 九、真实校准记录（M6-W1，2026-10-30）[2026-10-30-真实校准]

- **Claude Code headless 真实冒烟**（claude-code 2.1.314，Windows 本机）：
  `claude -p`（stdin 任务）→ 网关 `/v1/messages`（anthropic 入站）→ 翻译
  → 硅基流动（openai-chat 上游）→ 回翻 → claude 解析成功，exit=0，
  `result` 字段正确；dispatch 引擎全链路 `d-* · ok`。gemini-cli/codex
  因无对应订阅未实测（降级留档）。
- **OTLP 真实后端**（Jaeger v2.22.0 all-in-one，OTLP/HTTP JSON）：
  `receiver_accepted_spans=20` + `exporter_sent_spans=20` 双指标证实
  接收与落库（注：Windows 端口排除段 4301-4400/8838-8937 挡默认
  4317/4318/8888，本机配置挪 14317/14318/18888）。
- **缓存命中对账**：硅基流动回 cached=6912；网关 trace
  `{prompt:11879, completion:64, cached:6912}` ↔ claude 端
  `input_tokens:4967 + cache_read_input_tokens:6912`（11879=4967+6912
  咬合），claude 端成本核算随之按缓存价计算（$0.0488→$0.0226）。
- **校准抓出并修复的缺陷**（回归测试 m6w1_test.go，还原旧代码即失败）：
  1. msgsEncoder 流式回翻 usage 硬编码 `output_tokens:0` 且不处理
     evUsage——真实客户端用量统计全零；
  2. 事件层 gwUsage 无 cached 字段，缓存命中在客户端侧丢失——补齐
     gwUsage.cached 后四 decoder 提取（chat 的 prompt_tokens_details/
     responses 的 input_tokens_details/gemini 的 cachedContentTokenCount/
     anthropic 的 message_start.usage 折叠）、四 encoder 按各家语义展开
     （anthropic/responses 出口拆「未命中+命中」两列，chat/gemini 原样
     携带），五处非流式翻译同步对齐（内部统一「prompt 含命中」口径，
     出口按目标协议展开）。
- **sealed reasoning 终局**：本机无 Codex 订阅凭证，维持 M5-W2 的 (b)
  标注方案，降级留档。

## 十、边界与铁律对照

- ToS：仅启动用户已安装的 Agent（launch 失败报错指引），不代装不代登录。
- 纯 Go：进程管理仅 os/exec，无 cgo，交叉编译矩阵不变。
- fail-closed：dispatch.json 解析失败整体禁用；匹配无果拒绝；未知
  输出形状退 text 全文（不猜）。
- 五铁律不涉及（dispatch.json 为新配置，写路径走管理 API 同一闸门：
  PUT 校验 + 备份 + 审计，W5 接入）。
