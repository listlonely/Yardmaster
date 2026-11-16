#!/usr/bin/env node
// yardmaster 插件：agent-classifier（智能意图代理：LLM 意图分类 + 模型分配）
//
// 以 classify.intent 能力接入网关，承担两个钩子：
//   1. classify（docs §四·三）：消息 → 意图 id——**取代/兜底 L1 分类器**。
//      链序 自定义 L0 → 内置 L0 → L1（如已配置）→ 本插件。
//   2. pick（2026-11-13，策略 agent）：链策略配为 agent 时，网关把
//      {message, intent, candidates} 发给本插件，由 Agent **按请求语境
//      自主挑选厂商模型**（候选含模型/账号/可用状态）；返回 {pick: 序号}，
//      选中候选提到队首，其余保持基线顺序作故障转移。插件不可用/失败/
//      超时由网关回落 smart 基线。
//
// 分类策略（快者先行，慢者兜底）：
//   1. 缓存命中（512 条 LRU 语义：超限整体重置）
//   2. 关键词快路径（内置五意图中英关键词，零成本零延迟）
//   3. LLM 分类：经网关自身推理端点调用「分类脑」模型——享受网关的
//      路由/故障转移/代理，并自带 X-Gw-Internal 防递归头（分类调用
//      不再触发分类）。
//
// 配置入口（推荐）：控制台「路由 › 智能代理」弹窗选择分类脑——保存走
// PUT /api/plugins/{id}/options 写回 plugins.json（备份+审计+热生效，
// 实例重建经握手透传新 options）。手写 plugins.json 示例（等价）：
//
//   {
//     "plugins": [{
//       "id": "agent-classifier",
//       "enabled": true,
//       "command": ["node", "examples/agent-classifier.mjs"],
//       "capabilities": ["classify.intent"],
//       "options": {
//         "model": "glm-coding/glm-4.7-air",
//         "gateway": "http://127.0.0.1:8317",
//         "token": "（可选；仅局域网共享时需要）",
//         "timeout_ms": 1200
//       }
//     }]
//   }
//
// model 是「分类脑」（provider/model，经网关路由受额度管理）；建议选
// 便宜快速的模型；留空 = 仅关键词快路径。token 也可经 env 传
// （plugins.json "env":["YM_GATEWAY_TOKEN=…"]，凭证语义的 env 不进管理
// 面回显）；env 兜底：YM_CLASSIFIER_MODEL / YM_GATEWAY_URL / YM_GATEWAY_TOKEN。
// L1 关系：不配 `settings set l1` 时本插件即分类主力（取代 L1）；两者
// 同配时 L1 先行、本插件兜底。

import readline from 'node:readline';

const PROTO = 1;
const CAPS = ['classify.intent'];

// ---- 配置：握手回执 settings.options 优先，env 兜底 ----
let opts = {};
const gateway = () => (opts.gateway || process.env.YM_GATEWAY_URL || 'http://127.0.0.1:8317').replace(/\/+$/, '');
const model = () => opts.model || process.env.YM_CLASSIFIER_MODEL || '';
const token = () => opts.token || process.env.YM_GATEWAY_TOKEN || '';
const fetchTimeout = () => Math.max(200, Math.min(Number(opts.timeout_ms) || 1200, 2800));

// ---- 缓存（消息 → 意图；超限整体重置，与网关 L1 缓存同纪律）----
const CACHE_CAP = 512;
const cache = new Map();

// ---- 关键词快路径（内置意图；命中且在词表内才返回）----
const KEYWORDS = {
  coding: ['代码', '编程', '报错', 'bug', '重构', '实现', '函数', '编译', '调试', '测试用例', 'code', 'refactor', 'fix', 'debug', 'compile'],
  qa: ['什么是', '为什么', '解释', '介绍', '翻译', '是什么', '怎么理解', '区别', 'explain', 'what is', 'why'],
  writing: ['写一篇', '文案', '润色', '文章', '邮件', '报告草稿', '大纲', 'essay', 'polish', 'rewrite'],
  'long-context': ['长文档', '整个仓库', '全文', '超长', '长文本', '整份文件', '仓库级'],
  vision: ['图片', '截图', '看图', '识别图', '图像', '这一张图', 'image', 'screenshot'],
};

function quickIntent(text, vocabulary) {
  const lower = text.toLowerCase();
  for (const [intent, words] of Object.entries(KEYWORDS)) {
    if (!vocabulary.includes(intent)) continue; // 词表已裁剪则不猜
    for (const w of words) {
      if (lower.includes(w) || text.includes(w)) return intent;
    }
  }
  return '';
}

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

function cleanOutput(s) {
  return String(s ?? '').trim().replace(/^["'`，。\s]+|["'`，。\s]+$/g, '');
}

// LLM 分类：经网关推理端点调分类脑（X-Gw-Internal 防递归；X-Gw-Token
// 局域网共享场景鉴权）。
async function llmClassify(text, vocabulary) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), fetchTimeout());
  try {
    const sys =
      '你是网关的意图分类器。把用户消息分类到下列意图之一，\n' +
      '只输出意图 id 本身（不要标点、不要解释、不要输出词表之外的词）：\n' +
      vocabulary.map((v) => '- ' + v).join('\n');
    const r = await fetch(gateway() + '/v1/chat/completions', {
      method: 'POST',
      signal: ctrl.signal,
      headers: {
        'content-type': 'application/json',
        'X-Gw-Internal': 'l1',
        ...(token() ? { 'X-Gw-Token': token() } : {}),
      },
      body: JSON.stringify({
        model: model(),
        stream: false,
        max_tokens: 16,
        temperature: 0,
        messages: [
          { role: 'system', content: sys },
          { role: 'user', content: text },
        ],
      }),
    });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const j = await r.json();
    return cleanOutput(j?.choices?.[0]?.message?.content);
  } finally {
    clearTimeout(timer);
  }
}

async function classify(text, vocabulary) {
  const vocab = Array.isArray(vocabulary) && vocabulary.length ? vocabulary : [];
  if (!text || !vocab.length) return '';
  const hit = cache.get(text);
  if (hit !== undefined) return hit;
  let intent = quickIntent(text, vocab);
  if (!intent) {
    if (!model()) {
      // 未配置分类脑：仅关键词快路径（纯词表型用法，零成本）
      return '';
    }
    try {
      intent = await llmClassify(text, vocab);
    } catch (e) {
      process.stderr.write(`智能意图代理: LLM 分类失败（回落默认组）: ${e?.message || e}\n`);
      return ''; // 不缓存瞬时错误（网关 Registry 侧自带熔断）
    }
  }
  if (!vocab.includes(intent)) intent = ''; // 词表外视同未命中（不猜）
  if (cache.size >= CACHE_CAP) cache.clear();
  cache.set(text, intent);
  return intent;
}

// 策略 agent 的模型自主分配：把候选清单交给分类脑，按请求语境挑最合适的
// 一个（视觉→视觉模型、长文本→大窗口、简单问答→快便宜）。返回候选序号；
// -1 = 放弃选择（网关回落 smart 基线）。
async function pickModel(message, intent, candidates) {
  if (!message || !candidates.length) return -1;
  if (!model()) return -1; // 无分类脑无从判断
  // 硬规则先行：意图链已按能力分流（vision/long-context 意图），候选里
  // 命中对应关键词的模型直接选它——多数请求零 LLM 成本。
  const need = intent === 'vision' ? ['v', 'vision', 'vl', '图'] :
    intent === 'long-context' ? ['128k', 'long', 'max', '1m'] : null;
  if (need) {
    const idx = candidates.findIndex((c) =>
      need.some((k) => String(c.model || '').toLowerCase().includes(k)) && c.status === 'available');
    if (idx >= 0) return idx;
  }
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), fetchTimeout());
  try {
    const lines = candidates.map((c) =>
      `- 序号 ${c.index}：${c.member}（账号 ${c.account}，状态 ${c.status}）`).join('\n');
    const sys =
      '你是网关的模型分配器。根据用户消息的语境（任务类型、难度、是否需要特定能力），\n' +
      `从下列候选中选出最合适的一个${intent ? `（已判定意图：${intent}）` : ''}，\n` +
      '只输出该候选的「序号」数字，不要输出任何其他内容：\n' + lines;
    const r = await fetch(gateway() + '/v1/chat/completions', {
      method: 'POST',
      signal: ctrl.signal,
      headers: {
        'content-type': 'application/json',
        'X-Gw-Internal': 'l1',
        ...(token() ? { 'X-Gw-Token': token() } : {}),
      },
      body: JSON.stringify({
        model: model(),
        stream: false,
        max_tokens: 8,
        temperature: 0,
        messages: [
          { role: 'system', content: sys },
          { role: 'user', content: message },
        ],
      }),
    });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const j = await r.json();
    const idx = parseInt(cleanOutput(j?.choices?.[0]?.message?.content), 10);
    return Number.isInteger(idx) && idx >= 0 && idx < candidates.length ? idx : -1;
  } finally {
    clearTimeout(timer);
  }
}

// ---- 协议主体 ----
writeLine({ plugin: 'agent-classifier', proto: PROTO, capabilities: CAPS });

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
let started = false;
rl.on('line', (line) => {
  const env = safeParse(line);
  if (!env) return;
  if (!started) {
    // 首行 = 握手回执 {ok, granted, settings:{options}}
    started = true;
    opts = env?.settings?.options || {};
    process.stderr.write(
      `智能意图代理已连接（分类脑: ${model() || '未配置——仅关键词快路径'}, 网关: ${gateway()}）\n`);
    return;
  }
  switch (env.type) {
    case 'classify': {
      const p = env.payload || {};
      classify(String(p.text || ''), p.vocabulary).then((intent) => {
        writeLine({ v: PROTO, type: 'resp', id: env.id, payload: { intent } });
      }).catch(() => {
        writeLine({ v: PROTO, type: 'resp', id: env.id, payload: { intent: '' } });
      });
      break;
    }
    case 'pick': {
      // 策略 agent：从候选中按请求语境选模型。无分类脑/LLM 失败 →
      // pick:-1（网关回落 smart 基线，绝不因分配而失败）。
      const p = env.payload || {};
      pickModel(String(p.message || ''), p.intent || '', Array.isArray(p.candidates) ? p.candidates : [])
        .then((idx) => {
          writeLine({ v: PROTO, type: 'resp', id: env.id, payload: { pick: idx } });
        }).catch(() => {
          writeLine({ v: PROTO, type: 'resp', id: env.id, payload: { pick: -1 } });
        });
      break;
    }
    case 'ping':
      writeLine({ v: PROTO, type: 'pong' });
      break;
    default:
      break; // 未知信封忽略
  }
});
process.stdin.on('end', () => process.exit(0));
process.on('SIGTERM', () => process.exit(0));
