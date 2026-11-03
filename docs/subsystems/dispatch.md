# dispatch 子系统 —— 任务→Agent 派发（M5-W3 设计稿）

> [2026-10-30-派发设计定稿] M5-W3 出稿（开发文档 §9.6 主线可行性复核
> 点）。状态：**设计可行，主线采纳**——三家 headless 输出形状未实测是
> 主要风险，已用「text 输出模式为缺省 + 形状实测列入 W6」缓解，无需
> 切换插件体系备选。载体 `internal/dispatch`（W3：runner 原语）+
> `internal/app/run_cmd.go`（CLI run）。
> [2026-10-30-真实校准] M6-W1 已完成 Claude Code 真实冒烟（形状/事件
> 词表/缓存对账闭环，见 §九）；codex/gemini 降级留档。
> [2026-11-02-事件级实时] M6-W3 已交付：事件解析器按 W1 词表（§五）
> 流式解析 stdout，控制台事件视图 + CLI `dispatch run -events`；解析
> 失败回退 raw 原样（容错不变）。事件仅是输出视图的再加工，不改留存
> 语义。
> [2026-11-02-任务模板] M6-W4 已交付：dispatch.json `templates[]`
> （§五·一）命名任务 + {param} 占位 + 默认 agent/超时；CLI
> `dispatch run -t <模板> key=val…` 与控制台模板下拉双入口；缺参/
> 多传键/未知模板/引用缺失 agent 一律 fail-closed 拒绝。
> [2026-11-02-结果消费与重派] M6-W5 已交付：失败会话一键重派（§四·一，
> 新会话新候选链）+ 结果导出/复制（原文尾部）+ 会话筛选（agent/状态/
> 时间窗，客户端）；托盘通知为可砍项按计划砍除。

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
  Result）；**持久化（M6-W2）**：终态追加落盘
  `trace/dispatch-sessions.jsonl`（与决策 trace 同目录、同 30 天留存
  策略），serve 启动恢复最近 256 条进内存环形——控制台派发页历史跨
  重启可见。[2026-10-30-会话持久化]

- 留存/审计语义（M6-W2）：落盘内容 = 会话审计所需字段（任务摘要
  ≤512 字、输出尾部 ≤64KB、结局/退出码/耗时/起止时刻），**不含**
  Agent 环境变量（dispatch.json 中的 env 含凭证语义，不进持久化文件）
  与完整对话流（stdout 全文只在运行期/live 缓冲与 CLI 透传）。启动
  compact 过滤超期行（时间读不懂=不留，宁紧勿松）与坏行；恢复的
  `running` 会话一律标 `failed`（进程重启即被杀，不猜结果）；落盘
  先于终态信号（`close(finished)`）——Wait 返回即历史已落盘。

## 四、结果回传与派发 trace（复用 F9）

- 结果视图：`Result{agent, outcome, exit_code, output(尾 64KB), err,
  duration_ms}`；CLI `dispatch run` 直接透传 stdout + stderr 摘要行；
  控制台派发页展示会话列表 + 详情（W5）。
- trace 复用 TraceLog：派发会话记一条 `Trace{RoutePath:"dispatch",
  ClientProto:"dispatch", Served:"agent/<id>", Outcome, Candidates:[
  {Provider:agent, Result: ok|launch|timeout|...}], Usage:nil}`——
  观测页与 OTLP 导出对派发会话零改动生效（用量为 nil：headless 会话
  的 token 消耗经网关的模型请求已各自计量，不重复计）。

### 四·一、结果消费与重派（M6-W5）[2026-11-02-结果消费与重派]

- 失败会话一键重派：`POST /api/dispatch/sessions/{id}/redispatch`
  ——以**留存的任务摘要**（≤512 字；超长任务以 … 结尾提示，全文按
  §三留存纪律不落盘）与相同 agent/model 提交一个**全新会话**；复用
  trace 候选链语义：旧会话与其 trace 原样留档，重派作为新候选链独立
  留痕（不续跑旧进程）。仅 `failed` 可重派（`cancelled` 是用户主动
  取消，不鼓捣重跑）；agent 已被配置移除时 fail-closed 拒绝（不静默
  换 agent）。控制台入口：failed 行内「重派」按钮。
- 结果导出/复制：`GET /api/dispatch/sessions/{id}/export` 以附件下载
  留存输出尾部（text → `dispatch-<id>.txt`、jsonl → `.jsonl`——导出
  即留存语义，≤64KB 尾部）；控制台详情按钮条「导出」（fetch→blob
  下载，避免 location.href 附件导航以 ERR_ABORTED 收场）与「复制」
  （剪贴板写原文尾部）。
- 会话筛选：控制台客户端筛选（环形 ≤256+恢复条目，无需服务端分页）
  ——agent（按历史会话集合动态生成）、状态（running/done/failed/
  cancelled）、时间窗（近 1h/24h/7d，按 started_at）。
- 砍项：完成通知（托盘气泡）按开发文档 §9.7 可砍清单砍除。

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

- 事件视图（M6-W3）[2026-11-02-事件级实时]：`output: "jsonl"` 会话的
  stdout 由 LiveBuffer 流式喂入事件解析器（`internal/dispatch/events.go`），
  按上述词表归类六种事件：`init` / `text` / `tool`（tool_use 折叠为单行
  JSON 摘要）/ `toolresult` / `result`（Final）/ `raw`（读不懂=未知，
  原样保留——解析失败回退纯文本，内容零丢失）。留存护栏：单会话 512 条
  环形；文本 8KB / 工具摘要 512B / 回传 2KB 截断（UTF-8 安全）。半行经
  终态 Flush 收口（运行期视图读取不冲刷，避免半行与后续分块重复）。
  出口三处：控制台会话详情 `events` 字段（类型着色/工具折叠/终态高亮，
  可切原文视图）、CLI `dispatch run -events` 实时渲染、恢复会话从留存
  尾部现场解析（旧记录无 output_mode 时维持纯文本）。事件是输出视图的
  再加工，不改 Result.Output 留存与 ExtractResult 语义。
- 接线模板（经网关回环，[2026-10-30-claude真实冒烟] 验证）：env 三件套
  `ANTHROPIC_BASE_URL=http://127.0.0.1:8317`、`ANTHROPIC_AUTH_TOKEN=<网关key>`、
  `ANTHROPIC_MODEL=<provider/model 形式>`（planner 只认 provider/model；
  claude 对带前缀模型名报 unrecognized_model 警告但不影响调用）。

- 内置建议条目仅作 `dispatch init` 模板来源（写入前打印预览），
  **不自动注册**——命令形状由用户确认，ToS 边界（不代装、不代登录）
  由「启动失败即报错指引」兜底。

### 五·一、任务模板（M6-W4）[2026-11-02-任务模板]

- 配置形态（dispatch.json `templates[]`，与 agents 同文件同写路径——
  PUT /api/config/dispatch 校验闸自动覆盖）：`{name, task, agent?,
  model?, timeout?}`；task 含 `{param}` 占位（占位名限字母/数字/下划线/
  连字符；形如 `{}` 或 `{not a name}` 不识别为占位、原样保留——与 argv
  展开的「未知占位不猜语义」同款）。
- 校验（fail-closed）：name 非空唯一、task 非空、agent 引用必须存在于
  本文件 agents、timeout 可解析为正时长——任一违反整体拒绝（写路径 400
  不落盘，加载路径派发禁用）。
- 实例化（fail-closed）：缺占位报错（列出缺失名）；多传键报错（防手
  误）；替换仅按已验合法的占位名逐键进行。
- 运行语义：agent = 请求显式 > 模板默认 > match/default 匹配；model =
  请求显式 > 模板默认；模板 `timeout` 覆盖所选 agent 的超时（会话级，
  不改 dispatch.json）。意图分类照常作用于实例化后的任务文本。
- 双入口：CLI `dispatch run -t <模板> key=val…`（位置参数按 key=val
  传参，缺等号即用法错误）；控制台派发页模板下拉（GET
  /api/dispatch/templates 返回清单+占位名，选中即动态生成参数输入，
  提交 `{template, params}`）。

## 六、交付切分

| 周 | 交付 | 状态 |
| :-- | :-- | :-- |
| W3 | 本设计稿 + runner 原语（Run/Outcome/Result）+ CLI `run` 直通 | 已交付 |
| W4 | dispatch.json 加载校验（fail-closed）+ 匹配引擎 + 会话表 + 派发 trace | 已交付 |
| W5 | 管理面 API（run/agents/sessions/cancel + config/dispatch 写路径热替换）+ 控制台派发页 + CLI `dispatch` | 已交付 |
| W6 | 双平台冒烟 + 发布 v0.5.0 | 已交付（见下「冒烟记录与未实测项」） |
| M6-W1 | 真实校准（Claude Code 冒烟/OTLP/缓存对账/缺陷修复） | 已交付（见 §九） |
| M6-W2 | 会话持久化（dispatch-sessions.jsonl + 启动恢复） | 已交付（见 §三） |
| M6-W3 | jsonl 事件级实时（解析器/控制台事件视图/CLI `-events`） | 已交付（[2026-11-02-事件级实时]，见 §五） |
| M6-W4 | 任务模板（templates[]/CLI `-t`/控制台模板下拉） | 已交付（[2026-11-02-任务模板]，见 §五·一） |
| M6-W5 | 结果消费与重派（重派/导出复制/筛选；托盘通知砍） | 已交付（[2026-11-02-结果消费与重派]，见 §四·一） |
| M6-W6 | 全链路真实冒烟（模板→派发→事件流→持久化→重派→导出）+ 发布 v0.6.0 | 已交付（[2026-11-02-v0.6.0发布]，见上冒烟记录） |

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

M6-W3 冒烟（2026-11-02，Windows 本机）[2026-11-02-事件级实时]：

- 假 jsonl Agent（PowerShell 脚本按 W1 词表出流）→ CLI
  `dispatch run -events`：init/文本/工具折叠/回传/终态五类事件全渲染，
  摘要行 `ok`；无 `-events` 时原文透传 + 提取结果行不回归。
- 控制台：POST run → 详情 `events` 网络证据 5 条结构化（final 终态齐）；
  浏览器验证 3/3（事件视图着色/折叠/终态、原文/事件双向切换截图）。
- 容错实测：无 BOM 的 .ps1 中文被 PowerShell 5.1 按 ANSI 搅坏为非法
  JSON → 解析器正确回退 raw 原样保留（M2 的 BOM 教训在本子系统的
  正面复现——读不懂=未知，内容零丢失）。
- serve 启动日志确认恢复语义：`已恢复派发会话历史 count=2`（M6-W2
  落盘的冒烟会话跨重启可见）。

M6-W4 冒烟（2026-11-02，Windows 本机）[2026-11-02-任务模板]：

- CLI：`dispatch run -t intro name=小马 topic=网关` → 假 Agent stdin
  回显任务全文「向 小马 介绍 网关 的核心概念」（占位实例化字节级证据），
  `ok`；缺 topic → `模板 intro 缺少参数: topic（占位 {topic}）` 拒绝。
- 控制台：GET templates 网络证据（清单+占位名）→ 模板 run 返回实例化
  任务；浏览器验证 3/3 + 复测 3/3（**首轮实测抓出前端缺陷**：模板提交
  路径误用单元素 querySelector 致「派发」按钮无反应，改
  querySelectorAll 后闭环通过——事件视图含实例化正文与 success 终态）。

M6-W5 冒烟（2026-11-02，Windows 本机）[2026-11-02-结果消费与重派]：

- 重派网络证据：failed 会话 POST redispatch → 新会话同 agent/同任务
  摘要，原会话原样留档，新会话独立终态；导出证据：`Content-Disposition:
  attachment; filename="dispatch-<id>.jsonl"` + 原文尾部字节。
- 控制台浏览器验证（2/3→修复→3/3）：筛选 failed 只剩失败行且带「重
  派」、重派出新行+toast；**首轮实测抓出前端缺陷**：导出用
  `location.href` 附件下载在浏览器以 net::ERR_ABORTED 收场——改
  fetch→blob→对象 URL 下载（无导航副作用，文件名按 output_mode 生成）
  后复测通过（无新控制台错误 + 成功 toast）。

M6-W6 全链路真实冒烟（2026-11-02，Windows 本机，真实 HOME + 真实
Claude Code 2.1.314 → 网关 → 硅基流动）[2026-11-02-v0.6.0发布]：

- 模板→派发→事件流（CLI）：`dispatch run -events -t <模板> topic=…`
  → 真实 claude 多轮工具会话（Glob/Read/Grep + permission_denied 子
  类型）事件全渲染，`ok · 77.9s`；system 子类型（permission_denied）
  经 init 事件自然携带（词表容错生效）。**抓出 CLI 易用性缺陷**：Go
  flag 语义下位置参数后的 `-events` 会被当模板传参拒绝——保持
  fail-closed，报错改为可操作指引（标志需写在 -t 传参之前）。
- 派发→事件流（serve API）：POST run 模板形态 → 真实 claude done，
  events 3 条（init/text/result）。
- 持久化：serve 重启 → 5 条会话恢复（含历史），恢复会话详情从留存
  尾部现场解析出事件（无 live 依赖）。
- 重派：热替换加 slow agent（timeout 2s）→ timeout failed →
  redispatch → 新会话同 agent/任务独立终态；配置经 PUT 写路径加与
  还原（备份审计留痕，用户配置零残留）。
- 导出：claude jsonl 会话 `Content-Disposition: attachment;
  filename="dispatch-<id>.jsonl"`。
- 顺带实测：OTLP 后端未运行时降级路径（重试 4 次耗尽丢弃本批，不崩
  不阻断）；PUT /api/config/dispatch 中文 body 经 UTF-8 字节提交正常。

未实测项（如实标注）：

- ~~三家真实 Agent headless 形状~~ → **Claude Code 已实测**（2026-10-30
  M6-W1，见下「真实校准记录」）；codex/gemini 本机无对应订阅/登录，
  降级留档（形状沿用官方文档口径）。
- Linux 侧真实派发冒烟（无 WSL/实机；降级为 CI fake-agent 冒烟）。

> [2026-11-03-M9-W2 终局标注] codex/gemini headless 形状**正式降级
> 留档**（非悬置）：用户确认无两家订阅账户，真实登录校准条件不具备；
> 参考实现（Magpie agent-wiring）只做 CLI 配置接线、不跑 headless
> dispatch，无 stdout/jsonl 形状参考可借。CLI 本体已装（codex-cli
> 0.166.0 / gemini 0.66.0），有账户后按 M6-W1 claude 同口径冒烟即可
> 解除。同轮已完成的替代校准见 agent/registry.go——接线规格（写入
> Agent 配置的字段名）经 Magpie 真实世界实现交叉核验（codex
> `experimental_bearer_token`+`wire_api="responses"`；gemini-cli
> settings.json `security.auth.selectedType`+`~/.gemini/.env` 注入），
> 「Agent→网关」使用路径不受本次留档影响。

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
