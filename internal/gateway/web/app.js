// yardmaster 控制台（M2-W2 只读全景）：总览 / Provider / 路由 / 观测。
// 数据源：/api/state、/api/config/*、/api/trace（W1 建立的只读 API，
// 本周后端零改动）。
// 总览：数据卡 + 代理继承链 + 休眠账号；Provider：休眠 join、有效代理
// 标注、模型清单、用量进度条；路由：成员有效性标注 + 完整词表列；
// 观测：caller 筛选、行展开候选链详情、自动刷新。
// 编辑表单于 W3 接入写接口。

'use strict';

const $ = (id) => document.getElementById(id);
const q = (sel) => document.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// ---------- 访问令牌（M2-W5） ----------

// 会话内令牌（sessionStorage）：网关开启 token 校验后，API 401 驱动
// 输入模态框并自动重试；静态壳不设防（无敏感数据），数据面全过闸。
let authToken = sessionStorage.getItem('ym-token') || '';

async function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({}, opts.headers,
    authToken ? { 'X-Gw-Token': authToken } : {});
  const r = await fetch(path, opts);
  if (r.status === 401) {
    await promptToken(); // 取消则 reject 冒泡
    return api(path, opts); // 取到令牌后重试
  }
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
  return body;
}

// 单飞令牌提示：启动时多个并发请求同时 401，若各自弹框会叠层残留
// （实测缺陷），共享同一 Promise，所有等待方在令牌到手后各自重试。
let tokenPrompt = null;

function promptToken() {
  if (tokenPrompt) return tokenPrompt;
  tokenPrompt = new Promise((resolve, reject) => {
    const back = document.createElement('div');
    back.className = 'modal-back';
    back.innerHTML = `<div class="modal"><h3>需要访问令牌</h3>
      <p class="muted">网关已开启令牌校验（settings.json 的 token 字段）。令牌仅存于本会话。</p>
      <form>
        <label><span>令牌</span><input name="token" type="password" required autofocus></label>
        <div class="row">
          <span class="modal-err"></span>
          <button type="button" class="modal-cancel">取消</button>
          <button type="submit" class="primary">保存</button>
        </div>
      </form></div>`;
    $('modal-root').appendChild(back);
    const done = (fn, val) => { back.remove(); tokenPrompt = null; fn(val); };
    back.querySelector('.modal-cancel').onclick = () => done(reject, new Error('未提供访问令牌'));
    back.querySelector('form').onsubmit = (e) => {
      e.preventDefault();
      const t = new FormData(e.target).get('token').trim();
      if (!t) return;
      authToken = t;
      sessionStorage.setItem('ym-token', t);
      done(resolve, null);
    };
  });
  return tokenPrompt;
}

function toast(msg, isErr = false) {
  const t = $('toast');
  t.textContent = msg;
  t.classList.add('show');
  setTimeout(() => t.classList.remove('show'), 2600);
}

const fmtTokens = (n) => Number(n || 0).toLocaleString('zh-CN');

// 全局缓存：跨页 join 用（state.resting × providers config）。
const cache = { state: null, providers: null, settings: null };

async function refreshState() {
  cache.state = await api('/api/state');
  return cache.state;
}

// ---------- 标签页 ----------

document.querySelectorAll('#tabs a').forEach((a) => {
  a.addEventListener('click', (e) => {
    e.preventDefault();
    activateTab(a.dataset.tab);
  });
});

function activateTab(name) {
  document.querySelectorAll('#tabs a').forEach((a) =>
    a.classList.toggle('active', a.dataset.tab === name));
  document.querySelectorAll('.page').forEach((p) =>
    p.classList.toggle('active', p.id === 'page-' + name));
  if (name === 'observe') { loadTraces(); loadCost(); loadPricing(); }
}

// ---------- 总览 ----------

function statCard(label, value, sub) {
  return `<div class="stat"><div class="stat-v">${value}</div>
    <div class="stat-l">${esc(label)}</div>
    ${sub ? `<div class="stat-s muted">${esc(sub)}</div>` : ''}</div>`;
}

async function loadOverview() {
  try {
    const s = await refreshState();
    $('status').textContent = '运行中';
    $('status').classList.remove('err');
    $('ov-state').textContent =
      `版本 ${s.version} · 已运行 ${s.uptime} · ` +
      `Provider ${(s.providers || []).length} 个 · 路由组 ${(s.groups || []).length} 个`;
    const rk = Object.keys(s.resting || {});
    $('ov-resting').innerHTML = rk.length
      ? rk.map((k) => `<div><code>${esc(k)}</code> → 休眠至 ${esc((s.resting[k] || '').replace('T', ' '))}</div>`).join('')
      : '无';
    const ex = s.exhausted || [];
    $('ov-exhausted').innerHTML = ex.length
      ? ex.map((k) => `<div><code>${esc(k)}</code> <span class="badge resting">额度耗尽</span></div>`).join('')
      : '无';
  } catch (e) {
    $('status').textContent = '无法连接';
    $('status').classList.add('err');
    $('ov-state').textContent = e.message;
    return;
  }
  // 数据卡与代理链依赖 providers/settings（失败时独立降级，不拖垮状态区）
  try {
    if (!cache.providers) cache.providers = await api('/api/config/providers');
    if (!cache.settings) cache.settings = await api('/api/config/settings');
    renderStatCards();
    renderProxyChain();
  } catch (e) {
    $('stat-cards').innerHTML = `<div class="muted">数据卡加载失败: ${esc(e.message)}</div>`;
  }
}

function renderStatCards() {
  const provs = cache.providers.providers || [];
  const accCount = provs.reduce((n, p) => n + (p.accounts || []).length, 0);
  // 当日用量读 /api/state 的实时账号快照（Planner 内存值，无落盘节流滞后）
  const localDay = new Date().toLocaleDateString('sv');
  let used = 0, quotaAccs = 0;
  for (const a of (cache.state.accounts || [])) {
    if (a.usage_day === localDay) used += a.used_tokens || 0;
    if (a.quota_tokens_day > 0) quotaAccs++;
  }
  const rules = cache.rules ? (cache.rules.rules || []).length : '…';
  $('stat-cards').innerHTML =
    statCard('Provider', provs.length, `${provs.filter((p) => !p.disabled).length} 个启用`) +
    statCard('账号', accCount, `${quotaAccs} 个配置额度`) +
    statCard('路由组', (cache.state.groups || []).length) +
    statCard('任务规则', rules) +
    statCard('当日用量', fmtTokens(used), 'tokens（全部账号累计）');
}

function renderProxyChain() {
  const g = cache.settings.global_proxy || '';
  const rows = [`<div><b>全局</b>：${g ? `<code>${esc(g)}</code>` : '未设置（跟随系统代理）'}</div>`];
  const provs = cache.providers.providers || [];
  const special = provs.filter((p) => p.proxy || (p.accounts || []).some((a) => a.proxy));
  if (!special.length) {
    rows.push('<div class="muted">无 Provider/账号级代理覆写：全部流量走全局链。</div>');
  } else {
    for (const p of special) {
      for (const a of (p.accounts || [])) {
        const eff = a.proxy || p.proxy || g || '系统代理';
        const src = a.proxy ? '账号' : p.proxy ? 'Provider' : g ? '全局' : '系统';
        rows.push(`<div><code>${esc(p.id)}/${esc(a.name)}</code> → <code>${esc(eff)}</code> <span class="muted">（生效层：${src}）</span></div>`);
      }
    }
  }
  $('ov-proxy').innerHTML = rows.join('');
}

async function loadSettings() {
  try {
    const s = await api('/api/config/settings');
    cache.settings = s;
    $('set-proxy').value = s.global_proxy || '';
    $('set-l1').value = s.l1_classifier || '';
    $('set-l1-timeout').value = s.l1_timeout_ms || '';
    $('set-listen').value = s.listen || '';
    $('set-token').value = ''; // 令牌脱敏返回，留空即保留（placeholder 提示）
    $('set-token').placeholder = s.token ? `已设置（${s.token}），留空=保留` : '留空=保留现有；仅本机使用可留空';
  } catch { /* settings.json 不存在：保持空表单 */ }
}

$('btn-save-settings').addEventListener('click', async () => {
  const body = {
    global_proxy: $('set-proxy').value.trim(),
    l1_classifier: $('set-l1').value.trim(),
    l1_timeout_ms: parseInt($('set-l1-timeout').value, 10) || 0,
    listen: $('set-listen').value.trim(),
    token: $('set-token').value, // 空值由服务端按「保留现有」处理
  };
  try {
    const r = await api('/api/config/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    cache.settings = body;
    $('settings-msg').textContent =
      r.effect === 'restart' ? '已保存（重启 serve 后完全生效）' : '已保存并生效';
    toast('设置已保存（已写入写前备份）');
  } catch (e) {
    toast('保存失败: ' + e.message, true);
  }
});

$('btn-reload').addEventListener('click', async () => {
  try {
    const r = await api('/api/reload', { method: 'POST' });
    cache.providers = null; // 重载后强制重新拉取配置
    toast(`已重载：${r.providers} 个 provider`);
    loadOverview();
    loadProviders();
  } catch (e) {
    toast('重载失败: ' + e.message, true);
  }
});

// ---------- Provider ----------

const statusBadge = (st) => `<span class="badge ${esc(st || 'available')}">${esc(st || 'available')}</span>`;

// OAuth 账号过期时间展示（M3-W2）：expires_at 为 Unix 秒，0/缺失=未知。
const oauthExpiryText = (m) => {
  if (!m || !m.expires_at) return '未知';
  return new Date(m.expires_at * 1000).toLocaleString();
};

// usageBar：当日用量进度条（无额度设置时只显示数字）。
function usageBar(a) {
  const used = a.used_tokens || 0;
  const quota = a.quota_tokens_day || 0;
  if (quota <= 0) return `${fmtTokens(used)} tokens <span class="muted">（未设额度）</span>`;
  const pct = Math.min(100, Math.round((used / quota) * 100));
  const cls = pct >= 90 ? 'danger' : pct >= 70 ? 'warn' : 'ok';
  return `<div class="bar"><div class="bar-fill ${cls}" style="width:${pct}%"></div></div>
    <span class="muted">${fmtTokens(used)} / ${fmtTokens(quota)}（${pct}%）</span>`;
}

// effectiveProxy：账号 → Provider → 全局 → 系统（F3 继承链），标注生效层。
function effectiveProxy(a, p, global) {
  if (a.proxy) return { v: a.proxy, src: '账号' };
  if (p.proxy) return { v: p.proxy, src: 'Provider' };
  if (global) return { v: global, src: '全局' };
  return { v: '系统代理', src: '系统' };
}

async function loadProviders() {
  try {
    const f = await api('/api/config/providers');
    cache.providers = f;
    const global = (cache.settings && cache.settings.global_proxy) || '';
    const resting = (cache.state && cache.state.resting) || {};
    // 账号实时状态（状态/用量/额度）从 /api/state join（Planner 内存值）；
    // 密钥/代理/模型清单仍来自配置文件。
    const live = {};
    for (const a of ((cache.state && cache.state.accounts) || [])) live[a.key] = a;
    const el = $('prov-list');
    if (!(f.providers || []).length) {
      el.innerHTML = '<div class="card muted">尚未配置 provider（用 `yardmaster provider add` 或等 W3 控制台编辑）</div>';
      return;
    }
    el.innerHTML = (f.providers || []).map((p) => `
      <div class="card">
        <div class="row"><b>${esc(p.name || p.id)}</b>
          <code>${esc(p.id)}</code>
          <span class="badge">${esc(p.protocol)}</span>
          ${p.disabled ? '<span class="badge disabled">已停用</span>' : ''}
          ${p.proxy ? `<span class="muted">Provider 代理 ${esc(p.proxy)}</span>` : ''}
        </div>
        <table><thead><tr><th>账号</th><th>密钥</th><th>状态</th><th>当日用量</th><th>有效代理</th></tr></thead><tbody>
        ${(p.accounts || []).map((a) => {
          const restUntil = resting[`${p.id}/${a.name}`];
          const lv = live[`${p.id}/${a.name}`] || {};
          const view = {
            ...a,
            status: lv.status || a.status,
            used_tokens: lv.used_tokens ?? a.used_tokens,
            quota_tokens_day: lv.quota_tokens_day ?? a.quota_tokens_day,
          };
          const eff = effectiveProxy(a, p, global);
          const credCell = a.auth === 'oauth'
            ? `<span class="badge oauth">OAuth</span>${a.oauth?.state === 'stale' ? ' <span class="badge resting">已过期待刷新</span>' : ''}
               <div class="muted">过期 ${esc(oauthExpiryText(a.oauth))}</div>`
            : `<code>${esc(a.api_key || '（未设置）')}</code>`;
          // M3-W5 运行态：订阅窗口 / 余额 / 在途（/api/state join）
          let liveCell = '';
          if (lv.window_status === 'restricted' && (lv.window_reset_at || 0) * 1000 > Date.now()) {
            liveCell += `<span class="badge resting">窗口限频</span><div class="muted">重置 ${esc(new Date(lv.window_reset_at * 1000).toLocaleTimeString())}</div>`;
          }
          if (lv.balance_at) liveCell += `<div class="muted">余额 ¥${Number(lv.balance).toFixed(2)}</div>`;
          if (lv.inflight) liveCell += `<div class="muted">在途 ${esc(lv.inflight)}</div>`;
          return `
          <tr>
            <td>${esc(a.name)}</td>
            <td>${credCell}</td>
            <td>${statusBadge(view.status)}${restUntil ? `<div class="muted">休眠至 ${esc(restUntil.replace('T', ' '))}</div>` : ''}${liveCell}</td>
            <td>${usageBar(view)} <a class="quota-btn" data-p="${esc(p.id)}" data-a="${esc(a.name)}">额度</a></td>
            <td><code>${esc(eff.v)}</code> <span class="muted">（${eff.src}层）</span></td>
          </tr>`;
        }).join('') || '<tr><td colspan="5" class="muted">无账号</td></tr>'}
        </tbody></table>
        ${(p.models && p.models.length) ? `
          <details><summary class="muted">模型清单（${p.models.length} 个）</summary>
            <div class="chips">${p.models.map((m) => `<code>${esc(m.id)}</code>`).join(' ')}</div>
          </details>` : ''}
      </div>`).join('');
  } catch (e) {
    $('prov-list').innerHTML = `<div class="card muted">加载失败: ${esc(e.message)}</div>`;
  }
}

// ---------- 路由 ----------

const ruleCond = (r) => {
  const parts = [];
  if (r.intent) parts.push('意图=' + r.intent);
  if (r.regex) parts.push('正则 /' + r.regex + '/');
  if (r.max_tokens) parts.push('≤' + r.max_tokens + ' tokens');
  if (r.caller_key) parts.push('caller=' + r.caller_key);
  return parts.join(' · ') || '（兜底）';
};

// memberValid：组成员引用有效性（provider 存在 + 模型在清单内/无清单；
// group/<id> 引用存在）。返回 HTML 标注。
function memberValid(m, provs, groups) {
  if ((m || '').startsWith('group/')) {
    const ok = groups.has(m.slice(6));
    return ok ? '' : ' <span class="badge prefail">组不存在</span>';
  }
  const [provID, model] = (m || '').split('/');
  const p = provs.find((x) => x.id === provID);
  if (!p) return ' <span class="badge prefail">provider 不存在</span>';
  if (model && (p.models || []).length && !p.models.some((mm) => mm.id === model)) {
    return ' <span class="badge warn">模型不在清单</span>';
  }
  return '';
}

async function loadRouting() {
  try {
    const [state, rules, keys, intents, provCfg] = await Promise.all([
      api('/api/state'), api('/api/config/rules'),
      api('/api/config/keys'), api('/api/config/intents'),
      cache.providers ? Promise.resolve(cache.providers) : api('/api/config/providers'),
    ]);
    cache.rules = rules;
    cache.providers = provCfg;
    const provs = provCfg.providers || [];
    const groupIDs = new Set((state.groups || []).map((g) => g.id));
    const fill = (sel, rows, empty) => {
      q(sel).innerHTML = rows || `<tr><td colspan="9" class="muted">${empty}</td></tr>`;
    };
    fill('#tbl-groups tbody',
      (state.groups || []).map((g) => `
        <tr><td><code>${esc(g.id)}</code></td><td>${esc(g.strategy)}</td>
        <td>${(g.members || []).map((m) => `<code>${esc(m)}</code>${memberValid(m, provs, groupIDs)}`).join(' ')}</td>
        <td class="muted-actions"><a class="edit" data-g="${esc(g.id)}">编辑</a><a class="del" data-g="${esc(g.id)}">删除</a></td></tr>`).join(''),
      '未配置路由组');
    fill('#tbl-rules tbody',
      (rules.rules || []).map((r) => `
        <tr><td><code>${esc(r.id)}</code></td><td class="muted">${esc(r.desc || '—')}</td>
        <td>${esc(ruleCond(r))}</td>
        <td><code>${esc(r.target)}</code></td><td>${r.hits || 0}</td>
        <td class="muted-actions"><a class="edit" data-r="${esc(r.id)}">编辑</a><a class="del" data-r="${esc(r.id)}">删除</a></td></tr>`).join(''),
      '未配置规则');
    fill('#tbl-keys tbody',
      (keys.bindings || []).map((b) => `
        <tr><td><code>${esc(b.key)}</code></td><td><code>${esc(b.default_model)}</code></td></tr>`).join(''),
      '未配置绑定');
    fill('#tbl-intents tbody',
      (intents.intents || []).map((d) => `
        <tr><td><code>${esc(d.id)}</code></td><td>${esc(d.label || '')}</td>
        <td>${esc((d.keywords || []).join('、')) || '<span class="muted">—</span>'}</td>
        <td>${esc((d.regexes || []).join('；')) || '<span class="muted">—</span>'}</td>
        <td>${esc((d.examples || []).join('；')) || '<span class="muted">—</span>'}</td>
        <td>${d.priority || 0}</td>
        <td class="muted-actions"><a class="edit" data-i="${esc(d.id)}">编辑</a><a class="del" data-i="${esc(d.id)}">删除</a></td></tr>`).join(''),
      '无自定义意图（仅内置 L0/L1）');
  } catch (e) {
    toast('路由页加载失败: ' + e.message, true);
  }
}

// ---------- 观测 ----------

// traceDetail：行展开的候选链与判定详情（F9 失败链呈现）。
function traceDetail(t) {
  const rows = [];
  rows.push(`<div><b>路由路径</b>：${esc(t.route_path || '—')}
    ${t.intent ? ` · <b>意图</b>：${esc(t.intent)}` : ''}
    ${t.rule ? ` · <b>规则</b>：${esc(t.rule)}` : ''}
    ${t.served ? ` · <b>实际服务</b>：<code>${esc(t.served)}</code>（${esc(t.served_model || '')}）` : ''}
    ${t.ttfb_ms != null ? ` · <b>首字节</b>：${t.ttfb_ms}ms` : ''}
    ${t.usage ? ` · <b>token</b>：${fmtTokens(t.usage.prompt)}+${fmtTokens(t.usage.completion)}` : ''}</div>`);
  if (!(t.candidates || []).length) {
    rows.push('<div class="muted">无候选尝试记录。</div>');
  } else {
    rows.push('<table class="inner"><thead><tr><th>#</th><th>候选</th><th>上游模型</th><th>结果</th><th>状态</th><th>耗时</th><th>代理出口</th><th>原因</th></tr></thead><tbody>' +
      t.candidates.map((c, i) => `
        <tr>
          <td>${i + 1}</td>
          <td><code>${esc(c.provider)}/${esc(c.account)}</code></td>
          <td>${esc(c.model || '')}</td>
          <td><span class="badge ${c.result === 'ok' ? 'ok' : c.result === 'quota' || c.result === 'cred' ? 'prefail' : ''}">${esc(c.result)}</span></td>
          <td>${c.status || '—'}</td>
          <td>${c.dur_ms ?? '—'}ms</td>
          <td class="muted">${esc(c.proxy || '直连')}</td>
          <td class="muted">${esc(c.err || '—')}</td>
        </tr>`).join('') + '</tbody></table>');
  }
  return rows.join('');
}

async function loadTraces() {
  const limit = $('trace-limit').value;
  const result = $('trace-result').value;
  const agent = $('trace-agent').value.trim();
  try {
    const r = await api(`/api/trace?limit=${limit}${result ? `&result=${result}` : ''}${agent ? `&agent=${encodeURIComponent(agent)}` : ''}`);
    q('#tbl-trace tbody').innerHTML = (r.traces || []).map((t, i) => `
      <tr class="trace-row" data-idx="${i}">
        <td class="toggle muted">▸</td>
        <td class="muted">${esc((t.time || '').replace('T', ' ').slice(0, 19))}</td>
        <td><code>${esc(t.caller || '—')}</code></td>
        <td>${esc(t.request_model || '')}</td>
        <td>${esc(t.route_path || '')}</td>
        <td>${esc(t.target || '')}</td>
        <td><span class="badge ${t.outcome === 'ok' ? 'ok' : 'prefail'}">${esc(t.outcome)}</span>${t.http_status ? ` <span class="muted">${t.http_status}</span>` : ''}</td>
        <td class="muted">${(t.candidates || []).length || '—'}</td>
        <td>${t.total_ms ?? '—'} ms</td>
      </tr>
      <tr class="trace-detail" hidden><td></td><td colspan="8">${traceDetail(t)}</td></tr>`).join('')
      || '<tr><td colspan="9" class="muted">暂无记录</td></tr>';
    // 行点击展开/收起候选链详情
    q('#tbl-trace tbody').onclick = (e) => {
      const row = e.target.closest('tr.trace-row');
      if (!row) return;
      const detail = row.nextElementSibling;
      if (detail && detail.classList.contains('trace-detail')) {
        detail.hidden = !detail.hidden;
        row.querySelector('.toggle').textContent = detail.hidden ? '▸' : '▾';
      }
    };
  } catch (e) {
    toast('trace 加载失败: ' + e.message, true);
  }
}

$('btn-trace-refresh').addEventListener('click', loadTraces);
for (const id of ['trace-agent', 'trace-result', 'trace-limit']) {
  $(id).addEventListener('change', loadTraces);
}
$('trace-agent').addEventListener('keydown', (e) => { if (e.key === 'Enter') loadTraces(); });

// 自动刷新：勾选后每 30s 拉取一次（仅在观测页激活时生效）。
let traceTimer = null;
$('trace-auto').addEventListener('change', (e) => {
  clearInterval(traceTimer);
  if (e.target.checked) traceTimer = setInterval(() => {
    if (q('#page-observe').classList.contains('active')) loadTraces();
  }, 30000);
});

// ---------- 编辑（M2-W3）：表单模态框 ----------
//
// 写路径约定：每次提交前 fresh GET 构造全文档（把 last-writer-wins
// 窗口压到最小），经 PUT /api/config/*（服务端严格校验 + 写前备份 +
// 审计）；providers 的 api_key 以脱敏形回传，服务端保留现存密钥。
// 校验失败（客户端或服务端）不落盘，错误显示在表单内。

function formModal(title, bodyHTML, onSubmit) {
  const back = document.createElement('div');
  back.className = 'modal-back';
  back.innerHTML = `<div class="modal"><h3>${esc(title)}</h3>
    <form>${bodyHTML}
      <div class="row">
        <span class="modal-err"></span>
        <button type="button" class="modal-cancel">取消</button>
        <button type="submit" class="primary">保存</button>
      </div>
    </form></div>`;
  $('modal-root').appendChild(back);
  const close = () => back.remove();
  back.querySelector('.modal-cancel').onclick = close;
  back.querySelector('form').onsubmit = async (e) => {
    e.preventDefault();
    const errEl = back.querySelector('.modal-err');
    errEl.textContent = '';
    try {
      await onSubmit(back.querySelector('form'), close);
    } catch (ex) {
      errEl.textContent = ex.message; // 校验失败不落盘，错误留在表单内
    }
  };
  return { close };
}

const formVals = (form) => Object.fromEntries(new FormData(form).entries());
const lines = (s) => (s || '').split('\n').map((x) => x.trim()).filter(Boolean);
const compileCheck = (expr) => { try { new RegExp(expr); return null; } catch (e) { return e.message; } };

// providers 全文档写回：fresh GET → mutator 修改 → PUT（服务端保留密钥）
async function putProvidersDoc(mutator) {
  const cur = await api('/api/config/providers');
  const next = mutator(cur);
  const r = await api('/api/config/providers', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(next),
  });
  cache.providers = null; // 强制重拉（含脱敏密钥的新文件态）
  return r;
}

async function afterWrite() {
  await refreshState();
  loadOverview();
  loadProviders();
  loadRouting();
}

// 客户端成员校验（与服务端 validateProviders/validateMember 同规）
function memberClientCheck(m, provs, otherGroupIDs) {
  if (!m) return '成员引用为空';
  if (m.startsWith('group/')) {
    return otherGroupIDs.has(m.slice(6)) ? null : `引用的组 ${m} 不存在`;
  }
  const [provID, model] = m.split('/');
  const p = provs.find((x) => x.id === provID);
  if (!p) return `成员 ${m} 引用的 provider 不存在`;
  if (model && (p.models || []).length && !p.models.some((mm) => mm.id === model)) {
    return `模型 ${model} 不在 ${provID} 的清单内`;
  }
  return null;
}

function groupModal(existing) {
  const isEdit = !!existing;
  formModal(isEdit ? `编辑路由组 ${existing.id}` : '新建路由组', `
    <label><span>组 ID</span><input name="id" value="${esc(existing?.id || '')}" ${isEdit ? 'readonly' : ''} required></label>
    <label><span>策略</span><select name="strategy" id="gm-strategy">
      <option value="order"${existing?.strategy === 'order' ? ' selected' : ''}>order（固定顺序，用尽再换）</option>
      <option value="rotate"${existing?.strategy === 'rotate' ? ' selected' : ''}>rotate（每请求轮换）</option>
      <option value="smart"${existing?.strategy === 'smart' ? ' selected' : ''}>smart（按剩余额度比例）</option>
      <option value="usage"${existing?.strategy === 'usage' ? ' selected' : ''}>usage（当日用量升序，负载均衡）</option>
      <option value="weight"${existing?.strategy === 'weight' ? ' selected' : ''}>weight（平滑加权轮询）</option>
    </select></label>
    <label><span>成员（每行一个）</span><textarea name="members">${esc((existing?.members || []).join('\n'))}</textarea></label>
    <label><span>权重（weight 策略）</span><input name="weights" value="${esc(Object.entries(existing?.weights || {}).map(([k, v]) => `${k}=${v}`).join(', '))}" placeholder="键=权重，逗号分隔；键支持 provider/account · provider/model · provider"></label>
    <div class="hint">成员格式 provider/model、provider（取其清单）或 group/&lt;id&gt;；顺序即 order 策略的顺序。权重仅 weight 策略生效，缺省 1。</div>`,
  async (form, close) => {
    const v = formVals(form);
    const id = v.id.trim();
    if (!id) throw new Error('组 ID 不能为空');
    const members = lines(v.members);
    if (!members.length) throw new Error('至少需要一个成员');
    let weights;
    if (v.strategy === 'weight') {
      weights = {};
      for (const pair of (v.weights || '').split(',')) {
        const t = pair.trim();
        if (!t) continue;
        const [k, w] = t.split('=').map((x) => x.trim());
        const n = parseInt(w, 10);
        if (!k || !Number.isInteger(n) || n <= 0) throw new Error(`权重项非法: "${t}"（应为 键=正整数）`);
        weights[k] = n;
      }
    }
    const r = await putProvidersDoc((cur) => {
      const groups = cur.groups || [];
      const otherIDs = new Set(groups.filter((g) => g.id !== id).map((g) => g.id));
      if (otherIDs.has(id)) throw new Error(`组 ID ${id} 已存在`);
      for (const m of members) {
        const err = memberClientCheck(m, cur.providers || [], otherIDs);
        if (err) throw new Error(err);
      }
      const g = { id, strategy: v.strategy, members };
      if (weights && Object.keys(weights).length) g.weights = weights;
      const idx = groups.findIndex((x) => x.id === id);
      if (idx >= 0) groups[idx] = g; else groups.push(g);
      return { ...cur, groups };
    });
    close();
    toast(`路由组已保存（${r.effect === 'immediate' ? '已热生效' : r.effect}，写前备份已留底）`);
    afterWrite();
  });
}

async function deleteGroup(id) {
  if (!confirm(`删除路由组 ${id}？引用它的规则/绑定会失去目标。当前文件将先备份到 backup/ 目录，可手工还原。`)) return;
  await putProvidersDoc((cur) => ({ ...cur, groups: (cur.groups || []).filter((g) => g.id !== id) }));
  toast('路由组已删除（写前备份已留底）');
  afterWrite();
}

const BUILTIN_INTENTS = ['coding', 'qa', 'writing'];

async function refreshIntents() {
  const r = await api('/api/config/intents');
  return r.intents || [];
}

function intentDatalist(custom) {
  const ids = [...BUILTIN_INTENTS, ...(custom || []).map((d) => d.id)];
  return `<datalist id="dl-intents">${ids.map((i) => `<option value="${esc(i)}">`).join('')}</datalist>`;
}

async function ruleModal(existing) {
  const isEdit = !!existing;
  const customIntents = await refreshIntents(); // 词表注入 datalist（内置 + 自定义）
  formModal(isEdit ? `编辑规则 ${existing.id}` : '新建规则', `
    <label><span>规则 ID</span><input name="id" value="${esc(existing?.id || '')}" ${isEdit ? 'readonly' : ''} required></label>
    <label><span>说明</span><input name="desc" value="${esc(existing?.desc || '')}"></label>
    <label><span>意图条件</span><input name="intent" list="dl-intents" value="${esc(existing?.intent || '')}" placeholder="留空=不限"></label>
    <label><span>正则条件</span><input name="regex" value="${esc(existing?.regex || '')}" placeholder="对最新用户消息原文匹配；留空=不限"></label>
    <label><span>token 上限</span><input name="max_tokens" type="number" min="0" value="${existing?.max_tokens || ''}" placeholder="估算输入 ≤ 此值才命中；留空=不限"></label>
    <label><span>调用方 key</span><input name="caller_key" value="${esc(existing?.caller_key || '')}" placeholder="如 gw-claude-code；留空=不限"></label>
    <label><span>目标</span><input name="target" value="${esc(existing?.target || '')}" required placeholder="provider/model · provider · group/&lt;id&gt;"></label>
    ${intentDatalist(customIntents)}`,
  async (form, close) => {
    const v = formVals(form);
    const id = v.id.trim();
    if (!id) throw new Error('规则 ID 不能为空');
    const target = v.target.trim();
    if (!target) throw new Error('目标不能为空');
    if (v.regex.trim()) {
      const err = compileCheck(v.regex.trim());
      if (err) throw new Error('正则非法: ' + err);
    }
    const cur = await api('/api/config/rules');
    const rules = cur.rules || [];
    if (rules.some((r) => r.id === id && !isEdit)) throw new Error(`规则 ID ${id} 已存在`);
    const rule = {
      id, desc: v.desc.trim(), intent: v.intent.trim(), regex: v.regex.trim(),
      max_tokens: parseInt(v.max_tokens, 10) || 0,
      caller_key: v.caller_key.trim(), target,
      hits: (rules.find((r) => r.id === id) || {}).hits || 0, // 引擎按 ID 保留计数
    };
    const idx = rules.findIndex((r) => r.id === id);
    if (idx >= 0) rules[idx] = rule; else rules.push(rule);
    await api('/api/config/rules', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ rules }),
    });
    close();
    toast('规则已保存（mtime 热生效，写前备份已留底）');
    afterWrite();
  });
}

async function deleteRule(id) {
  if (!confirm(`删除规则 ${id}？当前文件将先备份到 backup/ 目录。`)) return;
  const cur = await api('/api/config/rules');
  await api('/api/config/rules', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ rules: (cur.rules || []).filter((r) => r.id !== id) }),
  });
  toast('规则已删除（写前备份已留底）');
  afterWrite();
}

function intentModal(existing) {
  const isEdit = !!existing;
  formModal(isEdit ? `编辑意图 ${existing.id}` : '新建自定义意图', `
    <label><span>意图 ID</span><input name="id" value="${esc(existing?.id || '')}" ${isEdit ? 'readonly' : ''} required placeholder="建议命名空间化，如 team/weekly-report"></label>
    <label><span>展示名</span><input name="label" value="${esc(existing?.label || '')}"></label>
    <label><span>优先级</span><input name="priority" type="number" value="${existing?.priority || 0}" placeholder="越大越先求值"></label>
    <label><span>关键词（逗号分隔）</span><input name="keywords" value="${esc((existing?.keywords || []).join(','))}" placeholder="大小写不敏感子串匹配"></label>
    <label><span>正则（每行一条）</span><textarea name="regexes">${esc((existing?.regexes || []).join('\n'))}</textarea></label>
    <label><span>示例句（每行一条，≤5）</span><textarea name="examples">${esc((existing?.examples || []).join('\n'))}</textarea></label>
    <div class="hint">示例句注入 L1 提示词作 few-shot；任务规则按 ID 引用此意图。</div>`,
  async (form, close) => {
    const v = formVals(form);
    const id = v.id.trim();
    if (!id) throw new Error('意图 ID 不能为空');
    const examples = lines(v.examples);
    if (examples.length > 5) throw new Error('示例句最多 5 条（F8 词表约束）');
    const regexes = lines(v.regexes);
    for (const re of regexes) {
      const err = compileCheck(re);
      if (err) throw new Error(`正则非法（${re}）: ` + err);
    }
    const intents = await refreshIntents();
    if (intents.some((d) => d.id === id && !isEdit)) throw new Error(`意图 ID ${id} 已存在`);
    const def = {
      id, label: v.label.trim(), priority: parseInt(v.priority, 10) || 0,
      keywords: (v.keywords || '').split(',').map((x) => x.trim()).filter(Boolean),
      regexes, examples,
    };
    const idx = intents.findIndex((d) => d.id === id);
    if (idx >= 0) intents[idx] = def; else intents.push(def);
    await api('/api/config/intents', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ intents }),
    });
    close();
    toast('意图已保存（热生效，写前备份已留底）');
    afterWrite();
  });
}

async function deleteIntent(id) {
  if (!confirm(`删除意图 ${id}？引用它的任务规则将不再命中。当前文件将先备份到 backup/ 目录。`)) return;
  const intents = await refreshIntents();
  await api('/api/config/intents', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ intents: intents.filter((d) => d.id !== id) }),
  });
  toast('意图已删除（写前备份已留底）');
  afterWrite();
}

function quotaModal(provID, accName) {
  const prov = (cache.providers?.providers || []).find((x) => x.id === provID);
  const acc = (prov?.accounts || []).find((x) => x.name === accName);
  formModal(`设置当日额度 · ${provID}/${accName}`, `
    <label><span>额度 (tokens/日)</span><input name="quota" type="number" min="0" step="1000" value="${acc?.quota_tokens_day || ''}" placeholder="留空或 0 = 不设额度"></label>
    <div class="hint">>0 后 smart 策略按剩余比例排序；跨日自动重置。写入会留备份。</div>`,
  async (form, close) => {
    const v = formVals(form);
    const quota = parseInt(v.quota, 10) || 0;
    if (quota === 0 && !confirm('额度为 0 表示不设额度（smart 将回落 rotate）。确认？')) return;
    await putProvidersDoc((cur) => {
      for (const p of cur.providers || []) {
        for (const a of p.accounts || []) {
          if (p.id === provID && a.name === accName) a.quota_tokens_day = quota;
        }
      }
      return cur;
    });
    close();
    toast(`额度已保存：${provID}/${accName} → ${quota || '不设'}`);
    afterWrite();
  });
}

// 表格操作列事件委托（编辑/删除/额度）
q('#tbl-groups tbody').addEventListener('click', (e) => {
  const g = e.target.dataset.g;
  if (!g) return;
  const cur = (cache.state.groups || []).find((x) => x.id === g);
  if (e.target.classList.contains('edit')) groupModal(cur);
  if (e.target.classList.contains('del')) deleteGroup(g);
});
q('#tbl-rules tbody').addEventListener('click', async (e) => {
  const id = e.target.dataset.r;
  if (!id) return;
  const cur = await api('/api/config/rules');
  const rule = (cur.rules || []).find((x) => x.id === id);
  if (e.target.classList.contains('edit')) ruleModal(rule);
  if (e.target.classList.contains('del')) deleteRule(id);
});
q('#tbl-intents tbody').addEventListener('click', async (e) => {
  const id = e.target.dataset.i;
  if (!id) return;
  const intents = await refreshIntents();
  const def = intents.find((x) => x.id === id);
  if (e.target.classList.contains('edit')) intentModal(def);
  if (e.target.classList.contains('del')) deleteIntent(id);
});
$('prov-list').addEventListener('click', (e) => {
  if (e.target.classList.contains('quota-btn')) {
    quotaModal(e.target.dataset.p, e.target.dataset.a);
  }
});
$('btn-add-group').addEventListener('click', () => groupModal(null));
$('btn-add-rule').addEventListener('click', () => ruleModal(null));
$('btn-add-intent').addEventListener('click', () => intentModal(null));

// ---------- 成本报告与单价表（M2-W4） ----------

const fmtUSD = (n) => '$' + Number(n || 0).toFixed(4);

async function loadCost() {
  const days = $('cost-days').value;
  try {
    const rep = await api(`/api/cost?days=${days}`);
    const table = (m, label) => {
      const keys = Object.keys(m || {}).sort((a, b) => m[b].cost - m[a].cost);
      if (!keys.length) return `<div class="muted">窗口内无 ${esc(label)} 数据</div>`;
      return `<table><thead><tr><th>${esc(label)}</th><th>调用</th><th>输入 tok</th><th>输出 tok</th><th>成本</th></tr></thead><tbody>` +
        keys.map((k) => {
          const b = m[k];
          return `<tr><td><code>${esc(k)}</code></td><td>${b.calls}</td>
            <td>${fmtTokens(b.input_tokens)}</td><td>${fmtTokens(b.output_tokens)}</td>
            <td>${fmtUSD(b.cost)}</td></tr>`;
        }).join('') + '</tbody></table>';
    };
    $('cost-body').innerHTML =
      `${table(rep.by_account, '账号')}${table(rep.by_model, '模型')}` +
      (rep.unpriced > 0
        ? `<div class="muted">未计价调用 ${rep.unpriced} 次（单价表无对应价格键；失败请求不计费）</div>`
        : '') +
      `<div class="muted">窗口 ${esc(rep.from)} ~ ${esc(rep.to)}</div>`;
  } catch (e) {
    $('cost-body').innerHTML = `<div class="muted">加载失败: ${esc(e.message)}</div>`;
  }
}

$('btn-cost-refresh').addEventListener('click', loadCost);
$('cost-days').addEventListener('change', loadCost);

function costTableBody(prices) {
  return (prices || []).map((p) => `
    <tr><td><code>${esc(p.model)}</code></td><td>${p.input_per_m}</td><td>${p.output_per_m}</td>
    <td class="muted-actions"><a class="edit" data-price="${esc(p.model)}">编辑</a><a class="del" data-price="${esc(p.model)}">删除</a></td></tr>`).join('')
    || '<tr><td colspan="4" class="muted">单价表为空（未配置的模型不计入成本，调用计入「未计价」）</td></tr>';
}

async function loadPricing() {
  try {
    const f = await api('/api/config/pricing');
    q('#tbl-pricing tbody').innerHTML = costTableBody(f.prices);
  } catch (e) {
    q('#tbl-pricing tbody').innerHTML = `<tr><td colspan="4" class="muted">加载失败: ${esc(e.message)}</td></tr>`;
  }
}

function priceModal(existing) {
  const isEdit = !!existing;
  formModal(isEdit ? `编辑单价 ${existing.model}` : '新建单价', `
    <label><span>价格键</span><input name="model" value="${esc(existing?.model || '')}" required placeholder="provider/model（精确）或裸 model"></label>
    <label><span>输入 $/1M</span><input name="input" type="number" min="0" step="0.000001" value="${existing?.input_per_m ?? ''}" required></label>
    <label><span>输出 $/1M</span><input name="output" type="number" min="0" step="0.000001" value="${existing?.output_per_m ?? ''}" required></label>
    <div class="hint">成本报告按此价 × trace 中的 token 用量计算；provider/model 精确键优先于裸 model 键。</div>`,
  async (form, close) => {
    const v = formVals(form);
    const model = v.model.trim();
    if (!model) throw new Error('价格键不能为空');
    const inP = parseFloat(v.input), outP = parseFloat(v.output);
    if (!Number.isFinite(inP) || !Number.isFinite(outP) || inP < 0 || outP < 0) {
      throw new Error('单价必须为非负数字');
    }
    const cur = await api('/api/config/pricing');
    const prices = cur.prices || [];
    if (prices.some((p) => p.model === model && !isEdit)) throw new Error(`价格键 ${model} 已存在`);
    const entry = { model, input_per_m: inP, output_per_m: outP };
    const idx = prices.findIndex((p) => p.model === model);
    if (idx >= 0) prices[idx] = entry; else prices.push(entry);
    await api('/api/config/pricing', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ prices }),
    });
    close();
    toast('单价已保存（写前备份已留底）');
    loadPricing();
    loadCost();
  });
}

async function deletePrice(model) {
  if (!confirm(`删除单价 ${model}？该模型的调用将计入「未计价」。当前文件将先备份到 backup/ 目录。`)) return;
  const cur = await api('/api/config/pricing');
  await api('/api/config/pricing', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ prices: (cur.prices || []).filter((p) => p.model !== model) }),
  });
  toast('单价已删除（写前备份已留底）');
  loadPricing();
  loadCost();
}

q('#tbl-pricing tbody').addEventListener('click', async (e) => {
  const model = e.target.dataset.price;
  if (!model) return;
  const cur = await api('/api/config/pricing');
  const entry = (cur.prices || []).find((p) => p.model === model);
  if (e.target.classList.contains('edit')) priceModal(entry);
  if (e.target.classList.contains('del')) deletePrice(model);
});
$('btn-add-price').addEventListener('click', () => priceModal(null));

// ---------- 启动 ----------

loadOverview();
loadSettings();
loadProviders();
loadRouting();
setInterval(() => {
  if (q('#page-overview').classList.contains('active')) loadOverview();
}, 15000);
