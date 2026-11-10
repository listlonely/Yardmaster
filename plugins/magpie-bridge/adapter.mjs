// magpie-bridge：Magpie 社区中间件 → yardmaster 插件协议的通用适配层
// （2026-11-10，验证载体 param-override）。
//
// 运行：node（≥18，实测 v20）或 bun。命令行：
//   node adapter.mjs <plugin-id> <middleware.js>
// 也可用环境变量 YM_PLUGIN_ID / MAGPIE_MIDDLEWARE。
//
// 协议（internal/plugin runtime.go）：NDJSON over stdio——
//   插件→宿主首行握手 {"plugin","proto":1,"capabilities":["provider.rewrite"]}
//   宿主→插件回执 {"ok":true,"granted":[...],"settings":{...}}
//   请求钩子  {"v":1,"type":"rewrite","id":"rN","payload":{...}}
//   回复      {"v":1,"type":"resp","id":"rN","payload":{...}}
//   心跳      {"v":1,"type":"ping"} → {"v":1,"type":"pong"}
//
// rewrite payload（网关→插件）：
//   {"body":{...上游请求体...},"model":"…","protocol":"…","path":"…",
//    "agent":"…","stream":true,"options":{...中间件 options...}}
// resp payload（插件→网关）三态：
//   {"changed":true,"body":{...}}   onRequest 返回了新请求体
//   {"changed":false}               onRequest 未改动（undefined）
//   {"reject":{"status":403,"reason":"…"}}  ctx.reject() 被调
//
// 中间件钩子约定与 magpie-community/plugins 一致：onRequest(body, ctx)，
// ctx = {options, model, path, protocol, agent, stream, reject(status, msg)}。
// 中间件单文件零 import（上游纪律），node/bun 皆可直接加载。
import { pathToFileURL } from 'node:url';

const pluginID = process.argv[2] || process.env.YM_PLUGIN_ID || 'magpie-bridge';
const mwPath = process.argv[3] || process.env.MAGPIE_MIDDLEWARE || '';
if (!mwPath) {
  process.stderr.write('magpie-bridge: 缺少中间件路径（argv[3] 或 MAGPIE_MIDDLEWARE）\n');
  process.exit(2);
}

let onRequest = null;
let onResponse = null;
let onEvent = null;
try {
  const mw = await import(pathToFileURL(mwPath).href);
  onRequest = typeof mw.onRequest === 'function' ? mw.onRequest : null;
  onResponse = typeof mw.onResponse === 'function' ? mw.onResponse : null;
  onEvent = typeof mw.onEvent === 'function' ? mw.onEvent : null;
  if (!onRequest && !onResponse && !onEvent) {
    process.stderr.write('magpie-bridge: 中间件未导出 onRequest/onResponse/onEvent\n');
    process.exit(2);
  }
} catch (e) {
  process.stderr.write(`magpie-bridge: 中间件加载失败: ${e?.message || e}\n`);
  process.exit(2);
}

const out = (v) => {
  process.stdout.write(JSON.stringify(v) + '\n');
};

// rewrite 调用一次中间件钩子；reject 收集器只记首次（Magpie 语义：
// 抛错/拒绝都应终止后续处理）。
function runRewrite(pl) {
  const body = pl.body;
  if (body === undefined || body === null || typeof body !== 'object') {
    return { changed: false };
  }
  let rejected = null;
  const ctx = {
    // options 优先取 payload（网关逐请求传入），回落握手 settings.options
    // （plugins.json Spec.Options 静态配置）。
    options: pl.options ?? handshakeOptions ?? undefined,
    model: pl.model ?? '',
    path: pl.path ?? '',
    protocol: pl.protocol ?? '',
    agent: pl.agent ?? '',
    stream: !!pl.stream,
    reject(status, message) {
      if (rejected) return;
      const s = Number(status);
      rejected = {
        status: Number.isInteger(s) && s >= 100 && s <= 599 ? s : 400,
        reason: String(message ?? '').trim() || 'rejected by middleware',
      };
    },
  };
  let next;
  try {
    next = onRequest ? onRequest(body, ctx) : undefined;
  } catch (e) {
    // Magpie 纪律：钩子抛错请求原样放行——改写失败绝不阻断推理
    process.stderr.write(`middleware threw: ${e?.message || e}\n`);
    return { changed: false };
  }
  if (rejected) return { reject: rejected };
  if (next === undefined || next === null) return { changed: false };
  if (typeof next !== 'object') return { changed: false };
  return { changed: true, body: next };
}

// runEvent：单 SSE 帧 → onEvent（word-guard 流式掩码等）。payload.event
// 为该帧 data: 行的 JSON 文本；钩子抛错 /未改动 → 原样透传（Magpie 纪律）。
// 帧文本级重写在网关侧完成（bridge 只处理事件对象）。
function runEvent(pl) {
  const json = String(pl.event ?? '');
  if (!json || !onEvent || !json.startsWith('{')) return { changed: false };
  let ev;
  try {
    ev = JSON.parse(json);
  } catch {
    return { changed: false }; // 非 JSON data（如 [DONE]）原样
  }
  const ctx = {
    options: pl.options ?? handshakeOptions ?? undefined,
    protocol: pl.protocol ?? '',
    agent: pl.agent ?? '',
  };
  let next;
  try {
    next = onEvent(ev, ctx);
  } catch (e) {
    process.stderr.write(`onEvent threw: ${e?.message || e}\n`);
    return { changed: false };
  }
  if (next && typeof next === 'object') {
    return { changed: true, event: JSON.stringify(next) };
  }
  return { changed: false };
}

// runResponse：完整上游响应 → onResponse（word-guard 回复掩码等）。
// payload.body 为 JSON 文本；钩子抛错 / ≥400 / 未改动 → 原样。
function runResponse(pl) {
  const raw = pl.body;
  if (typeof raw !== 'string' || !raw || !onResponse) return { changed: false };
  let body;
  try {
    body = JSON.parse(raw);
  } catch {
    return { changed: false };
  }
  const ctx = {
    options: pl.options ?? handshakeOptions ?? undefined,
    status: Number(pl.status) || 200,
    protocol: pl.protocol ?? '',
    agent: pl.agent ?? '',
  };
  let next;
  try {
    next = onResponse(body, ctx);
  } catch (e) {
    process.stderr.write(`onResponse threw: ${e?.message || e}\n`);
    return { changed: false };
  }
  if (next === undefined || next === null || typeof next !== 'object') return { changed: false };
  return { changed: true, body: JSON.stringify(next) };
}

// 握手行。按中间件实际导出的钩子声明能力（宿主 fail-closed：配置授予
// ∩ 此声明——网关据此判定是否在响应/流式路径介入，无钩子零开销）。
const caps = ['provider.rewrite'];
if (onResponse) caps.push('provider.rewrite.response');
if (onEvent) caps.push('provider.rewrite.event');
out({ plugin: pluginID, proto: 1, capabilities: caps });

let handshook = false;
let handshakeOptions; // 握手回执 settings.options（Spec.Options 静态配置）
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  buf += chunk;
  let i;
  while ((i = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (!line) continue;
    let env;
    try {
      env = JSON.parse(line);
    } catch {
      continue; // 协议外噪声忽略（与宿主 readLoop 同纪律）
    }
    if (!handshook) {
      // 宿主回执 {"ok":true,"granted":[...],"settings":{"options":{...}}}：
      // options 为插件静态配置。Granted 为空（配置未授予）时仍常驻——
      // resp 按 unchanged 回复，宿主侧 RequestByCap 只找 granted 含该
      // 能力的插件，不会误用。
      handshakeOptions = env.settings?.options;
      handshook = true;
      continue;
    }
    switch (env.type) {
      case 'ping':
        out({ v: 1, type: 'pong' });
        break;
      case 'rewrite':
        if (onRequest === null) {
          out({ v: 1, type: 'resp', id: env.id, payload: { changed: false } });
          break;
        }
        out({ v: 1, type: 'resp', id: env.id, payload: runRewrite(env.payload || {}) });
        break;
      case 'event':
        // 流式事件：payload {event(data: 行 JSON 文本), protocol, agent}。
        // 调 onEvent，changed 则返回新事件 JSON 文本。
        if (onEvent === null) {
          out({ v: 1, type: 'resp', id: env.id, payload: { changed: false } });
          break;
        }
        out({ v: 1, type: 'resp', id: env.id, payload: runEvent(env.payload || {}) });
        break;
      case 'response':
        // 完整上游响应（非流式）：payload {body(JSON 文本), status, protocol, agent}。
        if (onResponse === null) {
          out({ v: 1, type: 'resp', id: env.id, payload: { changed: false } });
          break;
        }
        out({ v: 1, type: 'resp', id: env.id, payload: runResponse(env.payload || {}) });
        break;
      default:
        break; // 未知消息忽略
    }
  }
});
process.stdin.on('end', () => process.exit(0)); // 宿主关闭管道 → 退出（supervise 会重启）
