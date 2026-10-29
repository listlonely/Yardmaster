# Yardmaster

**Yardmaster**（调车长）是一个运行在本机的 AI 网关：把多个 AI Agent（Claude Code、Codex、OpenCode 等）的请求，按任务类型自动路由到合适的模型与账号——Agent 只管发车，调车长负责把每趟请求拨上正确的股道。

```text
Agent ──▶ 127.0.0.1:8317 ──▶ Yardmaster ──▶ 按任务/额度/策略 ──▶ 多家上游模型
```

## 核心能力

- **双协议网关**：OpenAI Chat Completions ↔ Anthropic Messages 双向解析与翻译（文本 + 工具，流式/非流式）
- **Agent 接线**：声明式改写 Agent 配置指向网关（预览 diff、备份栈、一键还原、漂移检测）
- **路由引擎**：`provider/model`、裸 provider 自动组、`group/<id>` 跨上游路由组；order / rotate / smart 三策略
- **故障转移**：429/5xx/网络错自动切换候选并休眠，额度断流无感接续；会话粘性（`X-Gw-Session`）
- **任务规则 + 意图分类**：rules.json 首条命中改道；自定义意图词表热生效；内置 L0 启发式 + 可选 L1 小模型分类（缓存/冷却/回落）
- **决策追溯**：每次路由决策记录完整候选尝试链（谁调用、命中什么、试过谁、为何切换），落盘 30 天/200MB，`api/trace` 与 CLI 可查
- **代理挂载**：全局 → Provider → 账号三级继承（http/https/socks5/socks5h/direct），HTTP/2 半死连接健康检查

## 快速开始

```sh
# 1. 添加上游（预检验证凭证并拉取模型清单）
yardmaster provider add -id siliconflow -base-url https://api.siliconflow.cn/v1 -key sk-xxx
yardmaster provider probe siliconflow

# 2. 把 Agent 接到网关（预览确认后改写其配置）
yardmaster agents preview claude-code -model yardmaster-main claude-code
yardmaster agents wire claude-code -model yardmaster-main

# 3. 启动网关（仅监听 127.0.0.1）
yardmaster serve

# 4. 观测每一次路由决策
yardmaster trace list
yardmaster trace show <trace-id>
```

数据目录：`~/.yardmaster`（providers/keys/rules/intents/trace 均为可 diff 的 JSON/JSONL，原子写入）。

## 安全基线（不可协商）

- 默认仅监听 `127.0.0.1`；局域网共享为后续开关
- 上游密钥仅存本机，日志与界面输出经脱敏
- Agent 配置改写只动自己名下的键，原始配置进备份栈

## 平台

Linux（amd64/arm64）与 Windows（amd64），见 [Releases](../../releases)。开发文档见 `docs/`。

## 构建

```sh
go build -o bin/ ./cmd/yardmaster
go test ./...
```
