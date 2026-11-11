// context-guard：上下文超限保护 + LLM 摘要缓存（2026-11-11，magpie
// 网关中间件形态）。
//
// 定位：仅当请求 token 数达到阈值（接近上游模型上下文窗口）时介入——
// 平时零改动放行。触发时把旧对话历史替换为 LLM 摘要（按会话缓存），
// 缓存未命中则本地裁剪兜底，同时后台异步生成摘要供下次命中。目的：
// 超大上下文的首字节延迟从十秒级降到秒级，并防上游 400 超限报错。
//
//   {"trigger_tokens":160000}   触发阈值（估算法：ASCII/4 + CJK×1.0，
//                               含 messages+system+tools 全量）
//   {"target_tokens":80000}     裁剪预算：切点按轮迭代直到保留部分
//                               低于预算（或无可再裁）
//   {"keep_recent_turns":4}     起步保留的最近轮数（不够预算再往前裁）
//   {"tail_tool_max_chars":4000} 保留部分里超长 tool_result 的截断上限
//   {"cache_path":""}           缓存文件；缺省 $YM_APP_DIR/context-guard-
//                               cache.json（网关注入的数据目录），再兜底
//                               os.tmpdir()。勿跨网关实例共享。
//   {"summary":{...}}           摘要模型通道：{endpoint,api_key,model,
//                               protocol:"openai"|"anthropic",max_tokens}。
//                               直连上游不经网关（零递归）；缺省=纯本地
//                               裁剪模式（占位提示，无摘要）。
//
// 只处理带 messages 数组的请求（chat / anthropic）；responses / gemini
// 原样放行。当前轮（最后一条 assistant 之后的所有消息）永远保留。
//
// 与 history-trim 的关系：provider.rewrite 能力同一时刻只有一个插件
// 生效（宿主 RequestByCap 清单序取第一个），二者二选一。history-trim
// 是每请求轻量清洁，本插件是超限时的大刀。
//
// 对「单文件零 import」纪律的偏离（留档）：缓存持久化与 tmp 原子写需要
// node 内置模块，故顶层 `await import('node:fs')` 等——adapter 的动态
// import 会等顶层 await 完成（ESM 顶层 await），node ≥18 / bun 皆可
// 加载；npm 依赖仍为零。
const fs = await import('node:fs');
const path = await import('node:path');
const os = await import('node:os');

const SUMMARY_MARKER = '[context summary]';
const SUMMARY_INPUT_MAX_CHARS = 400000; // 摘要输入上限（≈100K token，超出取尾部）
const MAX_SESSIONS = 16;                // 缓存会话条数上限（超出删最旧）
const MAX_KEEP_SCAN = 64;               // 裁剪轮数迭代上限

// 顶层常量缓存（中间件进程内只解析一次；options 逐请求传，缓存路径
// 以 options.cache_path 优先，故文件路径在钩子内解析）。
const TMP_FALLBACK = path.join(os.tmpdir(), 'ym-context-guard-cache.json');

// ---------------------------------------------------------------------------
// 钩子入口（adapter 约定：返回 undefined=放行，返回对象=changed）
// ---------------------------------------------------------------------------

export function onRequest(body, ctx) {
  if (!body || typeof body !== 'object') return;
  const messages = body.messages;
  if (!Array.isArray(messages) || messages.length === 0) return;
  const o = resolveOptions(ctx && ctx.options);

  // 1) 估算 token（messages+system+tools；逐条预算供裁剪迭代复用）
  const perMsg = messages.map((m) => estimateText(JSON.stringify(m)));
  const fixed = estimateText(JSON.stringify(body.system ?? '') + JSON.stringify(body.tools ?? ''));
  let total = fixed;
  for (const t of perMsg) total += t;
  if (total < o.trigger_tokens) return;

  const file = cacheFile(o);
  const key = sessionKey(body, ctx);
  const doc = loadCache(file);
  const entry = doc.sessions && doc.sessions[key];

  // 2) 缓存命中：被摘要前缀与当前 messages 前缀指纹一致（会话尾部新增
  //    不影响命中），前缀替换为摘要占位。
  if (entry && Number.isInteger(entry.anchorLen) && entry.anchorLen >= 1 &&
      entry.anchorLen <= messages.length && typeof entry.summary === 'string' &&
      hashPrefix(messages, entry.anchorLen) === entry.anchorHash) {
    const kept = messages.slice(entry.anchorLen);
    truncateToolContents(kept, o.tail_tool_max_chars);
    const head = headSystemMessages(messages.slice(0, entry.anchorLen));
    const placeholder = { role: 'user', content: SUMMARY_MARKER + '\n' + entry.summary };
    return { ...body, messages: [...head, placeholder, ...kept] };
  }

  // 3) 缓存未命中：本地裁剪兜底（切点迭代到预算内），并异步生成摘要。
  const cut = findBudgetCut(messages, perMsg, total, o);
  const dropped = cut > 0 ? messages.slice(0, cut) : null;
  if (dropped) {
    const kept = messages.slice(cut);
    truncateToolContents(kept, o.tail_tool_max_chars);
    const head = headSystemMessages(dropped);
    const placeholder = {
      role: 'user',
      content: `${SUMMARY_MARKER}\n早前 ${dropped.length} 条消息已省略（超限保护，摘要生成中）。`,
    };
    scheduleSummarize(o, file, key, {
      dropped: dropped.filter((m) => m && m.role !== 'system'),
      oldSummary: entry && typeof entry.summary === 'string' ? entry.summary : '',
      anchorLen: cut,
      anchorHash: hashPrefix(messages, cut),
    });
    return { ...body, messages: [...head, placeholder, ...kept] };
  }

  // 4) 无安全切点（轮数不足/配对不可拆）：本次放行，仍排一次摘要。
  scheduleSummarize(o, file, key, {
    dropped: messages.filter((m) => m && m.role !== 'system'),
    oldSummary: entry && typeof entry.summary === 'string' ? entry.summary : '',
    anchorLen: messages.length,
    anchorHash: hashPrefix(messages, messages.length),
  });
  return;
}

// ---------------------------------------------------------------------------
// token 估算：ASCII 每 4 字符 1 token，非 ASCII（CJK 等）每字符 1 token
// —— 高估方向，宁可早触发不漏触发。
// ---------------------------------------------------------------------------

function estimateText(text) {
  if (!text) return 0;
  let ascii = 0;
  let wide = 0;
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) > 127) wide++;
    else ascii++;
  }
  return Math.ceil(ascii / 4 + wide);
}

// ---------------------------------------------------------------------------
// 会话键：claude-code 的 metadata.user_id 优先，回落 agent|model，
// 再回落首条 user 消息指纹（指纹用途，djb2 足够）。
// ---------------------------------------------------------------------------

function sessionKey(body, ctx) {
  const uid = body.metadata && body.metadata.user_id;
  if (typeof uid === 'string' && uid.trim()) return 'uid:' + uid.trim();
  const agent = ctx && typeof ctx.agent === 'string' ? ctx.agent : '';
  const model = ctx && typeof ctx.model === 'string' ? ctx.model : '';
  if (agent || model) return 'am:' + agent + '|' + model;
  for (const m of Array.isArray(body.messages) ? body.messages : []) {
    if (m && m.role === 'user') return 'msg:' + hashText(canonical(m));
  }
  return 'anon';
}

// ---------------------------------------------------------------------------
// canonical 序列化 + 指纹（canonical 同 history-trim：键排序，键序无关）
// ---------------------------------------------------------------------------

function canonical(value) {
  if (Array.isArray(value)) return '[' + value.map(canonical).join(',') + ']';
  if (value && typeof value === 'object') {
    return '{' + Object.keys(value).sort().map((k) => JSON.stringify(k) + ':' + canonical(value[k])).join(',') + '}';
  }
  return JSON.stringify(value) ?? 'null';
}

function hashText(text) {
  let h = 5381;
  for (let i = 0; i < text.length; i++) {
    h = ((h << 5) + h + text.charCodeAt(i)) >>> 0;
  }
  return h.toString(36) + ':' + text.length.toString(36);
}

function hashPrefix(messages, len) {
  let h = 5381;
  for (let i = 0; i < len; i++) {
    const part = hashText(canonical(messages[i]));
    for (let j = 0; j < part.length; j++) {
      h = ((h << 5) + h + part.charCodeAt(j)) >>> 0;
    }
    h = (h + i) >>> 0;
  }
  return h.toString(36) + ':' + len.toString(36);
}

// ---------------------------------------------------------------------------
// 缓存：单文件 JSON {sessions:{<key>:{summary,anchorLen,anchorHash,
// updated_at}}}。读 fail-soft（缺文件/损坏=空）；写 tmp+rename 原子落盘。
// 摘要完成回调在事件循环内逐个执行，同步写天然互斥。
// ---------------------------------------------------------------------------

function cacheFile(o) {
  if (o.cache_path) return o.cache_path;
  const dir = process.env.YM_APP_DIR;
  if (dir && dir.trim()) return path.join(dir.trim(), 'context-guard-cache.json');
  return TMP_FALLBACK;
}

function loadCache(file) {
  try {
    const raw = fs.readFileSync(file, 'utf8');
    const doc = JSON.parse(raw);
    if (doc && typeof doc === 'object' && doc.sessions && typeof doc.sessions === 'object') return doc;
  } catch (e) {
    if (e && e.code !== 'ENOENT') {
      process.stderr.write(`context-guard: 缓存读取失败（按空缓存处理）: ${e.message || e}\n`);
    }
  }
  return { sessions: {} };
}

function saveCache(file, doc) {
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const tmp = file + '.tmp';
    fs.writeFileSync(tmp, JSON.stringify(doc));
    fs.renameSync(tmp, file);
  } catch (e) {
    process.stderr.write(`context-guard: 缓存写入失败（下次重新摘要）: ${e.message || e}\n`);
  }
}

function pruneSessions(doc) {
  const keys = Object.keys(doc.sessions);
  if (keys.length <= MAX_SESSIONS) return;
  keys.sort((a, b) => String(doc.sessions[a].updated_at || '').localeCompare(String(doc.sessions[b].updated_at || '')));
  for (const k of keys.slice(0, keys.length - MAX_SESSIONS)) {
    delete doc.sessions[k];
  }
}

// ---------------------------------------------------------------------------
// 安全切点：切点必须落在「纯文本 user 消息」的轮起点上——
//   1. 该消息 role=user 且 content 不含 tool_result 块（anthropic 的
//      tool_result 必须紧跟前一条 assistant.tool_use，配对不可拆）；
//   2. 前一条是收尾的 assistant（不含 tool_use 块 / tool_calls）。
// chat 的 role="tool" 消息不是 user，天然不入选。
// ---------------------------------------------------------------------------

function isPureUserBoundary(messages, i) {
  const m = messages[i];
  if (!m || typeof m !== 'object' || m.role !== 'user') return false;
  if (i === 0) return true;
  if (Array.isArray(m.content)) {
    for (const b of m.content) {
      if (b && typeof b === 'object' && b.type === 'tool_result') return false;
    }
  }
  const p = messages[i - 1];
  if (!p || typeof p !== 'object' || p.role !== 'assistant') return false;
  if (Array.isArray(p.content)) {
    for (const b of p.content) {
      if (b && typeof b === 'object' && b.type === 'tool_use') return false;
    }
  }
  if (Array.isArray(p.tool_calls)) return false;
  return true;
}

function allBoundaries(messages) {
  const bounds = [];
  for (let i = 0; i < messages.length; i++) {
    if (isPureUserBoundary(messages, i)) bounds.push(i);
  }
  return bounds;
}

// cutAt：保留最近 k 个轮（最后一个轮起点=当前轮），切点为其前第 k 个
// 轮起点。切点 <1（裁光无意义/轮数不足）返回 -1。
function cutAt(bounds, k) {
  if (bounds.length <= k) return -1;
  const c = bounds[bounds.length - 1 - k];
  return c >= 1 ? c : -1;
}

// findBudgetCut：从 keep_recent_turns 起步，保留部分（含固定 system/
// tools 开销 + 占位）仍超预算则往前多裁一轮，直到预算内或无可再裁。
// 无任何可行切点返回 -1（调用方放行）。
function findBudgetCut(messages, perMsg, total, o) {
  const bounds = allBoundaries(messages);
  const fixed = total - perMsg.reduce((a, b) => a + b, 0);
  for (let k = o.keep_recent_turns; k <= MAX_KEEP_SCAN; k++) {
    const c = cutAt(bounds, k);
    if (c < 0) return -1;
    let kept = fixed;
    for (let i = c; i < perMsg.length; i++) kept += perMsg[i];
    kept += 64; // 占位消息估备
    if (kept <= o.target_tokens) return c;
    if (c === bounds[0] || cutAt(bounds, k + 1) < 0) return c; // 已到最早可切点
  }
  return -1;
}

// chat 形态的 system 消息独立保留（anthropic 的 system 在 body.system
// 字段，不在 messages，此函数对其天然为空）。
function headSystemMessages(from) {
  const head = [];
  for (const m of from) {
    if (m && m.role === 'system') head.push(m);
  }
  return head;
}

// ---------------------------------------------------------------------------
// 尾部 tool_result 截断（保留部分逐条，防大工具结果吃掉预算——内联
// history-trim 的 cutText：词边界回退 + 代理对保护）。
// ---------------------------------------------------------------------------

const TRUNCATION_SUFFIX = '\n...[truncated]';
const TRUNCATION_LOOKBACK = 80;

function isWordChar(char) {
  return char !== undefined && /\S/.test(char);
}

function findWhitespaceBackward(content, cutIndex) {
  const windowStart = Math.max(0, cutIndex - TRUNCATION_LOOKBACK);
  for (let i = cutIndex; i > windowStart; i--) {
    if (!isWordChar(content[i - 1])) return i - 1;
  }
  return -1;
}

function findWhitespaceForward(content, cutIndex) {
  const windowEnd = Math.min(content.length, cutIndex + TRUNCATION_LOOKBACK);
  for (let i = cutIndex; i < windowEnd; i++) {
    if (!isWordChar(content[i])) return i;
  }
  return -1;
}

function backOffToWordBoundary(content, cutIndex) {
  const onWordBoundary = !isWordChar(content[cutIndex - 1]) || !isWordChar(content[cutIndex]);
  if (onWordBoundary) return cutIndex;
  const backward = findWhitespaceBackward(content, cutIndex);
  if (backward !== -1) return backward;
  const forward = findWhitespaceForward(content, cutIndex);
  if (forward !== -1) return forward;
  return cutIndex;
}

function avoidSplitSurrogate(text, cutIndex) {
  if (cutIndex <= 0 || cutIndex >= text.length) return cutIndex;
  const hi = text.charCodeAt(cutIndex - 1);
  const lo = text.charCodeAt(cutIndex);
  return hi >= 0xd800 && hi <= 0xdbff && lo >= 0xdc00 && lo <= 0xdfff ? cutIndex - 1 : cutIndex;
}

function cutText(text, maxLength) {
  const cutIndex = avoidSplitSurrogate(text, backOffToWordBoundary(text, maxLength));
  return text.slice(0, cutIndex) + TRUNCATION_SUFFIX;
}

function truncateToolContents(messages, maxChars) {
  let changed = false;
  const out = messages.map((msg) => {
    if (!msg || typeof msg !== 'object') return msg;
    // chat 形态：assistant 的 tool_calls 参数不截（指令非结果）；tool
    // 角色消息 content 为字符串结果。
    if (msg.role === 'tool' && typeof msg.content === 'string') {
      if (msg.content.length <= maxChars) return msg;
      changed = true;
      return { ...msg, content: cutText(msg.content, maxChars) };
    }
    // anthropic 形态：user 消息块数组里的 tool_result 块。
    if (msg.role === 'user' && Array.isArray(msg.content)) {
      let innerChanged = false;
      const content = msg.content.map((block) => {
        if (!block || typeof block !== 'object' || block.type !== 'tool_result') return block;
        const inner = block.content;
        if (typeof inner === 'string') {
          if (inner.length <= maxChars) return block;
          innerChanged = true;
          return { ...block, content: cutText(inner, maxChars) };
        }
        if (Array.isArray(inner)) {
          let blockChanged = false;
          const nextInner = inner.map((part) => {
            if (!part || typeof part !== 'object' || part.type !== 'text') return part;
            if (typeof part.text !== 'string' || part.text.length <= maxChars) return part;
            blockChanged = true;
            return { ...part, text: cutText(part.text, maxChars) };
          });
          if (blockChanged) {
            innerChanged = true;
            return { ...block, content: nextInner };
          }
        }
        return block;
      });
      if (innerChanged) {
        changed = true;
        return { ...msg, content };
      }
    }
    return msg;
  });
  if (changed) {
    messages.length = 0;
    for (const m of out) messages.push(m);
  }
  return changed;
}

// ---------------------------------------------------------------------------
// 异步摘要：直连上游（不经网关=零递归）。fire-and-forget——adapter 的
// try/catch 只包同步 onRequest，整条异步链必须以 .catch 收尾（node 20
// 的 unhandled rejection 直接杀进程）。同一会话去重（inFlight）。
// ---------------------------------------------------------------------------

const inFlight = new Set();

function resolveOptions(raw) {
  const o = raw && typeof raw === 'object' ? raw : {};
  const int = (v, dflt, min, max) => {
    const n = Math.floor(Number(v));
    if (!Number.isFinite(n) || n < min || n > max) return dflt;
    return n;
  };
  return {
    trigger_tokens: int(o.trigger_tokens, 160000, 1, 100000000),
    target_tokens: int(o.target_tokens, 80000, 1, 100000000),
    keep_recent_turns: int(o.keep_recent_turns, 4, 1, 1000),
    tail_tool_max_chars: int(o.tail_tool_max_chars, 4000, 100, 10000000),
    cache_path: typeof o.cache_path === 'string' ? o.cache_path.trim() : '',
    summary: o.summary && typeof o.summary === 'object' ? o.summary : null,
  };
}

function scheduleSummarize(o, file, key, args) {
  const s = o.summary;
  if (!s || typeof s.endpoint !== 'string' || !s.endpoint.trim() ||
      typeof s.model !== 'string' || !s.model.trim()) {
    return; // 未配置摘要通道：纯本地裁剪模式
  }
  if (inFlight.has(key)) return;
  inFlight.add(key);
  const historyText = historyTextOf(args.dropped, args.oldSummary);
  callSummaryLLM(s, buildSummaryPrompt(args.oldSummary, historyText))
    .then((summary) => {
      const doc = loadCache(file);
      doc.sessions = doc.sessions || {};
      doc.sessions[key] = {
        summary,
        anchorLen: args.anchorLen,
        anchorHash: args.anchorHash,
        updated_at: new Date().toISOString(),
      };
      pruneSessions(doc);
      saveCache(file, doc);
    })
    .catch((err) => {
      process.stderr.write(`context-guard: 摘要失败（本次仅本地裁剪）: ${(err && err.message) || err}\n`);
    })
    .finally(() => inFlight.delete(key));
}

function historyTextOf(dropped, oldSummary) {
  let text = JSON.stringify(dropped);
  if (text.length > SUMMARY_INPUT_MAX_CHARS) {
    text = text.slice(-SUMMARY_INPUT_MAX_CHARS) + '\n…[更早部分因长度截去]';
  }
  return text;
}

function buildSummaryPrompt(oldSummary, historyText) {
  const lines = [
    '请将以下 AI 编码代理的对话历史压缩为一份摘要，供后续对话作为上下文使用。必须保留：',
    '1. 用户的任务目标与约束；2. 已做出的关键决定及原因；3. 已完成的代码变更（文件路径、函数名）；',
    '4. 未完成事项与下一步；5. 重要错误与修复。直接输出摘要正文，不要寒暄。',
  ];
  if (oldSummary) {
    lines.push('以下是此前的摘要（覆盖更早的历史），请把新历史的内容并入，输出一份合并后的完整摘要：\n' + oldSummary);
  }
  lines.push('对话历史：\n' + historyText);
  return lines.join('\n\n');
}

async function callSummaryLLM(s, prompt) {
  const maxTok = Number.isFinite(Number(s.max_tokens)) && Number(s.max_tokens) > 0
    ? Math.floor(Number(s.max_tokens)) : 4000;
  let resp;
  if (s.protocol === 'anthropic') {
    resp = await fetch(s.endpoint, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'x-api-key': String(s.api_key ?? ''),
        'anthropic-version': '2023-06-01',
      },
      body: JSON.stringify({ model: s.model, max_tokens: maxTok, messages: [{ role: 'user', content: prompt }] }),
    });
    if (!resp.ok) throw new Error('summary HTTP ' + resp.status);
    const data = await resp.json();
    const text = Array.isArray(data?.content)
      ? data.content.filter((b) => b && b.type === 'text').map((b) => b.text).join('\n')
      : '';
    if (!text) throw new Error('summary 空回复');
    return text;
  }
  // openai 形态（缺省）
  resp = await fetch(s.endpoint, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      authorization: 'Bearer ' + String(s.api_key ?? ''),
    },
    body: JSON.stringify({ model: s.model, max_tokens: maxTok, messages: [{ role: 'user', content: prompt }] }),
  });
  if (!resp.ok) throw new Error('summary HTTP ' + resp.status);
  const data = await resp.json();
  const text = data?.choices?.[0]?.message?.content;
  if (!text) throw new Error('summary 空回复');
  return String(text);
}
