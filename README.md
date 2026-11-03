# Yardmaster

**Yardmaster**（调车长）是一个运行在本机的 AI 网关：把多个 AI Agent（Claude Code、Codex、OpenCode 等）的请求，按任务类型自动路由到合适的模型与账号——Agent 只管发车，调车长负责把每趟请求拨上正确的股道。

```text
Agent ──▶ 127.0.0.1:8317 ──▶ Yardmaster ──▶ 按任务/额度/策略 ──▶ 多家上游模型
```

## 核心能力

- **四协议网关（v0.4.0）**：OpenAI Chat / OpenAI Responses / Anthropic Messages / Google Gemini 四协议任一入站可路由至任意协议上游（4×4 互转矩阵，文本 + 工具调用，流式/非流式）；流式经内存事件层（七类 gwEvent，四协议各一对 decoder/encoder，v0.5.0）逐事件翻译，无中间 JSON round-trip
- **任务→Agent 派发（v0.5.0，v0.6.0 深化）**：把任务文本交给本机已安装的 Agent（Claude Code / Codex / Gemini CLI 的 headless 模式）执行——意图→Agent 匹配（dispatch.json，fail-closed 校验）、headless 会话管理（stdin/stdout 协议、超时/取消、每 Agent 并发上限）、stdout 实时回传、终态记入决策 trace；v0.6.0：jsonl 事件级实时（六类事件解析，控制台事件视图着色/工具折叠/终态高亮，CLI `run -events`）、任务模板（`templates[]` + `dispatch run -t` + 控制台模板下拉）、会话持久化（跨重启可见）、失败一键重派、结果导出/复制、会话筛选；控制台「派发」页提交/实时输出/取消，CLI `dispatch agents|init|run`；ToS 边界：仅启动已安装的 Agent，不代装不代登录
- **成本缓存扣除（v0.5.0）**：usage 捕获缓存命中细分（openai `cached_tokens` / gemini `cachedContentTokenCount` / anthropic `cache_read_input_tokens` 折叠统一语义），命中部分按 `cached_input_per_m` 计价（缺省 0=扣除），报告桶单列缓存命中数
- **Agent 接线**：声明式改写 Agent 配置指向网关（预览 diff、备份栈、一键还原、漂移检测）
- **订阅账号（OAuth）**：一键导入 Claude Code / Codex / Gemini CLI 的已登录会话凭证（加密存储：Windows DPAPI / 其余 0600），到期自动续期（主动扫描 + 401 被动刷新），.providers.json 永不落 token
- **厂商预设**：`provider add -preset glm-coding` 等 13 个预设开箱即用；GLM 1113 自动纠偏到 Coding Plan 端点；Zhipu 200 伪装拒绝识别；认证头（x-api-key/Bearer）自动学习
- **路由引擎**：`provider/model`、裸 provider 自动组、`group/<id>` 跨上游路由组；order / rotate / smart / usage / weight（平滑加权）五策略
- **额度感知与 SwitchWhenSpent**：账号当日额度耗尽自动休眠至次日重置；订阅窗口（响应头学习）与余额轮询（QuotaProbe）喂路由；按 key 并发上限
- **成本核算（v0.7.0 起单价自动填充）**：模型单价表（pricing.json）× 决策 trace 聚合，按账号/模型出成本报告（零新增持久化）；`price sync` 从 models.dev 公开目录一键填充（数千模型，缓存命中价同步捕获），条目带来源标记，人工价受保护（`-force` 才覆盖），同名键多源冲突按字典序留痕；v0.7.0 增量：按用量同步（缺省只填充实际用到的模型，同名模型按本地供应商写 `provider/model` 精确键——不同供应商单价可不同）、价格旁双币显示（`settings set fx-usd-cny <汇率>` 后成本报告与单价表显示 ≈¥）；控制台观测页一键同步
- **插件体系（v0.7.0）**：不重编译网关接入自定义行为——子进程插件协议（长驻进程 + NDJSON 管道，语言无关：node/python/任意可执行体；OS 进程边界天然崩溃隔离，纯 Go 零 cgo）。四扩展点：`events.trace`（决策 trace 订阅）、`dispatch.parse`（Agent 输出行解析，agent 条目 `parser` 字段指向插件，行级回退内置词表）、`classify.intent`（意图分类兜底，词表外拒收）、`vendor.verdict`（200 伪装拒绝长尾特判，内置词表先行）。fail-closed 权限：握手授予 granted=配置∩声明、事件队列满丢最旧不阻塞推理、崩溃退避重启（1 小时 >10 次标 failed）、默认最小 env（`INHERIT_ENV=1` 才继承）；控制台「插件」页清单/启停（热生效）/日志尾，CLI `plugin list|enable|disable`，示例见 `examples/trace-logger.mjs`
- **故障转移**：429/5xx/网络错自动切换候选并休眠，额度断流无感接续；会话粘性（`X-Gw-Session`）
- **任务规则 + 意图分类**：rules.json 首条命中改道；自定义意图词表热生效；内置 L0 启发式 + 可选 L1 小模型分类（缓存/冷却/回落）
- **Web 控制台**：总览/Provider/路由/观测/派发/插件六页；配置读写全走管理 API（严格校验 + 写前备份栈 + 审计日志），不手改配置文件即可全量管理
- **决策追溯**：每次路由决策记录完整候选尝试链（谁调用、命中什么、试过谁、为何切换），落盘 30 天/200MB，`/api/trace`、控制台与 CLI 可查
- **代理挂载**：全局 → Provider → 账号三级继承（http/https/socks5/socks5h/direct），HTTP/2 半死连接健康检查
- **局域网共享**：`listen` + 强制令牌（token）；控制台/API/推断端点统一过闸，默认仅本机
- **桌面应用（app 模式，v0.8.1 起 Windows 双入口零黑窗）**：桌面入口 `yardmaster-app.exe`（Windows GUI subsystem，双击/开始菜单零终端窗口零闪现，图标/版本/DPI 资源齐全）；命令行 `yardmaster.exe`（console subsystem，serve/CLI/headless 派发契约不变）。两者共享同一套逻辑与数据目录——进程内网关 + 系统托盘 + 原生窗口（WebView2；关窗=隐藏到托盘，尺寸/位置记忆；WebView2 缺失自动回退浏览器窗口）；Linux/其他为浏览器 `--app` 独立窗口（exec 纯 Go，交叉编译矩阵不变）。控制台为侧栏式信息架构（Magpie 同构）+ 明暗双主题（跟随系统，侧栏脚部三态切换）。窗口关闭后台仍运行，重开/退出走托盘菜单；无头场景用 `serve`，网页控制台按指令打开：`yardmaster console`（浏览器）或 app 窗口
- **OTLP traces 导出（v0.4.0，默认关）**：settings.json `otel{enabled,endpoint,headers}` 开启后，决策 trace 映射 OTel span（gen_ai.* 语义约定）推送 OTLP-HTTP 兼容后端（Jaeger/Tempo/Langfuse），手写 JSON 零 SDK 依赖，失败只降级不阻断推理

## 安装

从 [Releases](../../releases) 下载，或用安装脚本（自动识别平台，加入 PATH）：

```sh
# Linux
curl -fsSL https://github.com/listlonely/Yardmaster/releases/latest/download/install.sh | bash -s -- --user

# Windows (PowerShell)
irm https://github.com/listlonely/Yardmaster/releases/latest/download/install.ps1 -OutFile install.ps1; .\install.ps1
```

## 快速开始

```sh
# 1. 添加上游（厂商预设开箱即用，或手填 base-url；预检验证凭证并拉取模型清单）
yardmaster provider presets                        # 查看全部厂商预设
yardmaster provider add -preset deepseek -key sk-xxx
yardmaster provider probe deepseek

# 2. 导入订阅账号（读 Agent 已登录会话，自动续期）
yardmaster provider add-oauth anthropic -from claude-code   # 或 codex / gemini-cli

# 3. 把 Agent 接到网关（预览确认后改写其配置）
yardmaster agents preview claude-code -model yardmaster-main claude-code
yardmaster agents wire claude-code -model yardmaster-main

# 4. 启动：直接运行 `yardmaster`（无参数）即桌面应用——应用窗口 + 托盘 + 进程内网关；
#    无头场景用 serve（不打开任何界面）；网页控制台按指令打开（yardmaster console）
yardmaster                # 应用模式：窗口 + 托盘（退出走托盘菜单）
yardmaster serve          # 无头模式（Agent 接线/服务器场景）
yardmaster console        # 在浏览器打开控制台（也可 -addr 指向远程网关）

# 5. 观测与经营
yardmaster trace list                 # 路由决策追溯
yardmaster price sync                 # 从 models.dev 目录自动填充单价（人工价受保护，-force 覆盖）
yardmaster price list                 # 单价表（来源列：models.dev / 人工）
yardmaster cost --days 7              # 成本报告
yardmaster tui                        # 终端面板：状态 / 追溯 / 绑定切换
yardmaster update check               # 升级提示

# 6. 任务→Agent 派发（v0.5.0，v0.6.0 深化，可选）
yardmaster dispatch init              # 预览三家建议配置（claude-code 已实测核实；按本机安装修改 command）
yardmaster dispatch agents            # 查看已配置的派发 Agent
yardmaster dispatch run "把 TODO 整理成清单"   # 派发（或在控制台「派发」页提交）
yardmaster dispatch run -events "长任务"        # 实时事件流渲染（jsonl agent）
yardmaster dispatch run -t <模板> key=val      # 按任务模板派发（dispatch.json templates[]）
echo "任务文本" | yardmaster run -- claude -p  # headless 原语直通（stdin→stdout 透传）
# 控制台「派发」页：事件视图、会话持久化（跨重启）、失败重派、结果导出/复制、筛选

# 7. 插件（v0.7.0，可选）
yardmaster plugin list                # 插件清单（plugins.json，损坏 fail-closed 拒改）
yardmaster plugin disable trace-logger   # 启停（写前备份；serve 运行中在控制台热启停）
# 控制台「插件」页：状态徽章/授予能力/崩溃计数/丢事件、启停（热生效）、stderr 日志尾
# 示例插件：examples/trace-logger.mjs（订阅决策 trace 落本地 JSONL，写法见 docs/subsystems/plugins.md）
```

> **派发实测情况（v0.6.0 如实标注）**：Claude Code（2.1.314）headless 全链路已真实冒烟——`claude -p` → 网关 `/v1/messages` → openai-chat 上游 → 回翻，事件词表（system/assistant/user/result）与缓存命中对账经实机核实，v0.6.0 全链路冒烟（模板→派发→事件流→持久化→重派→导出）通过；codex（`codex exec --json`）/ gemini（`gemini -p`）本机无对应订阅/登录，形状沿用官方文档口径降级留档。OTLP 真实后端（Jaeger v2.22）与真实上游缓存命中对账已实测；Linux 侧真实派发冒烟仍为未实测项（无实机，CI fake-agent 冒烟覆盖）。

> **桌面壳实测情况（v0.8.0 如实标注）**：Windows 原生窗口（Wails v3.0.0 + WebView2）全链路已真机冒烟——无参数启动、控制台写路径闭环（settings 同值回写经完整写路径）、关窗=隐藏到托盘（WM_CLOSE 实证）、窗口状态记忆落盘（`window-state.json`）、WebView2 缺失回退演练（`YARDMASTER_NO_NATIVE_WINDOW=1` → 回退浏览器 `--app` 窗口 + stdout 留痕）、侧栏信息架构与明暗双主题浏览器三轮核查（明暗 × app 态 × 六页遍历）。双击/开始菜单启动自动收起终端黑窗（孤立启动 FreeConsole，日志落数据目录 `app.log`；从终端启动行为零变化）。Linux 侧原生窗口为非目标（webkit2gtk cgo 破坏纯 Go 交叉编译矩阵），浏览器 `--app` 窗口路径行为等价 v0.7.0 由交叉构建+依赖链断言覆盖；Linux 真机托盘（D-Bus）冒烟与托盘菜单真机点击仍为未实测项（应急开关 `YARDMASTER_NO_NATIVE_WINDOW=1` 可随时禁用原生壳）。

数据目录：`~/.yardmaster`（providers/keys/rules/intents/pricing/settings 均为可 diff 的 JSON，写前自动备份到 `backup/`，原子写入）。

## 局域网共享（可选）

默认仅本机可访问。共享给局域网时**强制访问令牌**（三口校验：serve 启动、CLI 保存、管理 API 写回均拒绝无令牌的非回环配置）：

```sh
yardmaster settings set token <随机长令牌>
yardmaster settings set listen 0.0.0.0:8317
yardmaster serve   # Agent 与浏览器请求携带该令牌（Authorization: Bearer / x-api-key / X-Gw-Token）
```

## 安全基线（不可协商）

- 默认仅监听 `127.0.0.1`；非回环监听必须配置访问令牌
- 上游密钥与令牌仅存本机，日志与界面输出经脱敏（API 输出 `***+末 4 位`，脱敏往返不破坏原值）
- Agent 配置改写只动自己名下的键，原始配置进备份栈
- 配置文件损坏时拒绝覆写（读不懂 = 未知），绝不当作空配置覆盖

## 平台

Linux（amd64/arm64）与 Windows（amd64），见 [Releases](../../releases)。开发文档见 `docs/`。

## 构建

```sh
go build -o bin/ ./cmd/yardmaster
go test ./...
```
