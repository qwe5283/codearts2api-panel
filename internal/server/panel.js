(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const KEY_STORAGE = 'codearts2api-panel.admin-key';
  const THEME_STORAGE = 'codearts2api-panel.theme';
  const pages = {
    accounts: ['账号池', '管理凭据、查看账户状态与额度。'],
    balance: ['余额趋势', '按账号查看 CREDIT 积分水位与变化。'],
    models: ['模型目录', '查看模型通道与能力，复制 ID 接入客户端。'],
    usage: ['请求用量', '当前进程的请求指标与近期用量。'],
    config: ['服务配置', '管理网关配置，保存即热生效；装配期字段需重启。'],
    logs: ['运行日志', '检查最近请求、错误与服务运行状态。']
  };
  const readStorage = (storage, name) => { try { return storage.getItem(name) || ''; } catch { return ''; } };
  const writeStorage = (storage, name, value) => { try { value ? storage.setItem(name, value) : storage.removeItem(name); } catch { /* Session can still run in memory. */ } };
  let adminKey = readStorage(sessionStorage, KEY_STORAGE);
  let authenticated = false, authEpoch = 0, refreshID = 0, overviewInFlight = false;
  let currentPage = 'accounts', accounts = [], models = [], runtimeLogs = [], requestRows = [], requestRowsFromArchive = false;
  let visionBridgeEnabled = false;
  let requestAttemptErrors = {}; // request_id → 最后一次上游错误（内存态，仅用于 tooltip）
  let oauthSession = '', oauthTimer = null, oauthBusy = false, deleteUID = '', noteUID = '', logsLoading = false;
  let serverStartedAt = 0; // 由 overview 的 uptime_sec 换算，本地时钟每 30 秒刷新顶栏
  let theme = readStorage(localStorage, THEME_STORAGE) || (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');

  function esc(value) {
    return String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }
  function number(value) {
    return value !== null && value !== undefined && Number.isFinite(Number(value)) ? Number(value).toLocaleString('zh-CN', { maximumFractionDigits: 3 }) : '未知';
  }
  function duration(value) {
    if (value === null || value === undefined || !Number.isFinite(Number(value))) return '—';
    return Number(value) < 1000 ? Math.round(Number(value)) + ' ms' : (Number(value) / 1000).toFixed(1) + ' s';
  }
  function date(value) {
    if (!value) return '未知';
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? '未知' : parsed.toLocaleString('zh-CN', { hour12: false });
  }
  // 顶栏运行时长，格式与 WorkBuddy 面板一致：运行 3 天 5 时 12 分。
  function uptimeText(ms) {
    const up = Math.max(0, Math.floor(ms / 1000));
    return '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
  }
  function renderUptime() {
    if (serverStartedAt) $('uptime').textContent = uptimeText(Date.now() - serverStartedAt);
  }
  function emptyRow(columns, text) { return '<tr><td class="empty" colspan="' + columns + '">' + esc(text) + '</td></tr>'; }
  function errorText(data, status) {
    if (typeof data.error === 'string') return data.error;
    return data.error?.message || data.message || '请求失败（HTTP ' + status + '）';
  }
  function toast(message, failed = false) {
    const el = document.createElement('div');
    el.className = 'toast' + (failed ? ' error' : '');
    el.textContent = message;
    $('toasts').append(el);
    setTimeout(() => el.remove(), 6000);
  }
  function fieldError(id, message) { $(id).textContent = message; $(id).hidden = !message; }
  function showLogin(message = '') {
    authenticated = false;
    $('connection').textContent = '尚未连接';
    $('connection').classList.remove('online');
    ['addDialog', 'deleteDialog'].forEach(id => { if ($(id).open) $(id).close(); });
    fieldError('loginError', message);
    if (!$('loginDialog').open) $('loginDialog').showModal();
    $('loginKey').focus();
  }
  async function api(path, options = {}, keyOverride) {
    const epoch = authEpoch;
    const headers = new Headers(options.headers || {});
    const key = keyOverride === undefined ? adminKey : keyOverride;
    if (key) headers.set('Authorization', 'Bearer ' + key);
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    const response = await fetch('/admin/api/' + path, { ...options, headers, cache: 'no-store', credentials: 'same-origin' });
    if (epoch !== authEpoch) throw new Error('会话已结束');
    const data = await response.json().catch(() => ({}));
    if (response.status === 401 || response.status === 403) {
      if (keyOverride === undefined) { adminKey = ''; writeStorage(sessionStorage, KEY_STORAGE, ''); showLogin('管理密钥无效，请重新连接。'); }
      throw new Error('管理密钥无效或权限不足。');
    }
    if (!response.ok) throw new Error(errorText(data, response.status));
    return data;
  }
  const post = (path, body = {}) => api(path, { method: 'POST', body: JSON.stringify(body) });
  function applyTheme() {
    document.documentElement.dataset.theme = theme;
    // 图标指示「当前已是哪个主题」：深色显示太阳，浅色显示月亮。
    $('themeIconSun').hidden = theme !== 'dark';
    $('themeIconMoon').hidden = theme !== 'light';
    $('themeButton').setAttribute('aria-label', theme === 'dark' ? '切换浅色主题' : '切换深色主题');
  }
  $('themeButton').onclick = () => { theme = theme === 'dark' ? 'light' : 'dark'; writeStorage(localStorage, THEME_STORAGE, theme); applyTheme(); };
  applyTheme();
  function menu(open) {
    $('sidebar').classList.toggle('open', open);
    $('navBackdrop').hidden = !open;
    $('menuButton').setAttribute('aria-expanded', String(open));
  }
  $('menuButton').onclick = () => menu(!$('sidebar').classList.contains('open'));
  $('navBackdrop').onclick = () => menu(false);
  function navigate() {
    const requested = location.hash.slice(1);
    currentPage = Object.hasOwn(pages, requested) ? requested : 'accounts';
    const [title, description] = pages[currentPage];
    $('pageTitle').textContent = title; $('pageDescription').textContent = description; $('breadcrumb').textContent = '工作空间 / ' + title;
    document.querySelectorAll('.view').forEach(el => { el.hidden = el.id !== 'view-' + currentPage; });
    document.querySelectorAll('[data-page]').forEach(el => { const active = el.dataset.page === currentPage; el.classList.toggle('active', active); active ? el.setAttribute('aria-current', 'page') : el.removeAttribute('aria-current'); });
    menu(false);
    if (authenticated) refresh();
  }
  addEventListener('hashchange', navigate);
  function renderOverview(data) {
    const uptimeSec = Number(data.uptime_sec);
    if (Number.isFinite(uptimeSec)) {
      serverStartedAt = Date.now() - uptimeSec * 1000;
      $('uptime').title = '服务启动于 ' + date(data.started_at);
      renderUptime();
    }
    accounts = data.accounts || [];
    const stats = data.stats || {};
    [['statTotal', 'total'], ['statHealthy', 'healthy'], ['statCooling', 'cooling'], ['statDisabled', 'disabled'], ['statCredits', 'credits'], ['statSticky', 'sticky']].forEach(([id, name]) => { $(id).textContent = number(stats[name]); });
    $('accountCount').textContent = number(accounts.length);
    $('connection').textContent = stats.healthy > 0 ? '服务正常' : accounts.length ? '暂无可用账户' : '等待添加账户';
    $('connection').classList.add('online');
    renderAccounts();
  }
  function balanceCell(balance, field, error) {
    if (!balance || balance[field] === null || balance[field] === undefined) return '<span class="muted">未知</span>' + (error ? '<small class="account-error" title="' + esc(error) + '">查询失败</small>' : '');
    return '<span class="number">' + esc(number(balance[field])) + '</span>';
  }
  function renderAccounts() {
    const query = $('accountSearch').value.toLowerCase().trim();
    const filtered = accounts.filter(a => [a.nickname, a.user_name, a.note, a.uid, a.name].join(' ').toLowerCase().includes(query));
    $('accountsBody').innerHTML = filtered.map(a => {
      const uid = String(a.uid || a.name || '');
      const state = a.disabled ? ['bad', '已禁用'] : a.cooling ? ['warn', '冷却中'] : ['ok', '可用'];
      const button = (action, label, danger = false) => '<button class="button small' + (danger ? ' danger' : '') + '" data-account-action="' + action + '" data-uid="' + esc(uid) + '">' + label + '</button>';
      // 签到按自然日只领一次：了结后按钮变「已签」，点击只是回放本地结果，不再打上游。
      const checkinButton = a.checkin_done
        ? '<button class="button small done" data-account-action="checkin" data-uid="' + esc(uid) + '" title="' + esc((a.checkin && a.checkin.message ? a.checkin.message + ' · ' : '') + '今日已签到，每天只领一次') + '">已签</button>'
        : button('checkin', '签到');
      const expiry = a.permanent === true || a.credential_type === 'permanent' ? '永久凭据' : date(a.expires_at);
      // 备注是纯本地字段（如注册手机号），显示在账户名下面一行。
      const noteLine = a.note ? '<span class="account-note" title="' + esc(a.note) + '">' + esc(a.note) + '</span>' : '';
      return '<tr><td><span class="account-name">' + esc(a.nickname || a.user_name || a.name || '未命名账户') + '</span>' + noteLine + '<span class="account-id mono">' + esc(uid) + '</span>' + (a.last_error ? '<span class="account-error" title="' + esc(a.last_error) + '">' + esc(a.last_error) + '</span>' : '') + '</td><td><span class="pill ' + state[0] + '">' + state[1] + '</span></td><td>' + balanceCell(a.credit_balance, 'total', a.credit_balance_error) + '</td><td><span class="muted">' + esc(expiry) + '</span></td><td class="number">' + esc(number(a.active_concurrent)) + ' / ' + esc(number(a.max_concurrent)) + '</td><td><div class="row-actions">' + button('credits', '查额度') + checkinButton + button('keepalive', '保活') + button(a.disabled ? 'enable' : 'disable', a.disabled ? '启用' : '禁用') + (a.cooling ? button('clear-cooldown', '清冷却') : '') + button('note', '备注') + button('delete', '删除', true) + '</div></td></tr>';
    }).join('') || emptyRow(6, query ? '没有匹配的账户。' : '还没有账户。点击“添加账户”完成首次接入。');
  }
  function actionMessage(data, fallback) {
    const failed = Array.isArray(data.results) ? data.results.filter(r => r.ok === false) : [];
    const details = failed.map(r => (r.uid || '') + '：' + (r.message || r.credit_balance_error || '操作失败')).join('；');
    return [data.message || fallback, details].filter(Boolean).join(' · ');
  }
  async function accountAction(action, uid, button) {
    if (action === 'note') {
      noteUID = uid;
      const account = accounts.find(a => String(a.uid || a.name) === uid);
      $('noteAccountName').textContent = (account?.nickname || account?.user_name || '账户') + ' · ' + uid;
      $('noteInput').value = account?.note || '';
      fieldError('noteError', '');
      $('noteDialog').showModal();
      return;
    }
    if (action === 'delete') {
      deleteUID = uid;
      const account = accounts.find(a => String(a.uid || a.name) === uid);
      $('deleteAccountName').textContent = (account?.nickname || account?.user_name || '账户') + ' · ' + uid;
      $('deleteDialog').showModal();
      return;
    }
    button.disabled = true;
    try {
      const route = ['enable', 'disable', 'clear-cooldown'].includes(action) ? 'accounts/' + action : action;
      const data = await post(route, { uid, ...(action === 'disable' ? { reason: '面板手动禁用' } : {}) });
      toast(actionMessage(data, '操作完成'), data.ok === false);
      renderOverview(await api('overview'));
    } catch (error) { toast(error.message, true); }
    finally { button.disabled = false; }
  }
  $('accountsBody').onclick = event => { const button = event.target.closest('[data-account-action]'); if (button) accountAction(button.dataset.accountAction, button.dataset.uid, button); };
  $('accountSearch').oninput = renderAccounts;
  document.querySelectorAll('[data-bulk]').forEach(button => { button.onclick = async () => {
    button.disabled = true;
    try { const data = await post(button.dataset.bulk); toast(actionMessage(data, '操作完成'), data.ok === false); renderOverview(await api('overview')); }
    catch (error) { toast(error.message, true); }
    finally { button.disabled = false; }
  }; });
  $('confirmDelete').onclick = async () => {
    if (!deleteUID) return;
    $('confirmDelete').disabled = true;
    try { const data = await post('accounts/delete', { uid: deleteUID }); if (data.ok === false) throw new Error(data.message || '删除失败'); $('deleteDialog').close(); toast('账户已删除'); renderOverview(await api('overview')); }
    catch (error) { toast(error.message, true); }
    finally { $('confirmDelete').disabled = false; }
  };
  $('deleteDialog').addEventListener('close', () => { deleteUID = ''; $('deleteAccountName').textContent = ''; });
  $('confirmNote').onclick = async () => {
    if (!noteUID) return;
    $('confirmNote').disabled = true;
    try {
      const data = await post('accounts/note', { uid: noteUID, note: $('noteInput').value.trim() });
      if (data.ok === false) throw new Error(data.message || '保存失败');
      $('noteDialog').close();
      toast('备注已保存');
      renderOverview(await api('overview'));
    } catch (error) { fieldError('noteError', error.message); }
    finally { $('confirmNote').disabled = false; }
  };
  $('noteDialog').addEventListener('close', () => { noteUID = ''; $('noteAccountName').textContent = ''; $('noteInput').value = ''; });

  function renderModels() {
    const query = $('modelSearch').value.toLowerCase().trim();
    const filtered = models.filter(m => (String(m.id) + ' ' + (m.model_name || '')).toLowerCase().includes(query));
    $('modelCount').textContent = filtered.length;
    $('modelsBody').innerHTML = filtered.map(m => {
      const name = m.model_name ? '<small class="model-subtitle">' + esc(m.model_name) + '</small>' : '';
      // 模型 ID 列的原生 tooltip：模型描述，有 notice 时追加一行。
      const tip = [m.model_desc, m.notice].filter(Boolean).join('\n');
      const title = tip ? ' title="' + esc(tip).replace(/\n/g, '&#10;') + '"' : '';
      // 徽标行：上游标签（紫）、被隐藏的模型（橘）、输入模态（绿＝视觉理解 / 灰＝纯文本）。
      // 上游只在支持图片时才回 supports_images，因此「不是 true」就是纯文本；
      // 纯文本模型在视觉转述启用时再挂黄色「外置视觉理解」，与原生视觉能力区分。
      const badges = [
        m.model_tag ? '<span class="pill tag">' + esc(m.model_tag) + '</span>' : '',
        m.display_enabled === false ? '<span class="pill warn">辅助模型</span>' : '',
        m.supports_images === true ? '<span class="pill ok">视觉理解</span>' : '<span class="pill">纯文本</span>',
        m.supports_images !== true && visionBridgeEnabled ? '<span class="pill warn">外置视觉理解</span>' : ''
      ].filter(Boolean).join('');
      const badgeRow = badges ? '<div class="model-badges">' + badges + '</div>' : '';
      return '<tr><td class="mono"' + title + '>' + esc(m.id) + name + badgeRow + '</td><td>' + (m.ratio_display ? esc(m.ratio_display) : '<span class="muted">未知</span>') + '</td><td><span class="pill">常规模型</span></td><td class="number">' + esc(number(m.context_length ?? m.context_window)) + '</td><td class="number">' + esc(number(m.max_output_tokens ?? m.max_tokens)) + '</td><td><button class="button small" data-copy-model="' + esc(m.id) + '">复制 ID</button></td></tr>';
    }).join('') || emptyRow(6, query ? '没有符合条件的模型。' : '模型目录为空，请添加账户后刷新。');
  }
  $('modelSearch').oninput = renderModels;
  $('modelsBody').onclick = async event => {
    const button = event.target.closest('[data-copy-model]');
    if (!button) return;
    try { await navigator.clipboard.writeText(button.dataset.copyModel); toast('模型 ID 已复制'); } catch { toast('浏览器无法访问剪贴板，请选中模型 ID 复制。', true); }
  };
  function eventOK(event) { return event.ok === true || (event.ok === undefined && event.status >= 200 && event.status < 300 && event.outcome !== 'stream_error'); }
  // 只读归档里的 error 原文，历史记录也按同一套规则分类。
  // 只按底层原因分类：外层「重试耗尽/无可用账号」与内层错误同时出现时，以内层为准。
  // 重试次数已在「尝试次数」列展示，不再冒充失败原因；无法识别的错误保留「异常」。
  function failureKind(row) {
    if (eventOK(row)) return '';
    const text = String(row.error || '').toLowerCase();
    if (String(row.outcome || '').toLowerCase() === 'interrupted' ||
        /context canceled|context cancelled|client disconnected|client canceled|client cancelled/.test(text)) return 'cancel';
    if (/not multimodal|inferhub\.001001020/.test(text)) return 'capability';
    if (/not registered|not in the model catalog|model .*not available|not available on any configured account/.test(text)) return 'missing';
    // 4291 是业务码，不是 HTTP 429：福利额度耗尽不能当成可重试的瞬时排队。
    if (/inferhub\.4291\./.test(text)) return 'quota';
    if (/tm\.00001041|81111\.429|tpm limit|concurren|并发|(?:code|http)=429/.test(text)) return 'queue_limit';
    if (/apig\.0203|backend timeout|deadline exceeded|i\/o timeout|(?:code|http)=(?:408|504)/.test(text)) return 'upstream_timeout';
    if (/token invalid|token (?:expired|expiring)|(?:token|credential).{0,40}expired|expired.{0,40}(?:token|credential)|re-login required|unauthorized|forbidden|invalid signature|apig\.0301|apig\.0602|(?:code|http)=40[13]/.test(text)) return 'auth';
    if (/(?:code|http)=5\d\d/.test(text)) return 'upstream_error';
    // 参数/多模态内容/工具选择错误：换账号或重试也不会改变结果。
    if (/invalid_request|upstream rejected request params|request param is invalid|inferhub\.001001005|inferhub\.001001046|messages is empty|tool_choice function|invalid content block|unsupported content block|content must be a string|text block requires text|image_url (?:must be|requires)|max_tokens must|temperature must|top_p must|no user or tool message|parse request|request body exceeds/.test(text)) return 'invalid_request';
    if (/all accounts unavailable|no healthy account|no available account|no accounts configured|upstream attempts exhausted after 0 attempt/.test(text)) return 'no_account';
    return 'abnormal';
  }
  const FAILURE_BADGES = {
    cancel: ['客户端取消', 'warn'],
    missing: ['模型不存在', 'warn'],
    no_account: ['无可用账号', 'warn'],
    capability: ['模型能力不符', 'warn'],
    queue_limit: ['上游排队/限流', 'warn'],
    quota: ['福利额度耗尽', 'warn'],
    auth: ['账号鉴权失败', 'warn'],
    invalid_request: ['请求参数错误', 'warn'],
    upstream_timeout: ['上游超时', 'warn'],
    upstream_error: ['上游服务异常', 'warn'],
    abnormal: ['异常', 'bad'],
  };
  function failureBadge(row) {
    const kind = failureKind(row);
    if (!kind) return '';
    const [label, tone] = FAILURE_BADGES[kind];
    return '<span class="pill ' + tone + '">' + label + '</span>';
  }
  // 异常筛选只留没归进已知失败分支的请求；成功行没有失败原因，不会被选中。
  function matchesOutcomeFilter(row, filter) {
    if (filter === 'abnormal') return failureKind(row) === 'abnormal';
    return eventOK(row) === (filter === 'ok');
  }
  function tokenTotal(event) {
    if (event.total_tokens !== undefined && event.total_tokens !== null) return Number(event.total_tokens) || 0;
    return (Number(event.prompt_tokens) || 0) + (Number(event.completion_tokens) || 0);
  }
  // 进程指标随重启清零，归档（data/requests）才是跨重启的账本：两者合并去重后展示，
  // 归档里还没落盘的新请求也照样出现在列表里。没有归档时才退回纯进程数据。
  function usageView(data) {
    const live = Array.isArray(data.recent) ? data.recent : [];
    const archived = data.history && Array.isArray(data.history.recent) ? data.history.recent : [];
    if (!archived.length) return { fromArchive: false, recent: live };
    const seen = new Set(), merged = [];
    for (const row of [...live, ...archived]) {
      const key = row.request_id || [row.time, row.model, row.status, row.duration_ms].join('|');
      if (seen.has(key)) continue;
      seen.add(key); merged.push(row);
    }
    merged.sort((a, b) => new Date(b.time) - new Date(a.time));
    return { fromArchive: true, recent: merged };
  }
  // —— 请求用量 ——
  // 数据由服务端按窗口聚合（/admin/api/usage?window=1&from&to）：30 天的归档可能几十万条，
  // 逐条拉回浏览器既慢又没人看；这里只负责选窗口、发查询、渲染聚合结果。
  const USAGE_RANGES = { today: '今天', '24h': '近 24 小时', '3d': '近 3 天', '7d': '近 7 天', '30d': '近 30 天', custom: '自定义' };
  const USAGE_HOURS = { '24h': 24, '3d': 72, '7d': 168, '30d': 720 };
  let usageRange = '30d', usageCustom = { from: '', to: '' };
  function usageWindow() {
    const now = new Date();
    if (usageRange === 'custom') {
      const parse = value => { const at = value ? new Date(value) : null; return at && !Number.isNaN(at.getTime()) ? at : null; };
      // 两端都留空＝不设界（等同「全部」）；只填一端就只卡那一端。
      return { from: parse(usageCustom.from), to: parse(usageCustom.to), label: USAGE_RANGES.custom };
    }
    if (usageRange === 'today') {
      const start = new Date(now); start.setHours(0, 0, 0, 0);
      return { from: start, to: now, label: USAGE_RANGES.today };
    }
    const hours = USAGE_HOURS[usageRange] || USAGE_HOURS['30d'];
    return { from: new Date(now.getTime() - hours * 3600e3), to: now, label: USAGE_RANGES[usageRange] || USAGE_RANGES['30d'] };
  }
  // datetime-local 只认本地时间串，toISOString 是 UTC，会差出时区那几个小时。
  function localInputValue(date) {
    const pad = value => String(value).padStart(2, '0');
    return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate()) + 'T' + pad(date.getHours()) + ':' + pad(date.getMinutes());
  }
  // 窗口交给服务端算：from/to 用绝对时刻（RFC3339），两边不用各自解释一遍时间。
  function usageQuery() {
    const win = usageWindow(), parts = ['window=1'];
    if (win.from) parts.push('from=' + encodeURIComponent(win.from.toISOString()));
    if (win.to) parts.push('to=' + encodeURIComponent(win.to.toISOString()));
    return parts.join('&');
  }
  function renderUsage(stats) {
    const s = stats || {}, win = usageWindow(), label = win.label;
    // 控件始终跟着状态走：刷新、切页、登出后再回来，下拉框不会和实际生效的范围对不上。
    $('usageRange').value = usageRange;
    $('usageCustom').hidden = usageRange !== 'custom';
    const completed = Number(s.completed) || 0;
    const hit = Number(s.cache_hit_tokens) || 0, miss = Number(s.cache_miss_tokens) || 0;
    const cacheSamples = Number(s.cache_samples) || 0, tokenSamples = Number(s.token_samples) || 0;
    $('usageCompleted').textContent = number(completed);
    $('usageSuccess').textContent = completed > 0 ? (Number(s.success_rate) || 0).toFixed(1) + '%' : '—';
    $('usageOutcomes').textContent = '成功 ' + number(s.succeeded || 0) + ' · 失败 ' + number(s.failed || 0);
    $('usageDuration').textContent = completed > 0 ? duration(s.avg_duration_ms) : '—';
    $('usageTTFT').textContent = Number(s.ttft_samples) > 0 ? duration(s.ttft_ms) : '—';
    $('usageCacheHit').textContent = cacheSamples > 0 && hit + miss > 0 ? (hit / (hit + miss) * 100).toFixed(1) + '%' : '—';
    $('usageCacheNote').textContent = cacheSamples > 0 ? '命中 ' + number(hit) + ' · 未命中 ' + number(miss) : '上游未上报缓存用量';
    $('usagePromptTokens').textContent = tokenSamples > 0 ? number(s.prompt_tokens) : '—';
    $('usageCompletionTokens').textContent = tokenSamples > 0 ? number(s.completion_tokens) : '—';
    $('usageTokens').textContent = tokenSamples > 0 ? number(s.total_tokens) : '—';
    $('usageCompletedNote').textContent = label + ' · ' + number(completed) + ' 条请求' + (s.archive ? '（含本地归档）' : '');
    $('usageChartNote').textContent = label + ' · ' + (Array.isArray(s.hours) ? s.hours.length : 0) + ' 个有请求的小时' + (s.archive ? '；含本地归档 data/requests' : '') + '。';
    lastUsageStats = s;
    renderSummary(s);
    renderChart(Array.isArray(s.hours) ? s.hours : [], label);
  }
  // 汇总表两种口径：同一份窗口聚合里同时有 models 和 accounts，切换不必重新查询。
  // TPS 由服务端按「输出 token ÷ (耗时 - 首字时间)」加权算好，前端只负责显示。
  let summaryMode = 'model', lastUsageStats = {};
  function cacheRate(hit, miss) { return hit + miss > 0 ? (hit / (hit + miss) * 100).toFixed(1) + '%' : '—'; }
  function renderSummary(stats) {
    const s = stats || {}, byAccount = summaryMode === 'account';
    const head = byAccount
      ? ['账号', '请求', '成功', '输入', '缓存命中率', '输出', '总Token']
      : ['模型', '请求', '成功', '输入', '缓存命中率', '输出', '总Token', '平均TTFT', '输出TPS'];
    const rows = (byAccount ? s.accounts : s.models) || [];
    $('summaryMode').value = summaryMode;
    $('usageSummaryTitle').textContent = byAccount ? '按账号汇总' : '按模型汇总';
    $('usageSummaryNote').textContent = usageWindow().label + ' · ' + number(rows.length) + (byAccount ? ' 个账号' : ' 个模型');
    $('usageSummaryHead').innerHTML = '<tr>' + head.map(text => '<th>' + esc(text) + '</th>').join('') + '</tr>';
    $('usageModelsBody').innerHTML = rows.map(row => {
      const cells = [
        esc(byAccount ? row.account || '未知账号' : row.model || '未知模型'),
        number(row.requests),
        number(row.succeeded),
        number(row.prompt_tokens),
        cacheRate(Number(row.cache_hit_tokens) || 0, Number(row.cache_miss_tokens) || 0),
        number(row.completion_tokens),
        number(row.total_tokens),
      ];
      // 平均 TTFT 与输出 TPS 只对模型口径有意义：账号维度回答的是「谁在用、用了多少」。
      if (!byAccount) {
        cells.push(Number(row.ttft_samples) > 0 ? duration(row.ttft_ms) : '—');
        cells.push(Number(row.tps) > 0 ? Number(row.tps).toFixed(1) : '—');
      }
      return '<tr>' + cells.map((cell, index) => index === 0 ? '<td class="mono">' + cell + '</td>' : '<td>' + cell + '</td>').join('') + '</tr>';
    }).join('') || emptyRow(head.length, '该时间段内没有请求。换个范围或点「查询」再试。');
  }
  $('summaryMode').onchange = () => {
    summaryMode = $('summaryMode').value || 'model';
    renderSummary(lastUsageStats);
  };
  function renderChart(hours, rangeLabel = '') {
    const points = hours.map(hour => [Number(hour.t), Number(hour.count) || 0]).filter(point => Number.isFinite(point[0])).sort((a, b) => a[0] - b[0]);
    if (!points.length) { $('usageChart').textContent = (rangeLabel ? rangeLabel + '：' : '') + '没有请求数据。'; return; }
    const W = 1000, H = 220, left = 38, top = 12, bottom = 36, usableH = H - top - bottom;
    const max = Math.max(4, Math.ceil(Math.max(...points.map(p => p[1])) / 4) * 4);
    const slot = (W - left - 12) / points.length, width = Math.min(64, slot * .62);
    let svg = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="按小时统计的近期请求数"><title>近期请求数，每柱代表一个有请求的小时</title>';
    for (let i = 0; i <= 4; i++) { const y = top + usableH * i / 4; svg += '<line class="chart-grid" x1="' + left + '" y1="' + y + '" x2="990" y2="' + y + '"/><text x="30" y="' + (y + 4) + '" text-anchor="end">' + Math.round(max * (4 - i) / 4) + '</text>'; }
    points.forEach(([t, count], i) => { const x = left + slot * (i + .5), height = count / max * usableH, label = new Date(t).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', hour12: false }); svg += '<rect class="chart-bar" x="' + (x - width / 2) + '" y="' + (top + usableH - height) + '" width="' + width + '" height="' + height + '" rx="3"><title>' + esc(label) + '：' + count + ' 次请求</title></rect>'; if (i % Math.max(1, Math.ceil(points.length / 8)) === 0) svg += '<text x="' + x + '" y="207" text-anchor="middle">' + esc(label) + '</text>'; });
    $('usageChart').innerHTML = svg + '</svg>';
  }
  // 预设范围选中即查；自定义要点「查询」才查，免得两个框还没填完就先扫一遍归档。
  // 切到自定义时先拿「近 24 小时」垫底，用户看到的是有值的输入框而不是两个空框。
  $('usageRange').onchange = () => {
    usageRange = $('usageRange').value || '30d';
    if (usageRange === 'custom') {
      const now = new Date();
      if (!$('usageFrom').value) $('usageFrom').value = localInputValue(new Date(now.getTime() - 24 * 3600e3));
      if (!$('usageTo').value) $('usageTo').value = localInputValue(now);
      usageCustom = { from: $('usageFrom').value, to: $('usageTo').value };
    }
    refresh();
  };
  $('usageQuery').onclick = () => {
    usageCustom = { from: $('usageFrom').value, to: $('usageTo').value };
    refresh();
  };

  // —— 余额趋势：服务端记录真实余额水位，前端只读历史，不因看图额外请求上游。 ——
  const BALANCE_COLORS = ['var(--accent)', 'var(--purple)', 'var(--green)', 'var(--amber)', '#62b6cb', '#e58aae', '#9aae62', '#c98c5b'];
  let balanceDays = 30;
  const balanceName = row => row.name || row.uid || '未知账号';
  const signed = value => (value > 0 ? '+' : '') + number(value);
  function balanceDayKey(at) {
    const pad = value => String(value).padStart(2, '0');
    return at.getFullYear() + '-' + pad(at.getMonth() + 1) + '-' + pad(at.getDate());
  }
  function balanceDailyFor(row, day) {
    return (Array.isArray(row.daily) ? row.daily : []).find(item => item.day === day) || null;
  }
  function balanceChangeCell(daily) {
    if (!daily || !Number.isFinite(Number(daily.change))) return '<td>—</td>';
    const value = Number(daily.change), tone = value > 0 ? 'positive' : value < 0 ? 'negative' : '';
    return '<td class="number ' + tone + '"' + (daily.full ? '' : ' title="该日采样不完整，不宜与完整日直接比较"') + '>' + esc(signed(value)) + '</td>';
  }
  function renderBalance(data) {
    const rows = data && Array.isArray(data.series) ? data.series : [];
    const current = rows.reduce((sum, row) => sum + (Number(row.current) || 0), 0);
    const change = rows.reduce((sum, row) => sum + (Number(row.change) || 0), 0);
    const samples = rows.reduce((sum, row) => sum + (Array.isArray(row.points) ? row.points.length : 0), 0);
    $('balanceAccounts').textContent = number(rows.length);
    $('balanceCurrent').textContent = rows.length ? number(current) : '—';
    $('balanceChange').textContent = rows.length ? signed(change) : '—';
    $('balanceChange').className = change > 0 ? 'positive' : change < 0 ? 'negative' : '';
    $('balanceSamples').textContent = number(samples);
    $('balanceWindow').value = String(balanceDays);
    const now = new Date(data && data.now || Date.now()), validNow = Number.isNaN(now.getTime()) ? new Date() : now;
    const yesterday = new Date(validNow); yesterday.setDate(yesterday.getDate() - 1);
    const todayKey = balanceDayKey(validNow), yesterdayKey = balanceDayKey(yesterday);
    $('balanceBody').innerHTML = rows.map((row, index) => {
      const value = Number(row.change) || 0;
      return '<tr><td title="' + esc(row.uid || '') + '"><span class="balance-color-' + (index % BALANCE_COLORS.length) + '">●</span> ' + esc(balanceName(row)) + '</td>' +
        '<td class="number">' + esc(number(row.current)) + '</td><td class="number">' + esc(number(row.min)) + '</td><td class="number">' + esc(number(row.max)) + '</td>' +
        '<td class="number ' + (value > 0 ? 'positive' : value < 0 ? 'negative' : '') + '">' + esc(signed(value)) + '</td>' +
        balanceChangeCell(balanceDailyFor(row, todayKey)) + balanceChangeCell(balanceDailyFor(row, yesterdayKey)) +
        '<td class="number">' + number(Array.isArray(row.points) ? row.points.length : 0) + '</td></tr>';
    }).join('') || emptyRow(8, '暂无余额采样。服务启动后立即查询，之后每 30 分钟查询；成功查询按 5 分钟去重记录，也可在账号池手动刷新额度。');
    renderBalanceChart(rows);
    renderBalanceDaily(rows);
  }
  function renderBalanceChart(rows) {
    const host = $('balanceChart'), legend = $('balanceLegend');
    const series = rows.map(row => ({
      row,
      points: (Array.isArray(row.points) ? row.points : []).map(point => ({ ...point, t: Number(point.t) * 1000, c: Number(point.c) }))
        .filter(point => Number.isFinite(point.t) && point.t > 0 && Number.isFinite(point.c)).sort((a, b) => a.t - b.t)
    })).filter(item => item.points.length);
    if (!series.length) {
      host.textContent = '暂无余额采样。历史从第一次成功查询开始，未知余额不会显示为 0。';
      legend.innerHTML = '';
      $('balanceChartNote').textContent = '服务每 30 分钟刷新启用账号；手动刷新额度也走采样入口，同一账号 5 分钟内只记录一点。';
      return;
    }
    let t0 = Infinity, t1 = -Infinity, lo = Infinity, hi = -Infinity;
    series.forEach(item => item.points.forEach(point => {
      t0 = Math.min(t0, point.t); t1 = Math.max(t1, point.t); lo = Math.min(lo, point.c); hi = Math.max(hi, point.c);
      if (point.d && Number.isFinite(Number(point.f))) { lo = Math.min(lo, Number(point.f)); hi = Math.max(hi, Number(point.f)); }
    }));
    if (t0 === t1) { t0 -= 30 * 60e3; t1 += 30 * 60e3; }
    const spread = hi - lo || Math.max(1, Math.abs(hi) * .05), paddedHi = hi + spread * .12;
    lo = Math.max(0, lo - spread * .12); hi = Math.max(paddedHi, lo + 1);
    const W = 1000, H = 240, left = 68, right = 18, top = 15, bottom = 38, iw = W - left - right, ih = H - top - bottom;
    const X = at => left + iw * (at - t0) / (t1 - t0), Y = value => top + ih - ih * (value - lo) / (hi - lo);
    let svg = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="各账号 CREDIT 积分水位"><title>各账号 CREDIT 积分水位，直线连接真实采样点</title>';
    for (let i = 0; i <= 4; i++) {
      const value = lo + (hi - lo) * i / 4, y = Y(value);
      svg += '<line class="chart-grid" x1="' + left + '" y1="' + y + '" x2="' + (W - right) + '" y2="' + y + '"/><text x="' + (left - 8) + '" y="' + (y + 4) + '" text-anchor="end">' + esc(number(value)) + '</text>';
    }
    svg += '<line class="balance-axis" x1="' + left + '" y1="' + (top + ih) + '" x2="' + (W - right) + '" y2="' + (top + ih) + '"/>';
    const span = t1 - t0;
    for (let i = 0; i < 5; i++) {
      const at = t0 + span * i / 4, label = new Date(at).toLocaleString('zh-CN', span <= 2 * 864e5 ? { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false } : { month: '2-digit', day: '2-digit' });
      svg += '<text x="' + X(at) + '" y="' + (H - 10) + '" text-anchor="' + (i === 0 ? 'start' : i === 4 ? 'end' : 'middle') + '">' + esc(label) + '</text>';
    }
    series.forEach((item, index) => {
      const color = BALANCE_COLORS[index % BALANCE_COLORS.length], points = item.points.map(point => ({ ...point, x: X(point.t), y: Y(point.c) }));
      const path = points.map((point, i) => (i ? 'L' : 'M') + point.x.toFixed(1) + ' ' + point.y.toFixed(1)).join('');
      svg += '<path class="balance-line" stroke="' + color + '" d="' + path + '"/>';
      // 点少时保留每个点的悬停热区；点多了只画折线与末点，避免上千个 DOM 节点。
      if (points.length <= 64) points.forEach(point => {
        const title = balanceName(item.row) + ' · ' + new Date(point.t).toLocaleString('zh-CN', { hour12: false }) + '\n余额 ' + number(point.c) + (point.d ? '（日粒度）' : '');
        svg += '<circle cx="' + point.x + '" cy="' + point.y + '" r="5" fill="transparent"><title>' + esc(title) + '</title></circle>';
      });
      const last = points[points.length - 1];
      svg += '<circle cx="' + last.x + '" cy="' + last.y + '" r="3.2" fill="' + color + '"><title>' + esc(balanceName(item.row) + ' · 当前 ' + number(last.c)) + '</title></circle>';
    });
    host.innerHTML = svg + '</svg>';
    legend.innerHTML = series.map((item, index) => '<span class="balance-legend-item" title="' + esc(balanceName(item.row)) + '"><i class="balance-color-' + (index % BALANCE_COLORS.length) + '"></i><span>' + esc(balanceName(item.row)) + '</span></span>').join('');
    $('balanceChartNote').textContent = (balanceDays === 1 ? '近 24 小时' : '近 ' + balanceDays + ' 天') + ' · 真实时间轴；直线连接采样点' + (lo > 0 ? '；纵轴不从 0 起' : '') + '。';
  }
  function renderBalanceDaily(rows) {
    const host = $('balanceDaily'), daySet = new Set();
    rows.forEach(row => (Array.isArray(row.daily) ? row.daily : []).forEach(day => {
      if (day && day.day && Number.isFinite(Number(day.change))) daySet.add(day.day);
    }));
    const days = [...daySet].sort();
    if (!days.length) { host.textContent = '暂无按日数据。第一次有效采样后即会出现当日记录。'; return; }
    let hi = 0, lo = 0;
    rows.forEach(row => (Array.isArray(row.daily) ? row.daily : []).forEach(day => {
      if (daySet.has(day.day)) { hi = Math.max(hi, Number(day.change) || 0); lo = Math.min(lo, Number(day.change) || 0); }
    }));
    if (hi > 0) hi *= 1.12;
    if (lo < 0) lo *= 1.12;
    if (hi === 0 && lo === 0) { hi = 1; lo = -1; }
    else if (hi === 0) hi = Math.max(1, -lo * .15);
    else if (lo === 0) lo = -Math.max(1, hi * .15);
    const W = 1000, H = 220, left = 68, right = 18, top = 14, bottom = 38, iw = W - left - right, ih = H - top - bottom;
    const Y = value => top + ih - ih * (value - lo) / (hi - lo), zeroY = Y(0), groupW = iw / days.length, gap = rows.length > 1 ? 1 : 0;
    const bw = Math.max(1.5, Math.min(16, (groupW - 8 - gap * (rows.length - 1)) / Math.max(1, rows.length)));
    let svg = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="每日余额净变化"><title>每日余额净变化；不完整日期淡化</title>';
    for (let i = 0; i <= 4; i++) {
      const value = lo + (hi - lo) * i / 4, y = Y(value), zero = Math.abs(value) < Math.max(hi, -lo) / 8;
      svg += '<line class="' + (zero ? 'balance-zero' : 'chart-grid') + '" x1="' + left + '" y1="' + y + '" x2="' + (W - right) + '" y2="' + y + '"/><text x="' + (left - 8) + '" y="' + (y + 4) + '" text-anchor="end">' + esc(signed(value)) + '</text>';
    }
    days.forEach((day, dayIndex) => {
      const gx = left + dayIndex * groupW;
      rows.forEach((row, rowIndex) => {
        const daily = balanceDailyFor(row, day);
        if (!daily) return;
        const value = Number(daily.change) || 0, width = bw * rows.length + gap * (rows.length - 1);
        const x = gx + (groupW - width) / 2 + rowIndex * (bw + gap), y = value === 0 ? zeroY - 1 : Math.min(zeroY, Y(value)), height = value === 0 ? 2 : Math.max(1, Math.abs(Y(value) - zeroY));
        const title = balanceName(row) + ' · ' + day + '\n净变化 ' + signed(value) + '\n估算消耗 ' + number(daily.consumed) + ' · 到账 ' + number(daily.gained) + '（下界）' + (daily.full ? '' : '\n该日采样不完整');
        svg += '<rect x="' + x + '" y="' + y + '" width="' + bw + '" height="' + height + '" rx="1.5" fill="' + (value > 0 ? 'var(--green)' : value < 0 ? 'var(--red)' : 'var(--line)') + '"' + (daily.full ? '' : ' opacity=".42"') + '><title>' + esc(title) + '</title></rect>';
      });
      if (dayIndex % Math.max(1, Math.ceil(days.length / 8)) === 0) svg += '<text x="' + (gx + groupW / 2) + '" y="' + (H - 10) + '" text-anchor="middle">' + esc(day.slice(5)) + '</text>';
    });
    host.innerHTML = svg + '</svg>';
  }
  $('balanceWindow').onchange = () => { balanceDays = Number($('balanceWindow').value) || 30; refresh(); };

  function renderRequestLogs(data) {
    const view = usageView(data);
    requestRows = [...view.recent].sort((a, b) => new Date(b.time) - new Date(a.time));
    requestRowsFromArchive = view.fromArchive;
    requestAttemptErrors = (data && data.attempt_errors) || {};
    paintRequestLogs();
  }
  // 尝试次数：重试过（>1）才把最后一次上游错误挂到单元格的原生 tooltip 上；
  // 没有错误就不给 title，鼠标移上去不会弹出空提示。
  function attemptsCell(row) {
    const attempts = Number(row.attempts);
    const shown = Number.isFinite(attempts) && attempts > 0 ? String(attempts) : '—';
    const failure = attempts > 1 ? requestAttemptErrors[row.request_id] : '';
    return '<td class="number"' + (failure ? ' title="' + esc(failure) + '"' : '') + '>' + esc(shown) + '</td>';
  }
  // 下拉选项带条数：数字只描述已取回的这一批窗口（进程 100 条 + 归档 200 条），不是全量历史。
  // 「异常」与筛选判定同源（failureKind），不会出现「显示 3 条、点进去 2 条」的对不上。
  function paintOutcomeCounts() {
    let ok = 0, abnormal = 0;
    for (const row of requestRows) {
      if (eventOK(row)) ok++;
      else if (failureKind(row) === 'abnormal') abnormal++;
    }
    $('requestOutcomeAll').textContent = '全部 (' + number(requestRows.length) + ')';
    $('requestOutcomeOk').textContent = '成功 (' + number(ok) + ')';
    $('requestOutcomeFail').textContent = '失败 (' + number(requestRows.length - ok) + ')';
    $('requestOutcomeAbnormal').textContent = '异常 (' + number(abnormal) + ')';
  }
  // 筛选只作用于已取回的这一批记录（进程 100 条 + 归档 200 条），不额外请求后端。
  function paintRequestLogs() {
    paintOutcomeCounts();
    const filter = $('requestOutcome').value;
    const rows = filter ? requestRows.filter(row => matchesOutcomeFilter(row, filter)) : requestRows;
    $('requestLogsBody').innerHTML = rows.map(row => '<tr><td class="muted">' + esc(date(row.time)) + '</td><td class="mono">' + esc(row.model || '—') + '</td><td>' + esc(row.account || '—') + '</td><td><span class="request-result"><span class="pill ' + (eventOK(row) ? 'ok' : 'bad') + '">' + esc(eventOK(row) ? '成功' : row.outcome || '失败') + ' ' + esc(row.status ?? '') + '</span>' + failureBadge(row) + '</span>' + (!eventOK(row) && row.error ? '<small class="request-error" title="' + esc(row.error) + '">' + esc(row.error) + '</small>' : '') + '</td>' + attemptsCell(row) + '<td>' + esc(Number(row.ttfb_ms) > 0 ? duration(row.ttfb_ms) : '—') + (Number(row.attempts) > 1 && Number(row.queue_ms) > 0 ? '<small class="request-queue">含排队 ' + esc(duration(row.queue_ms).replace(' ', '')) + '</small>' : '') + '</td><td>' + esc(duration(row.duration_ms)) + '</td><td>' + esc(number(row.cache_hit_tokens || 0)) + '</td><td>' + esc(number(tokenTotal(row))) + '</td></tr>').join('') || emptyRow(9, filter ? '没有符合条件的请求。' : requestRowsFromArchive ? '本地归档中暂无请求记录。' : '当前进程尚无请求记录。');
  }
  function renderRuntimeLogs() {
    const box = $('runtimeLogs');
    const channel = $('logChannel').value;
    const rows = runtimeLogs.filter(row => !channel || row.ch === channel);
    const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
    box.innerHTML = rows.map(row => '<span class="log-line"><span class="log-time">' + esc(date(row.ts)) + '</span><span class="log-channel">' + esc({ sys: '系统', chat: '请求', task: '任务' }[row.ch] || row.ch || '') + '</span>' + esc(row.text) + '</span>').join('') || '<span class="muted">暂无日志。</span>';
    if (atEnd) box.scrollTop = box.scrollHeight;
  }
  $('logChannel').onchange = renderRuntimeLogs;
  $('requestOutcome').onchange = paintRequestLogs;
  async function loadLogs() {
    if (logsLoading) return;
    logsLoading = true;
    try {
      const [logs, usage] = await Promise.all([api('logs'), api('usage')]);
      runtimeLogs = logs.entries || [];
      renderRuntimeLogs();
      renderRequestLogs(usage);
      $('updatedAt').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    } finally { logsLoading = false; }
  }
  function configForEditor(config) {
    const copy = { ...config, api_key: '', admin_key: '' };
    return JSON.stringify(copy, null, 2);
  }
  // 表单字段 ↔ 配置点路径。填充与读取共用这张表，新增字段只改这里和 panel.html。
  // 未列出的字段（含未知扩展字段）在表单模式下原样保留，要用就用 Raw JSON。
  const CONFIG_FIELDS = [
    ['cfgListen', 'listen', 'text'],
    ['cfgDefaultModel', 'default_model', 'text'],
    ['cfgAuthDir', 'auth_dir', 'text'],
    ['cfgStateFile', 'state_file', 'text'],
    ['cfgOAuthCallbackHost', 'oauth_callback_host', 'text'],
    ['cfgLoginClientID', 'login_client_id', 'text'],
    ['cfgAccountStrategy', 'account_strategy', 'text'],
    ['cfgMaxConcurrent', 'max_concurrent', 'int'],
    ['cfgKeepaliveWindow', 'keepalive_window', 'text'],
    ['cfgSessionTTL', 'session_ttl', 'text'],
    ['cfgReserveCredits', 'reserve_credits', 'float'],
    ['cfgQueueRetrySeconds', 'queue_retry_seconds', 'int'],
    ['cfgQueueMaxAttempts', 'queue_max_attempts', 'int'],
    ['cfgSoftRate', 'cooldown.soft_rate', 'text'],
    ['cfgErrThreshold', 'cooldown.err_threshold', 'int'],
    ['cfgErrCooldown', 'cooldown.err_cooldown', 'text'],
    ['cfgTimeoutSeconds', 'upstream.timeout_seconds', 'int'],
    ['cfgMaxBodyMB', 'max_body_mb', 'int'],
    ['cfgImageShrinkEnabled', 'image_shrink.enabled', 'bool'],
    ['cfgImageShrinkTriggerBodyMB', 'image_shrink.trigger_body_mb', 'int'],
    ['cfgImageShrinkTargetImageMB', 'image_shrink.target_image_mb', 'int'],
    ['cfgImageShrinkTargetTotalMB', 'image_shrink.target_total_mb', 'int'],
    ['cfgImageShrinkMaxDimension', 'image_shrink.max_dimension', 'int'],
    ['cfgImageShrinkQuality', 'image_shrink.quality', 'int'],
    ['cfgVisionBridgeEnabled', 'vision_bridge.enabled', 'bool'],
    ['cfgVisionBridgeModel', 'vision_bridge.model', 'text'],
    ['cfgVisionBridgeMaxImages', 'vision_bridge.max_images', 'int'],
    ['cfgVisionBridgeMaxChars', 'vision_bridge.max_chars', 'int'],
    ['cfgVisionBridgeMaxTokens', 'vision_bridge.max_tokens', 'int'],
    ['cfgVisionBridgeTimeoutSeconds', 'vision_bridge.timeout_seconds', 'int'],
    ['cfgVisionBridgeTotalTimeoutSeconds', 'vision_bridge.total_timeout_seconds', 'int'],
    ['cfgVisionBridgePrompt', 'vision_bridge.prompt', 'text'],
    ['cfgWatchEnabled', 'watch.enabled', 'bool'],
    ['cfgCreditsAutoClaim', 'credits_auto_claim', 'bool'],
    ['cfgPollMinutes', 'watch.poll_minutes', 'int'],
    ['cfgRefreshSkew', 'watch.refresh_skew_minutes', 'int'],
    ['cfgKeepaliveInterval', 'watch.keepalive_interval_minutes', 'int'],
    ['cfgModelRefreshMinutes', 'model_refresh_minutes', 'int'],
    ['cfgCheckinHours', 'checkin_hours', 'hours'],
    ['cfgRequestLogRetentionDays', 'request_log_retention_days', 'int'],
  ];
  const configPathGet = (source, path) => path.split('.').reduce((value, key) => (value === null || value === undefined ? undefined : value[key]), source);
  function configPathSet(target, path, value) {
    const keys = path.split('.');
    let cursor = target;
    for (const key of keys.slice(0, -1)) { if (!cursor[key] || typeof cursor[key] !== 'object') cursor[key] = {}; cursor = cursor[key]; }
    cursor[keys[keys.length - 1]] = value;
  }
  function markConfigDirty() { $('configEditor').dataset.dirty = 'true'; }
  function markConfigClean() { $('configEditor').dataset.dirty = 'false'; }
  function fillConfigForm(config) {
    for (const [id, path, kind] of CONFIG_FIELDS) {
      const input = $(id), value = configPathGet(config, path);
      if (kind === 'bool') input.checked = value === true;
      else if (kind === 'hours') input.value = Array.isArray(value) ? value.join(', ') : '';
      else input.value = value === null || value === undefined ? '' : String(value);
      input.oninput = markConfigDirty; input.onchange = markConfigDirty;
    }
  }
  // 以最近读到的配置为底只覆盖表单字段：表单没覆盖到的键（含未知字段）不会丢。
  function readConfigForm(config) {
    const next = JSON.parse(JSON.stringify(config || {}));
    for (const [id, path, kind] of CONFIG_FIELDS) {
      const input = $(id), raw = String(input.value === undefined ? '' : input.value).trim();
      if (kind === 'bool') { configPathSet(next, path, input.checked === true); continue; }
      if (kind === 'hours') {
        const hours = raw === '' ? [] : raw.split(',').map(part => part.trim()).filter(Boolean).map(Number);
        if (hours.some(hour => !Number.isInteger(hour) || hour < 0 || hour > 23)) throw new Error('签到时点必须是 0-23 的整数，逗号分隔');
        configPathSet(next, path, hours);
        continue;
      }
      if (kind === 'int' || kind === 'float') {
        if (raw === '') continue; // 留空＝沿用原值，免得把必填项清成 0
        const number = Number(raw);
        if (!Number.isFinite(number) || kind === 'int' && !Number.isInteger(number)) throw new Error(path + (kind === 'int' ? ' 必须是整数' : ' 必须是数字'));
        configPathSet(next, path, kind === 'int' ? Math.trunc(number) : number);
        continue;
      }
      configPathSet(next, path, raw);
    }
    return next;
  }
  let configMode = 'form';
  function renderConfigMode() {
    const form = configMode === 'form';
    $('configForm').hidden = !form;
    $('configRaw').hidden = form;
    for (const [id, active] of [['configModeForm', form], ['configModeRaw', !form]]) {
      $(id).classList.toggle('active', active);
      $(id).setAttribute('aria-selected', String(active));
    }
    $('configHelp').textContent = form ? '表单只覆盖以上字段；其余字段请切到 Raw JSON 编辑。保存不会自动重启服务。' : '请使用合法 JSON。保存不会自动重启服务。';
  }
  let loadedConfig = {};
  async function loadConfig() {
    const data = await api('config');
    // 服务端已把密钥脱敏成空串；这里再显式清一次，保证表单克隆出的 patch 永不带密钥。
    loadedConfig = { ...(data.config || {}), api_key: '', admin_key: '' };
    $('configEditor').value = configForEditor(loadedConfig);
    fillConfigForm(loadedConfig);
    markConfigClean();
    $('newAdminKey').value = ''; $('newAPIKey').value = '';
    const overrides = Array.isArray(data.env_overrides) ? data.env_overrides : [];
    $('envOverrides').hidden = !overrides.length;
    $('envOverrides').textContent = overrides.length ? '以下字段由环境变量覆盖：' + overrides.join('、') + '。请同时检查启动环境。' : '';
    renderConfigMode();
  }
  // 切换模式一律重新读取配置文件（未保存的修改随之丢弃，这是页面上写明的行为）。
  async function switchConfigMode(mode) {
    if (mode === configMode) return;
    configMode = mode;
    renderConfigMode();
    try { await loadConfig(); toast('已重新读取配置文件'); }
    catch (error) { toast(error.message, true); }
  }
  $('configModeForm').onclick = () => switchConfigMode('form');
  $('configModeRaw').onclick = () => switchConfigMode('raw');
  $('configEditor').oninput = markConfigDirty;
  $('saveConfigButton').onclick = async () => {
    let config;
    // 以当前界面模式为准：Raw 模式解析编辑框，表单模式从输入控件重建。
    if (configMode === 'raw') {
      try { config = JSON.parse($('configEditor').value); if (!config || Array.isArray(config) || typeof config !== 'object') throw new Error('请填写配置对象'); }
      catch (error) { toast('配置 JSON 无效：' + error.message, true); return; }
    } else {
      try { config = readConfigForm(loadedConfig); }
      catch (error) { toast('表单有无效值：' + error.message, true); return; }
    }
    const newAdminKey = $('newAdminKey').value.trim();
    config.api_key = $('newAPIKey').value || config.api_key || '';
    config.admin_key = newAdminKey || config.admin_key || '';
    $('newAPIKey').value = ''; $('newAdminKey').value = '';
    $('saveConfigButton').disabled = true;
    try {
      const data = await api('config', { method: 'PUT', body: JSON.stringify(config) });
      if (data.ok === false) throw new Error(data.message || '保存失败');
      // 热生效后旧密钥立即失效：本次会话改用新管理密钥，避免下一次轮询被 401。
      if (newAdminKey) { adminKey = newAdminKey; writeStorage(sessionStorage, KEY_STORAGE, adminKey); }
      loadedConfig = { ...(data.config || config), api_key: '', admin_key: '' };
      $('configEditor').value = configForEditor(loadedConfig);
      fillConfigForm(loadedConfig);
      markConfigClean();
      const restart = Array.isArray(data.restart_required) ? data.restart_required.filter(Boolean) : [];
      $('configNotice').textContent = restart.length
        ? '配置已保存并热生效；标有「需重启」的字段需重启服务后生效。'
        : '配置已保存并立即生效。';
      toast(restart.length ? '配置已保存并热生效，标有「需重启」的字段待重启服务后生效' : '配置已保存并立即生效');
    } catch (error) { toast(error.message, true); }
    finally { $('saveConfigButton').disabled = false; }
  };
  async function refresh(forceModels = false) {
    if (!authenticated) return;
    const id = ++refreshID, page = currentPage;
    $('pageError').hidden = true; $('pageLoading').hidden = false; $('refreshButton').disabled = true;
    if (page === 'models' && forceModels) { $('refreshModelsButton').disabled = true; $('refreshModelsButton').textContent = '正在获取…'; }
    try {
      if (page === 'accounts') renderOverview(await api('overview'));
      else if (page === 'models') {
        const [data, config] = await Promise.all([api('models' + (forceModels ? '?refresh=1' : '')), api('config')]);
        models = data.data || [];
        visionBridgeEnabled = config.config?.vision_bridge?.enabled === true;
        renderModels();
        if (forceModels) toast('模型目录已重新获取');
      }
      else if (page === 'usage') renderUsage(await api('usage?' + usageQuery()));
      else if (page === 'balance') renderBalance(await api('balance_history?days=' + balanceDays));
      else if (page === 'config') { if ($('configEditor').dataset.dirty !== 'true') await loadConfig(); }
      else if (page === 'logs') await loadLogs();
      if (id === refreshID) $('updatedAt').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    } catch (error) { if (id === refreshID && authenticated) { $('pageError').textContent = error.message + '。点击右上角刷新重试。'; $('pageError').hidden = false; } }
    finally { if (id === refreshID) { $('pageLoading').hidden = true; $('refreshButton').disabled = false; $('refreshModelsButton').disabled = false; $('refreshModelsButton').textContent = '重新获取'; } }
  }
  $('refreshButton').onclick = () => { if (currentPage === 'config' && $('configEditor').dataset.dirty === 'true') { toast('配置含未保存的编辑，请先保存；切换页面不会丢失编辑。'); return; } refresh(); };
  $('refreshModelsButton').onclick = () => refresh(true);
  $('loginDialog').addEventListener('cancel', event => event.preventDefault());
  $('loginForm').onsubmit = async event => {
    event.preventDefault();
    const candidate = $('loginKey').value.trim();
    $('loginKey').value = ''; $('loginSubmit').disabled = true; fieldError('loginError', '');
    try { const data = await api('overview', {}, candidate); adminKey = candidate; writeStorage(sessionStorage, KEY_STORAGE, adminKey); authenticated = true; $('loginDialog').close(); renderOverview(data); navigate(); }
    catch (error) { fieldError('loginError', error.message); $('loginKey').focus(); }
    finally { $('loginSubmit').disabled = false; }
  };
  $('logoutButton').onclick = () => {
    authEpoch++; refreshID++; adminKey = ''; authenticated = false; writeStorage(sessionStorage, KEY_STORAGE, ''); stopOAuth();
    accounts = []; models = []; runtimeLogs = []; serverStartedAt = 0; $('uptime').textContent = '—';
    $('configEditor').value = ''; $('configEditor').dataset.dirty = 'false'; $('newAPIKey').value = ''; $('newAdminKey').value = '';
    ['statTotal', 'statHealthy', 'statCooling', 'statDisabled', 'statCredits', 'statSticky', 'usageCompleted', 'usageSuccess', 'usageDuration', 'usageTTFT', 'usageCacheHit', 'usagePromptTokens', 'usageCompletionTokens', 'usageTokens', 'balanceAccounts', 'balanceCurrent', 'balanceChange', 'balanceSamples'].forEach(id => { $(id).textContent = '—'; });
    renderAccounts(); renderModels(); renderRuntimeLogs(); renderRequestLogs({}); renderUsage({}); renderBalance({});
    $('pageLoading').hidden = true; $('pageError').hidden = true; $('refreshButton').disabled = false; showLogin();
  };
  document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => $(button.dataset.close).close(); });
  function stopOAuth() { if (oauthTimer) clearInterval(oauthTimer); oauthTimer = null; oauthSession = ''; }
  function clearAdd() { stopOAuth(); $('callbackURL').value = ''; $('importJSON').value = ''; $('importFile').value = ''; $('akskForm').reset(); $('oauthStart').textContent = '生成授权链接'; $('oauthCopy').hidden = true; $('oauthLink').hidden = true; $('oauthLink').removeAttribute('href'); $('oauthReady').hidden = true; fieldError('addError', ''); }
  $('addDialog').addEventListener('close', clearAdd);
  $('addAccountButton').onclick = () => { clearAdd(); $('addDialog').showModal(); };
  document.querySelectorAll('[data-add-mode]').forEach(button => { button.onclick = () => { const mode = button.dataset.addMode; document.querySelectorAll('[data-add-mode]').forEach(b => { b.classList.toggle('active', b === button); }); ['oauth', 'json', 'aksk'].forEach(name => { $(name === 'aksk' ? 'akskForm' : 'add-' + name).hidden = name !== mode; }); fieldError('addError', ''); }; });
  function setAuthURL(value) {
    const url = new URL(value);
    if (url.protocol !== 'https:' || !url.hostname.endsWith('.huaweicloud.com')) throw new Error('服务返回了不受支持的授权地址。');
    $('oauthLink').href = url.href;
    $('oauthCopy').hidden = false;
    $('oauthLink').hidden = false;
  }
  $('oauthCopy').onclick = async () => {
    const url = $('oauthLink').href;
    if (!url) return;
    try { await navigator.clipboard.writeText(url); toast('授权链接已复制'); }
    catch { toast('浏览器无法访问剪贴板，请右键“在新标签页打开授权页”复制链接。', true); }
  };
  $('oauthStart').onclick = async () => {
    $('oauthStart').disabled = true; fieldError('addError', ''); stopOAuth();
    $('oauthCopy').hidden = true; $('oauthLink').hidden = true; $('oauthLink').removeAttribute('href'); $('oauthReady').hidden = true;
    try { const data = await post('oauth/start'); if (data.ok === false) throw new Error(data.message || '无法发起授权'); setAuthURL(data.auth_url); oauthSession = data.session_id; $('oauthStart').textContent = '重新生成授权链接'; $('oauthReady').hidden = false; $('oauthStatus').textContent = '等待在浏览器完成授权…'; oauthTimer = setInterval(() => pollOAuth(), 3500); }
    catch (error) { fieldError('addError', error.message); }
    finally { $('oauthStart').disabled = false; }
  };
  async function oauthResult(data) {
    if (data.status === 'done') { stopOAuth(); $('addDialog').close(); toast(data.message || '账户已添加'); renderOverview(await api('overview')); }
    else if (data.ok === false || data.status === 'error') { stopOAuth(); throw new Error(data.message || '授权失败'); }
    else { $('oauthStatus').textContent = data.message || '等待浏览器完成授权…'; if (data.next_url) setAuthURL(data.next_url); }
  }
  async function pollOAuth() {
    if (!oauthSession || oauthBusy) return;
    const session = oauthSession;
    oauthBusy = true;
    try { const data = await post('oauth/poll', { session_id: session }); if (session === oauthSession) await oauthResult(data); }
    catch (error) { if (session === oauthSession) { stopOAuth(); fieldError('addError', error.message); } }
    finally { oauthBusy = false; }
  }
  $('oauthPoll').onclick = pollOAuth;
  $('oauthCallback').onclick = async () => {
    if (!oauthSession) { fieldError('addError', '请先生成授权链接。'); return; }
    const value = $('callbackURL').value.trim();
    if (!value) { fieldError('addError', '请粘贴浏览器的完整回调地址。'); return; }
    $('oauthCallback').disabled = true;
    try { const data = await post('oauth/import-callback', { session_id: oauthSession, callback_url: value }); $('callbackURL').value = ''; await oauthResult(data); }
    catch (error) { fieldError('addError', error.message); }
    finally { $('oauthCallback').disabled = false; }
  };
  async function importCredentials(payload) {
    const data = await post('accounts/import', payload);
    if (data.ok === false) throw new Error(data.message || '导入失败');
    $('addDialog').close(); toast('已导入 ' + (data.imported ?? data.accounts?.length ?? 1) + ' 个账户'); renderOverview(await api('overview'));
  }
  $('importFile').onchange = async () => {
    const file = $('importFile').files[0];
    if (!file) return;
    if (file.size > 1024 * 1024) { fieldError('addError', 'JSON 文件不能超过 1 MB。'); $('importFile').value = ''; return; }
    try { $('importJSON').value = await file.text(); fieldError('addError', ''); } catch { fieldError('addError', '无法读取文件。'); }
  };
  $('importJSONButton').onclick = async () => {
    $('importJSONButton').disabled = true;
    try { const payload = JSON.parse($('importJSON').value); if (!payload || typeof payload !== 'object') throw new Error('请提供凭据对象或数组'); await importCredentials(payload); }
    catch (error) { fieldError('addError', error.message); }
    finally { $('importJSONButton').disabled = false; }
  };
  $('akskForm').onsubmit = async event => {
    event.preventDefault(); const button = $('akskForm').querySelector('button[type=submit]'); button.disabled = true;
    const payload = Object.fromEntries(new FormData($('akskForm')));
    try { await importCredentials(payload); } catch (error) { fieldError('addError', error.message); }
    finally { button.disabled = false; }
  };
  // 与参考面板一致：只轮询可见日志页，避免后台标签页持续请求。
  setInterval(() => {
    if (authenticated && currentPage === 'logs' && !document.hidden) loadLogs().catch(() => {});
  }, 5000);
  // 账号池状态懒刷新：账号页可见时定期只拉 overview（纯内存读，不打上游），
  // 让并发占用/冷却/余额变化不必手动点刷新。后台标签页不轮询；有对话框开着、
  // 或用户正在搜索时不强刷（避免打断输入与选中状态）。
  setInterval(() => {
    if (!authenticated || document.hidden) return;
    if (currentPage !== 'accounts') return;
    if (document.querySelector('dialog[open]')) return; // 添加/删除/导入等对话框优先
    if ($('accountSearch') && $('accountSearch').value.trim()) return; // 搜索中不动结果集
    if (overviewInFlight) return; // 上一次还没回来就跳过这一拍，避免请求堆积
    overviewInFlight = true;
    api('overview').then(data => {
      if (currentPage !== 'accounts' || document.hidden) return;
      renderOverview(data);
      $('updatedAt').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    })
      .catch(() => {}) // 懒刷新失败保持旧数据，不弹错误横幅打断用户
      .finally(() => { overviewInFlight = false; });
  }, 10000);
  // 运行时长是本地推算的，30 秒跳一次即可，不必为它增加请求。
  setInterval(() => { if (authenticated) renderUptime(); }, 30000);
  navigate();
  if (adminKey) {
    api('overview').then(data => { authenticated = true; renderOverview(data); navigate(); }).catch(error => showLogin(error.message));
  } else showLogin();
})();
