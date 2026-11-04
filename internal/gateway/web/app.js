// yardmaster 控制台（2026-11-03 深度重构：Magpie 构造与交互对齐）
// —— 构造：.list/.row 行卡片取代 <table>；.who/.name/.sub 两行式信息层级；
//    应用壳（body 不滚、各视图独立滚动）；单一滑动高亮 .nav-hl；
// —— 交互：status() 状态行反馈（不再用 toast）；Magpie 式模态（含确认框）；
//    骨架屏加载态；服务商客户端查找；胶囊开关；document.hidden 轮询守卫。
// 数据源不变：/api/state、/api/config/*、/api/trace、/api/cost、/api/dispatch/*、
// /api/plugins/*、/api/pricing/sync、/api/reload。写路径五铁律不受影响：
// 每次提交前 fresh GET 构造全文档，经 PUT /api/config/*（服务端严格校验 +
// 写前备份 + 审计）；providers 的 api_key 以脱敏形回传，服务端保留现存密钥。

'use strict';

const $ = (id) => document.getElementById(id);
const q = (sel) => document.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmtTokens = (n) => Number(n || 0).toLocaleString('zh-CN');
const fmtUSD = (n) => '$' + Number(n || 0).toFixed(4);

// ---------- 应用窗口态（M5.5 桌面壳） ----------
// ?app=1：紧凑桌面密度 + 应用标题；浏览器常态不受影响。
if (new URLSearchParams(location.search).get('app') === '1') {
  document.body.classList.add('app');
  document.title = 'Yardmaster';
}
// Windows 字体栈（:root.win）。
if (/Windows/i.test(navigator.userAgent)) {
  document.documentElement.classList.add('win');
}

// ---------- 主题：三态循环 auto→light→dark，localStorage 持久化 ----------
const THEME_KEY = 'ym-theme';
function applyTheme(t) {
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme; // auto=跟随系统
  $('theme-text').textContent = { auto: '跟随系统', light: '亮色', dark: '暗色' }[t] || '跟随系统';
}
applyTheme(localStorage.getItem(THEME_KEY) || 'auto');
function cycleTheme() {
  const cur = localStorage.getItem(THEME_KEY) || 'auto';
  const next = { auto: 'light', light: 'dark', dark: 'auto' }[cur] || 'auto';
  localStorage.setItem(THEME_KEY, next);
  // 明暗互换的颜色过渡（一次性类；reduced-motion 由 CSS 旁路）
  const root = document.documentElement;
  root.classList.add('theming');
  clearTimeout(applyTheme.t);
  applyTheme.t = setTimeout(() => root.classList.remove('theming'), 340);
  applyTheme(next);
}
$('btn-theme').addEventListener('click', cycleTheme);

// 外链守卫（M8-W3）：原生窗口不追外部导航。
document.addEventListener('click', (e) => {
  const a = e.target.closest('a[href]');
  if (!a) return;
  const u = new URL(a.href, location.href);
  if ((u.protocol === 'http:' || u.protocol === 'https:') && u.origin !== location.origin) {
    e.preventDefault();
    status('外链已拦截（原生窗口不导航）：' + u.href, 'err', 5000);
  }
});

// ---------- 访问令牌（M2-W5） ----------
// 会话内令牌（sessionStorage）：API 401 驱动输入模态框并自动重试；
// 单飞共享同一 Promise，所有等待方在令牌到手后各自重试。
let authToken = sessionStorage.getItem('ym-token') || '';

// HTTP 状态 → 可操作文案（Magpie 式错误码映射的裁剪版）
const HTTP_HINT = {
  400: '请求被拒绝',
  403: '没有权限执行此操作',
  404: '资源不存在（可能刚被删除）',
  409: '校验失败，改动未落盘',
  500: '网关内部错误，请查看服务端日志',
  502: '上游服务不可达',
  503: '服务暂不可用，稍后重试',
};

async function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({}, opts.headers,
    authToken ? { 'X-Gw-Token': authToken } : {});
  let r;
  try {
    r = await fetch(path, opts);
  } catch (e) {
    throw new Error('无法连接网关（serve 是否在运行？）');
  }
  if (r.status === 401) {
    await promptToken(); // 取消则 reject 冒泡
    return api(path, opts); // 取到令牌后重试
  }
  const body = await r.json().catch(() => ({}));
  if (!r.ok) {
    const hint = HTTP_HINT[r.status] || `HTTP ${r.status}`;
    throw new Error(body.error ? `${hint}：${body.error}` : hint);
  }
  return body;
}

let tokenPrompt = null;

function promptToken() {
  if (tokenPrompt) return tokenPrompt;
  tokenPrompt = new Promise((resolve, reject) => {
    const m = openDialog({
      title: '需要访问令牌',
      body: `<p>网关已开启令牌校验（settings.json 的 token 字段）。令牌仅存于本会话。</p>
        <div class="frow"><span>令牌</span><input class="field" name="token" type="password" required autofocus></div>`,
      yes: '保存',
      onSubmit: async (form) => {
        const t = new FormData(form).get('token').trim();
        if (!t) throw new Error('令牌不能为空');
        authToken = t;
        sessionStorage.setItem('ym-token', t);
      },
    });
    m.then(resolve, reject).finally(() => { tokenPrompt = null; });
  });
  return tokenPrompt;
}

// ---------- status()：状态行反馈（Magpie 构造；替代 toast） ----------
// ok 默认 3.5s 自动消、err 8s；role=status/alert。
let statusTimer = 0;
function status(msg, kind = '', ms) {
  const s = $('status');
  if (!msg) { s.textContent = ''; s.title = ''; s.className = 'status'; return; }
  s.textContent = msg;
  s.title = msg;
  s.className = 'status ' + kind;
  s.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  clearTimeout(statusTimer);
  statusTimer = setTimeout(() => status(''), ms ?? (kind === 'err' ? 8000 : 3500));
}

// status() 后面加一组小工具：busy 态、失败重试盒、时间格式化。

// busy(btn, on)：按钮进行中反馈——禁用 + 文案暂存恢复（防连点）。
function busy(btn, on, label) {
  if (on) {
    btn.dataset.label = btn.textContent;
    btn.disabled = true;
    if (label) btn.textContent = label;
  } else {
    btn.disabled = false;
    if (btn.dataset.label) btn.textContent = btn.dataset.label;
  }
}

// errBox：加载失败的空状态盒；带重试入口（data-retry 委托见文末）。
const RETRY_MAP = {
  providers: () => { cache.providers = null; loadProviders(); },
  routing: () => loadRouting(),
  traces: () => loadTraces(),
  cost: () => loadCost(),
  pricing: () => loadPricing(),
  dispatch: () => loadDispatchSessions(),
  plugins: () => loadPlugins(),
};
const errBox = (msg, retry) =>
  `<div class="empty-state">${esc(msg)}${retry ? `<div class="retry-line"><button type="button" class="text add" data-retry="${retry}">重试</button></div>` : ''}</div>`;

// fmtTime：决策时间——当天只显时分秒，跨天带月日（跨天 trace 不再误导）。
function fmtTime(t) {
  if (!t) return '';
  const d = new Date(t);
  if (isNaN(d)) return t;
  const hm = d.toTimeString().slice(0, 8);
  if (d.toDateString() === new Date().toDateString()) return hm;
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${hm}`;
}

// ---------- 模态（Magpie .dialog 构造） ----------

// openDialog：表单模态。返回 Promise：提交成功 resolve，取消/关闭 reject。
function openDialog({ title, body, yes = '保存', danger = false, onSubmit }) {
  return new Promise((resolve, reject) => {
    const back = document.createElement('div');
    back.className = 'modal-back';
    back.innerHTML = `<div class="dialog" role="dialog" aria-modal="true" aria-label="${esc(title)}">
      <div class="ehead"><h3>${esc(title)}</h3>
        <button type="button" class="icon dlg-x" aria-label="关闭">
          <svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>
        </button></div>
      <form><div class="ebody">${body}</div>
        <div class="bar"><span class="modal-err"></span>
          <button type="button" class="text dlg-cancel">取消</button>
          <button type="submit" class="${danger ? 'btn-danger' : 'btn-primary'}">${esc(yes)}</button>
        </div></form></div>`;
    $('modal-root').appendChild(back);
    const close = (fn, val) => {
      back.remove();
      document.removeEventListener('keydown', onKey);
      fn(val);
    };
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); close(reject, new Error('已取消')); } };
    document.addEventListener('keydown', onKey);
    back.querySelector('.dlg-x').onclick = () => close(reject, new Error('已取消'));
    back.querySelector('.dlg-cancel').onclick = () => close(reject, new Error('已取消'));
    back.querySelector('form').onsubmit = async (e) => {
      e.preventDefault();
      const errEl = back.querySelector('.modal-err');
      errEl.textContent = '';
      try {
        await onSubmit(back.querySelector('form'));
        close(resolve, null);
      } catch (ex) {
        errEl.textContent = ex.message; // 校验失败不落盘，错误留在表单内
      }
    };
    const f = back.querySelector('input,select,textarea');
    if (f) f.focus();
  });
}

// confirmModal：确认框（替代原生 confirm()）。
function confirmModal(title, text, { yes = '删除', danger = true } = {}) {
  return openDialog({
    title, body: `<p>${text}</p>`, yes, danger,
    onSubmit: async () => {},
  }).then(() => true, () => false);
}

// formModal：编辑表单（旧接口兼容包装：回调式）。
function formModal(title, bodyHTML, onSubmit) {
  openDialog({ title, body: bodyHTML, onSubmit }).catch(() => {});
}

const formVals = (form) => Object.fromEntries(new FormData(form).entries());
const lines = (s) => (s || '').split('\n').map((x) => x.trim()).filter(Boolean);
const compileCheck = (expr) => { try { new RegExp(expr); return null; } catch (e) { return e.message; } };

// ---------- 骨架屏 ----------
function skeleton(el, n = 4) {
  const widths = [42, 30, 36, 24, 33, 27];
  el.innerHTML = Array.from({ length: n }, (_, i) =>
    `<div class="sk-row"><div class="sk-bone" style="width:${widths[i % widths.length]}%"></div></div>`).join('');
}

// ---------- 视图切换（data-tab/hash 机制不变；Magpie 构造的滑动高亮） ----------
const tabsBox = $('tabs');
const navHl = document.createElement('div');
navHl.className = 'nav-hl';
tabsBox.appendChild(navHl);

let view = '';

function placeHl(btn, instant = false) {
  if (!btn) { navHl.classList.remove('ready'); return; }
  const r = btn.getBoundingClientRect();
  const pr = tabsBox.getBoundingClientRect();
  if (instant) navHl.style.transition = 'none';
  navHl.style.height = r.height + 'px';
  navHl.style.transform = `translateY(${r.top - pr.top}px)`;
  if (instant) { navHl.getBoundingClientRect(); navHl.style.transition = ''; }
  navHl.classList.add('ready');
}

// 窗口缩放后行位置变了，高亮形状跟随重定位。
addEventListener('resize', () => placeHl(tabsBox.querySelector('.nav-it.on'), true));

document.querySelectorAll('#tabs .nav-it').forEach((b) => {
  b.addEventListener('click', () => { show(b.dataset.tab); b.blur(); });
  // 悬停时高亮形状跟随（Magpie navTopGo 心智）：离开导航区回到当前页
  b.addEventListener('pointerenter', (e) => { if (e.buttons === 0) placeHl(b); });
});
tabsBox.addEventListener('pointerleave', () => placeHl(tabsBox.querySelector('.nav-it.on')));

function show(name) {
  if (!document.getElementById('page-' + name)) name = 'overview';
  const first = !view;
  view = name;
  document.querySelectorAll('#tabs .nav-it').forEach((b) => {
    const on = b.dataset.tab === name;
    b.classList.toggle('on', on);
    if (on) { b.setAttribute('aria-current', 'page'); placeHl(b, first); }
    else b.removeAttribute('aria-current');
  });
  // 视图切换：支持的引擎走 View Transition（旧页淡出新页淡入，Magpie
  // navMorph 同构思路的轻量版）；首屏/不支持/减少动态时直接切。
  const swapViews = () => {
    document.querySelectorAll('#view-root .view').forEach((p) => {
      p.hidden = p.id !== 'page-' + name;
    });
  };
  if (first || !document.startViewTransition || matchMedia('(prefers-reduced-motion: reduce)').matches) {
    swapViews();
  } else {
    document.startViewTransition(swapViews);
  }
  const title = tabsBox.querySelector(`.nav-it[data-tab="${name}"]`)?.dataset.title || name;
  $('page-title').textContent = title;
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  // 激活即拉数据（观察者打开什么就看什么，Magpie show() 语义）
  if (name === 'overview') loadOverview();
  if (name === 'providers') ensureProviders();
  if (name === 'routing') loadRouting();
  if (name === 'observe') { loadTraces(); loadCost(); loadPricing(); }
  if (name === 'dispatch') { loadDispatchAgents(); loadDispatchTemplates(); loadDispatchSessions(); }
  if (name === 'plugins') loadPlugins();
}

// 视图激活且页面可见才轮询（Magpie 每视图自管轮询的纪律）
const activeAndVisible = (name) => !q('#page-' + name).hidden && !document.hidden;

// ---------- 全局缓存：跨页 join 用 ----------
const cache = { state: null, providers: null, settings: null, rules: null };

async function refreshState() {
  cache.state = await api('/api/state');
  // servedAnew（Magpie 6673171 增量吸收）：页面由旧二进制伺服、服务端已
  // 换新（用户升级并重启 serve）→ 自动重载一次跟上。页面由同一 serve
  // 伺服，重载后 boot 即新版本，无循环风险；dev 态两侧同名不触发。
  if (bootVersion === null) bootVersion = cache.state.version || '';
  else if (!servedReloaded && cache.state.version && cache.state.version !== bootVersion) {
    servedReloaded = true;
    location.reload();
  }
  if (!updInited) { updInited = true; checkUpdate(); } // B1：首次 state 到手即查一次
  return cache.state;
}
let bootVersion = null; // 首次 state 应答的版本（页面伺服版本）
let servedReloaded = false;
let updInited = false;

// ---------- 侧栏网关状态灯 ----------
function renderSideGw(ok) {
  const b = $('side-gw');
  b.hidden = false;
  b.classList.toggle('err', !ok);
  b.classList.toggle('off', false);
  $('side-gw-text').textContent = ok
    ? '网关运行中' + (cache.settings?.listen ? ' · ' + cache.settings.listen : '')
    : '网关不可达';
}

// ---------- 升级提示（B1：Magpie .update 可隐藏角标同构） ----------
// 浏览器端直查 GitHub latest release（CORS 开放；1h localStorage 缓存；
// 失败静默——升级提示是增强信息，不打扰）。dev 态不检查（同托盘
// trayUpdateLabel 语义）。页面伺服版本漂移的自动重载见 refreshState 的
// servedAnew。
const UPD_KEY = 'ym-upd-check'; // {tag, at}：1h TTL
const UPD_DISMISS = 'ym-upd-dismiss'; // 已忽略的 tag（下一个新版才再提示）
const UPD_API = 'https://api.github.com/repos/listlonely/Yardmaster/releases/latest';

// verNewer：分段数值比较（语义化版本三段，忽略前缀 v）。
function verNewer(a, b) {
  const p = (s) => String(s).replace(/^v/, '').split('.').map((x) => parseInt(x, 10) || 0);
  const pa = p(a), pb = p(b);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] || 0) - (pb[i] || 0);
    if (d) return d > 0;
  }
  return false;
}

async function checkUpdate() {
  const cur = cache.state?.version || '';
  if (!cur || cur === 'dev') return;
  // auto 判定依赖 settings（update 三态）；未加载先拉一次（失败则本
  // 轮按仅提示处理，托盘侧 1h 轮询兜底）。off=完全静默（不查 GitHub）。
  if (!cache.settings) { try { cache.settings = await api('/api/config/settings'); } catch { /* 降级 */ } }
  if (cache.settings?.update === 'off') return;
  const cached = JSON.parse(localStorage.getItem(UPD_KEY) || 'null');
  if (cached && Date.now() - cached.at < 3600e3) { offerUpdate(cached.tag, cur); return; }
  try {
    const r = await fetch(UPD_API, { headers: { Accept: 'application/vnd.github+json' } });
    if (!r.ok) return;
    const tag = String((await r.json()).tag_name || '').trim();
    if (!tag) return;
    localStorage.setItem(UPD_KEY, JSON.stringify({ tag, at: Date.now() }));
    offerUpdate(tag, cur);
  } catch { /* 网络失败静默 */ }
}

function offerUpdate(tag, cur) {
  const pill = $('upd-pill');
  if (!pill) return;
  if (!verNewer(tag, cur) || localStorage.getItem(UPD_DISMISS) === tag) { pill.hidden = true; return; }
  $('upd-link-text').textContent = '升级到 ' + tag;
  pill.hidden = false;
  // auto 态（M11-W3 settings.update=auto）：命中新版自动应用内安装，
  // 每标签页生命周期至多一次（失败留 toast，网关侧 1h 轮询会再触发）。
  if (cache.settings?.update === 'auto' && !autoInstallFired) {
    autoInstallFired = true;
    installUpdate();
  }
}
let autoInstallFired = false;

// installUpdate 应用内更新（M11-W3）：POST /api/update/install（下载
// 校验→aside 换侧→spawn 接力→网关重启），重启后页面经 servedAnew 自
// 动跟上新版本。失败恢复 pill 可重试，releases 页手动升级仍可用。
async function installUpdate() {
  const link = $('upd-link');
  if (!link || link.dataset.busy) return;
  link.dataset.busy = '1';
  $('upd-link-text').textContent = '更新中…';
  try {
    await api('/api/update/install', { method: 'POST' });
    $('upd-link-text').textContent = '已更新 · 重启中…';
    status('更新已安装，网关正在重启（控制台将自动重连）', 'ok');
  } catch (e) {
    delete link.dataset.busy;
    $('upd-link-text').textContent = '更新失败（点击重试）';
    status('应用内更新失败：' + e.message + '；可到 releases 页手动升级', 'err');
  }
}

// B1 pill 点击（M11-W3）：应用内安装替代跳转 releases 页（引擎未装配
// 等场景 API 报错落 toast，仍可从错误提示手动进 releases）。
$('upd-link').addEventListener('click', (e) => {
  e.preventDefault();
  installUpdate();
});

$('upd-x').addEventListener('click', (e) => {
  e.preventDefault();
  e.stopPropagation();
  localStorage.setItem(UPD_DISMISS, $('upd-link-text').textContent.replace('升级到 ', ''));
  $('upd-pill').hidden = true;
  status('已忽略该版本提示（下个新版才会再提示）', 'ok');
});
// 1h 复查（页面可见才算；Magpie renderUpdateBadge 同纪律）
setInterval(() => { if (!document.hidden) checkUpdate(); }, 3600e3);

// ---------- 总览 ----------

function statCard(label, value, sub) {
  return `<div class="stat"><div class="stat-v">${value}</div>
    <div class="stat-l">${esc(label)}</div>
    ${sub ? `<div class="stat-s">${esc(sub)}</div>` : ''}</div>`;
}

async function loadOverview() {
  try {
    const s = await refreshState();
    renderSideGw(true);
    $('ov-pill').textContent = '运行中';
    $('ov-pill').className = 'pill ok';
    $('ov-state').textContent =
      `版本 ${s.version} · 已运行 ${s.uptime} · ` +
      `服务商 ${(s.providers || []).length} 个 · 路由组 ${(s.groups || []).length} 个`;
    renderRest(s);
  } catch (e) {
    renderSideGw(false);
    $('ov-pill').textContent = '无法连接';
    $('ov-pill').className = 'pill err';
    $('ov-state').textContent = e.message;
    return;
  }
  // 数据卡与代理链依赖 providers/settings（失败时独立降级，不拖垮状态区）
  try {
    if (!cache.providers) cache.providers = await api('/api/config/providers');
    if (!cache.settings) cache.settings = await api('/api/config/settings');
    renderStatCards();
    renderProxyChain();
    renderSideGw(true);
  } catch (e) {
    $('stat-cards').innerHTML = `<div class="empty-state">数据卡加载失败：${esc(e.message)}</div>`;
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
    statCard('服务商', provs.length, `${provs.filter((p) => !p.disabled).length} 个启用`) +
    statCard('账号', accCount, `${quotaAccs} 个配置额度`) +
    statCard('路由组', (cache.state.groups || []).length) +
    statCard('任务规则', rules) +
    statCard('当日用量', fmtTokens(used), 'tokens（全部账号累计）');
}

// 休眠与额度（行卡片化）
function renderRest(s) {
  const rows = [];
  for (const [k, v] of Object.entries(s.resting || {})) {
    rows.push(`<div class="row">
      <div class="who"><div class="name"><code>${esc(k)}</code></div>
      <div class="sub">休眠至 ${esc((v || '').replace('T', ' '))}</div></div>
      <span class="pill resting">休眠</span></div>`);
  }
  for (const k of (s.exhausted || [])) {
    rows.push(`<div class="row">
      <div class="who"><div class="name"><code>${esc(k)}</code></div>
      <div class="sub">当日额度耗尽，休眠至次日重置</div></div>
      <span class="pill resting">额度耗尽</span></div>`);
  }
  $('ov-rest').innerHTML = rows.join('') ||
    '<div class="row"><div class="who"><div class="name muted2">无休眠中的账号，额度正常</div></div></div>';
}

// 代理链（F3 继承：账号 → 服务商 → 全局 → 系统），行卡片化
function renderProxyChain() {
  const g = cache.settings.global_proxy || '';
  const rows = [`<div class="row">
    <div class="who"><div class="name">全局</div>
    <div class="sub">${g ? `<code>${esc(g)}</code>` : '未设置（跟随系统代理）'}</div></div></div>`];
  const provs = cache.providers.providers || [];
  const special = provs.filter((p) => p.proxy || (p.accounts || []).some((a) => a.proxy));
  if (!special.length) {
    rows.push('<div class="row"><div class="who"><div class="name muted2">无服务商/账号级代理覆写：全部流量走全局链</div></div></div>');
  } else {
    for (const p of special) {
      for (const a of (p.accounts || [])) {
        const eff = a.proxy || p.proxy || g || '系统代理';
        const src = a.proxy ? '账号' : p.proxy ? '服务商' : g ? '全局' : '系统';
        rows.push(`<div class="row">
          <div class="who"><div class="name"><code>${esc(p.id)}/${esc(a.name)}</code></div>
          <div class="sub">生效层：${esc(src)} · <code>${esc(eff)}</code></div></div></div>`);
      }
    }
  }
  $('ov-proxy').innerHTML = rows.join('');
}

// ---------- 设置（总览页 pref 行） ----------

async function loadSettings() {
  try {
    const s = await api('/api/config/settings');
    cache.settings = s;
    $('set-proxy').value = s.global_proxy || '';
    $('set-l1').value = s.l1_classifier || '';
    $('set-l1-timeout').value = s.l1_timeout_ms || '';
    $('set-listen').value = s.listen || '';
    $('set-token').value = ''; // 令牌脱敏返回，留空即保留
    $('set-token-sub').textContent = s.token
      ? `已设置（${s.token}），留空=保留`
      : '留空=保留现有；仅本机使用可留空';
  } catch { /* settings.json 不存在：保持空表单 */ }
}

$('btn-save-settings').addEventListener('click', async () => {
  const b = $('btn-save-settings');
  const body = {
    global_proxy: $('set-proxy').value.trim(),
    l1_classifier: $('set-l1').value.trim(),
    l1_timeout_ms: parseInt($('set-l1-timeout').value, 10) || 0,
    listen: $('set-listen').value.trim(),
    token: $('set-token').value, // 空值由服务端按「保留现有」处理
  };
  busy(b, true, '保存中…');
  try {
    const r = await api('/api/config/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    cache.settings = { ...cache.settings, ...body };
    $('settings-msg').textContent =
      r.effect === 'restart' ? '已保存（重启 serve 后完全生效）' : '已保存并生效';
    status('设置已保存（已写入写前备份）', 'ok');
    renderSideGw(true);
  } catch (e) {
    status('保存失败: ' + e.message, 'err');
  } finally {
    busy(b, false);
  }
});

// reloadConfig：重读 providers.json 并热替换路由快照（顶栏按钮与命令面板共用）。
async function reloadConfig() {
  const r = await api('/api/reload', { method: 'POST' });
  cache.providers = null; // 重载后强制重新拉取配置
  status(`已重载：${r.providers} 个服务商`, 'ok');
  if (!q('#page-overview').hidden) loadOverview();
  if (!q('#page-providers').hidden) loadProviders();
  if (!q('#page-routing').hidden) loadRouting();
}
$('btn-reload').addEventListener('click', async () => {
  const b = $('btn-reload');
  b.classList.add('spin');
  try {
    await reloadConfig();
  } catch (e) {
    status('重载失败: ' + e.message, 'err');
  } finally {
    b.classList.remove('spin');
  }
});

// ---------- 服务商（行卡片构造 + 客户端查找） ----------

// 胶囊开关通用行为（trace/dispatch 自动刷新、插件启停共用）
function bindSwitch(el, onChange) {
  el.addEventListener('click', () => {
    const on = !el.classList.contains('on');
    el.classList.toggle('on', on);
    el.setAttribute('aria-checked', String(on));
    onChange(on);
  });
}
const statusBadge = (st) => `<span class="pill ${esc(st || 'available')}">${esc(st || 'available')}</span>`;

// OAuth 账号过期时间展示（M3-W2）：expires_at 为 Unix 秒，0/缺失=未知。
const oauthExpiryText = (m) => {
  if (!m || !m.expires_at) return '未知';
  return new Date(m.expires_at * 1000).toLocaleString();
};

// usageBar：当日用量进度条（无额度设置时只显示数字）。
function usageBar(a) {
  const used = a.used_tokens || 0;
  const quota = a.quota_tokens_day || 0;
  if (quota <= 0) {
    return `<span>${fmtTokens(used)}</span><span class="muted2">未设额度</span>`;
  }
  const pct = Math.min(100, Math.round((used / quota) * 100));
  const cls = pct >= 90 ? 'danger' : pct >= 70 ? 'warn' : 'ok';
  return `<div class="bar"><div class="bar-fill ${cls}" style="width:${pct}%"></div></div>
    <span>${fmtTokens(used)} / ${fmtTokens(quota)}（${pct}%）</span>`;
}

// effectiveProxy：账号 → 服务商 → 全局 → 系统（F3 继承链），标注生效层。
function effectiveProxy(a, p, global) {
  if (a.proxy) return { v: a.proxy, src: '账号' };
  if (p.proxy) return { v: p.proxy, src: '服务商' };
  if (global) return { v: global, src: '全局' };
  return { v: '系统代理', src: '系统' };
}

const initials = (s) => (String(s || '?').replace(/[^A-Za-z0-9\u4e00-\u9fa5]/g, '').slice(0, 2) || '?').toUpperCase();

function acctRow(p, a, live, resting, global) {
  const key = `${p.id}/${a.name}`;
  const restUntil = resting[key];
  const lv = live[key] || {};
  const view_ = {
    ...a,
    status: lv.status || a.status,
    used_tokens: lv.used_tokens ?? a.used_tokens,
    quota_tokens_day: lv.quota_tokens_day ?? a.quota_tokens_day,
  };
  const eff = effectiveProxy(a, p, global);
  const nameBadges = a.auth === 'oauth'
    ? `<span class="pill oauth">OAuth</span>${a.oauth?.state === 'stale' ? ' <span class="pill resting">待刷新</span>' : ''}`
    : '';
  const credSub = a.auth === 'oauth'
    ? `过期 ${esc(oauthExpiryText(a.oauth))}`
    : `<code>${esc(a.api_key || '（未设置）')}</code>`;
  const liveParts = [];
  if (restUntil) liveParts.push(`休眠至 ${esc(restUntil.replace('T', ' '))}`);
  if (lv.window_status === 'restricted' && (lv.window_reset_at || 0) * 1000 > Date.now()) {
    liveParts.push(`窗口限频 · 重置 ${esc(new Date(lv.window_reset_at * 1000).toLocaleTimeString())}`);
  }
  if (lv.balance_at) liveParts.push(`余额 ¥${Number(lv.balance).toFixed(2)}`);
  if (lv.inflight) liveParts.push(`在途 ${esc(lv.inflight)}`);
  const sub = [credSub, `代理 <code>${esc(eff.v)}</code>（${eff.src}层）`, ...liveParts].join(' · ');
  return `<div class="row acct">
    <div class="who">
      <div class="name">${esc(a.name)} ${nameBadges}${statusBadge(view_.status)}</div>
      <div class="sub">${sub}</div>
    </div>
    <div class="acct-usage">${usageBar(view_)}</div>
    <div class="row-acts">
      <button type="button" class="text quota-btn" data-p="${esc(p.id)}" data-a="${esc(a.name)}">额度</button>
      <button type="button" class="text del del-acct" data-p="${esc(p.id)}" data-a="${esc(a.name)}" title="从 providers.json 移除该账号（写前备份留底）">删除</button>
    </div>
  </div>`;
}

// accFolds：展开状态的 provider id 集合（重绘间保留交互态）。
const accFolds = new Set();
const ACC_VISIBLE = 3; // 超过此数折叠，其余入「展开其余 N 个账号」

function provCard(p, live, resting, global) {
  const accs = p.accounts || [];
  const head = `<div class="row prov-head ${p.disabled ? 'is-off' : ''}">
    <span class="tile" aria-hidden="true">${esc(initials(p.name || p.id))}</span>
    <div class="who">
      <div class="name">${esc(p.name || p.id)} <code>${esc(p.id)}</code>
        <span class="pill">${esc(p.protocol)}</span>
        ${p.disabled ? '<span class="pill disabled">已停用</span>' : ''}</div>
      <div class="sub">${accs.length} 个账号${p.proxy ? ` · 服务商代理 <code>${esc(p.proxy)}</code>` : ''}</div>
    </div>
  </div>`;
  const folded = accs.length > ACC_VISIBLE && !accFolds.has(p.id);
  const visible = folded ? accs.slice(0, ACC_VISIBLE) : accs;
  const hidden = folded ? accs.slice(ACC_VISIBLE) : [];
  const rowOf = (a) => acctRow(p, a, live, resting, global);
  let rows = visible.map(rowOf).join('');
  if (hidden.length) {
    rows += `<div class="acc-fold"><div class="acc-fold-inner">${hidden.map(rowOf).join('')}</div></div>`;
  }
  if (!accs.length) {
    rows = '<div class="row acct"><div class="who"><div class="name muted2">无账号</div></div></div>';
  }
  if (accs.length > ACC_VISIBLE) {
    rows += `<button type="button" class="acc-more" data-fold="${esc(p.id)}" aria-expanded="${!folded}">
      <span>${folded ? `展开其余 ${hidden.length} 个账号` : '收起账号'}</span>
      <svg class="chev" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="m4.5 6.5 3.5 3 3.5-3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
    </button>`;
  }
  const models = (p.models && p.models.length) ? `
    <details class="models-row">
      <summary><svg class="chev" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="m6.5 4.5 3 3.5-3 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
        模型清单（${p.models.length} 个）</summary>
      <div class="models-body"><div class="chips">${p.models.map((m) => `<code>${esc(m.id)}</code>`).join(' ')}</div></div>
    </details>` : '';
  return `<div class="list" style="margin:10px 0">${head}${rows}${models}</div>`;
}

// 客户端查找：按 provider id/名称/账号名过滤（防抖）
let pvFilter = '';
let pvTimer = 0;
$('pv-q').addEventListener('input', () => {
  clearTimeout(pvTimer);
  pvTimer = setTimeout(() => {
    pvFilter = $('pv-q').value.trim().toLowerCase();
    if (cache.providers) renderProviders();
  }, 120);
});

function renderProviders() {
  const f = cache.providers;
  const global = (cache.settings && cache.settings.global_proxy) || '';
  const resting = (cache.state && cache.state.resting) || {};
  const live = {};
  for (const a of ((cache.state && cache.state.accounts) || [])) live[a.key] = a;
  const provs = (f.providers || []).filter((p) => {
    if (!pvFilter) return true;
    const hay = `${p.id} ${p.name || ''} ${(p.accounts || []).map((a) => a.name).join(' ')}`.toLowerCase();
    return hay.includes(pvFilter);
  });
  const accTotal = provs.reduce((n, p) => n + (p.accounts || []).length, 0);
  $('pv-count').textContent = pvFilter
    ? `${provs.length}/${(f.providers || []).length} 个服务商 · ${accTotal} 个账号`
    : `${provs.length} 个服务商 · ${accTotal} 个账号`;
  $('prov-list').innerHTML = provs.map((p) => provCard(p, live, resting, global)).join('');
  $('prov-none').hidden = (f.providers || []).length > 0;
  if ((f.providers || []).length && !provs.length) {
    $('prov-list').innerHTML = '<div class="empty-state">没有匹配的服务商或账号</div>';
    $('prov-none').hidden = true;
  }
}

async function loadProviders() {
  try {
    if (!cache.providers) {
      skeleton($('prov-list'), 4);
      cache.providers = await api('/api/config/providers');
    }
    renderProviders();
  } catch (e) {
    $('prov-list').innerHTML = errBox('加载失败：' + e.message, 'providers');
  }
}

function ensureProviders() {
  if (cache.providers) renderProviders();
  else loadProviders();
}

// ---------- 添加服务商（M9：预设 tile + 表单，走 providers 写路径） ----------
// 与 CLI `provider add` 同语义：id 已存在 = 向其追加账号（账号名冲突拒
// 绝），不存在 = 新建服务商；api_key 新明文经服务端 keepProviderKeys
// 采纳（GET→PUT 往返不破坏现存密钥）。OAuth 订阅类不在此列（授权流在
// 浏览器外，仍走 CLI add-oauth）。

let presetsCache = null;

async function loadPresets() {
  if (presetsCache) return presetsCache;
  try {
    presetsCache = await api('/api/provider-presets');
  } catch {
    presetsCache = []; // 预设拉取失败不阻塞：自定义表单仍可用
  }
  return presetsCache;
}

const PROTOCOL_LABEL = {
  'openai-chat': 'OpenAI Chat',
  'anthropic-messages': 'Anthropic Messages',
  'openai-responses': 'OpenAI Responses',
  'gemini': 'Gemini',
};

async function addProviderModal() {
  const presets = await loadPresets();
  const tiles = [
    ...presets.map((p) => `<button type="button" class="tile-p" data-preset="${esc(p.id)}">
      <b>${esc(p.name)}</b>
      <span>${esc(PROTOCOL_LABEL[p.protocol] || p.protocol)}${p.note ? ` · ${esc(p.note)}` : ''}</span>
    </button>`),
    `<button type="button" class="tile-p custom" data-preset=""><b>自定义</b><span>手动填写全部字段</span></button>`,
  ].join('');
  const formHTML = `
    <div class="tiles" id="ap-tiles">${tiles}</div>
    ${frow('服务商 ID', `<input class="field mono" name="id" placeholder="如 deepseek" required>`)}
    ${frow('显示名', `<input class="field" name="name" placeholder="默认同 ID">`)}
    ${frow('协议', `<select class="field" name="protocol">
      ${Object.entries(PROTOCOL_LABEL).map(([v, l]) => `<option value="${v}">${v}（${l}）</option>`).join('')}
    </select>`)}
    ${frow('Base URL', `<input class="field mono" name="base_url" placeholder="https://api.example.com/v1" required>`, '上游完整前缀；网关按协议追加 /chat/completions、/v1/messages 等')}
    ${frow('API Key', `<input class="field mono" name="api_key" type="password" autocomplete="new-password" placeholder="sk-…（仅存本机 providers.json）" required>`)}
    ${frow('账号名', `<input class="field mono" name="account" value="main" required>`)}
    ${frow('账号代理', `<input class="field mono" name="proxy" placeholder="可选；http(s)/socks5(h)://… 或 direct">`)}
    ${frow('服务商代理', `<input class="field mono" name="provider_proxy" placeholder="可选；账号未配置时继承">`)}
    ${frow('最大并发', `<input class="field" name="max_concurrent" type="number" min="0" step="1" placeholder="0 = 不限">`)}`;
  const dlg = openDialog({
    title: '添加服务商',
    body: formHTML,
    onSubmit: async (form) => {
      const v = formVals(form);
      const id = v.id.trim();
      const accName = v.account.trim() || 'main';
      const key = v.api_key.trim();
      const baseURL = v.base_url.trim();
      if (!id) throw new Error('服务商 ID 不能为空');
      if (!baseURL) throw new Error('Base URL 不能为空');
      if (!key) throw new Error('API Key 不能为空');
      if (parseInt(v.max_concurrent, 10) < 0) throw new Error('最大并发不能为负');
      const r = await putProvidersDoc((cur) => {
        const providers = cur.providers || [];
        const target = providers.find((p) => p.id === id);
        if (target) {
          // 追加账号到已有服务商（CLI add 同语义；账号已存在时拒绝——
          // 换账号名，或用 CLI add -replace 更新凭证）
          if ((target.accounts || []).some((a) => a.name === accName)) {
            throw new Error(`账号 ${id}/${accName} 已存在（换账号名，或用 CLI provider add -replace 更新凭证）`);
          }
          target.accounts = [...(target.accounts || []), buildAccount(v, accName, key)];
          if (v.provider_proxy.trim()) target.proxy = v.provider_proxy.trim();
          if (baseURL && baseURL !== target.base_url) target.base_url = baseURL;
          return { ...cur, providers };
        }
        return { ...cur, providers: [...providers, {
          id, name: v.name.trim() || id,
          protocol: v.protocol,
          base_url: baseURL,
          ...(v.provider_proxy.trim() ? { proxy: v.provider_proxy.trim() } : {}),
          accounts: [buildAccount(v, accName, key)],
        }] };
      });
      status(`服务商已保存：${id}/${accName}（${r.effect === 'immediate' ? '已热生效' : r.effect}，写前备份已留底）`, 'ok');
      afterWrite();
    },
  });
  dlg.catch(() => {}); // 取消/关闭不算错误（吞掉未处理 rejection）
  // 预设 tile 点选 → 预填（CLI add -preset 同语义：显式改动不被覆盖）
  bindTiles($('modal-root'), presets);
}

function buildAccount(v, accName, key) {
  const acc = { name: accName, api_key: key };
  if (parseInt(v.max_concurrent, 10) > 0) acc.max_concurrent = parseInt(v.max_concurrent, 10);
  if (v.proxy.trim()) acc.proxy = v.proxy.trim();
  return acc;
}

// bindTiles 预设点选预填：id/name/protocol/base_url，自定义 tile 清空。
function bindTiles(root, presets) {
  const tiles = root.querySelector('#ap-tiles');
  const form = tiles.closest('form');
  tiles.addEventListener('click', (e) => {
    const t = e.target.closest('.tile-p');
    if (!t) return;
    tiles.querySelectorAll('.tile-p.on').forEach((x) => x.classList.remove('on'));
    t.classList.add('on');
    const p = presets.find((x) => x.id === t.dataset.preset);
    const set = (name, val) => { form.elements[name].value = val; };
    if (!p) { set('id', ''); set('name', ''); set('base_url', ''); return; }
    set('id', p.id);
    set('name', p.name);
    set('protocol', p.protocol);
    set('base_url', p.base_url);
  });
}

async function removeAccount(provID, accName) {
  const ok = await confirmModal(`删除账号 ${provID}/${accName}`,
    '从 providers.json 移除该账号及其密钥（服务端语义：空 api_key=保留，整行移除=删除）。当前文件将先备份到 backup/ 目录。');
  if (!ok) return;
  await putProvidersDoc((cur) => ({
    ...cur,
    providers: (cur.providers || []).map((p) => p.id !== provID ? p : {
      ...p,
      accounts: (p.accounts || []).filter((a) => a.name !== accName),
    }),
  }));
  status(`账号已删除：${provID}/${accName}（写前备份已留底）`, 'ok');
  afterWrite();
}

// ---------- 路由（行卡片构造） ----------

const STRATEGY_LABEL = {
  order: '固定顺序', rotate: '每请求轮换', smart: '按剩余额度',
  usage: '当日用量均衡', weight: '平滑加权轮询',
};

const ruleCond = (r) => {
  const parts = [];
  if (r.intent) parts.push('意图=' + r.intent);
  if (r.regex) parts.push('正则 /' + r.regex + '/');
  if (r.max_tokens) parts.push('≤' + r.max_tokens + ' tokens');
  if (r.caller_key) parts.push('caller=' + r.caller_key);
  return parts.join(' · ') || '（兜底）';
};

// memberValid：组成员引用有效性标注。
function memberValid(m, provs, groups) {
  if ((m || '').startsWith('group/')) {
    const ok = groups.has(m.slice(6));
    return ok ? '' : ' <span class="pill prefail">组不存在</span>';
  }
  const [provID, model] = (m || '').split('/');
  const p = provs.find((x) => x.id === provID);
  if (!p) return ' <span class="pill prefail">服务商不存在</span>';
  if (model && (p.models || []).length && !p.models.some((mm) => mm.id === model)) {
    return ' <span class="pill warn">模型不在清单</span>';
  }
  return '';
}

const listEmpty = (msg) => `<div class="empty-state">${esc(msg)}</div>`;

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

    $('list-groups').innerHTML = (state.groups || []).map((g) => `
      <div class="row">
        <div class="who">
          <div class="name"><code>${esc(g.id)}</code> <span class="pill">${esc(STRATEGY_LABEL[g.strategy] || g.strategy)}</span></div>
          <div class="sub wrap">成员：${(g.members || []).map((m) => `<code>${esc(m)}</code>${memberValid(m, provs, groupIDs)}`).join(' ') || '—'}</div>
        </div>
        <div class="row-acts">
          <button type="button" class="text edit" data-g="${esc(g.id)}">编辑</button>
          <button type="button" class="text del" data-g="${esc(g.id)}">删除</button>
        </div>
      </div>`).join('') || listEmpty('未配置路由组');

    $('list-rules').innerHTML = (rules.rules || []).map((r) => `
      <div class="row">
        <div class="who">
          <div class="name"><code>${esc(r.id)}</code> <span class="muted2">${esc(r.desc || '')}</span></div>
          <div class="sub">${esc(ruleCond(r))}</div>
        </div>
        <code>${esc(r.target)}</code>
        <span class="muted2" title="命中次数">${r.hits || 0} 次</span>
        <div class="row-acts">
          <button type="button" class="text edit" data-r="${esc(r.id)}">编辑</button>
          <button type="button" class="text del" data-r="${esc(r.id)}">删除</button>
        </div>
      </div>`).join('') || listEmpty('未配置规则');

    $('list-keys').innerHTML = (keys.bindings || []).map((b) => `
      <div class="row">
        <div class="who">
          <div class="name"><code>${esc(b.key)}</code></div>
          <div class="sub">默认模型 <code>${esc(b.default_model)}</code></div>
        </div>
      </div>`).join('') || listEmpty('未配置绑定');

    $('list-intents').innerHTML = (intents.intents || []).map((d) => `
      <div class="row">
        <div class="who">
          <div class="name"><code>${esc(d.id)}</code> ${d.label ? esc(d.label) : ''}</div>
          <div class="sub wrap">${esc((d.keywords || []).join('、') || (d.regexes || []).join('；')) || '<span class="muted2">无关键词（仅示例句 few-shot）</span>'}</div>
        </div>
        <span class="muted2">优先级 ${d.priority || 0}</span>
        <div class="row-acts">
          <button type="button" class="text edit" data-i="${esc(d.id)}">编辑</button>
          <button type="button" class="text del" data-i="${esc(d.id)}">删除</button>
        </div>
      </div>`).join('') || listEmpty('无自定义意图（仅内置 L0/L1）');
  } catch (e) {
    status('路由页加载失败: ' + e.message, 'err');
    ['list-groups', 'list-rules', 'list-keys', 'list-intents'].forEach((id) => {
      if (!$(id).innerHTML) $(id).innerHTML = errBox('加载失败: ' + e.message, 'routing');
    });
  }
}

// ---------- 写路径（M2-W3 契约不变）：fresh GET → 修改 → PUT ----------

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
  if (!q('#page-overview').hidden) loadOverview();
  if (!q('#page-providers').hidden) loadProviders();
  if (!q('#page-routing').hidden) loadRouting();
}

// 客户端成员校验（与服务端 validateProviders/validateMember 同规）
function memberClientCheck(m, provs, otherGroupIDs) {
  if (!m) return '成员引用为空';
  if (m.startsWith('group/')) {
    return otherGroupIDs.has(m.slice(6)) ? null : `引用的组 ${m} 不存在`;
  }
  const [provID, model] = m.split('/');
  const p = provs.find((x) => x.id === provID);
  if (!p) return `成员 ${m} 引用的服务商不存在`;
  if (model && (p.models || []).length && !p.models.some((mm) => mm.id === model)) {
    return `模型 ${model} 不在 ${provID} 的清单内`;
  }
  return null;
}

const frow = (label, field, hint) =>
  `<div class="frow"><span>${label}</span>${field}${hint ? `<div class="hint">${hint}</div>` : ''}</div>`;

function groupModal(existing) {
  const isEdit = !!existing;
  formModal(isEdit ? `编辑路由组 ${existing.id}` : '新建路由组',
    frow('组 ID', `<input class="field mono" name="id" value="${esc(existing?.id || '')}" ${isEdit ? 'readonly' : ''} required>`) +
    frow('策略', `<select class="field" name="strategy">
      <option value="order"${existing?.strategy === 'order' ? ' selected' : ''}>order（固定顺序，用尽再换）</option>
      <option value="rotate"${existing?.strategy === 'rotate' ? ' selected' : ''}>rotate（每请求轮换）</option>
      <option value="smart"${existing?.strategy === 'smart' ? ' selected' : ''}>smart（按剩余额度比例）</option>
      <option value="usage"${existing?.strategy === 'usage' ? ' selected' : ''}>usage（当日用量升序，负载均衡）</option>
      <option value="weight"${existing?.strategy === 'weight' ? ' selected' : ''}>weight（平滑加权轮询）</option>
    </select>`) +
    frow('成员', `<textarea class="field mono" name="members" rows="4">${esc((existing?.members || []).join('\n'))}</textarea>`,
      '成员格式 provider/model、provider（取其清单）或 group/&lt;id&gt;，每行一个；顺序即 order 策略的顺序') +
    frow('权重', `<input class="field mono" name="weights" value="${esc(Object.entries(existing?.weights || {}).map(([k, v]) => `${k}=${v}`).join(', '))}" placeholder="键=权重，逗号分隔">`,
      '仅 weight 策略生效，缺省 1；键支持 provider/account · provider/model · provider'),
  async (form) => {
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
    status(`路由组已保存（${r.effect === 'immediate' ? '已热生效' : r.effect}，写前备份已留底）`, 'ok');
    afterWrite();
  });
}

async function deleteGroup(id) {
  const ok = await confirmModal(`删除路由组 ${id}`,
    '引用它的规则/绑定会失去目标。当前文件将先备份到 backup/ 目录，可手工还原。');
  if (!ok) return;
  await putProvidersDoc((cur) => ({ ...cur, groups: (cur.groups || []).filter((g) => g.id !== id) }));
  status('路由组已删除（写前备份已留底）', 'ok');
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
  formModal(isEdit ? `编辑规则 ${existing.id}` : '新建规则',
    frow('规则 ID', `<input class="field mono" name="id" value="${esc(existing?.id || '')}" ${isEdit ? 'readonly' : ''} required>`) +
    frow('说明', `<input class="field" name="desc" value="${esc(existing?.desc || '')}">`) +
    frow('意图条件', `<input class="field mono" name="intent" list="dl-intents" value="${esc(existing?.intent || '')}" placeholder="留空=不限">`) +
    frow('正则条件', `<input class="field mono" name="regex" value="${esc(existing?.regex || '')}" placeholder="对最新用户消息原文匹配；留空=不限">`) +
    frow('token 上限', `<input class="field" name="max_tokens" type="number" min="0" value="${existing?.max_tokens || ''}" placeholder="估算输入 ≤ 此值才命中；留空=不限">`) +
    frow('调用方 key', `<input class="field mono" name="caller_key" value="${esc(existing?.caller_key || '')}" placeholder="如 gw-claude-code；留空=不限">`) +
    frow('目标', `<input class="field mono" name="target" value="${esc(existing?.target || '')}" required placeholder="provider/model · provider · group/&lt;id&gt;">`) +
    intentDatalist(customIntents),
  async (form) => {
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
    status('规则已保存（mtime 热生效，写前备份已留底）', 'ok');
    afterWrite();
  });
}

async function deleteRule(id) {
  const ok = await confirmModal(`删除规则 ${id}`, '当前文件将先备份到 backup/ 目录，可手工还原。');
  if (!ok) return;
  const cur = await api('/api/config/rules');
  await api('/api/config/rules', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ rules: (cur.rules || []).filter((r) => r.id !== id) }),
  });
  status('规则已删除（写前备份已留底）', 'ok');
  afterWrite();
}

function intentModal(existing) {
  const isEdit = !!existing;
  formModal(isEdit ? `编辑意图 ${existing.id}` : '新建自定义意图',
    frow('意图 ID', `<input class="field mono" name="id" value="${esc(existing?.id || '')}" ${isEdit ? 'readonly' : ''} required placeholder="建议命名空间化，如 team/weekly-report">`) +
    frow('展示名', `<input class="field" name="label" value="${esc(existing?.label || '')}">`) +
    frow('优先级', `<input class="field" name="priority" type="number" value="${existing?.priority || 0}" placeholder="越大越先求值">`) +
    frow('关键词', `<input class="field" name="keywords" value="${esc((existing?.keywords || []).join(','))}" placeholder="逗号分隔；大小写不敏感子串匹配">`) +
    frow('正则', `<textarea class="field mono" name="regexes" rows="3">${esc((existing?.regexes || []).join('\n'))}</textarea>`, '每行一条') +
    frow('示例句', `<textarea class="field" name="examples" rows="3">${esc((existing?.examples || []).join('\n'))}</textarea>`,
      '每行一条，≤5；注入 L1 提示词作 few-shot；任务规则按 ID 引用此意图'),
  async (form) => {
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
    status('意图已保存（热生效，写前备份已留底）', 'ok');
    afterWrite();
  });
}

async function deleteIntent(id) {
  const ok = await confirmModal(`删除意图 ${id}`, '引用它的任务规则将不再命中。当前文件将先备份到 backup/ 目录。');
  if (!ok) return;
  const intents = await refreshIntents();
  await api('/api/config/intents', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ intents: intents.filter((d) => d.id !== id) }),
  });
  status('意图已删除（写前备份已留底）', 'ok');
  afterWrite();
}

function quotaModal(provID, accName) {
  const prov = (cache.providers?.providers || []).find((x) => x.id === provID);
  const acc = (prov?.accounts || []).find((x) => x.name === accName);
  formModal(`设置当日额度 · ${provID}/${accName}`,
    frow('额度', `<input class="field" name="quota" type="number" min="0" step="1000" value="${acc?.quota_tokens_day || ''}" placeholder="留空或 0 = 不设额度">`,
      'tokens/日；>0 后 smart 策略按剩余比例排序；跨日自动重置。写入会留备份。'),
  async (form) => {
    const v = formVals(form);
    const quota = parseInt(v.quota, 10) || 0;
    if (quota === 0) {
      const ok = await confirmModal('确认不设额度？', '额度为 0 表示不设额度（smart 将回落 rotate）。', { yes: '确认', danger: false });
      if (!ok) throw new Error('已取消');
    }
    await putProvidersDoc((cur) => {
      for (const p of cur.providers || []) {
        for (const a of p.accounts || []) {
          if (p.id === provID && a.name === accName) a.quota_tokens_day = quota;
        }
      }
      return cur;
    });
    status(`额度已保存：${provID}/${accName} → ${quota || '不设'}`, 'ok');
    afterWrite();
  });
}

// 路由页操作列事件委托（编辑/删除）+ 服务商页额度入口
$('list-groups').addEventListener('click', (e) => {
  const g = e.target.dataset.g;
  if (!g) return;
  const cur = (cache.state.groups || []).find((x) => x.id === g);
  if (e.target.classList.contains('edit')) groupModal(cur);
  if (e.target.classList.contains('del')) deleteGroup(g);
});
$('list-rules').addEventListener('click', async (e) => {
  const id = e.target.dataset.r;
  if (!id) return;
  const cur = await api('/api/config/rules');
  const rule = (cur.rules || []).find((x) => x.id === id);
  if (e.target.classList.contains('edit')) ruleModal(rule);
  if (e.target.classList.contains('del')) deleteRule(id);
});
$('list-intents').addEventListener('click', async (e) => {
  const id = e.target.dataset.i;
  if (!id) return;
  const intents = await refreshIntents();
  const def = intents.find((x) => x.id === id);
  if (e.target.classList.contains('edit')) intentModal(def);
  if (e.target.classList.contains('del')) deleteIntent(id);
});
$('prov-list').addEventListener('click', (e) => {
  const btn = e.target.closest('.quota-btn');
  if (btn) { quotaModal(btn.dataset.p, btn.dataset.a); return; }
  const del = e.target.closest('.del-acct');
  if (del) { removeAccount(del.dataset.p, del.dataset.a); return; }
  // 账号折叠：纯 class 切换（grid-rows 动画播得出来；不重绘）
  const more = e.target.closest('.acc-more');
  if (more) {
    const fold = more.parentElement.querySelector('.acc-fold');
    if (!fold) return;
    const open = fold.classList.toggle('open');
    more.setAttribute('aria-expanded', String(open));
    more.querySelector('span').textContent = open ? '收起账号' : `展开其余 ${fold.querySelectorAll('.row').length} 个账号`;
    if (open) accFolds.add(more.dataset.fold);
    else accFolds.delete(more.dataset.fold);
  }
});
$('btn-add-provider').addEventListener('click', addProviderModal);
$('btn-add-group').addEventListener('click', () => groupModal(null));
$('btn-add-rule').addEventListener('click', () => ruleModal(null));
$('btn-add-intent').addEventListener('click', () => intentModal(null));

// ---------- 观测：决策追溯（行卡片 + 展开详情） ----------

// traceDetail：行展开的候选链与判定详情（F9 失败链呈现）。
function traceDetail(t) {
  const metas = [];
  metas.push(`<span><b>路径</b> ${esc(t.route_path || '—')}</span>`);
  if (t.intent) metas.push(`<span><b>意图</b> ${esc(t.intent)}</span>`);
  if (t.rule) metas.push(`<span><b>规则</b> ${esc(t.rule)}</span>`);
  if (t.served) metas.push(`<span><b>实际服务</b> <code>${esc(t.served)}</code>（${esc(t.served_model || '')}）</span>`);
  if (t.ttfb_ms != null) metas.push(`<span><b>首字节</b> ${t.ttfb_ms}ms</span>`);
  if (t.usage) metas.push(`<span><b>token</b> ${fmtTokens(t.usage.prompt)}+${fmtTokens(t.usage.completion)}</span>`);
  let cand = '<div class="muted2" style="font-size:11.5px;padding:2px 0">无候选尝试记录。</div>';
  if ((t.candidates || []).length) {
    cand = `<div class="cand head"><span>#</span><span>候选</span><span>上游模型</span><span>结果</span><span>状态</span><span>耗时</span><span>原因</span></div>` +
      t.candidates.map((c, i) => `
        <div class="cand">
          <span>${i + 1}</span>
          <span><code>${esc(c.provider)}/${esc(c.account)}</code></span>
          <span>${esc(c.model || '—')}</span>
          <span><span class="pill ${c.result === 'ok' ? 'ok' : c.result === 'quota' || c.result === 'cred' ? 'prefail' : ''}" style="padding:0 7px">${esc(c.result)}</span></span>
          <span>${c.status || '—'}</span>
          <span>${c.dur_ms ?? '—'}ms</span>
          <span class="muted2" title="${esc(c.proxy || '直连')}">${esc(c.err || (c.proxy ? '出口 ' + c.proxy : '直连'))}</span>
        </div>`).join('');
  }
  return `<div class="td-line">${metas.join('')}</div>${cand}`;
}

async function loadTraces() {
  const limit = $('trace-limit').value;
  const result = $('trace-result').value;
  const agent = $('trace-agent').value.trim();
  const box = $('list-trace');
  try {
    const r = await api(`/api/trace?limit=${limit}${result ? `&result=${result}` : ''}${agent ? `&agent=${encodeURIComponent(agent)}` : ''}`);
    // 轮询保态：自动刷新不重置已展开的详情行（Magpie redraw 保交互态）
    const openIdx = new Set([...box.querySelectorAll('.trace-row.open')].map((x) => x.dataset.idx));
    box.innerHTML = (r.traces || []).map((t, i) => {
      const open = openIdx.has(String(i));
      return `
      <div class="row hoverable trace-row ${open ? 'open' : ''}" data-idx="${i}">
        <svg class="trace-arrow" viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="m6.5 4.5 3 3.5-3 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
        <div class="who">
          <div class="name">${esc(t.request_model || '—')} <span class="pill ${t.outcome === 'ok' ? 'ok' : 'prefail'}">${esc(t.outcome)}</span>${t.http_status ? ` <span class="muted2">${t.http_status}</span>` : ''}</div>
          <div class="sub"><code>${esc(t.caller || '—')}</code> · ${esc(t.route_path || '—')} → ${esc(t.target || '—')} · ${esc(fmtTime(t.time))} · 候选 ${(t.candidates || []).length}</div>
        </div>
        <span class="muted2" style="font-variant-numeric:tabular-nums;flex:none">${t.total_ms ?? '—'} ms</span>
      </div>
      <div class="trace-detail" ${open ? '' : 'hidden'}>${traceDetail(t)}</div>`;
    }).join('')
      || listEmpty('暂无决策记录（网关收到请求后这里会出现完整候选链）');
  } catch (e) {
    box.innerHTML = errBox('trace 加载失败: ' + e.message, 'traces');
  }
}

$('list-trace').addEventListener('click', (e) => {
  const row = e.target.closest('.trace-row');
  if (!row) return;
  const detail = row.nextElementSibling;
  if (detail && detail.classList.contains('trace-detail')) {
    const open = detail.hidden;
    detail.hidden = !open;
    row.classList.toggle('open', open);
  }
});

$('btn-trace-refresh').addEventListener('click', loadTraces);
for (const id of ['trace-result', 'trace-limit']) {
  $(id).addEventListener('change', loadTraces);
}
let traceAgentTimer = 0;
$('trace-agent').addEventListener('input', () => {
  clearTimeout(traceAgentTimer);
  traceAgentTimer = setTimeout(loadTraces, 300);
});

// 自动刷新（胶囊开关）：仅观测页激活且页面可见时拉取。
let traceTimer = null;
bindSwitch($('trace-auto'), (on) => {
  clearInterval(traceTimer);
  if (on) traceTimer = setInterval(() => {
    if (activeAndVisible('observe')) loadTraces();
  }, 30000);
});

// ---------- 观测：成本报告（grid 行卡片） ----------

const COST_COLS = 'minmax(0,1.3fr) 60px 84px 84px minmax(96px,1fr) minmax(88px,.9fr)';

function costList(m, label, fx) {
  const keys = Object.keys(m || {}).sort((a, b) => m[b].cost - m[a].cost);
  if (!keys.length) return `<div class="list"><div class="empty-state">窗口内无 ${esc(label)} 数据</div></div>`;
  let rows = keys.map((k) => {
    const b = m[k];
    const ps = b.prices || [];
    const price = !ps.length ? '<span class="muted2">—</span>'
      : ps.length === 1 ? `${ps[0].in_per_m}/${ps[0].out_per_m}`
      : `<span title="${esc(ps.map((p) => p.key).join(', '))}">${ps.length} 种单价</span>`;
    return `<div class="grid-row">
      <span><code>${esc(k)}</code></span>
      <span class="num">${b.calls}</span>
      <span class="num">${fmtTokens(b.input_tokens)}</span>
      <span class="num">${fmtTokens(b.output_tokens)}</span>
      <span class="num">${price}</span>
      <span class="num">${fmtUSD(b.cost)}${fx > 0 ? ` <span class="muted2">≈¥${(b.cost * fx).toFixed(2)}</span>` : ''}</span>
    </div>`;
  }).join('');
  // 合计行（≥2 行才有聚合价值）
  if (keys.length >= 2) {
    const tot = keys.reduce((s, k) => ({
      calls: s.calls + (m[k].calls || 0),
      in: s.in + (m[k].input_tokens || 0),
      out: s.out + (m[k].output_tokens || 0),
      cost: s.cost + (m[k].cost || 0),
    }), { calls: 0, in: 0, out: 0, cost: 0 });
    rows += `<div class="grid-row total">
      <span>合计</span>
      <span class="num">${tot.calls}</span>
      <span class="num">${fmtTokens(tot.in)}</span>
      <span class="num">${fmtTokens(tot.out)}</span>
      <span class="num muted2">—</span>
      <span class="num">${fmtUSD(tot.cost)}${fx > 0 ? ` <span class="muted2">≈¥${(tot.cost * fx).toFixed(2)}</span>` : ''}</span>
    </div>`;
  }
  return `<div class="list scrollx"><div class="grid-head" style="--cols:${COST_COLS}">
    <span>${esc(label)}</span><span class="num">调用</span><span class="num">输入 tok</span>
    <span class="num">输出 tok</span><span class="num">单价 in/out</span><span class="num">成本</span>
  </div>${rows}</div>`;
}

async function loadCost() {
  const days = $('cost-days').value;
  try {
    const rep = await api(`/api/cost?days=${days}`);
    const fx = rep.fx_usd_cny || 0;
    const total = Object.values(rep.by_account || {}).reduce((s, b) => s + (b.cost || 0), 0);
    $('cost-body').innerHTML =
      costList(rep.by_account, '按账号', fx) +
      costList(rep.by_model, '按模型', fx) +
      (rep.unpriced > 0
        ? `<p class="foot-note">未计价调用 ${rep.unpriced} 次（单价表无对应价格键；失败请求不计费）</p>`
        : '') +
      (fx > 0
        ? `<p class="foot-note">合计 ${fmtUSD(total)} ≈¥${(total * fx).toFixed(2)}（汇率 ${fx}，仅展示换算）</p>`
        : `<p class="foot-note">合计 ${fmtUSD(total)}</p>`) +
      `<p class="foot-note">窗口 ${esc(rep.from)} ~ ${esc(rep.to)}</p>`;
  } catch (e) {
    $('cost-body').innerHTML = errBox('加载失败：' + e.message, 'cost');
  }
}

$('cost-days').addEventListener('change', loadCost);

// ---------- 观测：模型单价表 ----------

async function loadPricing() {
  const fx = (cache.settings && cache.settings.fx_usd_cny) || 0;
  try {
    const f = await api('/api/config/pricing');
    $('list-pricing').innerHTML = (f.prices || []).map((p) => `
      <div class="row">
        <div class="who">
          <div class="name"><code>${esc(p.model)}</code></div>
          <div class="sub">输入 ${p.input_per_m}${fx > 0 ? ` ≈¥${(p.input_per_m * fx).toFixed(2)}` : ''} · 输出 ${p.output_per_m}${fx > 0 ? ` ≈¥${(p.output_per_m * fx).toFixed(2)}` : ''}${p.source ? ` · ${esc(p.source)}` : ''}</div>
        </div>
        <div class="row-acts">
          <button type="button" class="text edit" data-price="${esc(p.model)}">编辑</button>
          <button type="button" class="text del" data-price="${esc(p.model)}">删除</button>
        </div>
      </div>`).join('') || listEmpty('单价表为空（未配置的模型不计入成本；可用「同步 models.dev 单价」填充）');
  } catch (e) {
    $('list-pricing').innerHTML = errBox('加载失败: ' + e.message, 'pricing');
  }
}

function priceModal(existing) {
  const isEdit = !!existing;
  formModal(isEdit ? `编辑单价 ${existing.model}` : '新建单价',
    frow('价格键', `<input class="field mono" name="model" value="${esc(existing?.model || '')}" required placeholder="provider/model（精确）或裸 model">`) +
    frow('输入', `<input class="field" name="input" type="number" min="0" step="0.000001" value="${existing?.input_per_m ?? ''}" required>`, '美元 / 1M tokens') +
    frow('输出', `<input class="field" name="output" type="number" min="0" step="0.000001" value="${existing?.output_per_m ?? ''}" required>`, '美元 / 1M tokens；成本报告按此价 × trace 中的 token 用量计算；精确键优先于裸 model 键'),
  async (form) => {
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
    status('单价已保存（写前备份已留底）', 'ok');
    loadPricing();
    loadCost();
  });
}

async function deletePrice(model) {
  const ok = await confirmModal(`删除单价 ${model}`, '该模型的调用将计入「未计价」。当前文件将先备份到 backup/ 目录。');
  if (!ok) return;
  const cur = await api('/api/config/pricing');
  await api('/api/config/pricing', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ prices: (cur.prices || []).filter((p) => p.model !== model) }),
  });
  status('单价已删除（写前备份已留底）', 'ok');
  loadPricing();
  loadCost();
}

$('list-pricing').addEventListener('click', async (e) => {
  const model = e.target.dataset.price;
  if (!model) return;
  const cur = await api('/api/config/pricing');
  const entry = (cur.prices || []).find((p) => p.model === model);
  if (e.target.classList.contains('edit')) priceModal(entry);
  if (e.target.classList.contains('del')) deletePrice(model);
});
$('btn-add-price').addEventListener('click', () => priceModal(null));

// models.dev 单价同步（M7-W1）：目录拉取可达数秒-数十秒，进行中反馈防重复点击。
$('btn-price-sync').addEventListener('click', async () => {
  const btn = $('btn-price-sync');
  btn.disabled = true;
  const orig = btn.textContent;
  btn.textContent = '⟳ 同步中…';
  try {
    const r = await api('/api/pricing/sync', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    const s = r.sync || {};
    status(`同步完成：新增 ${(s.added || []).length}、刷新 ${(s.updated || []).length}、保护跳过 ${(s.skipped || []).length}` +
      (s.no_price ? `（目录无成本模型 ${s.no_price} 个未填充）` : ''), 'ok', 8000);
    loadPricing();
    loadCost();
  } catch (e) {
    status('同步失败: ' + e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = orig;
  }
});

// ---------- 派发（M5-W5：任务→Agent headless） ----------

let dispSelected = null; // 当前查看输出的会话 ID
let dispEvView = true;   // 事件视图优先（M6-W3；解析失败/raw 退原文）
let dispLast = null;     // 最近一次详情缓存（视图切换复用）
let dispTemplates = [];  // 任务模板清单（M6-W4）

async function loadDispatchAgents() {
  const sel = $('disp-agent');
  const cur = sel.value;
  try {
    const r = await api('/api/dispatch/agents');
    sel.innerHTML = '<option value="">自动（按匹配规则）</option>' +
      (r.agents || []).map((a) => `<option value="${esc(a.id)}">${esc(a.id)}</option>`).join('');
    if (cur) sel.value = cur;
  } catch (e) {
    sel.innerHTML = '<option value="">派发不可用</option>';
    status('派发配置加载失败: ' + e.message, 'err');
  }
}

async function loadDispatchTemplates() {
  try {
    const r = await api('/api/dispatch/templates');
    dispTemplates = r.templates || [];
    const row = $('disp-tmplrow'), sel = $('disp-template');
    row.hidden = dispTemplates.length === 0;
    sel.innerHTML = '<option value="">自由任务（直接填写上方文本）</option>' +
      dispTemplates.map((t) => `<option value="${esc(t.name)}">${esc(t.name)}</option>`).join('');
  } catch (e) { /* 模板不可用不阻塞派发页（fail-open 观测增强） */ }
}

// dispRenderParams 选中模板后按占位名生成参数输入；切回自由任务还原。
function dispRenderParams() {
  const name = $('disp-template').value;
  const row = $('disp-params'), ta = $('disp-task');
  const t = dispTemplates.find((x) => x.name === name);
  if (!t) {
    row.hidden = true;
    row.innerHTML = '';
    ta.disabled = false;
    return;
  }
  ta.disabled = true;
  ta.value = `（模板 ${t.name}）${t.task}`;
  const names = t.params || [];
  row.hidden = false;
  row.innerHTML = `<div class="who"><div class="name">参数</div></div>
    <div class="chips">${names.map((n) =>
      `<input class="field disp-param" data-name="${esc(n)}" placeholder="{${esc(n)}}" style="width:11rem">`).join('')}</div>`;
}

const DISP_BADGE = { done: 'ok', failed: 'prefail', running: 'running', cancelled: 'cancelled' };

async function loadDispatchSessions() {
  try {
    const r = await api('/api/dispatch/sessions');
    const all = r.sessions || [];
    dispSyncAgentFilter(all);
    const rows = dispFilterSessions(all).map((s) => {
      const dur = s.result ? `${s.result.duration_ms} ms` : '…';
      const acts = [];
      if (s.status === 'running') acts.push(`<button type="button" class="text disp-cancel" data-id="${esc(s.id)}">取消</button>`);
      if (s.status === 'failed') acts.push(`<button type="button" class="text disp-redispatch" data-id="${esc(s.id)}" title="以留存任务摘要与相同 agent/model 提交全新会话">重派</button>`);
      return `<div class="row hoverable disp-session ${dispSelected === s.id ? 'sel' : ''}" data-id="${esc(s.id)}">
        <div class="who">
          <div class="name">${esc((s.task || '').slice(0, 60)) || '（无任务文本）'}</div>
          <div class="sub"><code>${esc(s.id)}</code> · ${esc(s.agent)} · ${esc(fmtTime(s.started_at))} · ${esc(dur)}${s.result && s.result.outcome ? ` · ${esc(s.result.outcome)}` : ''}</div>
        </div>
        <span class="pill ${DISP_BADGE[s.status] || ''}">${esc(s.status)}</span>
        ${acts.length ? `<div class="row-acts">${acts.join('')}</div>` : ''}
      </div>`;
    }).join('');
    $('list-dispatch').innerHTML = rows || listEmpty('暂无会话（提交第一个任务后这里会出现会话与事件流）');
    if (dispSelected) await showDispatchOutput(dispSelected);
    else hideDispatchOutput();
  } catch (e) {
    status('会话加载失败: ' + e.message, 'err');
    if (!$('list-dispatch').innerHTML) $('list-dispatch').innerHTML = errBox('会话加载失败: ' + e.message, 'dispatch');
  }
}

function hideDispatchOutput() {
  $('card-disp-output').hidden = true;
}

// dispSyncAgentFilter 按历史会话的 agent 集合刷新筛选项（保留当前选择）。
function dispSyncAgentFilter(all) {
  const sel = $('disp-f-agent');
  const cur = sel.value;
  const ids = [...new Set(all.map((s) => s.agent))].sort();
  sel.innerHTML = '<option value="">全部 Agent</option>' +
    ids.map((a) => `<option value="${esc(a)}">${esc(a)}</option>`).join('');
  sel.value = ids.includes(cur) ? cur : '';
}

// dispFilterSessions 客户端筛选（环形 ≤256+恢复条目）：agent / 状态 / 时间窗。
function dispFilterSessions(all) {
  const agent = $('disp-f-agent').value;
  const status_ = $('disp-f-status').value;
  const win = $('disp-f-time').value;
  let list = all;
  if (agent) list = list.filter((s) => s.agent === agent);
  if (status_) list = list.filter((s) => s.status === status_);
  if (win) {
    const cut = Date.now() - ({ '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3 }[win] || 0);
    list = list.filter((s) => new Date(s.started_at).getTime() >= cut);
  }
  return list;
}

async function showDispatchOutput(id) {
  dispSelected = id;
  try {
    const d = await api('/api/dispatch/sessions/' + encodeURIComponent(id));
    dispLast = d;
    dispRenderOutput(d);
  } catch (e) {
    hideDispatchOutput();
    dispSelected = null;
    dispLast = null;
  }
}

// dispRenderOutput：jsonl 会话默认事件视图；raw 与非 jsonl 回退原文。
function dispRenderOutput(d) {
  const pre = $('disp-output');
  const evs = d.events || [];
  const hasEv = evs.length > 0 && dispEvView;
  $('card-disp-output').hidden = false;
  $('disp-viewhint').textContent = hasEv ? '事件视图' : '原文视图';
  $('disp-viewid').textContent = dispSelected || '';
  $('disp-evtoggle').textContent = hasEv ? '切换原文' : '切换事件';
  pre.classList.toggle('ev', hasEv);
  if (hasEv) {
    pre.innerHTML = evs.map(dispEventHTML).join('');
  } else {
    pre.textContent = d.output || '（暂无输出）';
  }
  pre.scrollTop = pre.scrollHeight;
}

// dispEventHTML 单事件渲染：init 灰、text 正文、tool 折叠、result 终态。
function dispEventHTML(ev) {
  const t = esc(ev.text || '');
  switch (ev.kind) {
    case 'init':
      return `<div class="ev ev-init">init${ev.subtype ? ' · ' + esc(ev.subtype) : ''}</div>`;
    case 'text':
      return `<div class="ev ev-text">${t}</div>`;
    case 'tool':
      return `<details class="ev ev-tool"><summary>tool · ${esc(ev.tool || '?')}<span class="ev-sum">${t}</span></summary><pre>${t}</pre></details>`;
    case 'toolresult':
      return `<div class="ev ev-toolres">tool-result · ${t}</div>`;
    case 'result':
      return `<div class="ev ev-result">result${ev.subtype ? ' · ' + esc(ev.subtype) : ''} — ${t}</div>`;
    default:
      return `<div class="ev ev-raw">${t}</div>`;
  }
}

$('list-dispatch').addEventListener('click', async (e) => {
  const cancel = e.target.closest('.disp-cancel');
  const redis = e.target.closest('.disp-redispatch');
  const row = e.target.closest('.disp-session');
  if (cancel) {
    e.stopPropagation();
    try {
      await api('/api/dispatch/sessions/' + encodeURIComponent(cancel.dataset.id) + '/cancel', { method: 'POST' });
      status('已请求取消 ' + cancel.dataset.id, 'ok');
      loadDispatchSessions();
    } catch (ex) {
      status('取消失败: ' + ex.message, 'err');
    }
    return;
  }
  if (redis) {
    e.stopPropagation();
    try {
      const r = await api('/api/dispatch/sessions/' + encodeURIComponent(redis.dataset.id) + '/redispatch', { method: 'POST' });
      status(`已重派 → ${r.session.id}（原会话 ${r.redispatched_from} 留档不变）`, 'ok');
      dispSelected = r.session.id;
      loadDispatchSessions();
    } catch (ex) {
      status('重派失败: ' + ex.message, 'err');
    }
    return;
  }
  if (row) showDispatchOutput(row.dataset.id);
});

['disp-f-agent', 'disp-f-status', 'disp-f-time'].forEach((id) => $(id).addEventListener('change', loadDispatchSessions));

$('btn-dispatch-run').addEventListener('click', async () => {
  const b = $('btn-dispatch-run');
  const tmplName = $('disp-template').value;
  const body = { agent: $('disp-agent').value, model: $('disp-model').value.trim() };
  if (tmplName) {
    // 模板形态：{template, params}（缺参客户端先拦，服务端仍 fail-closed）。
    const params = {};
    let missing = [];
    document.querySelectorAll('#disp-params .disp-param').forEach((inp) => {
      const v = inp.value.trim();
      if (v) params[inp.dataset.name] = v;
      else missing.push(inp.dataset.name);
    });
    if (missing.length) { status('缺少参数: ' + missing.join(', '), 'err'); return; }
    body.template = tmplName;
    body.params = params;
  } else {
    body.task = $('disp-task').value.trim();
    if (!body.task) { status('任务文本不能为空', 'err'); return; }
  }
  busy(b, true, '派发中…');
  try {
    const r = await api('/api/dispatch/run', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    status(`已派发 → ${r.session.agent}（${r.session.id}${r.intent ? '，意图 ' + r.intent : ''}）`, 'ok');
    if (!tmplName) $('disp-task').value = '';
    dispSelected = r.session.id;
    loadDispatchSessions();
  } catch (e) {
    status('派发失败: ' + e.message, 'err');
  } finally {
    busy(b, false);
  }
});

$('disp-template').addEventListener('change', dispRenderParams);
$('btn-disp-refresh').addEventListener('click', loadDispatchSessions);
$('disp-evtoggle').addEventListener('click', () => {
  dispEvView = !dispEvView;
  if (dispLast) dispRenderOutput(dispLast);
});
// 复制/导出（M6-W5）：原文尾部（text/jsonl 原样）。
$('disp-copy').addEventListener('click', async () => {
  if (!dispLast) return;
  try {
    await navigator.clipboard.writeText(dispLast.output || '');
    status('已复制输出尾部', 'ok');
  } catch (e) {
    status('复制失败: ' + e.message, 'err');
  }
});
// 导出走 fetch→blob→对象 URL（blob 下载无导航副作用）。
$('disp-export').addEventListener('click', async () => {
  if (!dispSelected) return;
  try {
    const r = await fetch('/api/dispatch/sessions/' + encodeURIComponent(dispSelected) + '/export');
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const a = document.createElement('a');
    a.href = URL.createObjectURL(await r.blob());
    a.download = 'dispatch-' + dispSelected + ((dispLast && dispLast.output_mode === 'jsonl') ? '.jsonl' : '.txt');
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(a.href);
    status('已导出输出尾部', 'ok');
  } catch (e) {
    status('导出失败: ' + e.message, 'err');
  }
});
setInterval(() => {
  if (activeAndVisible('dispatch') && $('disp-auto').classList.contains('on')) {
    loadDispatchSessions();
  }
}, 5000);

// ---------- 插件（M7-W5：子进程插件管理面） ----------

function pluginBadge(st) {
  const cls = { running: 'ok', starting: 'resting', backoff: 'resting', failed: 'prefail', stopped: 'disabled' }[st] || '';
  return `<span class="pill ${cls}">${esc(st)}</span>`;
}

async function loadPlugins() {
  try {
    const r = await api('/api/plugins');
    $('list-plugins').innerHTML = (r.plugins || []).map((p) => {
      const granted = (p.granted || []).map((c) => `<code>${esc(c)}</code>`).join(' ') || '<span class="muted2">无</span>';
      const drops = p.dropped_events ? ` · <span class="pill prefail" style="padding:0 7px">丢 ${p.dropped_events}</span>` : '';
      return `<div class="row">
        <div class="who">
          <div class="name"><code>${esc(p.id)}</code></div>
          <div class="sub wrap">能力 ${granted} · 崩溃(1h) ${p.crash_1h || 0}${drops}</div>
        </div>
        ${pluginBadge(p.status)}
        <button type="button" class="lib-switch ${p.enabled ? 'on' : ''}" data-plug="toggle" data-id="${esc(p.id)}" role="switch" aria-checked="${p.enabled}" aria-label="启停 ${esc(p.id)}"><i></i></button>
        <button type="button" class="text" data-plug="log" data-id="${esc(p.id)}">日志</button>
      </div>`;
    }).join('') || listEmpty('未配置插件（plugins.json）');
  } catch (e) {
    $('list-plugins').innerHTML = errBox('插件清单加载失败: ' + e.message, 'plugins');
  }
}

$('list-plugins').addEventListener('click', async (e) => {
  const sw = e.target.closest('.lib-switch[data-plug]');
  const logBtn = e.target.closest('button[data-plug="log"]');
  const id = (sw || logBtn)?.dataset.id;
  if (!id) return;
  if (logBtn) {
    try {
      const r = await api('/api/plugins');
      const p = (r.plugins || []).find((x) => x.id === id);
      $('plugin-log-id').textContent = id;
      $('plugin-log').textContent = (p && p.log_tail && p.log_tail.length) ? p.log_tail.join('\n') : '（无输出）';
      $('card-plugin-log').hidden = false;
    } catch (err) {
      status('日志加载失败: ' + err.message, 'err');
    }
    return;
  }
  // 启停：立即翻转视觉态（乐观），失败回弹
  const wasOn = sw.classList.contains('on');
  sw.disabled = true;
  try {
    await api(`/api/plugins/${encodeURIComponent(id)}/${wasOn ? 'disable' : 'enable'}`, { method: 'POST' });
    status(wasOn ? '已停用' : '已启用（热生效）', 'ok');
    await loadPlugins();
  } catch (err) {
    status('操作失败: ' + err.message, 'err');
    sw.disabled = false;
  }
});
$('btn-plugins-refresh').addEventListener('click', loadPlugins);
$('plugin-log-close').addEventListener('click', () => { $('card-plugin-log').hidden = true; });

// ---------- 失败重试委托（errBox 的 data-retry） ----------
document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-retry]');
  if (!btn) return;
  (RETRY_MAP[btn.dataset.retry] || (() => {}))();
});

// ---------- 命令面板（A1：Ctrl+K；Magpie .pop 构造裁剪版） ----------
// 视图跳转 + 高频动作；↑↓ 选择 / Enter 执行 / Esc 关闭。零新增 API 面
// （动作全部复用既有处理器）。
async function probeQuotas() {
  status('余额探测中…');
  try {
    const r = await api('/api/quota/probe', { method: 'POST' });
    const ps = r.probes || [];
    if (!ps.length) { status('无账号配置余额探测（quota_probe）', '', 5000); return; }
    const fail = ps.filter((p) => p.error);
    status(`余额探测完成：${ps.length - fail.length}/${ps.length} 成功` +
      (fail.length ? `（失败：${fail.map((p) => p.provider + '/' + p.account).join('、')}）` : ''),
      fail.length ? 'warn' : 'ok', 6000);
    if (!q('#page-overview').hidden) loadOverview();
  } catch (e) {
    status('余额探测失败: ' + e.message, 'err');
  }
}

const cmdk = { open: false, items: [], filtered: [], active: 0, back: null, cleanup: null };

function cmdkRegistry() {
  const views = [...document.querySelectorAll('#tabs .nav-it')].map((b) => ({
    label: b.dataset.title, hint: '切换视图', kw: 'view ' + b.dataset.tab,
    run: () => show(b.dataset.tab),
  }));
  return views.concat([
    { label: '切换主题', hint: '跟随系统 → 亮 → 暗 循环', kw: 'theme 主题 亮色 暗色',
      run: cycleTheme },
    { label: '重载配置', hint: '重读 providers.json 并热替换路由快照', kw: 'reload 配置 重载 provider 服务商',
      run: reloadConfig },
    { label: '余额探测', hint: '立即轮询全部账号余额/额度', kw: 'probe quota 余额 额度 探测',
      run: probeQuotas },
    { label: '添加服务商', hint: '从厂商预设新建上游（或追加账号）', kw: 'add provider 新增 供应商 服务商 预设',
      run: () => { show('providers'); addProviderModal(); } },
  ]);
}

function openCmdk() {
  if (cmdk.open || q('.modal-back')) return; // 已开或表单模态在前时不叠加
  cmdk.open = true;
  const back = document.createElement('div');
  back.className = 'modal-back cmdk-back';
  back.innerHTML = `<div class="dialog cmdk" role="dialog" aria-modal="true" aria-label="命令面板">
    <div class="cmdk-head">
      <svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true"><circle cx="7" cy="7" r="4.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m10.5 10.5 3 3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
      <input id="cmdk-q" type="text" placeholder="输入命令或视图名…" autocomplete="off" spellcheck="false">
    </div>
    <div class="cmdk-list" id="cmdk-list" role="listbox" aria-label="命令列表"></div>
    <div class="cmdk-foot"><span><kbd>↑</kbd><kbd>↓</kbd> 选择</span><span><kbd>Enter</kbd> 执行</span><span><kbd>Esc</kbd> 关闭</span></div>
  </div>`;
  $('modal-root').appendChild(back);
  cmdk.back = back;
  cmdk.items = cmdkRegistry();
  cmdk.filtered = cmdk.items;
  cmdk.active = 0;
  renderCmdk();
  const input = back.querySelector('#cmdk-q');
  input.addEventListener('input', () => {
    const s = input.value.trim().toLowerCase();
    cmdk.filtered = !s ? cmdk.items.slice() : cmdk.items.filter((it) =>
      (it.label + ' ' + it.kw + ' ' + it.hint).toLowerCase().includes(s));
    cmdk.active = 0;
    renderCmdk();
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      cmdk.active = Math.min(cmdk.active + 1, cmdk.filtered.length - 1);
      renderCmdk();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      cmdk.active = Math.max(cmdk.active - 1, 0);
      renderCmdk();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      runCmdk();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      closeCmdk();
    }
  });
  // 焦点移出输入框后 Esc 仍可关闭（document 级，关闭时清理）
  const onKey = (e) => {
    if (e.key === 'Escape' && document.activeElement !== input) { e.preventDefault(); closeCmdk(); }
  };
  document.addEventListener('keydown', onKey);
  cmdk.cleanup = () => document.removeEventListener('keydown', onKey);
  back.addEventListener('mousedown', (e) => { if (e.target === back) closeCmdk(); });
  // 鼠标 hover 同步高亮行（键盘/指针共用同一 active 态）
  back.querySelector('#cmdk-list').addEventListener('mousemove', (e) => {
    const it = e.target.closest('.cmdk-it');
    if (!it) return;
    const i = Number(it.dataset.i);
    if (i !== cmdk.active) { cmdk.active = i; renderCmdk(); }
  });
  input.focus();
}

function renderCmdk() {
  const list = $('cmdk-list');
  if (!list) return;
  if (!cmdk.filtered.length) {
    list.innerHTML = '<div class="cmdk-empty">无匹配命令</div>';
    return;
  }
  list.innerHTML = cmdk.filtered.map((it, i) =>
    `<div class="cmdk-it${i === cmdk.active ? ' on' : ''}" role="option" aria-selected="${i === cmdk.active}" data-i="${i}">
      <div class="who"><div class="name">${esc(it.label)}</div><div class="sub">${esc(it.hint)}</div></div>
    </div>`).join('');
  list.querySelector('.cmdk-it.on')?.scrollIntoView({ block: 'nearest' });
}

function runCmdk() {
  const it = cmdk.filtered[cmdk.active];
  if (!it) return;
  closeCmdk();
  it.run();
}

function closeCmdk() {
  if (!cmdk.open) return;
  cmdk.open = false;
  cmdk.cleanup?.();
  cmdk.back?.remove();
  cmdk.back = null;
}

$('modal-root').addEventListener('click', (e) => {
  if (!cmdk.open) return;
  const it = e.target.closest('.cmdk-it');
  if (!it) return;
  cmdk.active = Number(it.dataset.i);
  renderCmdk();
  runCmdk();
});
$('btn-cmdk').addEventListener('click', openCmdk);
document.addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault();
    if (cmdk.open) closeCmdk(); else openCmdk();
  }
});

// ---------- `/` 聚焦当前视图的查找框（Magpie 键盘文化） ----------
document.addEventListener('keydown', (e) => {
  if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return;
  const tag = document.activeElement?.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
  const target = { providers: 'pv-q', observe: 'trace-agent' }[view];
  const inp = target && $(target);
  if (inp && !inp.closest('.view').hidden) {
    e.preventDefault();
    inp.focus();
    inp.select?.();
  }
});

// ---------- 启动（hash 恢复 + 首屏骨架 + 轮询守卫） ----------

show((location.hash || '').replace('#', '') || 'overview');
loadSettings();

// 总览 15s 轮询（仅总览激活且页面可见）
setInterval(() => {
  if (activeAndVisible('overview')) loadOverview();
}, 15000);
