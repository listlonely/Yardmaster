// yardmaster 控制台（2026-11-03 深度重构：Magpie 构造与交互对齐）
// —— 构造：.list/.row 行卡片取代 <table>；.who/.name/.sub 两行式信息层级；
//    应用壳（body 不滚、各视图独立滚动）；选中导航项自带卡片浮起 + accent 侧轨；
// —— 交互：status() 状态行反馈（不再用 toast）；Magpie 式模态（含确认框）；
//    骨架屏加载态；服务商客户端查找；胶囊开关；document.hidden 轮询守卫。
// 数据源不变：/api/state、/api/config/*、/api/trace、/api/cost、
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
// Windows 字体栈（:root.is-win）。类名曾用 win——与额度窗口卡组件类
// .win（max-width:460px 等）撞名，html 命中后整页收缩为 460px 窄条
// （2026-11-20 窗口窄条问题的真正根因），故更名 is-win。
if (/Windows/i.test(navigator.userAgent)) {
  document.documentElement.classList.add('is-win');
}

// ---------- 主题：三态 auto/light/dark，localStorage 持久化；侧栏脚部三态切换（E） ----------
const THEME_KEY = 'ym-theme';
function applyTheme(t) {
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme; // auto=跟随系统
  document.querySelectorAll('.side-theme button').forEach((b) => {
    b.classList.toggle('on', b.dataset.t === t);
  });
}
applyTheme(localStorage.getItem(THEME_KEY) || 'auto');
function cycleTheme() {
  const cur = localStorage.getItem(THEME_KEY) || 'auto';
  const next = { auto: 'light', light: 'dark', dark: 'auto' }[cur] || 'auto';
  setTheme(next);
}
function setTheme(t) {
  localStorage.setItem(THEME_KEY, t);
  // 明暗互换的颜色过渡（一次性类；reduced-motion 由 CSS 旁路）
  const root = document.documentElement;
  root.classList.add('theming');
  clearTimeout(setTheme.t);
  setTheme.t = setTimeout(() => root.classList.remove('theming'), 340);
  applyTheme(t);
}
document.querySelectorAll('.side-theme button').forEach((b) => {
  b.addEventListener('click', () => setTheme(b.dataset.t || 'auto'));
});

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
// footNote：底栏左侧的保存语义说明（G：写路径提示，不占表单区）。
function openDialog({ title, body, yes = '保存', danger = false, footNote, onSubmit }) {
  return new Promise((resolve, reject) => {
    const back = document.createElement('div');
    back.className = 'modal-back';
    back.innerHTML = `<div class="dialog" role="dialog" aria-modal="true" aria-label="${esc(title)}">
      <div class="ehead"><h3>${esc(title)}</h3>
        <button type="button" class="icon dlg-x" aria-label="关闭">
          <svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>
        </button></div>
      <form><div class="ebody">${body}</div>
        <div class="dlg-bar">${footNote ? `<span class="dlg-note">${footNote}</span>` : ''}<span class="modal-err"></span>
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

// ---------- 视图切换（data-tab/hash 机制不变；E：选中项自带卡片浮起 + accent 侧轨） ----------
const tabsBox = $('tabs');

// 页面副标题（crumb）：一句话点出该页管什么
const CRUMBS = {
  overview: '驾驶舱',
  providers: '上游与账号',
  routing: '意图与调用链',
  observe: '决策追溯与成本',
  plugins: '商店与适配',
  settings: '偏好与装配',
};

let view = '';

document.querySelectorAll('#tabs .nav-it').forEach((b) => {
  b.addEventListener('click', () => { show(b.dataset.tab); b.blur(); });
});

function show(name) {
  if (!document.getElementById('page-' + name)) name = 'overview';
  const first = !view;
  view = name;
  document.querySelectorAll('#tabs .nav-it').forEach((b) => {
    const on = b.dataset.tab === name;
    b.classList.toggle('on', on);
    if (on) b.setAttribute('aria-current', 'page');
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
  $('page-crumb').textContent = CRUMBS[name] || '';
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  // 激活即拉数据（观察者打开什么就看什么，Magpie show() 语义）
  if (name === 'overview') loadOverview();
  if (name === 'providers') ensureProviders();
  if (name === 'routing') loadRouting();
  if (name === 'observe') { loadTraces(); loadCost(); loadPricing(); }
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
  if (bootVersion === null) {
    bootVersion = cache.state.version || '';
    const bv = $('brand-ver'); // 品牌行版本号（E：侧栏顶部）
    if (bv) bv.textContent = bootVersion;
  }
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

// ---------- 侧栏网关状态卡（E：标题 + 地址） ----------
function renderSideGw(ok) {
  const b = $('side-gw');
  b.hidden = false;
  b.classList.toggle('err', !ok);
  b.classList.toggle('off', false);
  $('side-gw-t').textContent = ok ? '网关运行中' : '网关不可达';
  $('side-gw-text').textContent = ok
    ? (cache.settings?.listen || '127.0.0.1:8317')
    : 'serve 未响应';
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
  return `<div class="stat"><div class="stat-top"><span class="stat-l">${esc(label)}</span></div>
    <div class="stat-v">${value}</div>
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
    // l1_classifier / l1_timeout_ms 已从设置表单移除（CLI settings set 维护）；
    // 服务端对空值保留现值，此处省略即不清零。
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
  // 预览稿形：序号恒显（done 时绿底），完成对勾独立在行右侧
  return '<div class="gstep' + (done ? ' done' : ' todo') + '">' +
    '<span class="gnum">' + n + '</span>' +
    '<div><h4>' + esc(title) + '</h4><p>' + desc + '</p>' +
    (done ? '' : '<div class="gact">' + (actionHTML || '') + '</div>') +
    '</div>' +
    (done
      ? '<svg class="gtick" viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.5 8.5 7 12l5.5-7"/></svg>'
      : '') +
    '</div>';
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
    '# 其他 CLI（codex/gemini 等）\n' +
    '在各自配置文件里把 base_url 指向网关地址即可';
  g.innerHTML =
    '<div class="guide-head"><b>三步完成接线</b>' +
    '<button type="button" class="icon" id="guide-dismiss" title="隐藏引导（可在命令面板重新打开）" aria-label="隐藏引导">' +
    '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>' +
    '</button></div>' +
    '<div class="gsteps">' +
    gstep(1, '添加第一个服务商', 'Key 类：粘贴 Key 检测模型即可；订阅类：先在插件页启用适配，再按指引 CLI 授权导入账号。', done1,
      done1 ? '' : '<button type="button" class="text add" id="guide-add-pv">添加服务商</button>') +
    gstep(2, '把 CLI 客户端指向网关', '复制网关地址写进客户端配置（Claude Code / Trae / codex / gemini 等）。', done2,
      done2 ? '' : '<button type="button" class="text add" id="guide-copy-url">复制网关地址</button>' +
        '<details class="snip-d"><summary>接线示例</summary><pre class="snip">' + esc(snip) + '</pre></details>') +
    gstep(3, '发出第一个请求', '在任意客户端里发一句话；观测页会出现完整路由决策。', done3,
      done3 ? '' : '<button type="button" class="text add" id="guide-go-observe">打开观测</button>') +
    '</div>';
  $('guide-dismiss').addEventListener('click', () => { guide.dismissed = true; renderGuide(); });
  $('guide-add-pv')?.addEventListener('click', () => addProviderModal());
  $('guide-copy-url')?.addEventListener('click', async () => {
    if (await copyText(gwURL(), '已复制网关地址：' + gwURL() + '（粘到客户端配置里）')) {
      guide.wired = true;
      renderGuide();
    }
  });
  $('guide-go-observe')?.addEventListener('click', () => show('observe'));
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
// 首页快捷生成令牌（预览稿定稿）：复用设置页生成弹窗（含 plain 一次性展示）
$('ov-tok-gen').addEventListener('click', () => $('btn-token-gen').click());
$('ov-tok-goto').addEventListener('click', () => gotoSettings('#sec-tokens'));
$('ov-proxy-goto').addEventListener('click', () => gotoSettings('#sec-proxy'));
$('ov-api-links').addEventListener('click', (e) => {
  const b = e.target.closest('[data-apicopy]');
  if (b) copyText(b.dataset.apicopy, '已复制');
});
$('qa-routing').addEventListener('click', () => { show('routing'); });
$('qa-pv-add').addEventListener('click', () => { show('providers'); addProviderModal(); });
$('qa-probe').addEventListener('click', () => probeQuotas());
$('qa-reload').addEventListener('click', async () => {
  try { await reloadConfig(); } catch (e) { status('重载失败: ' + e.message, 'err'); }
});
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

// 胶囊开关通用行为（trace 自动刷新、插件启停共用）
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

// ---------- 多账户管理辅助（2026-11-19）：窗口紧凑行 / 行右列数字 /
// 行首语义点 / 引用统计 / 厂商头健康摘要。数据源 = config 文档 +
// /api/state 运行态快照，全部前端聚合，零后端新增读口。 ----------

// 本机当日（与 Go 侧 UsageDay "2006-01-02" 同形，跨日自动归零）
const todayStr = () => {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
};

// 窗口已用百分比（pct 直给优先，否则 used/total 推导）
const winPctOf = (w) => Math.max(0, Math.min(100, Math.round(
  w.pct != null ? w.pct : (w.total > 0 ? ((w.used || 0) / w.total) * 100 : 0))));

// 账号当日已用 token（usage_day 非当日视为 0，与后端 usedToday 同语义）
const usedTodayOf = (a, lv) =>
  ((lv && lv.usage_day) || a.usage_day) === todayStr() ? ((lv && lv.used_tokens) ?? a.used_tokens ?? 0) : 0;

// winsRowsHTML：订阅窗口紧凑行（保留 planWins 语义：窗口名 + 进度 +
// 百分比 + 已用/总量 + 重置时间；语义色 >80 红 / 60~80 琥珀 / 其余绿）
function winsRowsHTML(windows) {
  if (!Array.isArray(windows) || !windows.length) return '';
  return windows.map((w) => {
    const pct = winPctOf(w);
    const cls = pct > 80 ? 'danger' : pct >= 60 ? 'warn' : 'ok';
    const credits = (w.total > 0 || (w.used || 0) > 0) ? `${fmtTokens(w.used || 0)}/${fmtTokens(w.total || 0)}` : '';
    const reset = fmtResetAt(w.reset_at);
    return `<div class="win"><span class="wlb">${esc(w.name || '额度窗口')}</span>` +
      `<span class="bar"><span class="bar-fill ${cls}" style="width:${pct}%"></span></span>` +
      `<span class="wpct">${pct}%</span>` +
      `${credits ? `<span class="wcr">${esc(credits)}</span>` : ''}` +
      `${reset ? `<span class="wrst">${esc(reset)}</span>` : ''}</div>`;
  }).join('');
}

// acctMetric：行右列关键数字。订阅类=首个窗口 pct；按量类=余额；
// 副行恒为当日用量（周额度/5h 窗口与今日用量双读数并存）。
function acctMetric(a, lv) {
  const used = usedTodayOf(a, lv);
  const sub = `今日 ${fmtTokens(used)}`;
  const w = ((lv && lv.windows) || [])[0];
  if (w) return { val: `${winPctOf(w)}%`, sub };
  if (lv && lv.balance_at) return { val: '¥' + Number(lv.balance || 0).toFixed(2), sub };
  return { val: fmtTokens(used), sub: (a.quota_tokens_day || 0) > 0 ? `额度 ${fmtTokens(a.quota_tokens_day)}` : '今日用量' };
}

// acctDotCls：账号行首语义点。ok=可用 / warn=限频·停服·高负载 /
// bad=探测失败·prefail·凭证失败 / rest=休眠·停用。
function acctDotCls(a, lv, restUntil) {
  if (a.disabled || restUntil) return 'rest';
  if (lv.probe_err) return lv.probe_err.includes('已停服') ? 'warn' : 'bad';
  const st = (lv && lv.status) || a.status;
  if (st === 'bad-credential' || st === 'prefail') return 'bad';
  if (lv.window_status === 'restricted') return 'warn';
  return ((lv && lv.windows) || []).some((w) => winPctOf(w) > 80) ? 'warn' : 'ok';
}

// acctRefs：路由链成员对该账号的显式引用数（"provider/model@account"
// 席位形态）；纯 provider / provider/model 引用走轮转不指向具体账号。
function acctRefs(pid, accName, groups) {
  let n = 0;
  for (const g of groups || []) {
    for (const m of g.members || []) {
      if (m.startsWith(pid + '/') && m.endsWith('@' + accName)) n++;
    }
  }
  return n;
}

// pvHealthHTML：厂商头健康摘要 = 账号数 + 状态点阵 + 分布 + 当日合计/
// 余额合计——多账号时不必逐行扫即可判整体健康。
function pvHealthHTML(p, accs, live, resting) {
  let ok = 0, warn = 0, rest = 0, used = 0, bal = 0;
  const dots = accs.map((a) => {
    const key = `${p.id}/${a.name}`;
    const lv = live[key] || {};
    const cls = acctDotCls(a, lv, resting[key]);
    if (cls === 'ok') ok++;
    else if (cls === 'warn') warn++;
    else rest++;
    used += usedTodayOf(a, lv);
    if (lv.balance_at) bal += Number(lv.balance) || 0;
    return `<i class="${cls}"></i>`;
  }).join('');
  const parts = [`${accs.length} 个账号`];
  if (accs.length) parts.push(`<span class="dots">${dots}</span>`);
  const dist = [];
  if (ok) dist.push(`${ok} 可用`);
  if (warn) dist.push(`${warn} 受限`);
  if (rest) dist.push(`${rest} 休眠/停用`);
  if (dist.length) parts.push(`<span class="sum">${esc(dist.join(' · '))}</span>`);
  if (used > 0) parts.push(`<span class="sum">今日合计 <b>${fmtTokens(used)}</b> tok</span>`);
  if (bal > 0) parts.push(`<span class="sum">余额合计 <b>¥${bal.toFixed(2)}</b></span>`);
  return parts.join(' ');
}

// 账号拖拽握把（仅握把可拖：防误拖行内按钮/文本）
const gripSVG = '<svg class="grip" draggable="true" viewBox="0 0 10 14" width="10" height="14" aria-hidden="true"><g fill="currentColor"><circle cx="2.5" cy="2.5" r="1.4"/><circle cx="7.5" cy="2.5" r="1.4"/><circle cx="2.5" cy="7" r="1.4"/><circle cx="7.5" cy="7" r="1.4"/><circle cx="2.5" cy="11.5" r="1.4"/><circle cx="7.5" cy="11.5" r="1.4"/></g></svg>';

const fmtResetAt = (sec) => {
  if (!sec) return '';
  const d = new Date(sec * 1000);
  const now = new Date();
  const hm = d.toTimeString().slice(0, 5);
  if (d.toDateString() === now.toDateString()) return '今日 ' + hm;
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')} ${hm}`;
};

// effectiveProxy：账号 → 服务商 → 全局 → 系统（F3 继承链），标注生效层。
function effectiveProxy(a, p, global) {
  if (a.proxy) return { v: a.proxy, src: '账号' };
  if (p.proxy) return { v: p.proxy, src: '服务商' };
  if (global) return { v: global, src: '全局' };
  return { v: '系统代理', src: '系统' };
}

const initials = (s) => (String(s || '?').replace(/[^A-Za-z0-9\u4e00-\u9fa5]/g, '').slice(0, 2) || '?').toUpperCase();

// acctRow：账号行（2026-11-19 多账户管理定稿形态）。结构 =
// 握把(拖拽排序) + 状态点 + 身份(名称·主力·徽章·引用) + 订阅窗口紧凑行 +
// 右列关键数字(窗口%/余额·今日用量) + 行悬停动作 + 启停开关。
// idx=0 标「主力」——providers.json 顺序即轮转顺序，首位轮转首选。
function acctRow(p, a, idx, live, resting, global) {
  const key = `${p.id}/${a.name}`;
  const restUntil = resting[key];
  const lv = live[key] || {};
  const view_ = {
    ...a,
    status: lv.status || a.status,
    used_tokens: lv.used_tokens ?? a.used_tokens,
    quota_tokens_day: lv.quota_tokens_day ?? a.quota_tokens_day,
    windows: lv.windows || [],
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
  if (lv.probe_err) {
    // 探测失败不再静默（此前原因只在 serve 日志）：行内直接给出原因；
    // 厂商停服（如硅基流动 /v1/user/info 2026-08-14 下线）是稳态，
    // 用中性琥珀提示而非红色失败
    if (lv.probe_err.includes('已停服')) {
      liveParts.push(`<span class="pill warn" title="余额端点已被厂商下线（410 Gone），等官方替代 API 上线后自动恢复">端点已停服 · ${esc(lv.probe_err)}</span>`);
    } else {
      liveParts.push(`<span class="pill prefail" title="最近一次余额/窗口探测失败：${esc(lv.probe_err)}">探测失败 · ${esc(lv.probe_err)}</span>`);
    }
  }
  if (lv.window_status === 'restricted' && (lv.window_reset_at || 0) * 1000 > Date.now()) {
    liveParts.push(`窗口限频 · 重置 ${esc(new Date(lv.window_reset_at * 1000).toLocaleTimeString())}`);
  }
  if (lv.inflight) liveParts.push(`在途 ${esc(lv.inflight)}`);
  const sub = [credSub, `代理 <code>${esc(eff.v)}</code>（${eff.src}层）`, ...liveParts].join(' · ');
  // 订阅窗口紧凑行（保留周额度/5h 窗口语义）；无窗口但设了当日额度时
  // 退回额度进度行（旧 usageBar 信息不丢）
  const quotaPct = Math.min(100, Math.round(((view_.used_tokens || 0) / view_.quota_tokens_day) * 100));
  const wins = winsRowsHTML(view_.windows) || ((view_.quota_tokens_day || 0) > 0
    ? `<div class="win"><span class="wlb">当日额度</span><span class="bar"><span class="bar-fill ${quotaPct >= 90 ? 'danger' : quotaPct >= 70 ? 'warn' : 'ok'}" style="width:${quotaPct}%"></span></span><span class="wpct">${quotaPct}%</span><span class="wcr">${fmtTokens(view_.used_tokens || 0)}/${fmtTokens(view_.quota_tokens_day)}</span></div>`
    : '');
  const refs = acctRefs(p.id, a.name, cache.providers && cache.providers.groups);
  const m = acctMetric(a, lv);
  const dot = acctDotCls(a, lv, restUntil);
  return `<div class="row acct arow${a.disabled ? ' is-off' : ''}" data-acc="${esc(a.name)}">
    ${gripSVG}
    <span class="adot ${dot}"></span>
    <div class="who">
      <div class="name">${esc(a.name)} ${idx === 0 ? '<span class="pill main">主力</span>' : ''}${nameBadges}${a.disabled ? '<span class="pill resting">已停用</span>' : statusBadge(view_.status)}${refs ? `<span class="pill ref" title="路由链中有 ${refs} 个成员以 ${esc(p.id)}/…@${esc(a.name)} 席位形态显式引用该账号">路由 ×${refs}</span>` : ''}</div>
      <div class="sub">${sub}</div>
      ${wins}
    </div>
    <div class="ametric"><div class="am-val">${esc(m.val)}</div><div class="am-sub">${esc(m.sub)}</div></div>
    <div class="row-acts">
      <button type="button" class="text um-btn" data-p="${esc(p.id)}" data-a="${esc(a.name)}" title="该账号近 7 天按模型用量（成本报告同源）">明细</button>
      <button type="button" class="text quota-btn" data-p="${esc(p.id)}" data-a="${esc(a.name)}">额度</button>
      <button type="button" class="text del del-acct" data-p="${esc(p.id)}" data-a="${esc(a.name)}" title="从 providers.json 移除该账号（写前备份留底）">删除</button>
    </div>
    <button type="button" class="acc-sw${a.disabled ? '' : ' on'}" role="switch" aria-checked="${!a.disabled}" data-p="${esc(p.id)}" data-a="${esc(a.name)}" aria-label="${a.disabled ? '启用' : '停用'}账号 ${esc(a.name)}" title="${a.disabled ? '启用后重新参与路由' : '停用后路由自动跳过该账号（providers.json disabled 字段，热生效）'}"><i></i></button>
  </div>`;
}

// accFolds：展开状态的 provider id 集合（重绘间保留交互态）。
const accFolds = new Set();
const modelOpens = new Set(); // 「模型清单」details 展开态（重绘后保持）
const ACC_VISIBLE = 3; // 超过此数折叠，其余入「展开其余 N 个账号」

function provCard(p, live, resting, global) {
  const accs = p.accounts || [];
  // 行头对齐「厂商 → 服务类型」心智：预设内显示 厂商色瓦片 + 厂商·类型徽章，
  // 预设外回退 协议 pill（自定义中转/自建网关）
  const vb = provVendorBadge(p);
  // 健康摘要（2026-11-19）：账号数 + 状态点阵 + 分布 + 当日合计/余额合计，
  // 多账号时不必逐行扫即可判整体健康（数据全前端聚合）
  const head = `<div class="row prov-head ${p.disabled ? 'is-off' : ''}">
    <span class="vtile v${vb ? vtClass(vb.vendor.key) : vtClass(p.id)}" aria-hidden="true">${esc(initials(vb ? vb.vendor.name : (p.name || p.id)))}</span>
    <div class="who">
      <div class="name">${esc(p.name || p.id)} <code>${esc(p.id)}</code>
        ${vb ? `<span class="pill soft" title="${esc(vb.full)}">${esc(vb.vendor.name)} · ${esc(vb.short)}</span>` : `<span class="pill">${esc(p.protocol)}</span>`}
        ${p.disabled ? '<span class="pill disabled">已停用</span>' : ''}</div>
      <div class="sub pv-health">${pvHealthHTML(p, accs, live, resting)}${p.proxy ? ` · 服务商代理 <code>${esc(p.proxy)}</code>` : ''}</div>
    </div>
    <div class="row-acts">
      <button type="button" class="text edit-pv" data-pv="${esc(p.id)}" title="编辑显示名 / 代理设置（复用添加表单的网络的口径）">编辑</button>
      <button type="button" class="text probe-pv" data-pv="${esc(p.id)}" title="逐账号探测余额 / 订阅额度窗口（POST /api/quota/probe）">探测余额</button>
    </div>
  </div>`;
  const folded = accs.length > ACC_VISIBLE && !accFolds.has(p.id);
  const visible = folded ? accs.slice(0, ACC_VISIBLE) : accs;
  const hidden = folded ? accs.slice(ACC_VISIBLE) : [];
  const rowOf = (a, i) => acctRow(p, a, i, live, resting, global);
  // 账号区包一层导轨容器（左缩进 + 竖导轨 + 行首状态点）：归属层级可读
  let rows = visible.map(rowOf).join('');
  if (hidden.length) {
    rows += `<div class="acc-fold"><div class="acc-fold-inner">${hidden.map((a, i) => rowOf(a, i + ACC_VISIBLE)).join('')}</div></div>`;
  }
  if (!accs.length) {
    rows = '<div class="row acct"><div class="who"><div class="name muted2">无账号</div></div></div>';
  }
  // 折叠按钮在导轨容器外（竖导轨不穿过按钮）
  let more = '';
  if (accs.length > ACC_VISIBLE) {
    more = `<button type="button" class="acc-more" data-fold="${esc(p.id)}" aria-expanded="${!folded}">
      <span>${folded ? `展开其余 ${hidden.length} 个账号` : '收起账号'}</span>
      <svg class="chev" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="m4.5 6.5 3.5 3 3.5-3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
    </button>`;
  }
  // 卡内脚区（2026-11-19）：＋ 添加账号直达追加表单，免绕「添加服务商」
  const foot = `<div class="pv-foot">
    <button type="button" class="text add acc-add" data-p="${esc(p.id)}">＋ 添加账号</button>
    <span class="hint">⠿ 拖动调轮转顺序 · 开关停用即跳过 · 首个为「主力」</span>
  </div>`;
  const mlist = p.models || [];
  const modelChips = mlist.length
    ? `<div class="chips">${mlist.map((m) => `<code>${esc(m.id)}</code>`).join(' ')}</div>`
    : '<div class="models-empty">暂无模型——点「重新检测」拉取上游清单，或在下方手动添加</div>';
  const models = `
    <details class="models-row" data-mrow="${esc(p.id)}" ${modelOpens.has(p.id) ? 'open' : ''}>
      <summary><svg class="chev" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="m6.5 4.5 3 3.5-3 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
        模型清单（${mlist.length} 个）</summary>
      <div class="models-body">
        ${modelChips}
        <div class="models-add">
          <button type="button" class="text add-models-btn" data-p="${esc(p.id)}" title="拉取上游模型清单，点击勾选要添加的模型">添加模型</button>
          <button type="button" class="text reprobe-models" data-p="${esc(p.id)}" title="用已存密钥重新拉取上游清单（手动添加的不在上游清单中的模型会保留）">重新检测</button>
        </div>
      </div>
    </details>`;
  return `<div class="list" data-pid="${esc(p.id)}" style="margin:10px 0">${head}<div class="accs">${rows}</div>${more}${foot}${models}</div>`;
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

// 厂商 → 服务类型（Trae 同构：服务商下拉联动「配置方式」下拉，每个类型
// 对应一个 provider.Presets 条目；单一事实源仍是后端预设表，这里只是
// 分组视图与展示名。OAUTH_VENDORS：官方三家支持订阅会话（OAuth）导入。
const VENDOR_TYPES = [
  { key: 'zhipu', name: '智谱 GLM', types: [
    { preset: 'glm-coding', label: 'Coding Plan（OpenAI 协议）' },
    { preset: 'glm-coding-messages', label: 'Coding Plan（Anthropic 协议）' },
    { preset: 'glm', label: '按量计费（OpenAI 协议）' },
  ] },
  { key: 'zai', name: 'Z.ai', types: [
    { preset: 'zai-coding', label: 'Coding Plan（OpenAI 协议）' },
    { preset: 'zai-coding-messages', label: 'Coding Plan（Anthropic 协议）' },
    { preset: 'zai', label: '按量计费（OpenAI 协议）' },
  ] },
  { key: 'deepseek', name: 'DeepSeek', types: [
    { preset: 'deepseek', label: 'OpenAI 协议' },
    { preset: 'deepseek-messages', label: 'Anthropic 协议' },
  ] },
  { key: 'kimi', name: 'Kimi / Moonshot', types: [
    { preset: 'kimi', label: 'OpenAI 协议' },
    { preset: 'kimi-messages', label: 'Anthropic 协议' },
  ] },
  { key: 'siliconflow', name: 'SiliconFlow 硅基流动', types: [
    { preset: 'siliconflow', label: '标准 API（OpenAI 协议）' },
  ] },
  { key: 'anthropic', name: 'Anthropic 官方', types: [{ preset: 'anthropic', label: '官方 API' }] },
  { key: 'openai', name: 'OpenAI 官方', types: [{ preset: 'openai', label: '官方 API' }] },
  { key: 'gemini', name: 'Google Gemini 官方', types: [{ preset: 'gemini', label: '官方 API' }] },
];
const OAUTH_VENDORS = ['anthropic', 'openai', 'gemini'];

// 厂商反查：provider id（= 预设 id）→ 所属厂商 key / 瓦片形状。
// 同厂商的多个服务类型（glm-coding / glm-coding-messages / glm）由此
// 共享同一厂商色相；预设外的自定义服务商回退按自身 id 散列。
function vendorKeyOf(pid) {
  for (const v of VENDOR_TYPES) {
    if (v.types.some((t) => t.preset === pid)) return v.key;
  }
  return pid;
}
function vendorTileOf(pid) {
  for (const v of VENDOR_TYPES) {
    if (v.types.some((t) => t.preset === pid)) return { cls: vtClass(v.key), txt: vtText(v.name) };
  }
  return { cls: vtClass(pid), txt: vtText(pid) };
}
// 服务商行头徽章：厂商 · 服务类型（短标签）；未命中预设返回 null（回退协议 pill）
function provVendorBadge(p) {
  for (const v of VENDOR_TYPES) {
    const t = v.types.find((t) => t.preset === p.id);
    if (t) return { vendor: v, short: t.label.replace(/（[^）]*）$/, ''), full: t.label };
  }
  return null;
}

// 两段式添加（Trae 同构）：第一步 = 厂商瓦片墙入口；第二步 = 服务商下拉
// 联动「配置方式」下拉（厂商服务类型 → 套用预设），官方三家含订阅账号
// （OAuth）引导项；网络/技术字段收进高级选项。
async function addProviderModal(editID) {
  // 编辑态（2026-11-13）：editID 传入时复用同一两段式表单直进第二步——
  // 预填全部字段、API Key 留空保留（脱敏提示）、模型清单以勾选态预载、
  // ID 锁定；「添加」与「编辑」同构单一心智。
  const editing = editID ? (cache.providers?.providers || []).find((p) => p.id === editID) : null;
  const presets = (await loadPresets()).filter((p) => p.kind !== 'oauth');
  const byId = Object.fromEntries(presets.map((p) => [p.id, p]));
  const vendors = VENDOR_TYPES
    .map((v) => ({ ...v, types: v.types.filter((t) => byId[t.preset]) }))
    .filter((v) => v.types.length);
  const chev = '<svg class="chev" viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 4 4 4-4 4"/></svg>';
  const tile = (v) => {
    const kinds = [...new Set(v.types.map((t) => t.label.replace(/（[^）]*）$/, '')))].join(' · ');
    return `<button type="button" class="ap-tile" data-vendor="${esc(v.key)}">
      <span class="vtile v${vtClass(v.key)}">${esc(vtText(v.name))}</span>
      <span class="ap-tile-txt"><b>${esc(v.name)}</b><span>${esc(kinds)}</span></span>
      ${chev}</button>`;
  };
  const body =
    `<div id="ap-step1">` +
    `<button type="button" class="ap-tile custom" id="ap-custom">` +
    `<span class="vtile v7">＋</span><span class="ap-tile-txt"><b>自定义服务商</b><span>预设外的中转 / 自建网关，手填协议与 Base URL</span></span>${chev}</button>` +
    `<div class="ap-grid">${vendors.map(tile).join('')}</div>` +
    `<p class="ap-note">选厂商进入表单：选服务类型（配置方式）→ 粘贴 API Key → 检测 → 勾选模型 → 添加。订阅账号在「配置方式」里切换为 OAuth。</p>` +
    `</div>` +
    `<div id="ap-step2" hidden>` +
    `<div class="ap-backrow"><button type="button" class="text" id="ap-back">‹ 返回</button><b id="ap-step-title"></b></div>` +
    frow('服务商', `<select class="field" id="ap-vendor">` +
      vendors.map((v) => `<option value="${esc(v.key)}">${esc(v.name)}</option>`).join('') +
      `<option value="custom">自定义服务商…</option></select>`) +
    frow('配置方式', `<select class="field" id="ap-type"></select>`,
      '按厂商服务类型预填协议与端点；官方三家可选订阅账号（OAuth · CLI 导入）') +
    `<div id="ap-api-fields">` +
    `<div id="ap-keywrap">` +
    frow('API Key', `<div class="key-probe"><input class="field mono" name="api_key" type="password" autocomplete="new-password" placeholder="sk-…（仅存本机 providers.json）" required>` +
      `<button type="button" class="text add" id="ap-probe">检测并获取模型</button></div>`) +
    `</div>` +
    `<div class="ap-models" id="ap-models" hidden></div>` +
    `<details class="ap-adv" id="ap-adv" hidden><summary>` +
    `<svg class="chev" viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="m6.5 4.5 3 3.5-3 3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>` +
    `高级选项（网络 / 技术字段，预设已自动填写，一般无需改动）</summary><div class="ap-adv-body">` +
    `<div id="ap-netwrap">` +
    frow('网络', `<div class="net-pick"><select class="field" name="netmode">` +
      `<option value="inherit">跟随全局代理</option>` +
      `<option value="direct">直连（不走代理）</option>` +
      `<option value="custom">自定义代理…</option></select>` +
      `<input class="field mono" name="neturl" placeholder="http(s)/socks5(h)://…" hidden></div>`,
      '全局代理在「总览 → 设置」配置一次；这里只选是否走它') +
    `</div>` +
    frow('服务商 ID', `<input class="field mono" name="id" placeholder="如 deepseek" required>`) +
    frow('显示名', `<input class="field" name="name" placeholder="默认同 ID">`) +
    frow('协议', `<select class="field" name="protocol">` +
      Object.entries(PROTOCOL_LABEL).map(([v, l]) => `<option value="${v}">${v}（${l}）</option>`).join('') +
      `</select>`) +
    frow('Base URL', `<input class="field mono" name="base_url" placeholder="https://api.example.com/v1" required>`, '上游完整前缀；网关按协议追加 /chat/completions、/v1/messages 等') +
    frow('账号名', `<input class="field mono" name="account" value="main" required>`) +
    frow('最大并发', `<input class="field" name="max_concurrent" type="number" min="0" step="1" placeholder="0 = 不限">`) +
    `</div></details>` +
    `</div>` +
    `<div id="ap-oauth-fields" hidden>` +
    `<div class="ap-oauth-card"><b>订阅账号（OAuth）由社区插件适配导入</b>` +
    `<p>Claude / Codex / Gemini 的已登录会话凭证加密存储、到期自动续期；控制台不代登录，导入走 CLI 一次：</p>` +
    `<pre class="snip">yardmaster provider add-oauth anthropic -from claude-code
yardmaster provider add-oauth openai    -from codex
yardmaster provider add-oauth gemini    -from gemini-cli</pre>` +
    `<div class="gact"><button type="button" class="text add" id="ap-go-plugins">去插件页安装适配</button></div></div>` +
    `</div>` +
    `<div class="ap-note" id="ap-note"></div>` +
    `</div>`;
  const dlg = openDialog({
    title: editing ? `编辑服务商 · ${editing.name || editing.id}` : '添加服务商',
    body,
    yes: '添加',
    onSubmit: async (form) => {
      if (!q('#ap-oauth-fields').hidden) {
        throw new Error('订阅账号经插件适配导入：按上方引导走 CLI（provider add-oauth），不走本表单');
      }
      // ---- 编辑分支：同表单写回现服务商（API Key 留空=保留；勾选即清单） ----
      if (editing) {
        const v = formVals(form);
        const key = v.api_key.trim();
        const baseURL = v.base_url.trim();
        if (!baseURL) throw new Error('Base URL 不能为空');
        if (parseInt(v.max_concurrent, 10) < 0) throw new Error('最大并发不能为负');
        let proxy = '';
        if (v.netmode === 'direct') proxy = 'direct';
        else if (v.netmode === 'custom') {
          proxy = (v.neturl || '').trim();
          if (!proxy) throw new Error('选择自定义代理时需填写代理地址');
        }
        const box = q('#ap-models');
        await putProvidersDoc((cur) => {
          const t = (cur.providers || []).find((x) => x.id === editing.id);
          if (!t) throw new Error(`服务商 ${editing.id} 不存在`);
          if (v.name.trim()) t.name = v.name.trim();
          t.protocol = v.protocol;
          t.base_url = baseURL;
          if (proxy) t.proxy = proxy; else delete t.proxy;
          // 单账号：换 Key / 并发（Key 脱敏形或留空 = 保留现值，透传 fresh GET 的脱敏形）
          if ((editing.accounts || []).length === 1 && (t.accounts || [])[0]) {
            const acc = t.accounts[0];
            if (key && !key.startsWith('***')) acc.api_key = key;
            const mc = parseInt(v.max_concurrent, 10);
            if (mc > 0) acc.max_concurrent = mc; else delete acc.max_concurrent;
          }
          // 模型：以勾选为准（保留清单顺序，新增追加；清单区未渲染=不动）
          if (!box.hidden && box.querySelector('input[name=apmodel]')) {
            const chosen = [...box.querySelectorAll('input[name=apmodel]:checked')].map((x) => x.value);
            const have = new Set(chosen);
            const old = (t.models || []).map((m) => m.id);
            t.models = [...old.filter((id) => have.has(id)), ...chosen.filter((id) => !old.includes(id))]
              .map((id) => ({ id }));
          }
          return cur;
        });
        status(`服务商 ${editing.id} 已更新（热生效，写前备份已留底）`, 'ok');
        afterWrite();
        return;
      }
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
  const form = root.querySelector('form');
  const step1 = root.querySelector('#ap-step1');
  const step2 = root.querySelector('#ap-step2');
  const bar = root.querySelector('.dlg-bar');
  const probeBtn = root.querySelector('#ap-probe');
  const noteEl = root.querySelector('#ap-note');
  const adv = root.querySelector('#ap-adv');
  const titleEl = root.querySelector('#ap-step-title');
  const set = (name, val) => { form.elements[name].value = val; };
  const vendorSel = root.querySelector('#ap-vendor');
  const typeSel = root.querySelector('#ap-type');
  const setMode = (mode) => {
    root.querySelector('#ap-api-fields').hidden = mode !== 'api';
    root.querySelector('#ap-oauth-fields').hidden = mode !== 'oauth';
    const yesBtn = root.querySelector('button[type=submit]');
    if (yesBtn) yesBtn.hidden = mode === 'oauth'; // OAuth 无表单可提交：引导取代动作
  };
  const showStep = (n) => {
    step1.hidden = n !== 1;
    step2.hidden = n !== 2;
    bar.hidden = n !== 2; // 第一步是入口选择，无「添加」动作
  };
  const resetProbe = () => {
    const box = root.querySelector('#ap-models');
    box.hidden = true; box.innerHTML = '';
    probeBtn.disabled = false; probeBtn.textContent = '检测并获取模型';
  };
  // 配置方式 → 套用预设 / OAuth 引导 / 自定义手填（Trae 同构的联动核心）
  const applyType = (val) => {
    resetProbe();
    if (val === 'oauth') {
      setMode('oauth');
      const v = VENDOR_TYPES.find((x) => x.key === vendorSel.value);
      titleEl.textContent = v ? `${v.name} · 订阅账号（OAuth）` : '订阅账号（OAuth）';
      noteEl.textContent = '订阅账号由社区插件适配导入（CLI 一次），凭证加密存储、到期自动续期；导入后回服务商页管理。';
      return;
    }
    setMode('api');
    if (val === 'custom') {
      set('id', ''); set('name', ''); set('base_url', '');
      adv.hidden = false; adv.open = true;
      noteEl.textContent = '手动接入：展开高级选项填写协议与 Base URL，粘贴 Key 后检测模型（任意 OpenAI 兼容上游均可）。';
      titleEl.textContent = '自定义服务商接入';
      form.elements.id.focus();
      return;
    }
    const p = byId[val];
    if (!p) return;
    set('id', p.id); set('name', p.name); set('protocol', p.protocol); set('base_url', p.base_url);
    const v = VENDOR_TYPES.find((x) => x.key === vendorSel.value);
    titleEl.textContent = v ? `通过服务商添加 · ${v.name}` : '通过服务商添加';
    const ex = (cache.providers?.providers || []).find((x) => x.id === p.id);
    if (ex) {
      // 预设命中已有服务商：追加备用 Key，账号名可编辑
      set('name', ex.name || ex.id); set('protocol', ex.protocol); set('base_url', ex.base_url);
      adv.hidden = false; adv.open = true;
      noteEl.textContent = `${p.name} 已存在：将向其追加备用 Key，可改账号名后检测添加。`;
    } else {
      adv.hidden = true; adv.open = false; // 纯 Key 预设：技术字段全部自动，不展示
      noteEl.textContent = `已选 ${p.name}：粘贴 API Key → 检测 → 勾选模型 → 添加。`;
    }
  };
  // 服务商 → 重建「配置方式」选项并套用首个类型（官方三家追加 OAuth 项）
  const fillTypes = () => {
    const vk = vendorSel.value;
    if (vk === 'custom') {
      typeSel.innerHTML = '<option value="custom">手动接入（自填协议与 Base URL）</option>';
      applyType('custom');
      return;
    }
    const v = VENDOR_TYPES.find((x) => x.key === vk);
    if (!v) { typeSel.innerHTML = '<option value="custom">手动接入（自填协议与 Base URL）</option>'; applyType('custom'); return; }
    typeSel.innerHTML = v.types.map((t) => `<option value="${esc(t.preset)}">${esc(t.label)}</option>`).join('') +
      (OAUTH_VENDORS.includes(vk) ? `<option value="oauth">订阅账号（OAuth · CLI 导入）</option>` : '');
    applyType(typeSel.value);
  };
  // 厂商瓦片点选 → 进入第二步（下拉联动填充）
  const enterStep2 = (vendorKey) => {
    showStep(2);
    vendorSel.value = VENDOR_TYPES.some((v) => v.key === vendorKey) ? vendorKey : 'custom';
    fillTypes();
  };
  step1.addEventListener('click', (e) => {
    if (e.target.closest('#ap-custom')) { enterStep2('custom'); return; }
    const t = e.target.closest('.ap-tile');
    if (!t) return;
    enterStep2(t.dataset.vendor);
  });
  root.querySelector('#ap-back').addEventListener('click', () => showStep(1));
  vendorSel.addEventListener('change', fillTypes);
  typeSel.addEventListener('change', () => applyType(typeSel.value));
  // OAuth 引导：去插件页（关弹窗并切视图）
  root.querySelector('#ap-go-plugins').addEventListener('click', () => {
    root.closest('.modal-back')?.remove();
    show('plugins');
  });
  // 网络：选「自定义代理」时出现 URL 输入
  const netmode = form.elements.netmode, neturl = form.elements.neturl;
  netmode.addEventListener('change', () => {
    neturl.hidden = netmode.value !== 'custom';
    if (!neturl.hidden) neturl.focus();
  });
  // 检测：验证凭证并拉取模型清单（POST /api/provider/probe；会发一次真实请求）。
  // 编辑态：Key 留空 → provider_id 复测（服务端复用已存密钥）；输入新 Key → 显式检测。
  probeBtn.addEventListener('click', async () => {
    const errEl = root.querySelector('.modal-err');
    if (errEl) errEl.textContent = '';
    const key = form.elements.api_key.value.trim();
    let probeBody;
    if (editing) {
      probeBody = key
        ? { protocol: form.elements.protocol.value, base_url: form.elements.base_url.value.trim(), api_key: key }
        : { provider_id: editing.id };
    } else {
      if (!key) { if (errEl) errEl.textContent = '先粘贴 API Key 再检测'; form.elements.api_key.focus(); return; }
      probeBody = { protocol: form.elements.protocol.value, base_url: form.elements.base_url.value.trim(), api_key: key };
    }
    probeBtn.disabled = true; probeBtn.textContent = '检测中…';
    const box = root.querySelector('#ap-models');
    box.hidden = false;
    box.innerHTML = '<div class="ap-probing">正在验证凭证并拉取模型清单…（密钥错误会在这里报出）</div>';
    try {
      const r = await api('/api/provider/probe', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(probeBody),
      });
      const list = r.models || [];
      // 编辑态：上游清单 ∪ 当前清单（手动添加的不在上游清单中的模型保留）；
      // 默认只勾选当前清单已有的模型——上游全量不默认吸入（用户主动勾选才加）。
      const merged = editing
        ? [...new Set([...list, ...(editing.models || []).map((m) => m.id)])]
        : list;
      const prev = editing ? new Set((editing.models || []).map((m) => m.id)) : new Set();
      if (!merged.length) {
        box.innerHTML = '<div class="ap-probing">上游未返回模型清单（可跳过检测直接添加，模型稍后经「路由」使用时再确认）。</div>';
      } else {
        box.innerHTML =
          '<div class="ap-models-head"><span>支持的模型（' + merged.length + '）</span>' +
          '<button type="button" class="text" data-sel="all">全选</button>' +
          '<button type="button" class="text" data-sel="none">清空</button></div>' +
          merged.map((m) => '<label class="ap-model"><input type="checkbox" name="apmodel" value="' + esc(m) + '"' + (prev.has(m) ? ' checked' : '') + '><code>' + esc(m) + '</code></label>').join('');
        noteEl.textContent = editing
          ? '凭证有效，已拉取 ' + list.length + ' 个模型（含手动添加保留 ' + (merged.length - list.length) + ' 个）——默认只勾选当前清单，按需勾选后点「保存」。'
          : '凭证有效，已拉取 ' + list.length + ' 个模型——勾选要启用的模型，点「添加」完成。';
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
  // ---- 编辑态初始化：直进第二步，预填 + 模型清单勾选态预载 ----
  const patchForEdit = () => {
    const accs = editing.accounts || [];
    const acc = accs.length === 1 ? accs[0] : null;
    set('id', editing.id);
    form.elements.id.readOnly = true;
    set('name', editing.name || editing.id);
    set('protocol', editing.protocol);
    set('base_url', editing.base_url);
    if (acc) {
      form.elements.account.readOnly = true;
      set('account', acc.name);
      set('max_concurrent', String(acc.max_concurrent || 0));
    } else {
      form.elements.account.readOnly = true;
      set('account', `${accs.length} 个账号（各自在账号行管理）`);
      set('max_concurrent', '0');
    }
    const kEl = form.elements.api_key;
    kEl.value = '';
    kEl.required = false; // 编辑态 Key 可留空（保留现值）——否则 HTML5 约束验证拦截提交且无提示
    kEl.placeholder = (acc && acc.api_key)
      ? `已存密钥 ${acc.api_key} · 留空保留，输入新值即更换`
      : '已存密钥（OAuth 凭证型）· 留空保留';
    const pm = !editing.proxy ? 'inherit' : editing.proxy === 'direct' ? 'direct' : 'custom';
    netmode.value = pm;
    neturl.hidden = pm !== 'custom';
    if (pm === 'custom') set('neturl', editing.proxy);
    // 模型清单：当前清单全勾选（取消勾选 = 移除；重新检测可刷新合并）
    const box = root.querySelector('#ap-models');
    const ids = (editing.models || []).map((m) => m.id);
    box.hidden = !ids.length;
    box.innerHTML = ids.length
      ? '<div class="ap-models-head"><span>当前清单（' + ids.length + '）· 取消勾选即移除</span>' +
        '<button type="button" class="text" data-sel="all">全选</button>' +
        '<button type="button" class="text" data-sel="none">清空</button></div>' +
        ids.map((m) => '<label class="ap-model"><input type="checkbox" name="apmodel" value="' + esc(m) + '" checked><code>' + esc(m) + '</code></label>').join('')
      : '';
    probeBtn.textContent = '检测并获取模型';
    noteEl.textContent = '编辑模式：API Key 留空保留现值（输入新值即更换）；ID 与账号名不可改。';
    titleEl.textContent = `编辑服务商 · ${editing.name || editing.id}`;
    root.querySelector('button[type=submit]').textContent = '保存';
  };
  if (editing) {
    showStep(2);
    root.querySelector('#ap-back').hidden = true; // 编辑态没有第一步可回
    const vt = VENDOR_TYPES.find((v) => v.types.some((t) => t.preset === editing.id));
    vendorSel.value = vt ? vt.key : 'custom';
    fillTypes();
    if (vt) { typeSel.value = editing.id; applyType(typeSel.value); }
    patchForEdit();
    return;
  }
  showStep(1);
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

// 添加账号（2026-11-19 多账户管理）：卡内「＋ 添加账号」直达追加表单，
// 免绕「添加服务商」两段式。语义与 addProviderModal 追加分支同：同名
// 账号拒绝、api_key 经服务端 keepProviderKeys 采纳、即时热生效。
// OAuth 订阅账号仍走插件适配 CLI 导入（本表单只管 API Key 类）。
function addAccountModal(provID) {
  const p = (cache.providers?.providers || []).find((x) => x.id === provID);
  const dlg = openDialog({
    title: `添加账号 · ${p?.name || provID}`,
    body:
      frow('账号名', `<input class="field mono" name="account" placeholder="如 backup / work-2" required>`,
        'providers.json 内唯一即可；首位账号 = 主力（轮转首选），可用拖拽调整') +
      frow('API Key', `<input class="field mono" name="api_key" type="password" autocomplete="new-password" placeholder="sk-…（仅存本机 providers.json）" required>`) +
      frow('最大并发', `<input class="field" name="max_concurrent" type="number" min="0" step="1" placeholder="0 = 不限">`) +
      frow('网络', `<div class="net-pick"><select class="field" name="netmode">` +
        `<option value="inherit">跟随服务商/全局代理</option>` +
        `<option value="direct">直连（不走代理）</option>` +
        `<option value="custom">自定义代理…</option></select>` +
        `<input class="field mono" name="neturl" placeholder="http(s)/socks5(h)://…" hidden></div>`,
        '账号级代理优先于服务商与全局（F3 继承链）'),
    yes: '添加',
    footNote: '追加账号即时热生效（写前备份留底）；OAuth 订阅账号经插件适配 CLI 导入。',
    onSubmit: async (form) => {
      const v = formVals(form);
      const name = v.account.trim();
      const key = v.api_key.trim();
      if (!name) throw new Error('账号名不能为空');
      if (!key) throw new Error('API Key 不能为空');
      const acc = { name, api_key: key };
      const mc = parseInt(v.max_concurrent, 10);
      if (mc > 0) acc.max_concurrent = mc;
      if (v.netmode === 'direct') acc.proxy = 'direct';
      else if (v.netmode === 'custom') {
        const u = (v.neturl || '').trim();
        if (!u) throw new Error('选择自定义代理时需填写代理地址');
        acc.proxy = u;
      }
      await putProvidersDoc((cur) => {
        const t = (cur.providers || []).find((x) => x.id === provID);
        if (!t) throw new Error(`服务商 ${provID} 不存在`);
        if ((t.accounts || []).some((a) => a.name === name)) {
          throw new Error(`账号 ${provID}/${name} 已存在（换账号名，或用 CLI provider add -replace 更新凭证）`);
        }
        t.accounts = [...(t.accounts || []), acc];
        return cur;
      });
      status(`账号已添加：${provID}/${name}（热生效，写前备份已留底）`, 'ok');
      await afterWrite();
    },
  });
  dlg.catch(() => {}); // 取消/关闭不算错误
  const form = q('#modal-root .dialog:last-child form');
  form.elements.netmode.addEventListener('change', () => {
    form.elements.neturl.hidden = form.elements.netmode.value !== 'custom';
  });
}

// 启停账号（2026-11-19）：providers.json 账号级 disabled 字段写回 +
// 热重载——planner accountsOf 直接剔除停用账号，余额探测一并跳过。
async function toggleAccount(provID, accName, sw) {
  sw.disabled = true;
  try {
    await putProvidersDoc((cur) => {
      const t = (cur.providers || []).find((x) => x.id === provID);
      const a = t && (t.accounts || []).find((x) => x.name === accName);
      if (!a) throw new Error(`账号 ${provID}/${accName} 不存在`);
      if (a.disabled) delete a.disabled; else a.disabled = true;
      return cur;
    });
    status(`账号 ${provID}/${accName} 已${sw.classList.contains('on') ? '停用，路由将跳过' : '启用，重新参与路由'}（热生效）`, 'ok');
    await afterWrite();
  } catch (err) {
    status('切换失败: ' + err.message, 'err', 8000);
    sw.disabled = false;
  }
}

// ---------- 路由（融合视图：意图 = 一条有序调用链，2026-11 原型定稿） ----------
// 底层逻辑（关键词/正则/优先级）由网关内置，UI 只暴露「意图 → 调用链」
// 一个动作。落地映射（后端契约不变）：每条链 = 隐式组（chain-<意图>）+
// 规则引用；兜底 = 无条件规则 → chain-fallback。CLI 的 groups/rules 完整
// 契约保留；UI 之外的既有规则不受影响（编辑仅改 target）。

// 链徽章策略名与编辑链策略卡（STRATEGIES）同词——同一策略不出现两套叫法
const STRATEGY_LABEL = {
  smart: '智能', order: '顺序', rotate: '轮询',
  usage: '用量均衡', weight: '加权', agent: 'Agent 分配',
};
const ORDER_HINTS = {
  order: '成员顺序即调用顺序：1 号优先，额度用尽或故障才落到下一个',
  smart: '按各账号剩余额度自动分配，勾选顺序不影响分配',
  rotate: '每次请求轮流使用各成员，顺序仅作起始次序',
  usage: '按当日用量均衡分配，谁用得少谁上；顺序不生效',
  weight: '按权重比例分流：权重越高派发越多（缺省 1）；顺序不生效',
  agent: '由 Agent 插件按请求语境自主挑候选（链顺序仅作回落）；需装 Agent 类插件',
};

// 内置意图定义（前端写死，随版本发布；关键词/正则/优先级在网关侧）。
// icon 与预览稿对齐（意图行名前图标；插件/自定义意图无 icon 自然降级）。
// long-context / vision 的网关判定随后续版本落地，先开放配置——提前配
// 链，判定就绪即生效。
const INTENT_DEFS = [
  { id: 'coding', icon: '⌨', label: '编程', desc: '写代码、改代码、评审、测试等开发任务',
    rec: '建议配 2~3 个模型的调用链：多模型自动容灾，额度不会卡死在一个账号上' },
  { id: 'qa', icon: '💬', label: '问答', desc: '知识问答、解释、查资料等轻任务',
    rec: '便宜快速的模型就够；用量大时配一条低价链更省额度' },
  { id: 'writing', icon: '✍️', label: '写作', desc: '文案、文档、润色等长文任务',
    rec: '选输出质量好的模型；写作量大时同样建议配链' },
  { id: 'long-context', icon: '📚', label: '长上下文', desc: '超长文件 / 仓库级分析，优先大窗口模型',
    rec: '选上下文窗口最大的模型；单一模型即可' },
  { id: 'vision', icon: '🖼', label: '图片理解', desc: '带截图或图片输入的任务',
    rec: '必须选支持图片输入的模型——纯文本模型会让请求失败' },
  { id: 'agent', icon: '🤖', label: '智能代理', desc: '语义开放或跨域的请求——由智能代理（Agent 插件）按语境自主选厂商模型处理',
    rec: '链策略配「Agent 分配」；适合不确定该走哪条固定链的请求（需装 Agent 类插件）' },
];

// 意图试算（2026-11-16 用户定稿：独属于智能路由——路由页输入框已移除，
// 仅保留智能代理弹窗内的试算）：POST /api/intent/preview 走真实判定链
//（自定义 L0 → 分类脑/内置 L0 → L1）→ 预览意图与分配。
const INTENT_META = Object.fromEntries(INTENT_DEFS.map((d) => [d.id, d]));

// 成员显示名：'glm/glm-4.6@main' → 'glm-4.6@main'；'deepseek' → 'deepseek'
function memberLabel(m) {
  const at = (m || '').indexOf('@');
  const head = at >= 0 ? m.slice(0, at) : m;
  const slash = head.indexOf('/');
  const body = slash >= 0 ? head.slice(slash + 1) : head;
  return at >= 0 ? body + '@' + m.slice(at + 1) : body;
}

// ---------- D 链路可视化（节点 = 迷你厂商 tile + 模型 + 席位；箭头串联；策略徽章独立） ----------

// 厂商 tile 色相：provider id 稳定散列到 c1..c7（同一厂商恒定同色）
function vtClass(pid) {
  let h = 0;
  const s = String(pid || '');
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return (h % 7) + 1;
}
const vtText = (pid) => (String(pid || '?').replace(/[^A-Za-z0-9]/g, '').slice(0, 2) || '?').toUpperCase();

// 链节点：'glm-coding/glm-5.3-flash@main' → [GL][glm-5.3-flash][@main]；
// 'group/prog' → 组引用节点（⛓ tile，不展开成员）。
function chainNodeHTML(m) {
  if ((m || '').startsWith('group/')) {
    return `<span class="node" title="${esc(m)}"><span class="vt v7">⛓</span><span class="m">${esc(m.slice(6))}</span></span>`;
  }
  const slash = (m || '').indexOf('/');
  const pid = slash >= 0 ? m.slice(0, slash) : m;
  const tail = slash >= 0 ? m.slice(slash + 1) : '';
  const at = tail.indexOf('@');
  const model = at >= 0 ? tail.slice(0, at) : tail;
  const acc = at >= 0 ? tail.slice(at) : '';
  const vt = vendorTileOf(pid);
  return `<span class="node" title="${esc(m)}"><span class="vt v${vt.cls}">${esc(vt.txt)}</span><span class="m">${esc(model || pid)}</span>${acc ? `<span class="o">${esc(acc)}</span>` : ''}</span>`;
}
const CHAIN_ARR = '<svg class="arr" viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M3 8h9m-3.5-3.5L13 8l-4.5 3.5" stroke-linecap="round" stroke-linejoin="round"/></svg>';
function chainHTML(members, strategy) {
  const nodes = (members || []).map((m, i) =>
    chainNodeHTML(m) + (i < members.length - 1 ? CHAIN_ARR : '')).join('');
  const badge = strategy
    ? `<span class="strategy" title="组内候选的分配方式">⚙ ${esc(STRATEGY_LABEL[strategy] || strategy)}</span>` : '';
  return `<span class="chain">${nodes}${badge}</span>`;
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
    const [state, rules, intents, plugViews, provCfg, settings] = await Promise.all([
      api('/api/state'), api('/api/config/rules'), api('/api/config/intents'),
      api('/api/plugins').catch(() => ({ plugins: [] })),
      cache.providers ? Promise.resolve(cache.providers) : api('/api/config/providers'),
      api('/api/config/settings'),
    ]);
    cache.providers = provCfg;
    cache.rules = rules;
    cache.settings = settings;
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

    // Agent 插件就绪态：enabled + granted classify.intent + running。
    // 未就绪时「智能路由」区块整体不显示（判定与分配都依赖插件，
    // 显示一个不可用的入口只会造成困惑）。智能代理与其余意图分开：
    // 主意图列表恒不含 agent——独立区块渲染（含接管开关）。
    cache.agentReady = (plugViews.plugins || []).some((p) =>
      p.enabled && p.status === 'running' && (p.granted || []).includes('classify.intent'));
    const intentDefs = INTENT_DEFS.filter((d) => d.id !== 'agent');

    const rows = intentDefs.map((d) => chainRow(d, chainOf[d.id], provs, groupIDs));
    for (const d of pluginDefs) rows.push(chainRow(d, chainOf[d.id], provs, groupIDs, '插件'));
    for (const d of customDefs) rows.push(chainRow(d, chainOf[d.id], provs, groupIDs, '自定义'));
    rows.push(chainRow(
      { id: 'fallback', icon: '🫥', label: '兜底', desc: '未命中任何意图的请求走这里' },
      chainOf.fallback, provs, groupIDs));
    $('list-intents').innerHTML = rows.join('');
    renderAgentSection(chainOf.agent, plugViews.plugins);
    // 一键分配（Agent 分类插件装好后按意图自动路由的最后一环）：收集
    // 未配置意图，显示批量入口；行内单键在 chainRow 里渲染。
    const unconfigured = [];
    const collect = (d, ch) => {
      const ms = ch?.group ? ch.group.members : (ch?.members || []);
      if (!ms.length) unconfigured.push({ id: d.id, label: d.label || d.id });
    };
    for (const d of intentDefs) collect(d, chainOf[d.id]);
    for (const d of pluginDefs) collect(d, chainOf[d.id]);
    for (const d of customDefs) collect(d, chainOf[d.id]);
    collect({ id: 'fallback', label: '兜底' }, chainOf.fallback);
    cache.pendingIntents = unconfigured;
    $('btn-quick-alloc').hidden = !(unconfigured.length && recMember(provs));
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
  const rec = recMember(provs);
  const via = viaBadge
    ? `<span class="pill oauth" title="${viaBadge === '插件' ? '由插件 ' + esc(d.via) + ' 声明（classify.intent）' : '用户自定义意图（intents.json）'}">${esc(viaBadge)}</span>` : '';
  // 行内有效性标注：节点流之外集中呈现，不打断链路可读性
  const invalid = has ? members.map((m) => memberValid(m, provs, groupIDs)).join('') : '';
  // 意图描述：规则里保存的自定义描述（智能代理弹窗输入）优先于内置文案
  const subDesc = (chain?.rule?.desc || d.desc || '');
  return `<div class="row intent-row"><div class="who">
    <div class="name">${d.icon ? `${esc(d.icon)} ` : ''}${esc(d.label)} <code>${esc(d.id)}</code>${via}${hits ? `<span class="pill soft">${hits} 次/今日</span>` : ''}</div>
    <div class="sub">${esc(subDesc)}${d.via && viaBadge === '插件' ? ' · 由插件 ' + esc(d.via) + ' 提供分类' : ''}</div>
    ${has ? `<div style="margin-top:7px">${chainHTML(members, strategy)}${invalid}</div>` : ''}</div>` +
    (!has ? `<span class="muted2" style="flex:none">未配置</span>` : '') +
    `<div class="row-acts" style="opacity:1">
      ${!has && rec ? `<button type="button" class="text add" data-quick="${esc(d.id)}" data-label="${esc(d.label || d.id)}" title="用推荐模型立即创建调用链（smart 策略，装 Agent 分类插件后即可按意图自动路由；可随时编辑链）">一键分配</button>` : ''}
      <button type="button" class="text add" data-chain="${esc(d.id)}" data-label="${esc(d.label || d.id)}" data-rec="${esc(d.rec || d.desc || '')}">${has ? '编辑链' : '＋ 配置调用链'}</button>
    </div></div>`;
}

// ---------- 智能路由区块（2026-11-16）：智能代理与其余意图分开管理 ----------
// 接管开关（settings.agent_takeover）+ 分类脑就绪态 + 智能代理链配置。
// 开启前置：分类脑已配置（服务端同规校验；未配置时开关禁用并引导）。

// 分类脑当前值：优先启用+授权的 classify 插件视图里的 options.model
function agentBrainOf(views) {
  for (const p of views || []) {
    if (!p.enabled || !(p.granted || []).includes('classify.intent')) continue;
    const m = String((p.options || {}).model || '').trim();
    if (m) return { plug: p.id, model: m };
  }
  return null;
}

function renderAgentSection(chain, views) {
  const sec = $('agent-section'), lbl = $('sec-agent-label');
  if (!cache.agentReady) { sec.hidden = lbl.hidden = true; sec.innerHTML = ''; return; }
  lbl.hidden = sec.hidden = false;
  const takeover = !!(cache.settings && cache.settings.agent_takeover);
  const brain = agentBrainOf(views);
  const has = (chain?.group ? chain.group.members : (chain?.members || [])).length > 0;
  const d = INTENT_META.agent;
  // 行内不展示候选链与 ⚙ 策略徽章（用户定稿 2026-11-16）：接管语义下
  // 分类脑判意图+挑模型，候选池与 agent 策略在弹窗/后台工作，展示徒增误解。
  sec.innerHTML = `<div class="row intent-row"><div class="who">
    <div class="name">${d.icon} ${esc(d.label)} <code>agent</code><span class="pill ${takeover ? 'ok' : 'soft'}">${takeover ? '接管中' : '默认意图链'}</span></div>
    <div class="sub">${esc(d.desc)}${brain ? `；分类脑 <code>${esc(brain.model)}</code>` : '；未配置分类脑——点「编辑链」选择决策模型后可开启接管'}</div></div>` +
    `<label class="switch-line" style="flex:none" title="${brain ? '开启后由分类脑先行判定意图，失败自动回落内置规则' : '需先配置分类脑（点「编辑链」）'}"><button type="button" class="lib-switch${takeover ? ' on' : ''}" id="agent-takeover" role="switch" aria-checked="${takeover}"${brain ? '' : ' disabled'} aria-label="智能路由接管"><i></i></button><span>接管意图判定</span></label>` +
    `<div class="row-acts" style="opacity:1">
      <button type="button" class="text add" data-chain="agent" data-label="${esc(d.label)}">${has ? '编辑链' : '＋ 配置调用链'}</button>
    </div></div>`;
}

// 接管开关 + 区块内编辑链委托（区块 innerHTML 重绘不重复绑定）
$('agent-section').addEventListener('click', async (e) => {
  const edit = e.target.closest('[data-chain]');
  if (edit) { intentChainModal('agent', edit.dataset.label || '智能代理'); return; }
  const sw = e.target.closest('#agent-takeover');
  if (!sw || sw.disabled) return;
  const next = !sw.classList.contains('on');
  sw.disabled = true;
  try {
    await api('/api/config/settings', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agent_takeover: next }),
    });
    if (cache.settings) cache.settings.agent_takeover = next;
    status(next ? '智能路由接管已开启：分类脑先行判定意图（自定义关键词仍最优先，失败自动回落）' : '已切回默认意图链（内置规则判定）', 'ok');
    loadRouting(); // 重渲染区块（接管中/默认意图链 pill）
  } catch (err) {
    status('切换失败: ' + err.message, 'err');
    sw.disabled = false;
  }
});

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

// 添加模型（2026-11-13）：复用添加服务商的「检测 → 勾选」交互（.ap-model
// checkbox chips + 全选/清空）——零手输；上游清单经 provider_id 复测拉取，
// 已在清单中的模型勾选禁用并标记「已添加」。
async function addModelsModal(pid) {
  const p = (cache.providers?.providers || []).find((x) => x.id === pid);
  const dlg = openDialog({
    title: `添加模型 · ${p?.name || pid}`,
    body: '<div class="ap-models" id="am-box"><div class="ap-probing">正在用已存密钥拉取上游模型清单…</div></div>',
    yes: '添加所选',
    footNote: '勾选要加入的模型；已在清单中的不可再选',
    onSubmit: async (form) => {
      const sel = [...form.querySelectorAll('input[name=ammodel]:checked:not(:disabled)')].map((x) => x.value);
      if (!sel.length) throw new Error('未勾选任何新模型');
      await putProvidersDoc((cur) => {
        const t = (cur.providers || []).find((x) => x.id === pid);
        if (!t) throw new Error(`服务商 ${pid} 不存在`);
        const have = new Set((t.models || []).map((m) => m.id));
        t.models = [...(t.models || []), ...sel.filter((m) => !have.has(m)).map((m) => ({ id: m }))];
        return cur;
      });
      status(`已添加 ${sel.length} 个模型到 ${pid}（热生效）`, 'ok');
      await afterWrite();
    },
  });
  dlg.catch(() => {}); // 取消/关闭不算错误
  const root = q('#modal-root .dialog:last-child');
  const box = root.querySelector('#am-box');
  try {
    const r = await api('/api/provider/probe', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ provider_id: pid }),
    });
    const up = r.models || [];
    const have = new Set((p?.models || []).map((m) => m.id));
    box.innerHTML = up.length
      ? `<div class="ap-models-head"><span>上游清单 ${up.length} 个</span>` +
        '<button type="button" class="text" data-sel="all">全选</button>' +
        '<button type="button" class="text" data-sel="none">清空</button></div>' +
        up.map((m) => {
          const exists = have.has(m);
          return `<label class="ap-model"><input type="checkbox" name="ammodel" value="${esc(m)}"${exists ? ' checked disabled' : ''}><code>${esc(m)}</code>${exists ? '<span class="pill soft">已添加</span>' : ''}</label>`;
        }).join('')
      : '<div class="ap-probing">上游未返回模型清单——该端点不支持 /models，无法拉取勾选。</div>';
    root.querySelectorAll('#am-box [data-sel]').forEach((b) => {
      b.onclick = () => root.querySelectorAll('#am-box input[name=ammodel]:not(:disabled)')
        .forEach((x) => { x.checked = b.dataset.sel === 'all'; });
    });
  } catch (e) {
    box.innerHTML = `<div class="ap-probing">拉取失败：${esc(e.message)}</div>`;
  }
}

// 重新检测模型清单（2026-11-13）：POST /api/provider/probe 带 provider_id
// ——服务端按 id 复用已存密钥（前端只有脱敏形）。上游清单 + 保留手动
// 添加的不在上游清单中的模型（与 CLI provider probe 同语义）。
async function reprobeModels(pid) {
  status('正在用已存密钥拉取上游模型清单…（发一次真实请求）');
  try {
    const r = await api('/api/provider/probe', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ provider_id: pid }),
    });
    const up = r.models || [];
    let added = 0, kept = 0;
    await putProvidersDoc((doc) => {
      const p = (doc.providers || []).find((x) => x.id === pid);
      if (!p) throw new Error(`服务商 ${pid} 不存在`);
      const manual = (p.models || []).filter((m) => !up.includes(m.id));
      kept = manual.length;
      added = up.filter((id) => !(p.models || []).some((m) => m.id === id)).length;
      p.models = up.map((id) => ({ id })).concat(manual);
      return doc;
    });
    status(`模型清单已刷新：上游 ${up.length} 个（新增 ${added}）${kept ? `，保留手动添加 ${kept} 个` : ''}`, 'ok', 8000);
    await afterWrite();
    // 预检成功后自动组员体检（2026-11-20）：套餐不含的模型要到对话请求
    // 才暴露——后台实测被组引用的该服务商成员，确定性不可用者自动移出
    // 组（服务端覆写前留底）。不 await：不拖慢清单刷新的完成提示。
    auditGroupMembers(pid);
  } catch (e) { status('重新检测失败: ' + e.message, 'err', 8000); }
}

// 组员体检（2026-11-20）：POST /api/groups/audit——逐成员发最小对话
// 请求实测被组引用的模型，确定性不可用（套餐未开放/无权限/模型不存在）
// 者自动移出组并写回（服务端覆写前留底 + 热重载）。401/429/5xx/超时
// 一律保留。pid 非空时仅体检引用该服务商的成员；空串=全量。
async function auditGroupMembers(pid) {
  status('组员体检中：逐成员发最小请求实测（每个成员约 1~3 秒，确定性不可用者将自动移出组）…');
  try {
    const r = await api('/api/groups/audit', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(pid ? { provider_id: pid } : {}),
    });
    await afterWrite();
    const rep = r.report || {};
    const items = rep.items || [];
    if (!items.length) {
      status(pid ? `${pid} 的成员未被任何路由组引用，无需体检` : '当前没有路由组引用成员模型，无需体检', 'ok', 6000);
      return r;
    }
    const lines = items.map((it) =>
      it.skipped ? `△ ${it.ref} 跳过（${it.skipped}）`
        : it.removed ? `✗ ${it.ref} 已移除（${it.reason}）`
          : it.ok ? `✓ ${it.ref} 可用` : `△ ${it.ref} 保留（${it.reason}）`);
    const head = `组员体检完成：共 ${items.length} 个成员，移除 ${rep.removed || 0} 个`;
    console.log(head + '\n' + lines.join('\n'));
    openDialog({
      title: '组员体检报告',
      body: `<div class="list" style="max-height:50vh;overflow:auto">${lines.map((l) => `<div class="item"><code>${l}</code></div>`).join('')}</div>`,
      yes: '知道了',
      onSubmit: async () => {},
    });
    status(head + (rep.removed ? '，配置已写回（覆写前已留底）' : '，全部保留或跳过'), rep.removed ? 'err' : 'ok', 12000);
    return r;
  } catch (e) { status('组员体检失败: ' + e.message, 'err', 8000); return null; }
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

// 推荐成员（前端启发式）：第一个有模型清单的服务商的第一个模型
function recMember(provs) {
  const p = (provs || []).find((x) => (x.models || []).length);
  return p ? `${p.id}/${p.models[0].id}` : '';
}

// 隐式组 ID（UI 生成的链组统一 chain- 前缀；用户同名组直接复用）
const chainGroupID = (intentId) => 'chain-' + intentId;

// 策略选项卡（G：中文名 + 一句话说明；weight/agent 对齐网关策略枚举）。
// agent：Agent 插件按请求语境自主选模型（route.pick），不可用回落智能。
const STRATEGIES = [
  { k: 'smart', n: '智能', d: '按剩余额度 / 成功率自动选优（推荐）' },
  { k: 'agent', n: 'Agent 分配', d: '由 Agent 插件按请求语境自主选厂商模型；插件不可用回落智能（需装 Agent 类插件）' },
  { k: 'order', n: '顺序', d: '主备优先——按链顺序调用，失败才切下一个' },
  { k: 'rotate', n: '轮询', d: '候选依次轮换，均摊用量' },
  { k: 'usage', n: '用量均衡', d: '优先派给当日用量最少的候选' },
  { k: 'weight', n: '加权', d: '按权重比例分流（缺省权重 1）' },
];

// 调用链编辑器（G 重皮：席位池分组勾选 + 序号化链行 + 策略卡 + 实时链预览；
// 交互语义不变——模型@账号席位，分组头全选/半选，保存 = 隐式组 + 规则双写）。
// 智能代理专属配置弹窗（2026-11-13 二版）：与其他意图不同——无席位池/
// 排序/策略选择。核心是「意图试算」交互：输入一句话 → Agent 分类脑分析
// 意图并预览分配（同首页试算框，POST /api/intent/preview）；候选模型始终
// 与服务商页配置的清单同步（保存时全量重建 chain-agent 成员，运行时由
// Agent 插件按请求语境自主挑选），服务商页增删模型后重新保存即同步。
async function agentChainModal(label) {
  const provs = cache.providers?.providers || [];
  const members = provs.flatMap((p) => (p.models || []).map((m) => `${p.id}/${m.id}`));
  const accTotal = provs.reduce((n, p) => n + (p.accounts || []).length, 0);
  // 分类脑 = classify 插件的 options.model（读当前值，保存时整体写回）。
  // 未配置 → 插件仅内置关键词规则（与握手日志同口径）。视图不可达时按
  // 未配置渲染，不阻断弹窗。
  let brainPlug = null, brainCur = '';
  try {
    const pv = await api('/api/plugins');
    brainPlug = (pv.plugins || []).find((p) => p.enabled && p.status === 'running' && (p.granted || []).includes('classify.intent')) || null;
    brainCur = String((brainPlug && brainPlug.options || {}).model || '');
  } catch { brainPlug = null; }
  const dlg = openDialog({
    title: `智能代理 · 意图配置`,
    body:
      `<div class="pick-hint"><span>与其他意图不同：无需挑选候选模型——候选始终与服务商清单同步，运行时由智能代理（Agent 插件）按请求语境自主挑选最合适的模型</span></div>` +
      frow('分类脑', `<div class="net-pick"><select class="field" name="brain" style="flex:1;min-width:0">` +
        `<option value="">不配置（仅内置关键词规则）</option>` +
        members.map((m) => `<option value="${esc(m)}"${m === brainCur ? ' selected' : ''}>${esc(m)}</option>`).join('') +
        `</select><span class="pill ${brainCur ? 'ok' : 'warn'}">${brainCur ? '已配置' : '未配置'}</span></div>`,
        '读请求、判意图、挑候选的「决策模型」——建议选便宜快速的小模型；调用经网关自身路由（受意图分配与额度管理管辖），key 不出本机。留空则仅用内置关键词规则，语义开放请求将走兜底。') +
      `<div class="label" style="margin-top:12px"><span class="tick"></span><b>意图试算</b><span class="note">输入一句话预览判定与分配；分类脑配置后保存即生效</span></div>` +
      `<div class="intent-try" style="margin:4px 0 2px"><div class="search">` +
      `<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true"><circle cx="7" cy="7" r="4.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m10.5 10.5 3 3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>` +
      `<input type="text" name="agent_try" spellcheck="false" autocomplete="off" placeholder="输入一句话，预览 Agent 的判定与分配（如：帮我把这段话润色成公文）" aria-label="意图试算输入"></div>` +
      `<button type="button" class="btn-primary" id="agent-analyze">分析</button></div>` +
      `<div id="agent-try-result" hidden></div>` +
      `<div class="label" style="margin-top:12px"><span class="tick"></span><b>候选模型（与服务商清单同步）</b><span class="note">${members.length} 个模型 · ${accTotal} 个账号；服务商页增删模型后重新保存即同步</span></div>` +
      `<div class="chips" style="padding:2px 0 4px">${members.map((m) => `<code>${esc(m)}</code>`).join(' ') || '<span class="muted2" style="font-size:12px">清单为空——先在「服务商」页添加并检测模型</span>'}</div>`,
    yes: '保存',
    footNote: '保存 = 候选全量同步 + 分类脑写入（chain-agent 组 · plugins.json · 备份+审计+热生效）',
    onSubmit: async (form) => {
      if (!members.length) throw new Error('没有可用模型——先在「服务商」页添加并检测出模型清单');
      // 组：候选全量同步（strategy=agent；成员不含账号席位=自动展开全部账号）
      await putProvidersDoc((cur) => {
        const groups = cur.groups || [];
        const g = { id: 'chain-agent', strategy: 'agent', members };
        const idx = groups.findIndex((x) => x.id === 'chain-agent');
        if (idx >= 0) groups[idx] = g; else groups.push(g);
        return { ...cur, groups };
      });
      // 规则：指向组（已存在则只刷新指向，不碰用户其他字段）
      const cur = await api('/api/config/rules');
      const rules = cur.rules || [];
      let rule = rules.find((r) => r.intent === 'agent');
      if (!rule) rule = { id: 'intent-agent', intent: 'agent' };
      rule.target = 'group/chain-agent';
      const idx = rules.findIndex((r) => r.id === rule.id);
      if (idx >= 0) rules[idx] = rule;
      else {
        const fb = rules.findIndex((r) => !r.intent && !r.regex && !r.max_tokens && !r.caller_key);
        if (fb >= 0) rules.splice(fb, 0, rule); else rules.push(rule);
      }
      await api('/api/config/rules', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ rules }),
      });
      // 分类脑变更 → 整体写回插件 options（model 换新值，其余键保留）；
      // options 语义变化触发实例重建，新握手把配置透传给插件
      const nextBrain = (form.querySelector('[name=brain]')?.value || '').trim();
      const brainChanged = brainPlug && nextBrain !== brainCur;
      if (brainChanged) {
        await api(`/api/plugins/${encodeURIComponent(brainPlug.id)}/options`, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ...(brainPlug.options || {}), model: nextBrain }),
        });
      }
      status(`智能代理已保存：候选与服务商清单同步（${members.length} 个模型）` +
        (brainChanged ? `；分类脑 → ${nextBrain || '未配置（仅关键词规则）'}` : '') +
        '（热生效，写前备份已留底）', 'ok');
      afterWrite();
      if (brainChanged) {
        // options 变更触发插件实例重建，期间路由页按就绪态门控会暂时隐藏
        // 「智能代理」行——重建完成后延迟重拉一次，行自动回来
        setTimeout(() => { if (!q('#page-routing').hidden) loadRouting(); }, 2500);
      }
    },
  });
  dlg.catch(() => {}); // 取消/关闭不算错误
  // 弹窗内意图试算（POST /api/intent/preview，渲染语义见上方注释）
  const root = q('#modal-root .dialog:last-child');
  // 分类脑选择即时反映状态 pill（已配置/未配置）
  const brainSelEl = root.querySelector('[name=brain]');
  const brainPill = root.querySelector('.net-pick .pill');
  brainSelEl?.addEventListener('change', () => {
    const on = !!brainSelEl.value;
    if (brainPill) {
      brainPill.className = 'pill ' + (on ? 'ok' : 'warn');
      brainPill.textContent = on ? '已配置' : '未配置';
    }
  });
  const analyze = async () => {
    const inp = root.querySelector('input[name=agent_try]');
    const box = root.querySelector('#agent-try-result');
    const text = inp.value.trim();
    if (!text) { box.hidden = false; box.innerHTML = '<div class="ap-probing">先输入一句话再分析</div>'; return; }
    box.hidden = false;
    box.innerHTML = '<div class="ap-probing">智能代理分析中…（判定链：内置规则 → 分类脑，最长约 1.5s）</div>';
    try {
      const r = await api('/api/intent/preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      const meta = INTENT_META[r.intent];
      const title = meta ? `${meta.icon} ${meta.label}` : r.intent ? `❔ ${esc(r.intent)}` : '未判定';
      let detail;
      if (r.intent && (r.members || []).length) {
        detail = `<div style="margin-top:7px">${chainHTML(r.members, '')}` +
          (r.rule ? `<span class="pill soft" style="margin-left:6px">规则 ${esc(r.rule)}</span>` : '') + `</div>` +
          `<div class="sub" style="margin-top:6px">将按此链分配${r.intent === 'agent' ? '；链策略为 Agent 分配时由智能代理自主挑模型' : ''}</div>`;
      } else if (r.intent) {
        detail = `<div class="sub" style="margin-top:6px">意图「${esc(r.intent)}」尚未配置调用链——实际请求将走兜底</div>`;
      } else {
        detail = `<div class="sub" style="margin-top:6px">未能判定意图——实际请求将走兜底链</div>`;
      }
      box.innerHTML = `<div class="row"><div class="who"><div class="name">${title}<span class="pill soft">试算结果</span></div>${detail}</div></div>`;
    } catch (e) {
      box.innerHTML = `<div class="ap-probing">试算失败: ${esc(e.message)}</div>`;
    }
  };
  root.querySelector('#agent-analyze').addEventListener('click', analyze);
  root.querySelector('input[name=agent_try]').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); analyze(); }
  });
}

function intentChainModal(intentId, label, recHint) {
  if (intentId === 'agent') { agentChainModal(label); return; } // 智能代理走专属形态
  const provs = cache.providers?.providers || [];
  const chain = (cache.chainOf || {})[intentId] || {};
  const curStrategy = chain.group?.strategy || 'smart';
  const curMembers = chain.group?.members || chain.members || [];
  // 直连目标（provider/model）进编辑器后转为链成员；group/ 引用不进编辑
  let picked = curMembers.filter((m) => !m.startsWith('group/'));
  let strategy = STRATEGIES.some((s) => s.k === curStrategy) ? curStrategy : 'smart';
  const rec = recMember(provs);
  const dlg = openDialog({
    title: `调用链 · ${label}`,
    body:
      `<div class="pick-hint"><span>${esc(recHint || '未命中任何意图的请求走这里；建议配最稳的模型或链')}</span>` +
      (rec && !picked.includes(rec) ? `<button type="button" class="text add" id="pick-rec">一键加入推荐 ${esc(memberLabel(rec))}</button>` : '') +
      `</div>` +
      `<div class="label"><span class="tick"></span><b>席位池</b><span class="note">勾选加入调用链；分组头全选 / 半选</span></div>` +
      `<div class="pick-provs" id="pick-provs"></div>` +
      `<div class="label"><span class="tick"></span><b>当前链</b><span class="note">按序调用；候选失败自动休眠至重置并切下一候选</span></div>` +
      `<div class="ord-list" id="pick-order"></div>` +
      `<div class="hint" id="pick-order-hint"></div>` +
      `<div class="label"><span class="tick"></span><b>策略</b><span class="note">组内候选的分配方式；编辑后新会话生效，进行中请求保持不变</span></div>` +
      `<div class="strategy-grid" id="pick-strategy"></div>` +
      `<div class="label"><span class="tick"></span><b>链预览</b></div>` +
      `<div class="chain-box" id="chain-preview"></div>`,
    yes: '保存',
    footNote: '保存 = 隐式组 chain-' + esc(intentId) + ' + 规则双写（备份 + 审计 + 热生效）',
    onSubmit: async () => {
      // 整商成员吸收该商细选席位；保持已选顺序；空 = 走兜底
      const wholes = picked.filter((m) => !m.includes('/') && !m.includes('@'));
      const members = picked.filter((m) => !(m.includes('/') && !m.includes('@') && wholes.includes(m.split('/')[0])));
      for (const m of members) {
        const err = memberClientCheck(m, provs, new Set());
        if (err) throw new Error(err);
      }
      await saveIntentChain(intentId, strategy, members);
    },
  });
  dlg.catch(() => {});
  const root = q('#modal-root .dialog:last-child');

  function renderCandidates() {
    root.querySelector('#pick-provs').innerHTML = provs.map((p) => {
      const accs = (p.accounts || []).length ? p.accounts : [{ name: '' }];
      const vals = accs.flatMap((a) => (p.models || []).map((m) => a.name ? `${p.id}/${m.id}@${a.name}` : `${p.id}/${m.id}`));
      const on = vals.filter((v) => picked.includes(v)).length;
      const rows = accs.map((a) => (p.models || []).map((m) => {
        const val = a.name ? `${p.id}/${m.id}@${a.name}` : `${p.id}/${m.id}`;
        const vt = vendorTileOf(p.id);
        return `<label class="pick-row"><input type="checkbox" value="${esc(val)}"${picked.includes(val) ? ' checked' : ''}><span class="vtile inl v${vt.cls}">${esc(vt.txt)}</span><code>${esc(m.id)}${a.name ? `<i class="at">@${esc(a.name)}</i>` : ''}</code></label>`;
      }).join('')).join('');
      const ht = vendorTileOf(p.id);
      return `<div class="pick-group">
        <label class="pick-head"><input type="checkbox" data-selall="${esc(p.id)}"><span class="vtile inl v${ht.cls}">${esc(ht.txt)}</span>${esc(p.name || p.id)}<span class="pill soft">${on}/${vals.length}</span></label>
        ${rows || '<div class="pick-row muted2">清单为空（检测后生成）</div>'}
      </div>`;
    }).join('');
    root.querySelectorAll('#pick-provs input[type=checkbox]:not([data-selall])').forEach((inp) => {
      inp.checked = picked.includes(inp.value);
    });
    syncSelAll(root, picked);
  }
  function renderStrategy() {
    root.querySelector('#pick-strategy').innerHTML = STRATEGIES.map((s) =>
      `<button type="button" class="strategy-opt${strategy === s.k ? ' on' : ''}" data-st="${s.k}">
        <span class="st"><b>${s.n}</b><span class="en">${s.k}</span></span><p>${s.d}</p>
      </button>`).join('');
  }
  function renderPicked() {
    const box = root.querySelector('#pick-order');
    box.innerHTML = picked.length
      ? picked.map((m, i) => `<div class="ord-item">
          <span class="ord-n">${i + 1}</span>${chainNodeHTML(m)}<span style="flex:1"></span>
          <button type="button" class="icon" data-i="${i}" data-d="-1"${i === 0 ? ' disabled' : ''} aria-label="上移"><svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 12V4M4.5 7.5 8 4l3.5 3.5"/></svg></button>
          <button type="button" class="icon" data-i="${i}" data-d="1"${i === picked.length - 1 ? ' disabled' : ''} aria-label="下移"><svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 4v8M4.5 8.5 8 12l3.5-3.5"/></svg></button>
          <button type="button" class="icon" data-x="${i}" aria-label="移出" style="color:var(--red)"><svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg></button>
        </div>`).join('')
      : '<div class="muted2" style="font-size:12px;padding:6px 2px">链为空——保存后该意图回退走兜底</div>';
    root.querySelector('#pick-order-hint').textContent = ORDER_HINTS[strategy] || '';
    root.querySelectorAll('#pick-provs input[type=checkbox]:not([data-selall])').forEach((inp) => {
      inp.checked = picked.includes(inp.value);
    });
    syncSelAll(root, picked);
    renderStrategy();
    root.querySelector('#chain-preview').innerHTML = picked.length
      ? chainHTML(picked, strategy)
      : '<span class="muted2" style="font-size:12px">（空）</span>';
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
    if (b.dataset.x != null) { picked.splice(Number(b.dataset.x), 1); renderPicked(); return; }
    const i = Number(b.dataset.i);
    const j = i + Number(b.dataset.d);
    if (j < 0 || j >= picked.length) return;
    [picked[i], picked[j]] = [picked[j], picked[i]];
    renderPicked();
  });
  root.querySelector('#pick-strategy').addEventListener('click', (e) => {
    const b = e.target.closest('[data-st]');
    if (!b) return;
    strategy = b.dataset.st;
    renderPicked();
  });
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

// 一键分配：用推荐模型为意图立即创建调用链——Agent 分类插件装好后
// 「分类 → 按意图路由」的最后一环。写路径同编辑链（隐式组 + 规则双写，
// 备份 + 审计 + 热生效）。Agent 意图的链默认即 agent 策略（Agent 自主挑模型）。
async function quickAllocate(ids) {
  const provs = cache.providers?.providers || [];
  const rec = recMember(provs);
  if (!rec) throw new Error('没有可用模型——先在「服务商」页添加并检测出模型清单');
  for (const it of ids) {
    await saveIntentChain(it.id, it.id === 'agent' ? 'agent' : 'smart', [rec]);
  }
  status(`已分配 ${ids.length} 条调用链（${memberLabel(rec)}）：${ids.map((i) => i.label).join('、')}`, 'ok', 6000);
  afterWrite();
}

// 路由页操作列事件委托（编辑链 / 一键分配）+ 服务商页额度入口
$('list-intents').addEventListener('click', (e) => {
  const quick = e.target.closest('[data-quick]');
  if (quick) {
    quickAllocate([{ id: quick.dataset.quick, label: quick.dataset.label }])
      .catch((err) => status('一键分配失败: ' + err.message, 'err', 8000));
    return;
  }
  const btn = e.target.closest('[data-chain]');
  if (!btn) return;
  intentChainModal(btn.dataset.chain, btn.dataset.label, btn.dataset.rec);
});
// 组员体检按钮（2026-11-20）：空串=全量审计所有被组引用的成员
$('btn-group-audit').addEventListener('click', () => auditGroupMembers(''));
// 批量一键分配（页头按钮：为全部未配置意图创建推荐链）
$('btn-quick-alloc').addEventListener('click', async () => {
  const list = cache.pendingIntents || [];
  if (!list.length) return;
  const rec = recMember(cache.providers?.providers || []);
  if (!rec) { status('没有可用模型——先在「服务商」页添加并检测出模型清单', 'err', 8000); return; }
  const ok = await confirmModal('一键分配推荐链',
    `将为 ${list.length} 个未配置意图各创建一条调用链（${memberLabel(rec)} · smart 策略，可随时编辑链调整）：\n${list.map((i) => i.label).join('、')}`,
    { yes: '分配', danger: false });
  if (!ok) return;
  try { await quickAllocate(list); } catch (e) { status('一键分配失败: ' + e.message, 'err', 8000); }
});
$('prov-list').addEventListener('click', (e) => {
  const probeBtn = e.target.closest('.probe-pv');
  if (probeBtn) { probeBalanceModal(probeBtn.dataset.pv); return; }
  const ep = e.target.closest('.edit-pv');
  if (ep) { addProviderModal(ep.dataset.pv); return; } // 编辑 = 同一表单的编辑态（复用两段式 UI）
  const rp = e.target.closest('.reprobe-models');
  if (rp) { reprobeModels(rp.dataset.p); return; }
  const amb = e.target.closest('.add-models-btn');
  if (amb) { addModelsModal(amb.dataset.p); return; }
  const btn = e.target.closest('.quota-btn');
  if (btn) { quotaModal(btn.dataset.p, btn.dataset.a); return; }
  const um = e.target.closest('.um-btn');
  if (um) { acctDetailModal(um.dataset.p, um.dataset.a); return; }
  const del = e.target.closest('.del-acct');
  if (del) { removeAccount(del.dataset.p, del.dataset.a); return; }
  // 账号启停开关（2026-11-19）：写 providers.json disabled 字段热生效
  const sw = e.target.closest('.acc-sw');
  if (sw) { toggleAccount(sw.dataset.p, sw.dataset.a, sw); return; }
  // 卡内添加账号（2026-11-19）：直达追加表单
  const addAcc = e.target.closest('.acc-add');
  if (addAcc) { addAccountModal(addAcc.dataset.p); return; }
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
// 账号拖拽排序（2026-11-19）：仅握把可拖，仅同卡 .accs 内重排；落点即
// 写回 providers.json 账号顺序（首个 = 主力 = 轮转首选，rotate/smart/
// order 各策略的账号次序同源）。折叠段账号不参与拖拽但保持原相对次序。
let accDragRow = null;
$('prov-list').addEventListener('dragstart', (e) => {
  const grip = e.target.closest && e.target.closest('.grip');
  if (!grip) { e.preventDefault(); return; }
  accDragRow = grip.closest('.arow');
  if (!accDragRow) { e.preventDefault(); return; }
  accDragRow.classList.add('dragging');
  e.dataTransfer.effectAllowed = 'move';
  try { e.dataTransfer.setData('text/plain', ''); } catch (_) { /* IE 兼容态可空 */ }
});
$('prov-list').addEventListener('dragend', () => {
  if (accDragRow) accDragRow.classList.remove('dragging');
  document.querySelectorAll('.arow.drag-over').forEach((x) => x.classList.remove('drag-over'));
  accDragRow = null;
});
$('prov-list').addEventListener('dragover', (e) => {
  const row = e.target.closest('.arow');
  if (!accDragRow || !row || row === accDragRow || row.parentNode !== accDragRow.parentNode) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'move';
  document.querySelectorAll('.arow.drag-over').forEach((x) => x.classList.remove('drag-over'));
  row.classList.add('drag-over');
});
$('prov-list').addEventListener('drop', async (e) => {
  const row = e.target.closest('.arow');
  if (!accDragRow || !row || row === accDragRow || row.parentNode !== accDragRow.parentNode) return;
  e.preventDefault();
  row.classList.remove('drag-over');
  const a = accDragRow.getBoundingClientRect(), b = row.getBoundingClientRect();
  accDragRow.parentNode.insertBefore(accDragRow, a.top < b.top ? row : row.nextSibling);
  // DOM 重排 → 按新顺序写回账号数组（含折叠段整体次序）
  const accsEl = accDragRow.closest('.accs');
  const pid = accDragRow.closest('.list')?.dataset.pid;
  const names = [...accsEl.querySelectorAll('.arow')].map((r) => r.dataset.acc);
  const dragRow = accDragRow;
  accDragRow = null;
  try {
    await putProvidersDoc((cur) => {
      const t = (cur.providers || []).find((x) => x.id === pid);
      if (!t) throw new Error(`服务商 ${pid} 不存在`);
      const byName = new Map((t.accounts || []).map((x) => [x.name, x]));
      const next = names.map((n) => byName.get(n)).filter(Boolean);
      if (next.length !== (t.accounts || []).length) throw new Error('账号清单已变化，排序取消（请刷新后重试）');
      t.accounts = next;
      return cur;
    });
    status(`账号顺序已保存：首位 ${names[0]} = 主力（热生效）`, 'ok');
    await afterWrite();
  } catch (err) {
    status('排序保存失败: ' + err.message, 'err', 8000);
    dragRow.classList.remove('dragging');
    await loadProviders(); // 回滚到服务端真实顺序
  }
});
// 模型清单展开态（toggle 不冒泡，捕获段接住）
$('prov-list').addEventListener('toggle', (e) => {
  const d = e.target.closest('details[data-mrow]');
  if (!d) return;
  if (d.open) modelOpens.add(d.dataset.mrow); else modelOpens.delete(d.dataset.mrow);
}, true);
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

// ---------- 插件（订阅适配=商店普通条目 + 商店索引 + 信息分层） ----------

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
  'vendor.verdict': '厂商判定',
};

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

// 侧栏插件入口徽章（E：已装插件计数）
function renderPlugBadge(views) {
  const b = $('nav-plug-badge');
  if (!b) return;
  const n = (views || []).length;
  b.hidden = !n;
  b.textContent = n || '';
}

async function loadPlugins() {
  let views = [];
  try {
    const r = await api('/api/plugins');
    views = r.plugins || [];
  } catch (e) {
    $('list-plugins').innerHTML = errBox('插件清单加载失败: ' + e.message, 'plugins');
  }
  renderPlugBadge(views);
  const store = await loadStore();
  renderStore(views, store);
  renderPluginsList(views);
}

function renderStore(views, store) {
  const chips = [''].concat([...new Set(Object.values(PLUGIN_CATS))]);
  $('store-chips').innerHTML = chips.map((c) =>
    '<button type="button" class="cat-chip' + (storeFilter === c ? ' on' : '') + '" data-cat="' + esc(c) + '">' + esc(c || '全部') + '</button>').join('');
  const installed = new Set(views.map((p) => p.id));
  const list = (store || []).filter((s) => !installed.has(s.id) && (!storeFilter || PLUGIN_CATS[s.cap] === storeFilter));
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
  const btn = document.querySelector(`[data-store-install="${CSS.escape(id)}"]`);
  busy(btn, true, '安装中…');
  try {
    await api(`/api/plugin-store/${encodeURIComponent(id)}/install`, { method: 'POST' });
    const entry = (storeCache || []).find((s) => s.id === id);
    if (entry?.cap === 'classify.intent') {
      // 分类类插件装好后的闭环引导：去路由页把意图分配到调用链
      status(`已安装并启用 ${id}（热生效）——去「路由」页为各意图一键分配调用链，即可按意图自动路由`, 'ok', 10000);
    } else {
      status(`已安装并启用 ${id}（热生效）`, 'ok');
    }
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

async function probeQuotas() {
  status('余额探测中…');
  try {
    const r = await api('/api/quota/probe', { method: 'POST' });
    const ps = r.probes || [];
    if (!ps.length) { status('无账号配置余额探测（quota_probe）', '', 5000); return; }
    const fail = ps.filter((p) => p.error && !p.error.includes('已停服'));
    status(`余额探测完成：${ps.length - fail.length}/${ps.length} 成功` +
      (fail.length ? `（失败：${fail.map((p) => p.provider + '/' + p.account).join('、')}）` : ''),
      fail.length ? 'warn' : 'ok', 6000);
    if (!q('#page-overview').hidden) loadOverview();
  } catch (e) {
    status('余额探测失败: ' + e.message, 'err');
  }
}

// ---------- H1 探测余额弹窗（逐账号「检测中 → 结果」行内更新） ----------
// 订阅账号展示双窗口额度、按量账号展示余额；弹窗标注端点与「纯 API ·
// 无需登录」语义；未配置 quota_probe 端点时给引导空态。
function probeResultHTML(hit) {
  if (!hit) return '<span class="pill warn">无结果</span>';
  if (hit.error) {
    // 厂商停服（HTTP 410）是稳态而非故障：琥珀提示，不计失败
    if (hit.error.includes('已停服')) {
      return `<span class="pill warn">端点已停服</span> ${esc(hit.error)}（等官方替代 API）`;
    }
    return `<span class="pill err">失败</span> ${esc(hit.error)}`;
  }
  if ((hit.windows || []).length) {
    return hit.windows.map((w) => {
      const pct = w.pct != null ? Math.round(w.pct)
        : (w.total > 0 ? Math.round(((w.used || 0) / w.total) * 100) : null);
      return `<code>${esc(w.name || '额度窗口')}</code>${pct != null ? ` ${pct}%` : ''}` +
        `（${esc(fmtTokens(w.used))} / ${esc(fmtTokens(w.total))}）` +
        (w.reset_at ? ` · 重置 ${esc(fmtResetAt(w.reset_at))}` : '');
    }).join('<br>');
  }
  if (hit.balance != null) return `余额 <code>¥${Number(hit.balance).toFixed(2)}</code>`;
  return '<span class="pill ok">可达</span>';
}

async function probeBalanceModal(provID) {
  const p = (cache.providers?.providers || []).find((x) => x.id === provID);
  if (!p) return;
  const openGuide = () => openDialog({
    title: `探测余额 · ${p.name || p.id}`,
    body: `<div class="empty-state" style="padding:26px"><b>未配置探测端点</b>在该服务商高级配置里填写 QuotaProbe URL（如 GLM Coding Plan 的 <code>GET /api/monitor/usage/quota/limit</code>），保存后即可一键探测余额与订阅窗口。</div>`,
    yes: '知道了', onSubmit: async () => {},
  }).catch(() => {});
  if (!p.quota_probe?.url) { openGuide(); return; }
  const accs = p.accounts || [];
  if (!accs.length) { openGuide(); return; }
  openDialog({
    title: `探测余额 · ${p.name || p.id}`,
    body: `<p class="ap-note" style="margin-top:0">端点 <code class="num">${esc(p.quota_probe.url)}</code> · Bearer API Key · 纯 API 探测，无需账号登录；key 只进服务端内存</p>
      <div class="list clip">${accs.map((a) => `
        <div class="row" style="min-height:46px">
          <div class="who">
            <div class="name" style="font-size:12.5px">${esc(a.name)}${a.auth === 'oauth' ? ' <span class="pill oauth">OAuth</span>' : ''}</div>
            <div class="sub probe-res" data-acc="${esc(a.name)}">检测中…</div>
          </div>
          <span class="sk-bone" style="width:96px"></span>
        </div>`).join('')}</div>`,
    yes: '完成', onSubmit: async () => {},
  }).catch(() => {});
  try {
    const r = await api('/api/quota/probe', { method: 'POST' });
    const ps = r.probes || [];
    // 弹窗可能已被用户关掉：以 DOM 为准逐行更新（找不到行就静默）
    q('#modal-root').querySelectorAll('.probe-res[data-acc]').forEach((el) => {
      const hit = ps.find((x) => x.provider === provID && x.account === el.dataset.acc);
      el.innerHTML = probeResultHTML(hit);
      el.closest('.row')?.querySelector('.sk-bone')?.remove();
    });
    const okN = ps.filter((x) => x.provider === provID && !x.error).length;
    status(`余额探测完成：${okN}/${accs.length} 个账号`, okN ? 'ok' : 'warn', 6000);
    if (!q('#page-overview').hidden) loadOverview();
  } catch (e) {
    q('#modal-root').querySelectorAll('.probe-res[data-acc]').forEach((el) => {
      el.innerHTML = `<span class="pill err">探测失败</span> ${esc(e.message)}`;
      el.closest('.row')?.querySelector('.sk-bone')?.remove();
    });
  }
}

// ---------- 命令面板（A1：Ctrl+K；Magpie .pop 构造裁剪版） ----------
// 视图跳转 + 高频动作；↑↓ 选择 / Enter 执行 / Esc 关闭。零新增 API 面
// （动作全部复用既有处理器）。
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
