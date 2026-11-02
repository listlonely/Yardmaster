#!/usr/bin/env node
// yardmaster 示例插件：trace-logger（M7-W5）
//
// 订阅 events.trace 能力，把网关决策 trace 逐条落到本地 JSONL 文件
//（默认 %USERPROFILE%/.yardmaster/plugins/trace-logger.log）。演示
// 子进程插件协议的最小实现：
//
//   1. stdout 打印一行握手 JSON（声明能力）
//   2. 读父进程回执（granted）
//   3. 逐行读 stdin 信封：event → 处理；ping → 回 pong
//
// 运行（plugins.json）：
//
//   {
//     "plugins": [{
//       "id": "trace-logger",
//       "enabled": true,
//       "command": ["node", "examples/trace-logger.mjs"],
//       "capabilities": ["events.trace"]
//     }]
//   }
//
// 环境说明：默认插件进程只收到 plugins.json env 里的变量（最小 env，
// 隔离语义）；确需继承网关全量环境时在 env 加 "INHERIT_ENV": "1"
//（用户显式选择，设计稿 §五 信任边界）。
// 独立测试（不经网关）：node examples/trace-logger.mjs < /dev/null

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import readline from 'node:readline';

const PROTO = 1;
const CAPS = ['events.trace'];

// 日志目录：YARDMASTER_PLUGIN_LOG 覆盖，否则 ~/.yardmaster/plugins/
const logPath = process.env.YARDMASTER_PLUGIN_LOG ||
  path.join(os.homedir(), '.yardmaster', 'plugins', 'trace-logger.log');

function writeLine(v) {
  process.stdout.write(JSON.stringify(v) + '\n');
}

function safeParse(s) {
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

// 握手：声明能力；父进程回 {"ok":true,"granted":[...],"settings":{}}
writeLine({ plugin: 'trace-logger', proto: PROTO, capabilities: CAPS });

fs.mkdirSync(path.dirname(logPath), { recursive: true });
const stream = fs.createWriteStream(logPath, { flags: 'a' });

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on('line', (line) => {
  let env;
  try {
    env = JSON.parse(line);
  } catch {
    return; // 读不懂=忽略（协议外噪声不致命）
  }
  switch (env.type) {
    case 'event': {
      // 事件 payload 即网关决策 Trace 的 JSON 对象（协议：信封 payload
      // 为内联值，见 docs/subsystems/plugins.md §四·一；元数据，无对话内容）
      const tr = typeof env.payload === 'string' ? safeParse(env.payload) : env.payload;
      stream.write(JSON.stringify(tr ?? { raw: env.payload }) + '\n');
      break;
    }
    case 'ping':
      writeLine({ v: PROTO, type: 'pong' });
      break;
    default:
      break; // 未知信封忽略
  }
});

rl.on('close', () => stream.end());
// 父进程消失（网关停止/崩溃判死）时退出，由网关守候循环管理生命周期。
process.stdin.on('end', () => process.exit(0));
process.on('SIGTERM', () => process.exit(0));

process.stderr.write(`trace-logger 已启动，日志: ${logPath}\n`);
