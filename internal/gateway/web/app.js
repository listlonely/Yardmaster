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

async function api(path, opts) {
  const r = await fetch(path, opts);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
  return body;
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
  if (name === 'observe') loadTraces();
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
  } catch { /* settings.json 不存在：保持空表单 */ }
}

$('btn-save-settings').addEventListener('click', async () => {
  const body = {
    global_proxy: $('set-proxy').value.trim(),
    l1_classifier: $('set-l1').value.trim(),
    l1_timeout_ms: parseInt($('set-l1-timeout').value, 10) || 0,
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
          return `
          <tr>
            <td>${esc(a.name)}</td>
            <td><code>${esc(a.api_key || '（未设置）')}</code></td>
            <td>${statusBadge(view.status)}${restUntil ? `<div class="muted">休眠至 ${esc(restUntil.replace('T', ' '))}</div>` : ''}</td>
            <td>${usageBar(view)}</td>
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
        <td>${(g.members || []).map((m) => `<code>${esc(m)}</code>${memberValid(m, provs, groupIDs)}`).join(' ')}</td></tr>`).join(''),
      '未配置路由组');
    fill('#tbl-rules tbody',
      (rules.rules || []).map((r) => `
        <tr><td><code>${esc(r.id)}</code></td><td class="muted">${esc(r.desc || '—')}</td>
        <td>${esc(ruleCond(r))}</td>
        <td><code>${esc(r.target)}</code></td><td>${r.hits || 0}</td></tr>`).join(''),
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
        <td>${d.priority || 0}</td></tr>`).join(''),
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

// ---------- 启动 ----------

loadOverview();
loadSettings();
loadProviders();
loadRouting();
setInterval(() => {
  if (q('#page-overview').classList.contains('active')) loadOverview();
}, 15000);
