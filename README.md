# Yardmaster

**Yardmaster**（调车长）是一个运行在本机的 AI 网关：把多个 CLI 客户端（Claude Code、Codex、OpenCode 等）的请求，按任务类型自动路由到合适的模型与账号——客户端只管发车，调车长负责把每趟请求拨上正确的股道。

```text
CLI 客户端 ──▶ 127.0.0.1:8317 ──▶ Yardmaster ──▶ 按任务/额度/策略 ──▶ 多家上游模型
```

## 核心能力

- **四协议网关**：OpenAI Chat / OpenAI Responses / Anthropic Messages / Google Gemini 四协议任一入站可路由至任意协议上游（4×4 互转矩阵，流式/非流式）；流式经内存事件层（四协议各一对 decoder/encoder）逐事件翻译，无中间 JSON round-trip；**多模态与 thinking 跨协议透传（IR v2）**：图片输入四协议互译（messages image / chat image_url / responses input_image / gemini inlineData，不可携带场景 fail-closed 留痕不静默）、思考流出三通道（GLM `reasoning_content` / anthropic thinking 块 / gemini thought part，chat 客户端按协议形状不产出）、reasoning_tokens 细分入账、中途 system 消息 `<system-reminder>` 包装（Claude Code 兼容）、504/524 超时语义分级
- **任务规则 + 意图分类**：请求按意图自动分配模型——rules.json 首条命中改道；自定义意图词表热生效；内置 L0 启发式 + 可选 L1 小模型分类（缓存/冷却/回落）；控制台「路由」页以意图分配表呈现（意图链融合编辑，规则与链双写）
- **插件体系**：不重编译网关接入自定义行为——子进程插件协议（长驻进程 + NDJSON 管道，语言无关：node/python/任意可执行体；OS 进程边界天然崩溃隔离，纯 Go 零 cgo）。三扩展点：`events.trace`（决策 trace 订阅）、`classify.intent`（意图声明与分类，词表外拒收）、`vendor.verdict`（200 伪装拒绝长尾特判，内置词表先行）。fail-closed 权限：握手授予 granted=配置∩声明、事件队列满丢最旧不阻塞推理、崩溃退避重启（1 小时 >10 次标 failed）、默认最小 env（`INHERIT_ENV=1` 才继承）；控制台「插件」页清单/启停（热生效）/日志尾 + 插件商店，CLI `plugin list|enable|disable`，示例见 `examples/trace-logger.mjs`
- **成本缓存扣除**：usage 捕获缓存命中细分（openai `cached_tokens` / gemini `cachedContentTokenCount` / anthropic `cache_read_input_tokens` 折叠统一语义），命中部分按 `cached_input_per_m` 计价（缺省 0=扣除），报告桶单列缓存命中数
- **订阅账号（OAuth）**：一键导入 Claude Code / Codex / Gemini CLI 的已登录会话凭证（加密存储：Windows DPAPI / 其余 0600），到期自动续期（主动扫描 + 401 被动刷新），.providers.json 永不落 token
- **厂商预设**：`provider add -preset glm-coding` 等 13 个预设开箱即用；GLM 1113 自动纠偏到 Coding Plan 端点；Zhipu 200 伪装拒绝识别；认证头（x-api-key/Bearer）自动学习
- **路由引擎**：`provider/model`、裸 provider 自动组、`group/<id>` 跨上游路由组；order / rotate / smart / usage / weight（平滑加权）五策略
- **额度感知与 SwitchWhenSpent**：账号当日额度耗尽自动休眠至次日重置；订阅窗口（响应头学习）与余额轮询（QuotaProbe）喂路由；按 key 并发上限
- **成本核算（单价自动填充）**：模型单价表（pricing.json）× 决策 trace 聚合，按账号/模型出成本报告（零新增持久化）；`price sync` 从 models.dev 公开目录一键填充（数千模型，缓存命中价同步捕获），条目带来源标记，人工价受保护（`-force` 才覆盖），同名键多源冲突按字典序留痕；增量：按用量同步（缺省只填充实际用到的模型，同名模型按本地供应商写 `provider/model` 精确键——不同供应商单价可不同）、价格旁双币显示（`settings set fx-usd-cny <汇率>` 后成本报告与单价表显示 ≈¥）；控制台观测页一键同步
- **故障转移**：429/5xx/网络错自动切换候选并休眠，额度断流无感接续；会话粘性（`X-Gw-Session`）
- **Web 控制台**：总览/Provider/路由/观测/插件五页；配置读写全走管理 API（严格校验 + 写前备份栈 + 审计日志），不手改配置文件即可全量管理
- **决策追溯**：每次路由决策记录完整候选尝试链（谁调用、命中什么、试过谁、为何切换），落盘 30 天/200MB，`/api/trace`、控制台与 CLI 可查
- **代理挂载**：全局 → Provider → 账号三级继承（http/https/socks5/socks5h/direct），HTTP/2 半死连接健康检查
- **局域网共享**：`listen` + 强制令牌（token）；控制台/API/推断端点统一过闸，默认仅本机
- **桌面应用（app 模式，Windows 双入口零黑窗）**：桌面入口 `yardmaster-app.exe`（Windows GUI subsystem，双击/开始菜单零终端窗口零闪现，图标/版本/DPI 资源齐全）；命令行 `yardmaster.exe`（console subsystem，serve/CLI 契约不变）。两者共享同一套逻辑与数据目录——进程内网关 + 系统托盘 + 原生窗口（WebView2；关窗=隐藏到托盘，可在设置改为直接退出；单实例：二次双击自动前置既有窗口；尺寸/位置记忆；WebView2 缺失自动回退浏览器窗口）；**托盘气泡通知**：上游账号休眠（限频防刷屏，settings `notify` 可关）；**开机自启动**：设置页开关（settings `autostart`，写当前用户注册表 Run 键，即时生效并启动自愈）；**托盘菜单动态化**：版本行/「升级到 vX.Y.Z」动态入口/状态摘要行（休眠账号）；Linux/其他为浏览器 `--app` 独立窗口（exec 纯 Go，交叉编译矩阵不变）。控制台为侧栏式信息架构（Magpie 同构）+ 明暗双主题（跟随系统，侧栏脚部三态切换）。窗口关闭后台仍运行，重开/退出走托盘菜单；无头场景用 `serve`，网页控制台按指令打开：`yardmaster console`（浏览器）或 app 窗口
- **OTLP traces/metrics 导出（默认关）**：settings.json `otel{enabled,endpoint,headers}` 开启后，决策 trace 映射 OTel span（gen_ai.* 语义约定）推送 OTLP-HTTP 兼容后端（Jaeger/Tempo/Langfuse），并同源聚合三指标（requests/请求时长直方图/tokens）以累积型快照推送 `/v1/metrics`；手写 JSON 零 SDK 依赖，失败只降级不阻断推理

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

# 2. 导入订阅账号（读 CLI 已登录会话，自动续期）
yardmaster provider add-oauth anthropic -from claude-code   # 或 codex / gemini-cli

# 3. 把 CLI 客户端接到网关（在客户端配置里把 base_url 指向网关地址）
#    Claude Code：ANTHROPIC_BASE_URL=<网关地址>（环境变量或 settings.json）
#    Trae 等 OpenAI 兼容客户端：API 地址 <网关地址>/v1

# 4. 启动：直接运行 `yardmaster`（无参数）即桌面应用——应用窗口 + 托盘 + 进程内网关；
#    无头场景用 serve（不打开任何界面）；网页控制台按指令打开（yardmaster console）
yardmaster                # 应用模式：窗口 + 托盘（退出走托盘菜单）
yardmaster serve          # 无头模式（服务器场景）
yardmaster console        # 在浏览器打开控制台（也可 -addr 指向远程网关）

# 5. 观测与经营
yardmaster trace list                 # 路由决策追溯
yardmaster price sync                 # 从 models.dev 目录自动填充单价（人工价受保护，-force 覆盖）
yardmaster price list                 # 单价表（来源列：models.dev / 人工）
yardmaster cost --days 7              # 成本报告
yardmaster tui                        # 终端面板：状态 / 追溯 / 绑定切换
yardmaster update check               # 升级提示
yardmaster update download            # 下载最新版并校验暂存（SHA256 边下边验）
yardmaster update install             # 自动更新安装（换侧+接力重启，失败自动回滚）

# 6. 插件（可选）
yardmaster plugin list                # 插件清单（plugins.json，损坏 fail-closed 拒改）
yardmaster plugin disable trace-logger   # 启停（写前备份；serve 运行中在控制台热启停）
# 控制台「插件」页：状态徽章/授予能力/崩溃计数/丢事件、启停（热生效）、stderr 日志尾
# 示例插件：examples/trace-logger.mjs（订阅决策 trace 落本地 JSONL，写法见 docs/subsystems/plugins.md）
```

> **桌面壳实测情况（如实标注）**：Windows 原生窗口（Wails v3.0.0 + WebView2）全链路已真机冒烟——无参数启动、控制台写路径闭环（settings 同值回写经完整写路径）、关窗=隐藏到托盘（WM_CLOSE 实证）、窗口状态记忆落盘（`window-state.json`）、WebView2 缺失回退演练（`YARDMASTER_NO_NATIVE_WINDOW=1` → 回退浏览器 `--app` 窗口 + stdout 留痕）、侧栏信息架构与明暗双主题浏览器三轮核查。双击/开始菜单启动自动收起终端黑窗（孤立启动 FreeConsole，日志落数据目录 `app.log`；从终端启动行为零变化）。Linux 侧原生窗口为非目标（webkit2gtk cgo 破坏纯 Go 交叉编译矩阵），浏览器 `--app` 窗口路径行为等价性由交叉构建+依赖链断言覆盖；Linux 真机托盘（D-Bus）冒烟与托盘菜单真机点击仍为未实测项（应急开关 `YARDMASTER_NO_NATIVE_WINDOW=1` 可随时禁用原生壳）。

> **M9 真实化增量（如实标注）**：托盘气泡通知真机实证（beeep 走 WinRT COM）；托盘菜单动态化真机在位（版本行/状态摘要/升级入口）；GLM Coding Plan 真实用量端点 `/api/monitor/usage/quota/limit` 真机采样并接入余额轮询（`finance/balance` 对 Coding Plan key 为 404，按量 key 专属）；GLM 1113 纠偏经直连复核确认上游行为漂移（按量端点现放行 Coding Plan key），保留为兜底；sealed reasoning 维持 (b) 边界标注（无 Codex 凭证）；OAuth 续期全链路无凭证源正式留档。未实测项完整定稿见 `docs/subsystems/app.md` §十（无悬置项）。

数据目录：`~/.yardmaster`（providers/keys/rules/intents/pricing/settings 均为可 diff 的 JSON，写前自动备份到 `backup/`，原子写入）。

## 局域网共享（可选）

默认仅本机可访问。共享给局域网时**强制访问令牌**（三口校验：serve 启动、CLI 保存、管理 API 写回均拒绝无令牌的非回环配置）：

```sh
yardmaster settings set token <随机长令牌>
yardmaster settings set listen 0.0.0.0:8317
yardmaster serve   # CLI 客户端与浏览器请求携带该令牌（Authorization: Bearer / x-api-key / X-Gw-Token）
```

## 安全基线（不可协商）

- 默认仅监听 `127.0.0.1`；非回环监听必须配置访问令牌
- 上游密钥与令牌仅存本机，日志与界面输出经脱敏（API 输出 `***+末 4 位`，脱敏往返不破坏原值）
- 本地配置文件（providers/keys/rules/intents/settings）仅由网关自身写闸写入，写前备份栈留底
- 配置文件损坏时拒绝覆写（读不懂 = 未知），绝不当作空配置覆盖

## 平台

Linux（amd64/arm64）与 Windows（amd64），见 [Releases](../../releases)。开发文档见 `docs/`。

## 构建

```sh
go build -o bin/ ./cmd/yardmaster
go test ./...
```
