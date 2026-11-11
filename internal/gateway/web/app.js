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
      body: `<p>网关已开启令牌校验（legacy 单令牌或任一命名令牌存在即启用）。令牌仅存于本会话。</p>
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
        <div class="dlg-bar"><span class="modal-err"></span>
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
  const title = tabsBox.querySelector(`.nav-it[data-tab="${name}"]`)?.dataset.title ||
    ({ settings: '设置' }[name] || name);
  $('page-title').textContent = title;
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  // 激活即拉数据（观察者打开什么就看什么，Magpie show() 语义）
  if (name === 'overview') loadOverview();
  if (name === 'providers') ensureProviders();
  if (name === 'routing') loadRouting();
  if (name === 'observe') { loadTraces(); loadCost(); loadPricing(); }
  if (name === 'agents' || name === 'dispatch') { loadDispatchAgents(); loadDispatchTemplates(); }
  if (name === 'dispatch') loadDispatchSessions();
  if (name === 'plugins') loadPlugins();
  if (name === 'settings') loadSettings();
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

// gwURL：网关对外地址（页面自身来源即网关地址，含端口最准确）
const gwURL = () => location.origin;

// listenPending：监听配置与当前服务地址不一致（改过 listen 但 serve 未
// 重启）时返回待生效的 host:port，否则返回 null。listen 空=缺省
// 127.0.0.1:8317（DefaultAddr）；PUT 校验保证非空时为 host:port 形。
// 设置页保存提示与令牌弹框共用——「对不上」的根源是配置值（待重启）
// 与服务值（location.host）双轨，必须在 UI 层点明。
function listenPending() {
  const cfg = (cache.settings?.listen || '127.0.0.1:8317').toLowerCase();
  return cfg === location.host.toLowerCase() ? null : cfg;
}

async function loadOverview() {
  try {
    const s = await refreshState();
    renderSideGw(true);
    $('gw-url').textContent = gwURL() + '/v1'; // API 访问链接写全（OpenAI 兼容基址，Trae「API 地址」栏直填）
    $('ov-pill').textContent = '运行中';
    $('ov-pill').className = 'pill ok';
    $('hero').classList.remove('err');
    const provs = cache.providers ? (cache.providers.providers || []) : [];
    const accs = provs.reduce((n, p) => n + (p.accounts || []).length, 0);
    $('ov-state').textContent =
      `版本 ${s.version} · 已运行 ${s.uptime} · 服务商 ${provs.length || '…'} 个（${accs ? accs + ' 个账号' : '…'}）`;
    renderApiLinks();
    renderRest(s);
  } catch (e) {
    renderSideGw(false);
    $('ov-pill').textContent = '无法连接';
    $('ov-pill').className = 'pill err';
    $('hero').classList.add('err');
    $('ov-state').textContent = e.message;
    renderGuide();
    return;
  }
  // 数据卡、令牌栏与代理链依赖 providers/settings（失败时独立降级，不拖垮状态区）
  try {
    if (!cache.providers) cache.providers = await api('/api/config/providers');
    if (!cache.settings) cache.settings = await api('/api/config/settings');
    if (!cache.rules) cache.rules = await api('/api/config/rules');
    renderStatCards();
    renderOvTokens();
    renderProxyChain();
    renderSideGw(true);
  } catch (e) {
    $('stat-cards').innerHTML = `<div class="empty-state">数据卡加载失败：${esc(e.message)}</div>`;
    const tok = $('ov-tokens');
    if (tok) tok.innerHTML = `<div class="row"><div class="who"><div class="name muted2">令牌列表加载失败：${esc(e.message)}</div></div></div>`;
  }
  renderGuide();
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
  // 意图分配统计（融合视图）：意图规则数 + 兜底（无条件规则）= 已配置链
  const rules = (cache.rules && cache.rules.rules) || [];
  const intentRules = rules.filter((r) => r.intent && r.target).length;
  const hasFallback = rules.some((r) => r.target && !r.intent && !r.regex && !r.max_tokens && !r.caller_key);
  const configured = intentRules + (hasFallback ? 1 : 0);
  $('stat-cards').innerHTML =
    statCard('服务商', provs.length, `${provs.filter((p) => !p.disabled).length} 个启用`) +
    statCard('账号', accCount, `${quotaAccs} 个配置额度`) +
    statCard('意图分配', configured, configured ? '在「路由」页编辑' : '尚未配置调用链') +
    statCard('当日用量', fmtTokens(used), 'tokens（全部账号累计）');
}

// hero API 端点速览（首页写全三种格式接入地址；端点均已实测可用）。
// Gemini 端点含链首模型名（tokenSampleModel，与令牌弹窗同源）。
function renderApiLinks() {
  const el = $('ov-api-links');
  if (!el) return;
  const u = gwURL();
  const m = tokenSampleModel(cache.state);
  const eps = [
    ['OpenAI Chat', u + '/v1/chat/completions'],
    ['Anthropic Messages', u + '/v1/messages'],
    ['Gemini', u + '/v1beta/models/' + (m || '{模型ID}') + ':generateContent'],
  ];
  el.innerHTML = eps.map(([tag, ep]) =>
    '<div class="api-line"><span class="api-tag">' + esc(tag) + '</span><code>' + esc(ep) + '</code>' +
    '<button type="button" class="text add" data-apicopy="' + esc(ep) + '">复制</button></div>').join('');
}

// 首页访问令牌栏（掩码只读展示；生成/轮换/删除在设置页，行尾「去配置」跳转）
function renderOvTokens() {
  const el = $('ov-tokens');
  if (!el) return;
  const toks = (cache.settings && cache.settings.tokens) || [];
  el.innerHTML = toks.map((t) => `<div class="row">
    <div class="who">
      <div class="name">${esc(t.name)} <code>${esc(t.mask || '')}</code></div>
      <div class="sub">创建于 ${esc(t.created || '—')}</div></div>
    </div>`).join('') ||
    '<div class="row"><div class="who"><div class="name muted2">尚无令牌——点右上「去配置」生成一枚发给客户端</div></div></div>';
}

// 跳设置页并滚动到锚点区（视图切换动画后定位，260ms 与快捷动作同拍）
function gotoSettings(anchor) {
  show('settings');
  setTimeout(() => {
    const el = q(anchor);
    if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, 260);
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

// ---------- 设置（独立页，2026-11 批2 从总览拆出） ----------

async function loadSettings() {
  try {
    const s = await api('/api/config/settings');
    cache.settings = s;
    $('set-proxy').value = s.global_proxy || '';
    $('set-l1').value = s.l1_classifier || '';
    $('set-l1-timeout').value = s.l1_timeout_ms || '';
    $('set-listen').value = s.listen || '';
    $('set-update').value = s.update || 'check';
    $('set-close').value = s.close_action || 'hide';
    $('set-fx').value = s.fx_usd_cny > 0 ? s.fx_usd_cny : '';
    $('set-notify').classList.toggle('on', s.notify !== false); // 指针语义：nil=开
    $('set-notify').setAttribute('aria-checked', String(s.notify !== false));
    $('set-autostart').classList.toggle('on', s.autostart === true); // 指针语义：nil=关
    $('set-autostart').setAttribute('aria-checked', String(s.autostart === true));
    renderTokens(s.tokens || []);
  } catch { /* settings.json 不存在：保持空表单 */ }
}

// ---------- 通用偏好（批2 界面收尾：通知/更新/汇率走 settings；色板本地） ----------

bindSwitch($('set-notify'), () => {}); // 态随表单保存（无即时副作用）
bindSwitch($('set-autostart'), () => {}); // 态随表单保存（保存后网关钩子写注册表）
$('set-colorblind').addEventListener('click', () => {
  const on = !$('set-colorblind').classList.contains('on');
  setColorblind(on);
});

function setColorblind(on) {
  if (on) document.documentElement.setAttribute('data-colorblind', '1');
  else document.documentElement.removeAttribute('data-colorblind');
  localStorage.setItem('ym-colorblind', on ? '1' : '0');
  $('set-colorblind').classList.toggle('on', on);
  $('set-colorblind').setAttribute('aria-checked', String(on));
}
setColorblind(localStorage.getItem('ym-colorblind') === '1'); // 启动即应用

$('btn-save-settings').addEventListener('click', async () => {
  const b = $('btn-save-settings');
  const body = {
    global_proxy: $('set-proxy').value.trim(),
    l1_classifier: $('set-l1').value.trim(),
    l1_timeout_ms: parseInt($('set-l1-timeout').value, 10) || 0,
    listen: $('set-listen').value.trim(),
    notify: $('set-notify').classList.contains('on'),
    autostart: $('set-autostart').classList.contains('on'),
    update: $('set-update').value,
    close_action: $('set-close').value,
    fx_usd_cny: parseFloat($('set-fx').value) || 0, // 0=保留现值（服务端语义）
  };
  busy(b, true, '保存中…');
  try {
    const r = await api('/api/config/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    cache.settings = { ...cache.settings, ...body };
    // listen 待重启时点明双轨：当前服务地址（页面来源）与重启后将生效
    // 的配置地址——否则令牌弹框/引导的地址与监听字段「对不上」。
    const pending = listenPending();
    $('settings-msg').textContent =
      r.effect === 'restart'
        ? pending
          ? '已保存（重启 serve 后生效：监听地址将变为 ' + pending + '，当前仍为 ' + location.host + '）'
          : '已保存（重启 serve 后完全生效）'
        : '已保存并生效';
    status('设置已保存（已写入写前备份）', 'ok');
    renderSideGw(true);
  } catch (e) {
    status('保存失败: ' + e.message, 'err');
  } finally {
    busy(b, false);
  }
});

// ---------- 访问令牌（批2 多令牌 · 命名归属；plain 仅生成/轮换时可见） ----------

// copyText：剪贴板写入 + 状态反馈（令牌弹框/引导复制共用）。clipboard
// API 失败（权限/失焦）时 textarea + execCommand 兜底（原型同构）。
async function copyText(text, okMsg) {
  try {
    await navigator.clipboard.writeText(text);
    status(okMsg || '已复制', 'ok');
    return true;
  } catch {
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.cssText = 'position:fixed;opacity:0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      if (!ok) throw new Error('execCommand 复制失败');
      status(okMsg || '已复制', 'ok');
      return true;
    } catch (e2) {
      status('复制失败: ' + e2.message, 'err');
      return false;
    }
  }
}

function renderTokens(toks) {
  const list = $('list-tokens');
  if (!list) return; // 设置页未挂载（HTML 兜底）
  list.innerHTML = toks.map((t) =>
    `<div class="row"><div class="who">
      <div class="name">${esc(t.name)} <code>${esc(t.mask)}</code></div>
      <div class="sub">创建于 ${esc(t.created || '—')}</div></div>
      <div class="row-acts" style="opacity:1">
        <button type="button" class="text" data-ttok="rotate" data-id="${esc(t.id)}">轮换</button>
        <button type="button" class="text del" data-ttok="del" data-id="${esc(t.id)}">删除</button>
      </div></div>`).join('') || listEmpty('尚无令牌——生成一枚发给客户端（如 Trae、claude-code）');
  const sub = $('tokens-sub');
  if (sub) sub.textContent = toks.length
    ? `共 ${toks.length} 枚令牌 · 按客户端隔离，泄露时轮换该枚即可，其余不受影响`
    : '';
}

// 示例模型名（令牌弹窗「添加方案」）：取 fallback 链首成员——未命中
// 意图的请求都兜底到这条链，对任何客户端都成立；无 fallback 组退用
// 第一个有成员的组；全网无组时返回 ''（文案退回「选链上的模型」）。
// members 是 provider/model@account 复合格式（/api/state StateGroup），
// 客户端模型 ID 要纯模型名：去 @account 尾、去首段 provider 前缀
// （模型名本身可含 /，如 Qwen/Qwen3.8-27B，故只剥第一个 /）。
function tokenSampleModel(state) {
  const gs = (state && state.groups) || [];
  const pick = gs.find((g) => String(g.id || '').includes('fallback') && (g.members || []).length)
    || gs.find((g) => (g.members || []).length);
  if (!pick) return '';
  const noAcc = String(pick.members[0]).split('@')[0];
  const i = noAcc.indexOf('/');
  return i >= 0 ? noAcc.slice(i + 1) : noAcc;
}

// 生成/轮换后的弹框：网关地址 + 令牌明文，两项各自可复制。监听配置
// 待重启时追加提示——弹窗地址（页面来源=当前服务地址）与设置页监听
// 字段（待重启配置值）「对不上」的点明处。
// 添加方案（2026-11-09）：三种 API 格式（OpenAI Chat / Anthropic
// Messages / Gemini）各给完整端点，均可一键复制；端点实测三者皆通。
// Trae 等客户端按基址填写时去掉端点尾巴即可（OpenAI → /v1，
// Anthropic → 根地址）。模型 ID 统一填 fallback 链首命名——请求里
// 的模型名只是占位，意图规则命中会改写实际路由（gateway.go 判定链）。
function tokenCreatedDialog(name, plain) {
  const url = gwURL();
  const pending = listenPending();
  const pendingNote = pending
    ? '<p class="muted2">注意：监听地址已改为 <code>' + esc(pending) + '</code>，重启 serve 后网关地址将变为 <code>http://' + esc(pending) + '</code>——上列地址在重启前仍有效。</p>'
    : '';
  const m = tokenSampleModel(cache.state);
  const modelTip = m
    ? '模型 ID 均填 <code>' + esc(m) + '</code>（fallback 链首；意图规则会接管实际路由）'
    : '模型 ID 选链上的模型';
  const fmtRow = (label, endpoint) =>
    '<div class="frow"><span>' + esc(label) + '</span><div class="oauth-cmd" style="margin:0"><code>' + esc(endpoint) + '</code>' +
    '<button type="button" class="text add" data-tcopy="' + esc(endpoint) + '">复制</button></div></div>';
  const back = openDialog({
    title: `令牌已就绪 · ${name}`,
    body:
      '<p>请立即复制保存——落盘只存哈希，关闭后只显示掩码。把下面两项填进客户端即可接入网关。</p>' +
      '<div class="frow"><span>网关地址</span><div class="oauth-cmd" style="margin:0"><code>' + esc(url) + '</code>' +
      '<button type="button" class="text add" data-tcopy="' + esc(url) + '">复制</button></div></div>' +
      '<div class="frow"><span>访问令牌</span><div class="oauth-cmd" style="margin:0"><code>' + esc(plain) + '</code>' +
      '<button type="button" class="text add" data-tcopy="' + esc(plain) + '">复制</button></div></div>' +
      pendingNote +
      '<p class="muted2">添加方案（三种 API 格式任选其一，API Key 均为上方令牌，' + modelTip + '）：</p>' +
      fmtRow('OpenAI Chat', url + '/v1/chat/completions') +
      fmtRow('Anthropic Messages', url + '/v1/messages') +
      fmtRow('Gemini', url + '/v1beta/models/' + (m || '{模型ID}') + ':generateContent') +
      '<p class="muted2">按基址填写的客户端（如 Trae 自定义模型）：OpenAI 格式填 <code>' + esc(url) + '/v1</code>，Anthropic 格式填 <code>' + esc(url) + '</code>，Gemini 格式填 <code>' + esc(url) + '/v1beta</code>（客户端自动补全路径）。</p>',
    yes: '完成',
    onSubmit: async () => {},
  }).catch(() => {});
  void back;
  q('#modal-root .dialog:last-child').addEventListener('click', (e) => {
    const b = e.target.closest('[data-tcopy]');
    if (b) copyText(b.dataset.tcopy, '已复制');
  });
}

$('btn-token-gen').addEventListener('click', () => {
  openDialog({
    title: '生成访问令牌',
    body: frow('归属（发给谁）', '<input class="field" name="tname" required placeholder="如 Trae · 笔记本 / claude-code · 台式机">', '按客户端隔离：泄露时轮换这一枚即可，其余令牌不受影响'),
    yes: '生成',
    onSubmit: async (form) => {
      const name = formVals(form).tname.trim();
      if (!name) throw new Error('归属名不能为空');
      const r = await api('/api/tokens', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      });
      status('令牌已生成：' + name + '（写前备份已留底）', 'ok');
      loadSettings(); // 刷新列表（含旧条目）
      setTimeout(() => tokenCreatedDialog(name, r.plain), 60);
    },
  }).catch(() => {});
});

$('list-tokens').addEventListener('click', async (e) => {
  const b = e.target.closest('[data-ttok]');
  if (!b) return;
  const id = b.dataset.id;
  const name = b.closest('.row')?.querySelector('.name')?.childNodes[0]?.textContent?.trim() || id;
  if (b.dataset.ttok === 'rotate') {
    const ok = await confirmModal(`轮换「${name}」的令牌？`, '旧令牌即刻失效——该客户端需要换成新令牌，其他令牌不受影响。', { yes: '轮换', danger: true });
    if (!ok) return;
    try {
      const r = await api(`/api/tokens/${encodeURIComponent(id)}/rotate`, { method: 'POST' });
      status('令牌已轮换：' + name, 'ok');
      loadSettings();
      setTimeout(() => tokenCreatedDialog(name, r.plain), 60);
    } catch (err) {
      status('轮换失败: ' + err.message, 'err');
    }
    return;
  }
  if (b.dataset.ttok === 'del') {
    const ok = await confirmModal(`删除「${name}」的令牌？`, '该客户端将无法访问网关，需删除其配置或重新发令牌。', { yes: '删除', danger: true });
    if (!ok) return;
    try {
      await api(`/api/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' });
      status('令牌已删除：' + name, 'ok');
      loadSettings();
    } catch (err) {
      status('删除失败: ' + err.message, 'err');
    }
  }
});

// ---------- 三步上手引导（首页驾驶舱；状态持久化 localStorage） ----------

const guide = {
  get dismissed() { return localStorage.getItem('ym-guide-dismissed') === '1'; },
  set dismissed(v) { v ? localStorage.setItem('ym-guide-dismissed', '1') : localStorage.removeItem('ym-guide-dismissed'); },
  get wired() { return localStorage.getItem('ym-guide-wired') === '1'; },
  set wired(v) { v ? localStorage.setItem('ym-guide-wired', '1') : localStorage.removeItem('ym-guide-wired'); },
};

function gstep(n, title, desc, done, actionHTML) {
  return '<div class="gstep' + (done ? ' done' : '') + '">' +
    '<span class="gnum">' + (done ? '✓' : n) + '</span>' +
    '<div><h4>' + esc(title) + '</h4><p>' + desc + '</p>' +
    (done ? '<span class="gtick">已完成</span>' : '<div class="gact">' + (actionHTML || '') + '</div>') +
    '</div></div>';
}

function renderGuide() {
  const g = $('guide');
  if (!g) return;
  const provs = cache.providers ? (cache.providers.providers || []) : [];
  const done1 = provs.length > 0;
  const done2 = guide.wired;
  const done3 = !!cache.state && (cache.state.uptime || '') !== '';
  if ((done1 && done2 && done3) || guide.dismissed) { g.hidden = true; return; }
  g.hidden = false;
  const url = gwURL();
  const snip =
    '# Claude Code（环境变量）\n' +
    'ANTHROPIC_BASE_URL=' + url + '\n' +
    'ANTHROPIC_AUTH_TOKEN=<网关令牌；未设令牌可留空>\n\n' +
    '# Trae（设置 → 模型 → 添加模型 · OpenAI 兼容）\n' +
    'API 地址 ' + url + '/v1 · API Key <网关令牌，未设可任意> · 模型选链上的模型\n\n' +
    '# 或用 CLI 一键接线（自动改写配置，可回滚）：\n' +
    'yardmaster agents wire claude-code -model yardmaster-main';
  g.innerHTML =
    '<div class="guide-head"><b>三步完成接线</b>' +
    '<button type="button" class="icon" id="guide-dismiss" title="隐藏引导（可在命令面板重新打开）" aria-label="隐藏引导">' +
    '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>' +
    '</button></div>' +
    '<div class="gsteps">' +
    gstep(1, '添加第一个服务商', 'Key 类：粘贴 Key 检测模型即可；订阅类：先在插件页启用适配，再按指引 CLI 授权导入账号。', done1,
      done1 ? '' : '<button type="button" class="text add" id="guide-add-pv">添加服务商</button>') +
    gstep(2, '把 Agent 指向网关', '复制网关地址写进 Agent 配置；或用 CLI 一键接线。', done2,
      done2 ? '' : '<button type="button" class="text add" id="guide-copy-url">复制网关地址</button>' +
        '<details class="snip-d"><summary>接线示例</summary><pre class="snip">' + esc(snip) + '</pre></details>') +
    gstep(3, '发出第一个请求', '在任意 Agent 里发一句话；观测页会出现完整路由决策。', done3,
      done3 ? '' : '<button type="button" class="text add" id="guide-go-observe">打开观测</button>' +
        '<button type="button" class="text add" id="guide-go-dispatch">派发一个任务试试</button>') +
    '</div>';
  $('guide-dismiss').addEventListener('click', () => { guide.dismissed = true; renderGuide(); });
  $('guide-add-pv')?.addEventListener('click', () => addProviderModal());
  $('guide-copy-url')?.addEventListener('click', async () => {
    if (await copyText(gwURL(), '已复制网关地址：' + gwURL() + '（粘到 Agent 配置里）')) {
      guide.wired = true;
      renderGuide();
    }
  });
  $('guide-go-observe')?.addEventListener('click', () => show('observe'));
  $('guide-go-dispatch')?.addEventListener('click', () => { show('dispatch'); setTimeout(() => $('disp-task').focus(), 260); });
}

// reloadConfig：重读 providers.json 并热替换路由快照（顶栏按钮与命令面板共用）。
async function reloadConfig() {
  const r = await api('/api/reload', { method: 'POST' });
  cache.providers = null; // 重载后强制重新拉取配置
  status(`已重载：${r.providers} 个服务商`, 'ok');
  if (!q('#page-overview').hidden) loadOverview();
  if (!q('#page-providers').hidden) loadProviders();
  if (!q('#page-routing').hidden) loadRouting();
}

// ---------- 首页驾驶舱快捷动作（批2：hero 四动作 + 复制地址点亮引导） ----------

$('btn-copy-url').addEventListener('click', async () => {
  const apiURL = gwURL() + '/v1';
  if (await copyText(apiURL, '已复制 API 地址：' + apiURL + '（粘到客户端「API 地址」栏）')) {
    guide.wired = true;
    renderGuide();
  }
});
// 首页令牌/代理栏「去配置」→ 设置页对应区块；hero API 速览端点复制
$('ov-tok-goto').addEventListener('click', () => gotoSettings('#sec-tokens'));
$('ov-proxy-goto').addEventListener('click', () => gotoSettings('#sec-proxy'));
$('ov-api-links').addEventListener('click', (e) => {
  const b = e.target.closest('[data-apicopy]');
  if (b) copyText(b.dataset.apicopy, '已复制');
});
$('qa-dispatch').addEventListener('click', () => { show('dispatch'); setTimeout(() => $('disp-task').focus(), 260); });
$('qa-pv-add').addEventListener('click', () => { show('providers'); addProviderModal(); });
$('qa-probe').addEventListener('click', () => probeQuotas());
$('qa-reload').addEventListener('click', async () => {
  try { await reloadConfig(); } catch (e) { status('重载失败: ' + e.message, 'err'); }
});
$('btn-settings').addEventListener('click', () => show('settings'));
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
      <button type="button" class="text um-btn" data-p="${esc(p.id)}" data-a="${esc(a.name)}" title="该账号近 7 天按模型用量（成本报告同源）">明细</button>
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

// ---------- 添加服务商（2026-11 定稿：无订阅预设 + 检测模型 + 模型勾选） ----------
// 订阅类适配由社区插件提供（插件页先启用），此处只添加 API Key 类。
// 「检测并获取模型」走 POST /api/provider/probe（服务端按协议请求上游
// /models，api_key 仅进内存）。写入语义与 CLI add 同：id 已存在=追加
// 备用 Key（同名账号拒绝），api_key 经服务端 keepProviderKeys 采纳。

let presetsCache = null;

async function loadPresets() {
  if (presetsCache) return presetsCache;
  try {
    presetsCache = await api('/api/provider-presets');
  } catch {
    presetsCache = []; // 预设拉取失败不阻塞：手动接入仍可用
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
  const presets = (await loadPresets()).filter((p) => p.kind !== 'oauth');
  const tiles = presets.map((p) =>
    `<button type="button" class="tile-p" data-preset="${esc(p.id)}">
      <b>${esc(p.name)}</b>
      <span>${esc(PROTOCOL_LABEL[p.protocol] || p.protocol)}${p.note ? ` · ${esc(p.note)}` : ''}</span>
    </button>`).join('');
  const body =
    `<div class="ap-subhint"><span>Claude / Codex / Gemini 订阅的适配由社区插件提供：先到「插件」页启用对应订阅适配，并在那里添加账号。此处只添加 API Key 类服务商。</span>` +
    `<button type="button" class="text add" data-act-go-plugins>去插件页</button></div>` +
    `<div class="tiles" id="ap-tiles">${tiles}</div>` +
    `<div class="after-list" style="margin:0 2px 8px"><button type="button" class="text add" id="ap-custom">手动接入（预设外的中转 / 自建网关）</button></div>` +
    `<div class="ap-note" id="ap-note">选一个预设 → 粘贴 API Key → 检测 → 勾选要启用的模型 → 添加；预设外的上游用「手动接入」。</div>` +
    `<div id="ap-keywrap">` +
    frow('API Key', `<div class="key-probe"><input class="field mono" name="api_key" type="password" autocomplete="new-password" placeholder="sk-…（仅存本机 providers.json）" required>` +
      `<button type="button" class="text add" id="ap-probe">检测并获取模型</button></div>`) +
    `</div>` +
    `<div class="ap-models" id="ap-models" hidden></div>` +
    `<div id="ap-netwrap">` +
    frow('网络', `<div class="net-pick"><select class="field" name="netmode">` +
      `<option value="inherit">跟随全局代理</option>` +
      `<option value="direct">直连（不走代理）</option>` +
      `<option value="custom">自定义代理…</option></select>` +
      `<input class="field mono" name="neturl" placeholder="http(s)/socks5(h)://…" hidden></div>`,
      '全局代理在「总览 → 设置」配置一次；这里只选是否走它') +
    `</div>` +
    `<details class="ap-adv" id="ap-adv" hidden><summary>` +
    `<svg class="chev" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="m6.5 4.5 3 3.5-3 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>` +
    `高级选项（预设已自动填写，一般无需改动）</summary><div class="ap-adv-body">` +
    frow('服务商 ID', `<input class="field mono" name="id" placeholder="如 deepseek" required>`) +
    frow('显示名', `<input class="field" name="name" placeholder="默认同 ID">`) +
    frow('协议', `<select class="field" name="protocol">` +
      Object.entries(PROTOCOL_LABEL).map(([v, l]) => `<option value="${v}">${v}（${l}）</option>`).join('') +
      `</select>`) +
    frow('Base URL', `<input class="field mono" name="base_url" placeholder="https://api.example.com/v1" required>`, '上游完整前缀；网关按协议追加 /chat/completions、/v1/messages 等') +
    frow('账号名', `<input class="field mono" name="account" value="main" required>`) +
    frow('最大并发', `<input class="field" name="max_concurrent" type="number" min="0" step="1" placeholder="0 = 不限">`) +
    `</div></details>`;
  const dlg = openDialog({
    title: '添加服务商',
    body,
    yes: '添加',
    onSubmit: async (form) => {
      const v = formVals(form);
      const id = v.id.trim();
      const accName = v.account.trim() || 'main';
      const key = v.api_key.trim();
      const baseURL = v.base_url.trim();
      if (!id) throw new Error('服务商 ID 不能为空（选预设自动填，或展开高级选项手填）');
      if (!baseURL) throw new Error('Base URL 不能为空');
      if (!key) throw new Error('API Key 不能为空');
      if (parseInt(v.max_concurrent, 10) < 0) throw new Error('最大并发不能为负');
      const box = q('#ap-models');
      const models = (!box.hidden && box.querySelector('input[name=apmodel]'))
        ? [...box.querySelectorAll('input[name=apmodel]:checked')].map((x) => x.value)
        : [];
      // 网络：跟随全局（空）/ 直连（direct）/ 自定义 URL
      let proxy = '';
      if (v.netmode === 'direct') proxy = 'direct';
      else if (v.netmode === 'custom') {
        proxy = (v.neturl || '').trim();
        if (!proxy) throw new Error('选择自定义代理时需填写代理地址');
      }
      const acc = { name: accName, api_key: key };
      if (parseInt(v.max_concurrent, 10) > 0) acc.max_concurrent = parseInt(v.max_concurrent, 10);
      if (proxy) acc.proxy = proxy;
      const r = await putProvidersDoc((cur) => {
        const providers = cur.providers || [];
        const target = providers.find((p) => p.id === id);
        if (target) {
          // 追加账号到已有服务商（CLI add 同语义；账号已存在时拒绝）
          if ((target.accounts || []).some((a) => a.name === accName)) {
            throw new Error(`账号 ${id}/${accName} 已存在（换账号名，或用 CLI provider add -replace 更新凭证）`);
          }
          target.accounts = [...(target.accounts || []), acc];
          if (models.length) {
            const have = new Set((target.models || []).map((m) => m.id));
            target.models = [...(target.models || []), ...models.filter((m) => !have.has(m)).map((m) => ({ id: m }))];
          }
          return { ...cur, providers };
        }
        return { ...cur, providers: [...providers, {
          id, name: v.name.trim() || id,
          protocol: v.protocol,
          base_url: baseURL,
          models: models.map((m) => ({ id: m })),
          accounts: [acc],
        }] };
      });
      status(`服务商已保存：${id}/${accName}${models.length ? `（含 ${models.length} 个模型）` : ''}（${r.effect === 'immediate' ? '已热生效' : r.effect}，写前备份已留底）`, 'ok');
      afterWrite();
    },
  });
  dlg.catch(() => {}); // 取消/关闭不算错误（吞掉未处理 rejection）
  const root = q('#modal-root .dialog:last-child');
  const tilesEl = root.querySelector('#ap-tiles');
  const form = root.querySelector('form');
  const probeBtn = root.querySelector('#ap-probe');
  const noteEl = root.querySelector('#ap-note');
  const adv = root.querySelector('#ap-adv');
  const set = (name, val) => { form.elements[name].value = val; };
  // 「去插件页」：关弹窗并切视图
  root.querySelector('[data-act-go-plugins]').addEventListener('click', () => {
    root.closest('.modal-back')?.remove();
    show('plugins');
  });
  // 预设点选 → 预填（CLI add -preset 同语义：显式改动不被覆盖）
  tilesEl.addEventListener('click', (e) => {
    const t = e.target.closest('.tile-p');
    if (!t) return;
    tilesEl.querySelectorAll('.tile-p.on').forEach((x) => x.classList.remove('on'));
    t.classList.add('on');
    const p = presets.find((x) => x.id === t.dataset.preset);
    const box = root.querySelector('#ap-models');
    box.hidden = true; box.innerHTML = '';
    probeBtn.disabled = false; probeBtn.textContent = '检测并获取模型';
    set('id', p.id); set('name', p.name); set('protocol', p.protocol); set('base_url', p.base_url);
    const ex = (cache.providers?.providers || []).find((x) => x.id === p.id);
    if (ex) {
      // 预设命中已有服务商：追加备用 Key，账号名可编辑
      set('name', ex.name || ex.id); set('protocol', ex.protocol); set('base_url', ex.base_url);
      adv.hidden = false; adv.open = true;
      noteEl.textContent = `${p.name} 已存在：将向其追加备用 Key，可改账号名后检测添加。`;
    } else {
      adv.hidden = true; adv.open = false; // 纯 Key 预设：技术字段全部自动，不展示
      noteEl.textContent = `已选预设 ${p.name}：粘贴 API Key → 检测 → 勾选模型 → 添加。`;
    }
    form.elements.api_key.focus();
  });
  // 手动接入：清空预填 + 展开高级选项
  root.querySelector('#ap-custom').addEventListener('click', () => {
    tilesEl.querySelectorAll('.tile-p.on').forEach((x) => x.classList.remove('on'));
    const box = root.querySelector('#ap-models');
    box.hidden = true; box.innerHTML = '';
    probeBtn.disabled = false; probeBtn.textContent = '检测并获取模型';
    set('id', ''); set('name', ''); set('base_url', '');
    adv.hidden = false; adv.open = true;
    noteEl.textContent = '手动接入：填写协议与 Base URL，粘贴 Key 后检测模型（任意 OpenAI 兼容上游均可）。';
    form.elements.id.focus();
  });
  // 网络：选「自定义代理」时出现 URL 输入
  const netmode = form.elements.netmode, neturl = form.elements.neturl;
  netmode.addEventListener('change', () => {
    neturl.hidden = netmode.value !== 'custom';
    if (!neturl.hidden) neturl.focus();
  });
  // 检测：验证凭证并拉取模型清单（POST /api/provider/probe）
  probeBtn.addEventListener('click', async () => {
    const errEl = root.querySelector('.modal-err');
    if (errEl) errEl.textContent = '';
    const key = form.elements.api_key.value.trim();
    if (!key) { if (errEl) errEl.textContent = '先粘贴 API Key 再检测'; form.elements.api_key.focus(); return; }
    probeBtn.disabled = true; probeBtn.textContent = '检测中…';
    const box = root.querySelector('#ap-models');
    box.hidden = false;
    box.innerHTML = '<div class="ap-probing">正在验证凭证并拉取模型清单…（密钥错误会在这里报出）</div>';
    try {
      const r = await api('/api/provider/probe', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ protocol: form.elements.protocol.value, base_url: form.elements.base_url.value.trim(), api_key: key }),
      });
      const list = r.models || [];
      if (!list.length) {
        box.innerHTML = '<div class="ap-probing">上游未返回模型清单（可跳过检测直接添加，模型稍后经「路由」使用时再确认）。</div>';
      } else {
        box.innerHTML =
          '<div class="ap-models-head"><span>支持的模型（' + list.length + '）</span>' +
          '<button type="button" class="text" data-sel="all">全选</button>' +
          '<button type="button" class="text" data-sel="none">清空</button></div>' +
          list.map((m) => '<label class="ap-model"><input type="checkbox" name="apmodel" value="' + esc(m) + '" checked><code>' + esc(m) + '</code></label>').join('');
        noteEl.textContent = '凭证有效，已拉取 ' + list.length + ' 个模型——勾选要启用的模型（默认全选），点「添加」完成。';
      }
      probeBtn.disabled = false; probeBtn.textContent = '重新检测';
    } catch (e) {
      box.hidden = true; box.innerHTML = '';
      probeBtn.disabled = false; probeBtn.textContent = '检测并获取模型';
      if (errEl) errEl.textContent = '检测失败：' + e.message;
      else status('检测失败: ' + e.message, 'err');
    }
  });
  root.querySelector('#ap-models').addEventListener('click', (e) => {
    const b = e.target.closest('[data-sel]');
    if (!b) return;
    root.querySelectorAll('#ap-models input[name=apmodel]').forEach((x) => { x.checked = b.dataset.sel === 'all'; });
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

// ---------- 路由（融合视图：意图 = 一条有序调用链，2026-11 原型定稿） ----------
// 底层逻辑（关键词/正则/优先级）由网关内置，UI 只暴露「意图 → 调用链」
// 一个动作。落地映射（后端契约不变）：每条链 = 隐式组（chain-<意图>）+
// 规则引用；兜底 = 无条件规则 → chain-fallback。CLI 的 groups/rules 完整
// 契约保留；UI 之外的既有规则不受影响（编辑仅改 target）。

const STRATEGY_LABEL = {
  order: '固定顺序', rotate: '每请求轮换', smart: '按剩余额度',
  usage: '当日用量均衡', weight: '平滑加权',
};
const STRATEGY_OPTION = {
  smart: '按剩余额度分配（推荐）', order: '顺序用尽再换',
  rotate: '每次请求轮换', usage: '当日用量均衡',
};
const ORDER_HINTS = {
  order: '成员顺序即调用顺序：1 号优先，额度用尽或故障才落到下一个',
  smart: '按各账号剩余额度自动分配，勾选顺序不影响分配',
  rotate: '每次请求轮流使用各成员，顺序仅作起始次序',
  usage: '按当日用量均衡分配，谁用得少谁上；顺序不生效',
};

// 内置意图定义（前端写死，随版本发布；关键词/正则/优先级在网关侧）。
// long-context / vision 的网关判定随后续版本落地，先开放配置——提前配
// 链，判定就绪即生效。
const INTENT_DEFS = [
  { id: 'coding', label: '编程', desc: '写代码、改代码、评审、测试等开发任务',
    rec: '建议配 2~3 个模型的调用链：多模型自动容灾，额度不会卡死在一个账号上' },
  { id: 'qa', label: '问答', desc: '知识问答、解释、查资料等轻任务',
    rec: '便宜快速的模型就够；用量大时配一条低价链更省额度' },
  { id: 'writing', label: '写作', desc: '文案、文档、润色等长文任务',
    rec: '选输出质量好的模型；写作量大时同样建议配链' },
  { id: 'long-context', label: '长上下文', desc: '超长文件 / 仓库级分析，优先大窗口模型',
    rec: '选上下文窗口最大的模型；单一模型即可' },
  { id: 'vision', label: '图片理解', desc: '带截图或图片输入的任务',
    rec: '必须选支持图片输入的模型——纯文本模型会让请求失败' },
];

// 成员显示名：'glm/glm-4.6@main' → 'glm-4.6@main'；'deepseek' → 'deepseek'
function memberLabel(m) {
  const at = (m || '').indexOf('@');
  const head = at >= 0 ? m.slice(0, at) : m;
  const slash = head.indexOf('/');
  const body = slash >= 0 ? head.slice(slash + 1) : head;
  return at >= 0 ? body + '@' + m.slice(at + 1) : body;
}

// 行内有效性标注（与后端 validateMember 同规）。
function memberValid(m, provs, groups) {
  if ((m || '').startsWith('group/')) {
    return groups.has(m.slice(6)) ? '' : ' <span class="pill prefail">组不存在</span>';
  }
  const at = (m || '').indexOf('@');
  const head = at >= 0 ? m.slice(0, at) : m;
  const acc = at >= 0 ? m.slice(at + 1) : '';
  const slash = head.indexOf('/');
  const provID = slash >= 0 ? head.slice(0, slash) : head;
  const model = slash >= 0 ? head.slice(slash + 1) : '';
  const p = (provs || []).find((x) => x.id === provID);
  if (!p) return ' <span class="pill prefail">服务商不存在</span>';
  if (acc && !(p.accounts || []).some((a) => a.name === acc)) return ' <span class="pill prefail">账号不存在</span>';
  if (model && (p.models || []).length && !p.models.some((mm) => mm.id === model)) {
    return ' <span class="pill warn">模型不在清单</span>';
  }
  return '';
}

async function loadRouting() {
  try {
    const [state, rules, intents, plugViews, provCfg] = await Promise.all([
      api('/api/state'), api('/api/config/rules'), api('/api/config/intents'),
      api('/api/plugins').catch(() => ({ plugins: [] })),
      cache.providers ? Promise.resolve(cache.providers) : api('/api/config/providers'),
    ]);
    cache.providers = provCfg;
    cache.rules = rules;
    const provs = provCfg.providers || [];
    const groupIDs = new Set((state.groups || []).map((g) => g.id));

    // 链状态反推：规则（intent=X 精确 / 无条件=兜底）→ target=group/<id>
    // → 组成员与策略；直连 target（provider/model[/@account]）显示为单成员链。
    const chainOf = {};
    for (const r of (rules.rules || [])) {
      const plain = !r.intent && !r.regex && !r.max_tokens && !r.caller_key;
      const key = plain ? 'fallback' : (r.intent || '');
      if (!key || chainOf[key]) continue;
      if ((r.target || '').startsWith('group/')) {
        const g = (state.groups || []).find((x) => x.id === r.target.slice(6));
        if (g) chainOf[key] = { rule: r, group: g };
      } else {
        chainOf[key] = { rule: r, group: null, members: [r.target] };
      }
    }
    cache.chainOf = chainOf;

    // 插件声明意图（enabled + granted classify.intent，GET /api/plugins 已过滤口径）
    const pluginDefs = (plugViews.plugins || []).filter((p) => p.enabled && (p.intents || []).length)
      .flatMap((p) => p.intents.map((i) => ({ ...i, via: p.id })));
    const customDefs = (intents.intents || []).map((d) => ({ id: d.id, label: d.label || d.id, desc: '用户自定义意图（CLI / intents.json 维护）', via: 'custom' }));

    const rows = INTENT_DEFS.map((d) => chainRow(d, chainOf[d.id], provs, groupIDs));
    for (const d of pluginDefs) rows.push(chainRow(d, chainOf[d.id], provs, groupIDs, '插件'));
    for (const d of customDefs) rows.push(chainRow(d, chainOf[d.id], provs, groupIDs, '自定义'));
    rows.push(chainRow(
      { id: 'fallback', label: '兜底', desc: '未命中任何意图的请求走这里' },
      chainOf.fallback, provs, groupIDs));
    $('list-intents').innerHTML = rows.join('');
  } catch (e) {
    status('路由页加载失败: ' + e.message, 'err');
    if (!$('list-intents').innerHTML) $('list-intents').innerHTML = errBox('加载失败: ' + e.message, 'routing');
  }
}

function chainRow(d, chain, provs, groupIDs, viaBadge) {
  const members = chain?.group ? chain.group.members : (chain?.members || []);
  const strategy = chain?.group ? chain.group.strategy : '';
  const has = members.length > 0;
  const hits = (chain?.rule || {}).hits || 0;
  const via = viaBadge
    ? `<span class="pill oauth" title="${viaBadge === '插件' ? '由插件 ' + esc(d.via) + ' 声明（classify.intent）' : '用户自定义意图（intents.json）'}">${esc(viaBadge)}</span>` : '';
  return `<div class="row"><div class="who">
    <div class="name">${esc(d.label)} <code>${esc(d.id)}</code>${via}</div>
    <div class="sub">${esc(d.desc || '')}${d.via && viaBadge === '插件' ? ' · 由插件 ' + esc(d.via) + ' 提供分类' : ''}</div></div>` +
    (has
      ? `<div class="chain-preview"><span class="pill">${esc(STRATEGY_LABEL[strategy] || strategy || '—')}</span>` +
        members.map((m, i) => `<span class="ord-chip mini" title="${esc(m)}"><b>${i + 1}</b><code>${esc(memberLabel(m))}</code></span>${memberValid(m, provs, groupIDs)}`).join('') +
        `</div>`
      : `<span class="muted2" style="flex:none">未配置</span>`) +
    (hits ? `<span class="muted2" title="命中次数">${hits} 次</span>` : '') +
    `<div class="row-acts" style="opacity:1">
      <button type="button" class="text add" data-chain="${esc(d.id)}" data-label="${esc(d.label || d.id)}" data-rec="${esc(d.rec || d.desc || '')}">${has ? '编辑链' : '＋ 配置调用链'}</button>
    </div></div>`;
}

// ---------- 写路径（M2-W3 契约不变）：fresh GET → 修改 → PUT ----------

const listEmpty = (msg) => `<div class="empty-state">${esc(msg)}</div>`;

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

// 客户端成员校验（与服务端 validateProviders/validateMember 同规：
// provider/model、provider/model@account、provider、group/<id>）
function memberClientCheck(m, provs, otherGroupIDs) {
  if (!m) return '成员引用为空';
  if (m.startsWith('group/')) {
    return otherGroupIDs.has(m.slice(6)) ? null : `引用的组 ${m} 不存在`;
  }
  const at = m.indexOf('@');
  const head = at >= 0 ? m.slice(0, at) : m;
  const acc = at >= 0 ? m.slice(at + 1) : '';
  const slash = head.indexOf('/');
  const provID = slash >= 0 ? head.slice(0, slash) : head;
  const model = slash >= 0 ? head.slice(slash + 1) : '';
  const p = provs.find((x) => x.id === provID);
  if (!p) return `成员 ${m} 引用的服务商不存在`;
  if (acc && !(p.accounts || []).some((a) => a.name === acc)) return `账号 ${acc} 不在 ${provID} 中`;
  if (model && (p.models || []).length && !p.models.some((mm) => mm.id === model)) {
    return `模型 ${model} 不在 ${provID} 的清单内`;
  }
  return null;
}

const frow = (label, field, hint) =>
  `<div class="frow"><span>${label}</span>${field}${hint ? `<div class="hint">${hint}</div>` : ''}</div>`;

// 服务商的 Key/账号归属摘要（选择器里让用户看清「谁在服务」）
function provMetaText(p, state) {
  const n = (p.accounts || []).length;
  let s = n ? `${n} 个账号` : '无账号';
  const resting = Object.keys(state.resting || {}).filter((k) => k.startsWith(p.id + '/')).length +
    (state.exhausted || []).filter((k) => k.startsWith(p.id + '/')).length;
  if (resting) s += ` · ${resting} 个休眠中`;
  return s;
}

// 推荐成员（前端启发式）：第一个有模型清单的服务商的第一个模型
function recMember(provs) {
  const p = (provs || []).find((x) => (x.models || []).length);
  return p ? `${p.id}/${p.models[0].id}` : '';
}

// 隐式组 ID（UI 生成的链组统一 chain- 前缀；用户同名组直接复用）
const chainGroupID = (intentId) => 'chain-' + intentId;

// 调用链编辑器（融合后唯一的分配交互）：策略 + 有序成员（模型@账号
// 席位，勾选先后即优先级；分组头「全选」勾齐该商全部席位、半选态可见）。
function intentChainModal(intentId, label, recHint) {
  const provs = cache.providers?.providers || [];
  const chain = (cache.chainOf || {})[intentId] || {};
  const curStrategy = chain.group?.strategy || 'smart';
  const curMembers = chain.group?.members || chain.members || [];
  // 直连目标（provider/model）进编辑器后转为链成员；group/ 引用不进编辑
  let picked = curMembers.filter((m) => !m.startsWith('group/'));
  const rec = recMember(provs);
  const dlg = openDialog({
    title: `调用链 · ${label}`,
    body:
      `<div class="pick-hint"><span>${esc(recHint || '未命中任何意图的请求走这里；建议配最稳的模型或链')}</span>` +
      (rec && !picked.includes(rec) ? `<button type="button" class="text add" id="pick-rec">一键加入推荐 ${esc(memberLabel(rec))}</button>` : '') +
      `</div>` +
      frow('分配策略', `<select class="field" name="strategy">` +
        Object.entries(STRATEGY_OPTION).map(([v, l]) =>
          `<option value="${v}"${curStrategy === v ? ' selected' : ''}>${l}</option>`).join('') +
        `</select>`) +
      `<div class="frow"><span>成员与顺序</span><div class="pick-order" id="pick-order"></div>` +
      `<div class="hint" id="pick-order-hint"></div>` +
      `<div class="pick-provs" id="pick-provs"></div>` +
      `<div class="hint">成员精确到「模型@账号」——哪个模型、走哪把 Key，一目了然；勾「全选」= 该商全部席位（此后新增的模型不自动加入）。休眠/耗尽的席位自动跳过，勾选先后即优先级；清空保存 = 该意图走兜底。</div></div>`,
    yes: '保存',
    onSubmit: async (form) => {
      const v = formVals(form);
      // 整商成员吸收该商细选席位；保持已选顺序；空 = 走兜底
      const wholes = picked.filter((m) => !m.includes('/') && !m.includes('@'));
      const members = picked.filter((m) => !(m.includes('/') && !m.includes('@') && wholes.includes(m.split('/')[0])));
      for (const m of members) {
        const err = memberClientCheck(m, provs, new Set());
        if (err) throw new Error(err);
      }
      await saveIntentChain(intentId, v.strategy, members);
    },
  });
  dlg.catch(() => {});
  const root = q('#modal-root .dialog:last-child');
  const state = cache.state || {};

  function renderCandidates() {
    root.querySelector('#pick-provs').innerHTML = provs.map((p) => {
      const accs = (p.accounts || []).length ? p.accounts : [{ name: '' }];
      const chips = accs.map((a) => (p.models || []).map((m) => {
        const val = a.name ? `${p.id}/${m.id}@${a.name}` : `${p.id}/${m.id}`;
        return `<label class="pick-chip"><input type="checkbox" value="${esc(val)}"${picked.includes(val) ? ' checked' : ''}><code>${esc(m.id)}${a.name ? `<i class="at">@${esc(a.name)}</i>` : ''}</code></label>`;
      }).join('')).join('');
      return `<div class="pick-prov"><b>${esc(p.name || p.id)}<span class="pick-meta">${esc(provMetaText(p, state))}</span></b>` +
        `<label class="pick-chip whole"><input type="checkbox" data-selall="${esc(p.id)}">全选</label>` +
        `<div class="chips">${chips || '<span class="muted2">清单为空（检测后生成）</span>'}</div></div>`;
    }).join('');
    syncSelAll(root, picked);
    root.querySelectorAll('#pick-provs input[type=checkbox]:not([data-selall])').forEach((inp) => {
      inp.checked = picked.includes(inp.value);
    });
  }
  function updateHint() {
    root.querySelector('#pick-order-hint').textContent =
      ORDER_HINTS[root.querySelector('select[name=strategy]').value] || '';
  }
  function renderPicked() {
    const box = root.querySelector('#pick-order');
    box.innerHTML = picked.length
      ? picked.map((m, i) =>
        `<span class="ord-chip"><b>${i + 1}</b><code>${esc(memberLabel(m))}</code>` +
        `<button type="button" class="ord-mv" data-i="${i}" data-d="-1"${i === 0 ? ' disabled' : ''} aria-label="上移">↑</button>` +
        `<button type="button" class="ord-mv" data-i="${i}" data-d="1"${i === picked.length - 1 ? ' disabled' : ''} aria-label="下移">↓</button>` +
        `<button type="button" class="ord-x" data-i="${i}" aria-label="移除">×</button></span>`).join('')
      : `<span class="muted2">尚未选择成员——在下方勾选（留空保存 = 走兜底）</span>`;
    root.querySelectorAll('#pick-provs input[type=checkbox]:not([data-selall])').forEach((inp) => {
      inp.checked = picked.includes(inp.value);
    });
    syncSelAll(root, picked);
    updateHint();
  }
  root.querySelector('#pick-provs').addEventListener('change', (e) => {
    const inp = e.target;
    if (!inp || inp.type !== 'checkbox') return;
    if (inp.dataset.selall) {
      // 全选：勾齐/清空该服务商全部席位（追加顺序按 chip 排列）
      const pid = inp.dataset.selall;
      const vals = [...root.querySelectorAll('#pick-provs input[type=checkbox]:not([data-selall])')]
        .filter((x) => x.value.startsWith(pid + '/')).map((x) => x.value);
      if (inp.checked) vals.forEach((v) => { if (!picked.includes(v)) picked.push(v); });
      else picked = picked.filter((m) => !vals.includes(m));
      renderPicked();
      return;
    }
    if (!inp.value) return;
    if (inp.checked) { if (!picked.includes(inp.value)) picked.push(inp.value); }
    else picked = picked.filter((m) => m !== inp.value);
    renderPicked();
  });
  root.querySelector('#pick-order').addEventListener('click', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    const i = Number(b.dataset.i);
    if (b.classList.contains('ord-x')) picked.splice(i, 1);
    else if (b.classList.contains('ord-mv')) {
      const j = i + Number(b.dataset.d);
      if (j < 0 || j >= picked.length) return;
      [picked[i], picked[j]] = [picked[j], picked[i]];
    } else return;
    renderPicked();
  });
  root.querySelector('select[name=strategy]').addEventListener('change', updateHint);
  root.querySelector('#pick-rec')?.addEventListener('click', () => {
    if (!rec || picked.includes(rec)) return;
    picked.push(rec);
    renderCandidates();
    renderPicked();
  });
  renderCandidates();
  renderPicked();
}

// 分组头全选框同步（含半选态）
function syncSelAll(root, picked) {
  root.querySelectorAll('#pick-provs input[data-selall]').forEach((inp) => {
    const pid = inp.dataset.selall;
    const vals = [...root.querySelectorAll('#pick-provs input[type=checkbox]:not([data-selall])')]
      .filter((x) => x.value.startsWith(pid + '/')).map((x) => x.value);
    const on = vals.filter((v) => picked.includes(v)).length;
    inp.checked = vals.length > 0 && on === vals.length;
    inp.indeterminate = on > 0 && on < vals.length;
  });
}

// 保存调用链 = 隐式组 + 规则双写（providers groups + rules，均 fresh
// GET → 修改 → PUT，写前备份+审计由服务端统一处理）。
async function saveIntentChain(intentId, strategy, members) {
  const isFallback = intentId === 'fallback';
  const gid = chainGroupID(intentId);
  if (members.length) {
    // 1. 组：建/改（chain- 前缀组；同名用户组复用）
    await putProvidersDoc((cur) => {
      const groups = cur.groups || [];
      const otherIDs = new Set(groups.filter((g) => g.id !== gid).map((g) => g.id));
      for (const m of members) {
        const err = memberClientCheck(m, cur.providers || [], otherIDs);
        if (err) throw new Error(err);
      }
      const g = { id: gid, strategy, members };
      const idx = groups.findIndex((x) => x.id === gid);
      if (idx >= 0) groups[idx] = g; else groups.push(g);
      return { ...cur, groups };
    });
    // 2. 规则：intent 链=精确意图规则；兜底=无条件规则（恒最后）
    const cur = await api('/api/config/rules');
    const rules = cur.rules || [];
    let rule;
    if (isFallback) {
      rule = rules.find((r) => !r.intent && !r.regex && !r.max_tokens && !r.caller_key);
      if (!rule) rule = { id: 'fallback' };
    } else {
      rule = rules.find((r) => r.intent === intentId);
      if (!rule) rule = { id: 'intent-' + intentId.replace(/[^a-zA-Z0-9_-]+/g, '-'), intent: intentId };
    }
    const hadCond = !!(rule.regex || rule.max_tokens || rule.caller_key);
    rule.target = 'group/' + gid;
    const idx = rules.findIndex((r) => r.id === rule.id);
    if (idx >= 0) rules[idx] = rule;
    else if (isFallback) rules.push(rule);
    else {
      // 插到第一条无条件规则之前（兜底规则保持最后）
      const fb = rules.findIndex((r) => !r.intent && !r.regex && !r.max_tokens && !r.caller_key);
      if (fb >= 0) rules.splice(fb, 0, rule); else rules.push(rule);
    }
    await api('/api/config/rules', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ rules }),
    });
    status(`「${intentId}」调用链已保存${hadCond ? '（已更新既有规则的指向）' : '（热生效，写前备份已留底）'}`, 'ok');
  } else {
    // 清空 = 该意图走兜底：删规则；chain- 组无其他引用时一并删除
    const ok = await confirmModal(`清空「${intentId}」调用链？`,
      '该意图的路由规则将删除，请求走默认路由。写前备份已留底。', { yes: '清空', danger: false });
    if (!ok) throw new Error('已取消');
    const cur = await api('/api/config/rules');
    const match = (r) => isFallback
      ? !r.intent && !r.regex && !r.max_tokens && !r.caller_key
      : r.intent === intentId && !r.regex && !r.max_tokens && !r.caller_key;
    const rules = (cur.rules || []).filter((r) => !match(r));
    await api('/api/config/rules', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ rules }),
    });
    const stillRef = rules.some((r) => r.target === 'group/' + gid);
    if (!stillRef) {
      await putProvidersDoc((c) => ({ ...c, groups: (c.groups || []).filter((g) => g.id !== gid) }));
    }
    status(`「${intentId}」调用链已清空（该意图走兜底）`, 'ok');
  }
  afterWrite();
}

// 说明：任务规则的高级条件（正则 / token 上限 / 调用方）与自定义意图
// 词表已从 UI 收敛（2026-11 融合定稿）——意图识别由网关内置（开发端维
// 护），用户只做「编辑调用链」一个动作；CLI 的 groups/rules/intents 完整
// 契约保留，服务端写路径校验不放松。

// acctDetailModal：账号×模型用量明细（批3；数据源 GET /api/cost 的
// by_account_model 二级聚合，与「观测 › 成本报告」同源）。
async function acctDetailModal(provID, accName) {
  const prefix = `${provID}/${accName}/`;
  let rep;
  try {
    rep = await api('/api/cost?days=7');
  } catch (e) {
    status('成本报告加载失败: ' + e.message, 'err');
    return;
  }
  const rows = Object.entries(rep.by_account_model || {})
    .filter(([k]) => k.startsWith(prefix))
    .sort((a, b) => b[1].cost - a[1].cost);
  const fx = rep.fx_usd_cny || 0;
  const fmtUSDv = (n) => '$' + Number(n || 0).toFixed(4);
  const body = rows.length
    ? rows.map(([k, b]) => {
      const model = k.slice(prefix.length);
      const prices = (b.prices || []).map((p) => p.in_per_m + '/' + p.out_per_m).join(' ');
      return `<div class="frow"><span><code>${esc(model)}</code></span>` +
        `<div class="acct-um"><span>${b.calls} 次</span><span>${fmtTokens(b.input_tokens)} in</span>` +
        `<span>${fmtTokens(b.output_tokens)} out</span><b>${fmtUSDv(b.cost)}${fx > 0 ? ` <span class="muted2">≈¥${(b.cost * fx).toFixed(2)}</span>` : ''}</b></div>` +
        (prices ? `<div class="hint">单价 in/out（$/1M）：${esc(prices)}</div>` : '') + `</div>`;
    }).join('') +
      `<p class="foot-note">窗口 ${esc(rep.from)} ~ ${esc(rep.to)}；失败请求不计费，无单价调用计入「未计价」。</p>`
    : `<p class="muted2">近 7 天该账号暂无模型调用（或全部未计价）。</p>`;
  openDialog({ title: `按模型用量 · ${provID}/${accName}`, body, yes: '关闭', onSubmit: async () => {} }).catch(() => {});
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

// 路由页操作列事件委托（编辑链）+ 服务商页额度入口
$('list-intents').addEventListener('click', (e) => {
  const btn = e.target.closest('[data-chain]');
  if (!btn) return;
  intentChainModal(btn.dataset.chain, btn.dataset.label, btn.dataset.rec);
});
$('prov-list').addEventListener('click', (e) => {
  const btn = e.target.closest('.quota-btn');
  if (btn) { quotaModal(btn.dataset.p, btn.dataset.a); return; }
  const um = e.target.closest('.um-btn');
  if (um) { acctDetailModal(um.dataset.p, um.dataset.a); return; }
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
// 清除记录：内存 + 落盘一并删除（成本报告同源，历史时段随之归零）
$('btn-trace-clear').addEventListener('click', async () => {
  const ok = await confirmModal('清除全部调用记录？',
    '决策追溯将从内存与落盘一并删除，无法恢复；成本报告的历史数据随之清零。正在进行的请求不受影响，新请求照常记录。',
    { yes: '清除', danger: true });
  if (!ok) return;
  try {
    await api('/api/trace', { method: 'DELETE' });
    status('调用记录已清除', 'ok');
    loadTraces(); loadCost();
  } catch (e) { status('清除失败: ' + e.message, 'err'); }
});
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
    const agents = r.agents || [];
    sel.innerHTML = '<option value="">自动（按匹配规则）</option>' +
      agents.map((a) => `<option value="${esc(a.id)}">${esc(a.id)}</option>`).join('');
    if (cur) sel.value = cur;
    renderAgentList(agents);
    loadDispatchCatalog(); // 目录区搭车刷新（已装条目过滤依赖最新 agents）
  } catch (e) {
    sel.innerHTML = '<option value="">派发不可用</option>';
    $('list-agents').innerHTML = errBox('Agent 清单加载失败: ' + e.message, 'dispatch');
    status('派发配置加载失败: ' + e.message, 'err');
  }
}

// ---------- Agent 目录（2026-11-11：真下载安装——包 zip 解压物化注册） ----------

async function loadDispatchCatalog() {
  const el = $('list-agent-catalog');
  if (!el) return;
  try {
    const r = await api('/api/dispatch/catalog');
    const entries = (r.catalog || []).filter((e) => !e.installed); // 已安装条目不显示（商店同构）
    el.innerHTML = entries.map((e) => {
      const cli = e.found
        ? `✓ 已检测到 ${esc(e.probe)} CLI`
        : `✗ 未检测到 ${esc(e.probe)} CLI（可先安装，装好 CLI 后即用）`;
      return `<div class="row">
        <div class="who">
          <div class="name"><code>${esc(e.spec.id)}</code></div>
          <div class="sub">${esc(e.desc)}<br>${cli}${e.docs ? ` · <a href="${esc(e.docs)}" target="_blank" rel="noopener">文档</a>` : ''}</div>
        </div>
        <div class="row-acts">
          <button type="button" class="text add" data-catalog="${esc(e.spec.id)}">安装</button>
        </div>
      </div>`;
    }).join('') || listEmpty('目录条目均已安装——用上方「＋ 新建 Agent」自定义即可');
  } catch (e) {
    el.innerHTML = errBox('Agent 目录加载失败: ' + e.message, 'agents');
  }
}

async function installCatalog(id) {
  const btn = document.querySelector(`[data-catalog="${CSS.escape(id)}"]`);
  if (btn) { btn.disabled = true; btn.textContent = '安装中…'; }
  try {
    const r = await api(`/api/dispatch/catalog/${encodeURIComponent(id)}/install`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}',
    });
    status(`Agent ${id} 已安装（下载→解压→备份留底→热生效）· ${r.note || ''}`, 'ok', 9000);
    loadDispatchAgents(); // 列表 + 目录区（已装过滤）+ 派发下拉同步
  } catch (e) {
    status(`安装失败: ${e.message}`, 'err', 8000);
    if (btn) { btn.disabled = false; btn.textContent = '安装'; }
  }
}

$('list-agent-catalog').addEventListener('click', (e) => {
  const id = e.target.dataset.catalog;
  if (id) installCatalog(id);
});

// ---------- Agent 管理区（2026-11-11：dispatch.json agents[] 的增删改） ----------
// 写路径同走管理 API 写闸（fresh GET → 构造全文档 → PUT /api/config/dispatch：
// 严格校验 + 写前备份 + 审计 + 热重载）。env 原样编辑不脱敏——dispatch.json
// 为本机明文信任态，GET 本就明文返回。templates 字段每次全文档往返原样保留。

const linesOf = (s) => (s || '').split('\n').map((l) => l.trim()).filter((l) => l !== '');

function renderAgentList(agents) {
  const el = $('list-agents');
  if (!el) return;
  el.innerHTML = agents.map((a) => {
    const cmd = a.command.join(' ');
    const badges = [
      a.default ? '兜底' : '',
      a.output === 'jsonl' ? 'jsonl' : '',
      a.parser ? `parser:${a.parser}` : '',
      a.match && a.match.intent ? `intent=${a.match.intent}` : '',
      a.match && a.match.prompt_regex ? 'prompt~' : '',
      a.match && a.match.model_regex ? 'model~' : '',
    ].filter(Boolean).join(' · ');
    const meta = [
      cmd.length > 90 ? cmd.slice(0, 90) + '…' : cmd,
      a.timeout ? `超时 ${a.timeout}` : '',
      a.max_concurrent > 1 ? `并发 ${a.max_concurrent}` : '',
      badges,
    ].filter(Boolean).join(' · ');
    return `<div class="row">
      <div class="who">
        <div class="name"><code>${esc(a.id)}</code></div>
        <div class="sub">${esc(meta)}</div>
      </div>
      <div class="row-acts">
        <button type="button" class="text edit" data-agent="${esc(a.id)}">编辑</button>
        <button type="button" class="text del" data-agent="${esc(a.id)}">删除</button>
      </div>
    </div>`;
  }).join('') || listEmpty('暂无 Agent——派发将不可用；点「＋ 新建 Agent」添加');
}

function agentModal(existing) {
  const isEdit = !!existing;
  const a = existing || {};
  formModal(isEdit ? `编辑 Agent ${a.id}` : '新建 Agent',
    frow('ID', `<input class="field mono" name="id" value="${esc(a.id || '')}" ${isEdit ? 'readonly' : 'required'} spellcheck="false" placeholder="唯一标识，如 claude-code">`) +
    frow('命令', `<textarea class="field mono" name="command" rows="3" required spellcheck="false" placeholder="每行一个参数，如：&#10;claude&#10;-p&#10;--output-format stream-json">${esc((a.command || []).join('\n'))}</textarea>`, '每行一个 argv 参数（空格/引号原样保留）；argv 自身含换行的极端配置不支持') +
    frow('工作目录', `<input class="field mono" name="work_dir" value="${esc(a.work_dir || '')}" spellcheck="false" placeholder="可选；默认网关进程工作目录">`) +
    frow('环境变量', `<textarea class="field mono" name="env" rows="2" spellcheck="false" placeholder="每行一个 KEY=VALUE">${esc((a.env || []).join('\n'))}</textarea>`, '每行一个环境变量（原样显示，本机明文配置）') +
    frow('超时', `<input class="field mono" name="timeout" value="${esc(a.timeout || '')}" placeholder="如 10m / 30s / 1h；空 = 缺省">`) +
    frow('最大并发', `<input class="field" name="max_concurrent" type="number" min="0" step="1" value="${a.max_concurrent || ''}" placeholder="默认 1">`) +
    frow('输出格式', `<select class="sel" name="output"><option value="">自动（text）</option><option value="text"${a.output === 'text' ? ' selected' : ''}>text · 纯文本</option><option value="jsonl"${a.output === 'jsonl' ? ' selected' : ''}>jsonl · stream-json 事件流</option></select>`) +
    frow('解析插件', `<input class="field mono" name="parser" value="${esc(a.parser || '')}" spellcheck="false" placeholder="可选；输出解析插件 id，不可用=该行 raw 回退">`) +
    frow('兜底', `<label class="switch-line"><input type="checkbox" name="default"${a.default ? ' checked' : ''}><span>无匹配规则时使用（至多一个）</span></label>`) +
    frow('match · intent', `<input class="field mono" name="m_intent" value="${esc((a.match && a.match.intent) || '')}" spellcheck="false" placeholder="意图精确等于；可选">`) +
    frow('match · model_regex', `<input class="field mono" name="m_model" value="${esc((a.match && a.match.model_regex) || '')}" spellcheck="false" placeholder="请求模型正则；可选">`) +
    frow('match · prompt_regex', `<input class="field mono" name="m_prompt" value="${esc((a.match && a.match.prompt_regex) || '')}" spellcheck="false" placeholder="任务文本正则；可选">`,
      '全部留空且不勾兜底 = 仅显式指定时可用'),
  async (form) => {
    const v = formVals(form);
    const id = (v.id || '').trim();
    if (!id) throw new Error('ID 不能为空');
    const command = linesOf(v.command);
    if (command.length === 0) throw new Error('命令不能为空（每行一个参数）');
    const timeout = (v.timeout || '').trim();
    if (timeout && !/^\d+(\.\d+)?(ms|s|m|h)$/.test(timeout)) {
      throw new Error('超时须为如 10m / 30s / 1h 的时长');
    }
    const mModel = (v.m_model || '').trim(), mPrompt = (v.m_prompt || '').trim();
    for (const [name, re] of [['model_regex', mModel], ['prompt_regex', mPrompt]]) {
      if (re) { try { new RegExp(re); } catch { throw new Error(`match.${name} 正则非法: ${re}`); } }
    }
    const rawEnv = linesOf(v.env);
    const env = rawEnv.filter((l) => l.includes('='));
    if (rawEnv.length !== env.length) throw new Error('环境变量每行须为 KEY=VALUE');
    const isDefault = v.default === 'on';
    const spec = {
      id,
      command,
      ...(v.work_dir.trim() ? { work_dir: v.work_dir.trim() } : {}),
      ...(env.length ? { env } : {}),
      ...(timeout ? { timeout } : {}),
      ...(parseInt(v.max_concurrent, 10) > 0 ? { max_concurrent: parseInt(v.max_concurrent, 10) } : {}),
      ...(v.output ? { output: v.output } : {}),
      ...(v.parser.trim() ? { parser: v.parser.trim() } : {}),
      ...(isDefault ? { default: true } : {}),
      ...((v.m_intent || '').trim() || mModel || mPrompt ? { match: {
        ...((v.m_intent || '').trim() ? { intent: v.m_intent.trim() } : {}),
        ...(mModel ? { model_regex: mModel } : {}),
        ...(mPrompt ? { prompt_regex: mPrompt } : {}),
      } } : {}),
    };
    // fresh GET 构造全文档（templates 原样保留）
    const cur = await api('/api/config/dispatch');
    const agents = cur.agents || [];
    const idx = agents.findIndex((x) => x.id === id);
    if (isEdit && idx < 0) throw new Error(`Agent ${id} 已不存在（配置可能被并行修改），请刷新后重试`);
    if (!isEdit && idx >= 0) throw new Error(`Agent ${id} 已存在`);
    if (isDefault && agents.some((x) => x.default && x.id !== id)) {
      throw new Error(`已有兜底 Agent（default 至多一个）：${agents.filter((x) => x.default).map((x) => x.id).join('、')}`);
    }
    if (idx >= 0) agents[idx] = spec; else agents.push(spec);
    await api('/api/config/dispatch', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agents, templates: cur.templates || [] }),
    });
    status(`Agent ${id} 已保存（写前备份已留底，热生效）`, 'ok');
    loadDispatchAgents();
  });
}

async function deleteAgent(id) {
  const ok = await confirmModal(`删除 Agent ${id}`, '该定义将从 dispatch.json 移除（写前备份留底；引用它的任务模板会被服务端拒绝）。');
  if (!ok) return;
  const cur = await api('/api/config/dispatch');
  const before = (cur.agents || []).length;
  const agents = (cur.agents || []).filter((x) => x.id !== id);
  if (agents.length === before) {
    status(`Agent ${id} 已不存在`, 'err');
    loadDispatchAgents();
    return;
  }
  await api('/api/config/dispatch', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ agents, templates: cur.templates || [] }),
  });
  status(`Agent ${id} 已删除（热生效）`, 'ok');
  loadDispatchAgents();
}

$('list-agents').addEventListener('click', async (e) => {
  const id = e.target.dataset.agent;
  if (!id) return;
  const r = await api('/api/dispatch/agents');
  const entry = (r.agents || []).find((x) => x.id === id);
  if (!entry) {
    status(`Agent ${id} 已不存在`, 'err');
    loadDispatchAgents();
    return;
  }
  if (e.target.classList.contains('edit')) agentModal(entry);
  if (e.target.classList.contains('del')) deleteAgent(id);
});
$('btn-agent-add').addEventListener('click', () => agentModal(null));

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
// 清除会话历史：终态会话内存+落盘清除；运行中会话不受影响
$('btn-disp-clear').addEventListener('click', async () => {
  const ok = await confirmModal('清除派发会话历史？',
    '终态会话将从列表与落盘中删除，无法恢复；运行中的会话不受影响，结束后照常出现在列表。',
    { yes: '清除', danger: true });
  if (!ok) return;
  try {
    await api('/api/dispatch/sessions', { method: 'DELETE' });
    status('会话历史已清除', 'ok');
    loadDispatchSessions();
  } catch (e) { status('清除失败: ' + e.message, 'err'); }
});
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

// ---------- 插件（2026-11 定稿：订阅适配走社区插件 + 商店索引 + 信息分层） ----------

function pluginBadge(st) {
  const cls = { running: 'ok', starting: 'resting', backoff: 'resting', failed: 'prefail', stopped: 'disabled' }[st] || '';
  return `<span class="pill ${cls}">${esc(st)}</span>`;
}

// 能力 → 分类（开发端定映射，插件声明能力即自动归类）
const PLUGIN_CATS = {
  'provider.oauth': '订阅适配',
  'provider.rewrite': '请求改写',
  'classify.intent': '意图分类',
  'events.trace': '观测订阅',
  'dispatch.parse': '派发解析',
  'vendor.verdict': '厂商判定',
};

// 订阅适配注册表（展示元数据）：适配本体在社区插件，主程序无厂商代
// 码；安装走 /api/plugin-store/{id}/install（后端内置索引），授权走
// CLI add-oauth。
const SUB_ADAPTERS = [
  { id: 'claude-auth', pkg: '@ym-community/claude-auth', vendor: 'Claude 订阅', src: 'claude-code' },
  { id: 'codex-auth', pkg: '@ym-community/codex-auth', vendor: 'Codex 订阅', src: 'codex' },
  { id: 'gemini-auth', pkg: '@ym-community/gemini-auth', vendor: 'Gemini 订阅', src: 'gemini-cli' },
];
let storeCache = null;
let storeFilter = ''; // 商店分类筛选（''=全部）

async function loadStore() {
  if (storeCache) return storeCache;
  try {
    const r = await api('/api/plugin-store');
    storeCache = r.store || [];
  } catch {
    storeCache = []; // 索引拉取失败不阻塞插件页其余区块
  }
  return storeCache;
}

async function loadPlugins() {
  let views = [];
  try {
    const r = await api('/api/plugins');
    views = r.plugins || [];
  } catch (e) {
    $('list-plugins').innerHTML = errBox('插件清单加载失败: ' + e.message, 'plugins');
  }
  const store = await loadStore();
  renderSubAdapters(views, store);
  renderStore(views, store);
  renderPluginsList(views);
}

function renderSubAdapters(views, store) {
  $('list-subadapters').innerHTML = SUB_ADAPTERS.map((a) => {
    const plug = views.find((x) => x.id === a.id);
    const on = !!(plug && plug.enabled);
    const state = on ? '<span class="pill ok">已启用</span>'
      : plug ? '<span class="pill disabled">已停用</span>'
      : '<span class="pill disabled">未安装</span>';
    const entry = (store || []).find((x) => x.id === a.id);
    const acts = !plug
      ? `<button type="button" class="text add" data-subact="install" data-s="${esc(a.id)}" ${entry && !entry.download_url ? 'title="安装包暂未发布，可先手动接入 plugins.json".' : ''}>安装</button>`
      : '<button type="button" class="text add" data-subact="goacct">去添加账号</button>';
    return '<div class="row"><div class="who">' +
      '<div class="name">' + esc(a.vendor) + ' <code>' + esc(a.pkg) + '</code></div>' +
      '<div class="sub">授权走 CLI：<code>yardmaster provider add-oauth ' + esc(a.src) + '</code> · 多账号轮换</div></div>' +
      state +
      (plug ? '<button type="button" class="lib-switch ' + (on ? 'on' : '') + '" data-subtoggle="' + esc(a.id) + '" role="switch" aria-checked="' + on + '" aria-label="启停 ' + esc(a.id) + '"><i></i></button>' : '') +
      '<div class="row-acts" style="opacity:1">' + acts + '</div>' +
      '</div>';
  }).join('');
}

function renderStore(views, store) {
  const chips = [''].concat([...new Set(Object.values(PLUGIN_CATS))]);
  $('store-chips').innerHTML = chips.map((c) =>
    '<button type="button" class="cat-chip' + (storeFilter === c ? ' on' : '') + '" data-cat="' + esc(c) + '">' + esc(c || '全部') + '</button>').join('');
  const installed = new Set(views.map((p) => p.id));
  // 订阅适配在上方专区展示，商店列表不重复
  const list = (store || []).filter((s) => s.cap !== 'provider.oauth')
    .filter((s) => !installed.has(s.id) && (!storeFilter || PLUGIN_CATS[s.cap] === storeFilter));
  $('list-store').innerHTML = list.map((s) => {
    const unpublished = !s.download_url;
    return '<div class="row"><div class="who">' +
    '<div class="name"><code>' + esc(s.id) + '</code><span class="pill">' + esc(PLUGIN_CATS[s.cap] || '扩展') + '</span><span class="muted2" style="font-weight:400">' + esc(s.ver) + ' · ' + esc(s.author) + '</span></div>' +
    '<div class="sub wrap">' + esc(s.desc) + (unpublished ? ' <span class="pill disabled">安装包未发布</span>' : '') + '</div>' +
    ((s.intents || []).length ? '<div class="sub wrap">安装后声明意图：' + s.intents.map((i) => '<code>' + esc(i.id) + '</code>').join(' ') + '</div>' : '') +
    '</div>' +
    '<button type="button" class="btn-primary" data-store-install="' + esc(s.id) + '"' + (unpublished ? ' title="安装包暂未发布，可先手动接入 plugins.json".' : '') + '>安装</button>' +
    '</div>';
  }).join('') || listEmpty('该分类下暂无可安装插件');
}

// 一键安装（真端点）：POST /api/plugin-store/{id}/install——服务端下载
// zip 校验落盘注册热生效；失败错误内联在状态行（未发布包如实提示）。
async function installFromStore(id) {
  const btn = document.querySelector(`[data-store-install="${CSS.escape(id)}"]`) ||
    document.querySelector(`[data-subact="install"][data-s="${CSS.escape(id)}"]`);
  busy(btn, true, '安装中…');
  try {
    await api(`/api/plugin-store/${encodeURIComponent(id)}/install`, { method: 'POST' });
    status(`已安装并启用 ${id}（热生效）`, 'ok');
    storeCache = null;
    await loadPlugins();
  } catch (e) {
    status('安装失败: ' + e.message, 'err', 8000);
  } finally {
    busy(btn, false);
  }
}

function renderPluginsList(views) {
  $('list-plugins').innerHTML = views.map((p) => {
    const granted = (p.granted || []).map((c) => `<code>${esc(c)}</code>`).join(' ') || '<span class="muted2">无</span>';
    const cat = PLUGIN_CATS[(p.granted || [])[0]] || '扩展';
    const ints = (p.intents || []).map((i) => `<code>${esc(i.id)}</code>`).join(' ');
    const intsLine = ints ? `<div class="sub wrap">声明意图 ${ints}（可在「路由 › 意图分配」为其配调用链）</div>` : '';
    const drops = p.dropped_events ? ` · <span class="pill prefail" style="padding:0 7px">丢 ${p.dropped_events}</span>` : '';
    return `<div class="row">
      <div class="who">
        <div class="name"><code>${esc(p.id)}</code><span class="pill">${esc(cat)}</span></div>
        ${p.desc ? `<div class="sub wrap">${esc(p.desc)}</div>` : ''}
        <div class="sub wrap">能力 ${granted} · 崩溃(1h) ${p.crash_1h || 0}${drops}</div>
        ${intsLine}
      </div>
      ${pluginBadge(p.status)}
      <button type="button" class="lib-switch ${p.enabled ? 'on' : ''}" data-plug="toggle" data-id="${esc(p.id)}" role="switch" aria-checked="${p.enabled}" aria-label="启停 ${esc(p.id)}"><i></i></button>
      <button type="button" class="text" data-plug="log" data-id="${esc(p.id)}">日志</button>
    </div>`;
  }).join('') || listEmpty('未配置插件（plugins.json）');
}

$('list-subadapters').addEventListener('click', async (e) => {
  const act = e.target.closest('[data-subact]');
  if (act) {
    if (act.dataset.subact === 'install') { installFromStore(act.dataset.s); return; }
    if (act.dataset.subact === 'goacct') { show('providers'); return; }
  }
  const sw = e.target.closest('[data-subtoggle]');
  if (!sw) return;
  const id = sw.dataset.subtoggle;
  const wasOn = sw.classList.contains('on');
  sw.disabled = true;
  try {
    await api(`/api/plugins/${encodeURIComponent(id)}/${wasOn ? 'disable' : 'enable'}`, { method: 'POST' });
    status(wasOn ? '已停用（订阅适配下线后其服务商账号不可用）' : '已启用（热生效）', 'ok');
    await loadPlugins();
  } catch (err) {
    status('操作失败: ' + err.message, 'err');
    sw.disabled = false;
  }
});
$('store-chips').addEventListener('click', (e) => {
  const chip = e.target.closest('[data-cat]');
  if (!chip) return;
  storeFilter = chip.dataset.cat;
  loadPlugins();
});
$('list-store').addEventListener('click', (e) => {
  const btn = e.target.closest('[data-store-install]');
  if (!btn) return;
  installFromStore(btn.dataset.storeInstall);
});

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
    { label: '设置', hint: '监听 / 令牌 / 代理 / 分类器', kw: 'settings 设置 配置 令牌',
      run: () => show('settings') },
    { label: '切换主题', hint: '跟随系统 → 亮 → 暗 循环', kw: 'theme 主题 亮色 暗色',
      run: cycleTheme },
    { label: '重载配置', hint: '重读 providers.json 并热替换路由快照', kw: 'reload 配置 重载 provider 服务商',
      run: reloadConfig },
    { label: '余额探测', hint: '立即轮询全部账号余额/额度', kw: 'probe quota 余额 额度 探测',
      run: probeQuotas },
    { label: '添加服务商', hint: '从厂商预设新建上游（或追加账号）', kw: 'add provider 新增 供应商 服务商 预设',
      run: () => { show('providers'); addProviderModal(); } },
    { label: '生成访问令牌', hint: '为客户端发放命名令牌（设置页）', kw: 'token 令牌 发放 生成 apikey',
      run: () => { show('settings'); setTimeout(() => $('btn-token-gen').click(), 260); } },
    { label: '显示上手引导', hint: '重新打开首页的三步引导卡', kw: 'guide 引导 上手 新手 接线',
      run: () => { localStorage.removeItem('ym-guide-dismissed'); localStorage.removeItem('ym-guide-wired'); show('overview'); renderGuide(); } },
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
