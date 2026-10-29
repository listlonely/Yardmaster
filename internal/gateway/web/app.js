// yardmaster 控制台（M2-W1 框架）：总览 / Provider / 路由 / 观测。
// 数据源：/api/state、/api/config/*、/api/trace、/api/reload。
// W1 范围：只读展示 + 设置保存（写路径端到端）+ 重载按钮；
// Provider/路由的编辑表单于 W2/W3 接入。

'use strict';

const $ = (id) => document.getElementById(id);
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

async function loadOverview() {
  try {
    const s = await api('/api/state');
    $('status').textContent = '运行中';
    $('status').classList.remove('err');
    const restCount = Object.keys(s.resting || {}).length;
    $('ov-state').textContent =
      `版本 ${s.version} · 已运行 ${s.uptime} · ` +
      `Provider ${(s.providers || []).length} 个 · 路由组 ${(s.groups || []).length} 个`;
    const rk = Object.keys(s.resting || {});
    $('ov-resting').innerHTML = rk.length
      ? rk.map((k) => `<div><code>${esc(k)}</code> → ${esc(s.resting[k])}</div>`).join('')
      : '无';
  } catch (e) {
    $('status').textContent = '无法连接';
    $('status').classList.add('err');
    $('ov-state').textContent = e.message;
  }
}

async function loadSettings() {
  try {
    const s = await api('/api/config/settings');
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
    toast(`已重载：${r.providers} 个 provider`);
    loadOverview();
  } catch (e) {
    toast('重载失败: ' + e.message, true);
  }
});

// ---------- Provider ----------

const statusBadge = (st) => `<span class="badge ${esc(st || 'available')}">${esc(st || 'available')}</span>`;

async function loadProviders() {
  try {
    const f = await api('/api/config/providers');
    const el = $('prov-list');
    if (!(f.providers || []).length) {
      el.innerHTML = '<div class="card muted">尚未配置 provider（用 `yardmaster provider add` 或等 W2 控制台添加）</div>';
      return;
    }
    el.innerHTML = (f.providers || []).map((p) => `
      <div class="card">
        <div class="row"><b>${esc(p.name || p.id)}</b>
          <code>${esc(p.id)}</code>
          <span class="badge">${esc(p.protocol)}</span>
          ${p.disabled ? '<span class="badge disabled">已停用</span>' : ''}
          ${p.proxy ? `<span class="muted">代理 ${esc(p.proxy)}</span>` : ''}
        </div>
        <table><thead><tr><th>账号</th><th>密钥</th><th>状态</th><th>当日用量</th><th>账号代理</th></tr></thead><tbody>
        ${(p.accounts || []).map((a) => `
          <tr>
            <td>${esc(a.name)}</td>
            <td><code>${esc(a.api_key || '（未设置）')}</code></td>
            <td>${statusBadge(a.status)}${a.resting_until ? `<span class="muted">至 ${esc(new Date(a.resting_until * 1000).toLocaleTimeString())}</span>` : ''}</td>
            <td>${a.used_tokens ? esc(a.used_tokens) : 0}${a.quota_tokens_day ? ` / ${esc(a.quota_tokens_day)} tokens` : ' tokens'}</td>
            <td>${esc(a.proxy || '—')}</td>
          </tr>`).join('') || '<tr><td colspan="5" class="muted">无账号</td></tr>'}
        </tbody></table>
        ${p.models && p.models.length ? `<div class="muted">模型清单 ${p.models.length} 个</div>` : ''}
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

async function loadRouting() {
  try {
    const [groups, rules, keys, intents] = await Promise.all([
      api('/api/state'), api('/api/config/rules'),
      api('/api/config/keys'), api('/api/config/intents'),
    ]);
    const fill = (sel, rows, empty) => {
      document.querySelector(sel).innerHTML = rows || `<tr><td colspan="9" class="muted">${empty}</td></tr>`;
    };
    fill('#tbl-groups tbody',
      (groups.groups || []).map((g) => `
        <tr><td><code>${esc(g.id)}</code></td><td>${esc(g.strategy)}</td>
        <td>${(g.members || []).map((m) => `<code>${esc(m)}</code>`).join(' ')}</td></tr>`).join(''),
      '未配置路由组');
    fill('#tbl-rules tbody',
      (rules.rules || []).map((r) => `
        <tr><td><code>${esc(r.id)}</code></td><td>${esc(ruleCond(r))}</td>
        <td><code>${esc(r.target)}</code></td><td>${r.hits || 0}</td></tr>`).join(''),
      '未配置规则');
    fill('#tbl-keys tbody',
      (keys.bindings || []).map((b) => `
        <tr><td><code>${esc(b.key)}</code></td><td><code>${esc(b.default_model)}</code></td></tr>`).join(''),
      '未配置绑定');
    fill('#tbl-intents tbody',
      (intents.intents || []).map((d) => `
        <tr><td><code>${esc(d.id)}</code></td><td>${esc(d.label || '')}</td>
        <td>${esc((d.keywords || []).join('、'))}</td><td>${d.priority || 0}</td></tr>`).join(''),
      '无自定义意图（仅内置 L0/L1）');
  } catch (e) {
    toast('路由页加载失败: ' + e.message, true);
  }
}

// ---------- 观测 ----------

async function loadTraces() {
  const limit = $('trace-limit').value;
  const result = $('trace-result').value;
  try {
    const r = await api(`/api/trace?limit=${limit}${result ? `&result=${result}` : ''}`);
    document.querySelector('#tbl-trace tbody').innerHTML = (r.traces || []).map((t) => `
      <tr>
        <td class="muted">${esc((t.time || '').replace('T', ' ').slice(0, 19))}</td>
        <td><code>${esc(t.caller || '—')}</code></td>
        <td>${esc(t.request_model || '')}</td>
        <td>${esc(t.route_path || '')}</td>
        <td>${esc(t.target || '')}</td>
        <td><span class="badge ${t.outcome === 'ok' ? 'ok' : 'prefail'}">${esc(t.outcome)}</span>${t.http_status ? ` <span class="muted">${t.http_status}</span>` : ''}</td>
        <td class="muted">${(t.candidates || []).map((c) =>
          `${esc(c.provider)}/${esc(c.account)}:${esc(c.result)}`).join('<br>')}</td>
        <td>${t.total_ms ?? '—'} ms</td>
      </tr>`).join('') || '<tr><td colspan="8" class="muted">暂无记录</td></tr>';
  } catch (e) {
    toast('trace 加载失败: ' + e.message, true);
  }
}

$('btn-trace-refresh').addEventListener('click', loadTraces);

// ---------- 启动 ----------

loadOverview();
loadSettings();
loadProviders();
loadRouting();
setInterval(loadOverview, 15000);
