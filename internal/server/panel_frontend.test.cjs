const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const base = __dirname + '/';
const html = fs.readFileSync(base + 'panel.html', 'utf8');
const script = fs.readFileSync(base + 'panel.js', 'utf8');
const all = [], ids = new Map(), events = {}, requests = [], copiedURLs = [], intervals = [];
let oauthStarts = 0, oauthURLOverride = '', clipboardError = false;
class Element {
  constructor(attrs = '') { this.attrs = attrs; this.id = /\bid="([^"]+)"/.exec(attrs)?.[1] || ''; this.value = ''; this.textContent = ''; this.innerHTML = ''; this.hidden = /\bhidden\b/.test(attrs); this.open = false; this.disabled = false; this.dataset = {}; this.handlers = {}; this.classes = new Set((/\bclass="([^"]+)"/.exec(attrs)?.[1] || '').split(' ')); for (const a of attrs.matchAll(/data-([\w-]+)="([^"]*)"/g)) this.dataset[a[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = a[2]; this.classList = { add: x => this.classes.add(x), remove: x => this.classes.delete(x), contains: x => this.classes.has(x), toggle: (x, enabled) => { if (enabled === undefined) enabled = !this.classes.has(x); enabled ? this.classes.add(x) : this.classes.delete(x); return enabled; } }; }
  addEventListener(name, cb) { (this.handlers[name] ||= []).push(cb); }
  setAttribute(k, v) { this[k] = v; }
  removeAttribute(k) { delete this[k]; }
  showModal() { this.open = true; }
  close() { this.open = false; for (const cb of this.handlers.close || []) cb(); }
  focus() {}
  append(...children) { (this.children ||= []).push(...children); }
  remove() {}
  reset() { this.value = ''; }
  querySelector() { return new Element(); }
}
const ancestors = [];
for (const match of html.matchAll(/<(\/?)([\w-]+)\b([^>]*)>/g)) {
  const [, closing, tag, attrs] = match;
  if (closing) { const index = ancestors.findLastIndex(el => el.tagName === tag); if (index >= 0) ancestors.length = index; continue; }
  const el = new Element(attrs); el.tagName = tag; el.parentElement = ancestors.at(-1);
  for (const name of ['href', 'target', 'rel']) { const value = new RegExp('\\b' + name + '="([^"]*)"').exec(attrs)?.[1]; if (value !== undefined) el[name] = value; }
  all.push(el); if (el.id) { assert(!ids.has(el.id), 'duplicate id ' + el.id); ids.set(el.id, el); }
  if (!/^(area|base|br|col|embed|hr|img|input|link|meta|param|source|track|wbr)$/.test(tag)) ancestors.push(el);
}
const selector = s => s === '.view' ? all.filter(e => e.classes.has('view')) : /^\[data-([\w-]+)\]$/.test(s) ? all.filter(e => Object.hasOwn(e.dataset, s.slice(6, -1).replace(/-([a-z])/g, (_, c) => c.toUpperCase()))) : [];
const storage = () => { const values = new Map(); return { getItem: k => values.get(k), setItem: (k, v) => values.set(k, v), removeItem: k => values.delete(k) }; };
const account = { uid: 'u1', nickname: '<img src=x onerror=alert(1)>', note: '13800001111 <b>&</b>', credit_balance: null, active_concurrent: 0, max_concurrent: 1, expires_at: '2027-01-01T00:00:00Z' };
const overview = { uptime_sec: 9000, started_at: '2026-01-01T00:00:00Z', stats: { total: 1, healthy: 1, cooling: 0, disabled: 0, credits: 1234.5, sticky: 2 }, accounts: [account] };
const configFixture = {
 listen: ':7866', admin_key: 'server-secret', api_key: 'api-secret', max_body_mb: 48, session_ttl: '30m', reserve_credits: 12.5,
 image_shrink: { enabled: true, trigger_body_mb: 6, target_image_mb: 3, target_total_mb: 7, max_dimension: 1600, quality: 78, extension: { preserved: 'image' } },
 vision_bridge: { enabled: true, model: 'Qwen3-VL-235B', max_images: 6, max_chars: 7000, max_tokens: 1600, timeout_seconds: 110, total_timeout_seconds: 85, prompt: '先概括图片。\n逐行转写文字。', extension: { preserved: 'vision' } },
 extension: { preserved: true },
};
for (const [id, value] of [['cfgSessionTTL', '30m'], ['cfgReserveCredits', 12.5]]) {
 assert(ids.has(id), 'scheduling control is missing: ' + id);
 let parent = ids.get(id).parentElement;
 while (parent && parent.id !== 'configForm') parent = parent.parentElement;
 assert(parent, 'scheduling control must be inside the config form: ' + id);
}
const multimodalControls = [
 ['cfgMaxBodyMB', 48, 64],
 ['cfgImageShrinkEnabled', true, false], ['cfgImageShrinkTriggerBodyMB', 6, 5],
 ['cfgImageShrinkTargetImageMB', 3, 2], ['cfgImageShrinkTargetTotalMB', 7, 4],
 ['cfgImageShrinkMaxDimension', 1600, 1800], ['cfgImageShrinkQuality', 78, 82],
 ['cfgVisionBridgeEnabled', true, false], ['cfgVisionBridgeModel', 'Qwen3-VL-235B', 'kimi-k2.6-vl'],
 ['cfgVisionBridgeMaxImages', 6, 4], ['cfgVisionBridgeMaxChars', 7000, 8000],
 ['cfgVisionBridgeMaxTokens', 1600, 2000], ['cfgVisionBridgeTimeoutSeconds', 110, 80],
 ['cfgVisionBridgeTotalTimeoutSeconds', 85, 100], ['cfgVisionBridgePrompt', '先概括图片。\n逐行转写文字。', '请按顺序描述。\n保留表格。'],
];
for (const [id, value] of multimodalControls) {
 assert(ids.has(id), 'multimodal control is missing from panel.html: ' + id);
 const input = ids.get(id);
 assert.equal(input.tagName, id === 'cfgVisionBridgePrompt' ? 'textarea' : 'input', 'wrong control element: ' + id);
 if (typeof value !== 'string') assert(input.attrs.includes('type="' + (typeof value === 'boolean' ? 'checkbox' : 'number') + '"'), 'wrong input type: ' + id);
 let parent = input.parentElement;
 while (parent && parent.id !== 'configForm') parent = parent.parentElement;
 assert(parent, 'multimodal control must be inside the config form: ' + id);
}
const usage = { completed: 2, succeeded: 1, failed: 1, avg_duration_ms: 500, attempt_errors: { 'req-retried': 'upstream 429: TPM <limit> & retry later' }, recent: [{ request_id: 'req-retried', time: new Date().toISOString(), model: 'test-model', ok: true, status: 200, attempts: 3, total_tokens: 1200, completion_tokens: 200, duration_ms: 500, prompt_tokens: 1000, cache_hit_tokens: 800, cache_miss_tokens: 200, ttfb_ms: 300, queue_ms: 10100 }, { request_id: 'req-first-try', time: new Date().toISOString(), model: 'failed-model', ok: false, outcome: 'http_error', status: 503, attempts: 1, error: 'upstream <failed> & unavailable', duration_ms: 100, ttfb_ms: 900 }] };
const usageStats = { from: '2026-10-04T00:00:00Z', to: '2026-11-02T16:00:00Z', archive: true, completed: 128, succeeded: 126, failed: 2, success_rate: 98.4, avg_duration_ms: 4200, ttft_ms: 620, ttft_samples: 120, cache_hit_tokens: 800, cache_miss_tokens: 200, cache_samples: 90, prompt_tokens: 1000, completion_tokens: 200, total_tokens: 1200, token_samples: 100, models: [{ model: 'test-model', requests: 128, succeeded: 126, prompt_tokens: 700000, completion_tokens: 400000, total_tokens: 1100000, cache_hit_tokens: 500000, cache_miss_tokens: 200000, cache_samples: 90, ttft_ms: 1800, ttft_samples: 120, tps: 42.5 }, { model: 'plain-model', requests: 4, succeeded: 4, prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, cache_hit_tokens: 0, cache_miss_tokens: 0, cache_samples: 0, ttft_ms: 0, ttft_samples: 0, tps: 0 }], accounts: [{ account: 'alice(uid00001)', requests: 100, succeeded: 99, prompt_tokens: 600000, completion_tokens: 300000, total_tokens: 900000, cache_hit_tokens: 400000, cache_miss_tokens: 200000, cache_samples: 80 }, { account: 'bob(uid00002)', requests: 32, succeeded: 31, prompt_tokens: 100100, completion_tokens: 100050, total_tokens: 200150, cache_hit_tokens: 100000, cache_miss_tokens: 0, cache_samples: 10 }], hours: [{ t: Date.UTC(2026, 10, 2, 1), count: 3 }, { t: Date.UTC(2026, 10, 2, 2), count: 5 }] };const balanceDay = offset => { const d = new Date(); d.setDate(d.getDate() + offset); const pad = n => String(n).padStart(2, '0'); return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()); };
const balanceNow = Date.now();
const balanceHistory = { now: new Date(balanceNow).toISOString(), days: 30, series: [
 { uid: 'u1', name: '<img src=x onerror=alert(1)>', current: 12.75, min: 10, max: 12.75, change: 2.75, points: [{ t: Math.floor((balanceNow - 36e5) / 1000), c: 10 }, { t: Math.floor(balanceNow / 1000), c: 12.75 }], daily: [{ day: balanceDay(0), first: 10, last: 12.75, change: 2.75, min: 10, max: 12.75, n: 2, full: false, consumed: 0, gained: 2.75 }, { day: balanceDay(-1), first: 8.5, last: 10, change: 1.5, min: 8.5, max: 10, n: 40, full: true, consumed: 0, gained: 1.5 }] },
 { uid: 'u2', name: '第二个账号', current: 99.5, min: 99.5, max: 100, change: -0.5, points: [{ t: Math.floor((balanceNow - 36e5) / 1000), c: 100 }, { t: Math.floor(balanceNow / 1000), c: 99.5 }], daily: [{ day: balanceDay(0), first: 100, last: 99.5, change: -0.5, min: 99.5, max: 100, n: 2, full: false, consumed: 0.5, gained: 0 }] }
] };
const document = { getElementById: id => { assert(ids.has(id), 'missing id ' + id); return ids.get(id); }, querySelector: s => s === 'dialog[open]' ? all.find(e => e.tagName === 'dialog' && e.open) || null : new Element(), querySelectorAll: selector, createElement: () => new Element(), documentElement: { dataset: {} }, hidden: false };
const context = vm.createContext({ document, console, sessionStorage: storage(), localStorage: storage(), matchMedia: () => ({ matches: false }), addEventListener: (n, cb) => { events[n] = cb; }, location: { hash: '#accounts' }, setTimeout: () => 1, clearTimeout() {}, setInterval: (cb, ms) => { intervals.push({ cb, ms }); return intervals.length; }, clearInterval() {}, Headers, URL, navigator: { clipboard: { writeText: async value => { if (clipboardError) throw new Error("clipboard blocked"); copiedURLs.push(value); } } }, fetch: async (url, opts) => { requests.push({ url, opts }); let data; const path = url.replace('/admin/api/', ''); if (path === 'overview') data = overview; else if (path.startsWith('models')) data = { data: [{ id: '<model>', model_name: 'Kimi <K3>', model_desc: '描述 <D>', notice: '注意 & 提示', model_tag: '闲时 <T>', display_enabled: false, supports_images: true, ratio_display: '0.7x', context_length: 1000 }, { id: 'plain-model' }] }; else if (path === "oauth/start") { oauthStarts++; data = { ok: true, session_id: "session-" + oauthStarts, auth_url: oauthURLOverride || "https://codearts.huaweicloud.com/portal/authorize?ticket_id=test-" + oauthStarts }; } else if (path.startsWith('usage')) data = path.includes('window=1') ? usageStats : usage; else if (path.startsWith('balance_history')) data = { ...balanceHistory, days: Number((/days=(\d+)/.exec(path) || [])[1]) || 30 }; else if (path === 'logs') data = { entries: [{ ts: new Date().toISOString(), ch: 'sys', text: '<script>alert(1)</script>' }] }; else if (path === 'config') data = opts.method === 'PUT' ? { ok: true, config: JSON.parse(opts.body), restart_required: ['listen'] } : { config: configFixture, env_overrides: [] }; else data = { ok: true }; return { ok: true, status: 200, json: async () => data }; } });
const tick = () => new Promise(resolve => setImmediate(resolve));
(async () => {
 vm.runInContext(script, context);
 assert(ids.get('loginDialog').open, 'login dialog must gate first visit');
 ids.get('loginKey').value = 'test-admin-secret';
 await ids.get('loginForm').onsubmit({ preventDefault() {} }); await tick();
 assert(!ids.get('loginDialog').open); assert.equal(ids.get('loginKey').value, '');
 assert.equal(ids.get('statCredits').textContent, '1,234.5', '总套餐积分应使用 overview 合计');
 assert.equal(ids.get('statSticky').textContent, '2', '粘性会话应显示当前绑定数');
 assert(ids.get('accountsBody').innerHTML.includes('&lt;img')); assert(!ids.get('accountsBody').innerHTML.includes('<img')); assert(ids.get('accountsBody').innerHTML.includes('未知'));
 // 备注显示在账户名下，且按 HTML 转义（与昵称同一套 esc）。
 assert(ids.get('accountsBody').innerHTML.includes('account-note'), 'note must render its own line');
 assert(ids.get('accountsBody').innerHTML.includes('13800001111 &lt;b&gt;&amp;&lt;/b&gt;'), 'note must be escaped');
 assert(!ids.get('accountsBody').innerHTML.includes('13800001111 <b>'), 'note must not inject raw HTML');
 assert(/data-account-action="note"/.test(ids.get('accountsBody').innerHTML), 'note button must exist');
 assert.equal(ids.get('uptime').textContent, '运行 2 时 30 分', '顶栏运行时长按 uptime_sec 换算');
 assert(ids.get('uptime').title.startsWith('服务启动于'), '顶栏运行时长要能说明启动时刻');
 // 签到按钮按「今日是否已签」切换：未签显示「签到」，已签显示「已签」并带本地结果提示。
 const checkinButton = () => /<button[^>]*data-account-action="checkin"[^>]*>(签到|已签)<\/button>/.exec(ids.get('accountsBody').innerHTML);
 assert.equal(checkinButton()[1], '签到', 'not signed today must show 签到');
 account.checkin_done = true; account.checkin = { ok: true, message: '领取成功（100 积分）' };
 ids.get('accountSearch').oninput();
 const signed = checkinButton();
 assert.equal(signed[1], '已签', 'signed today must show 已签');
 assert(signed[0].includes('class="button small done"'), '已签 must carry the done style');
 assert(signed[0].includes('title="领取成功（100 积分） · 今日已签到，每天只领一次"'), '已签 must explain itself: ' + signed[0]);
 account.checkin_done = false; account.checkin = undefined; ids.get('accountSearch').oninput();
 assert.equal(checkinButton()[1], '签到');
 for (const page of ['models', 'balance', 'usage', 'logs', 'config']) { context.location.hash = '#' + page; events.hashchange(); await tick(); await tick(); assert.equal(ids.get('pageError').hidden, true, 'page failed: ' + page); }
 assert(ids.get('balanceChart').innerHTML.includes('<svg'), '余额趋势应渲染 SVG 折线');
 assert(ids.get('balanceDaily').innerHTML.includes('<svg'), '每日变化应渲染 SVG 柱状图');
 assert.equal(ids.get('balanceAccounts').textContent, '2');
 assert.equal(ids.get('balanceCurrent').textContent, '112.25', '余额合计必须保留小数');
 assert.equal(ids.get('balanceChange').textContent, '+2.25', '净变化必须保留小数');
 assert(ids.get('balanceBody').innerHTML.includes('&lt;img'), '账号名必须转义');
 assert(!ids.get('balanceBody').innerHTML.includes('<img'), '账号名不得注入 HTML');
 assert(ids.get('balanceBody').innerHTML.includes('+2.75') && ids.get('balanceBody').innerHTML.includes('-0.5'), '账号日变化应保留小数');
 assert(ids.get('balanceDaily').innerHTML.includes('opacity=".42"'), '不完整日期必须淡化');
 context.location.hash = '#balance'; events.hashchange(); await tick(); await tick();
 let beforeBalanceRange = requests.length;
 ids.get('balanceWindow').value = '1'; ids.get('balanceWindow').onchange(); await tick(); await tick();
 assert.equal(requests.length, beforeBalanceRange + 1, '余额窗口切换只发一次历史查询');
 assert(requests.at(-1).url.endsWith('/balance_history?days=1'), '余额窗口要传给服务端');
 assert(ids.get('balanceChartNote').textContent.includes('近 24 小时'), '图表说明要跟随窗口');
 ids.get('balanceWindow').value = '30'; ids.get('balanceWindow').onchange(); await tick(); await tick();
 assert(ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 assert(ids.get('modelsBody').innerHTML.includes('Kimi &lt;K3&gt;'));
 assert(!ids.get('modelsBody').innerHTML.includes('<K3>'));
 assert(ids.get('modelsBody').innerHTML.includes('title="描述 &lt;D&gt;&#10;注意 &amp; 提示"'));
 assert(ids.get('modelsBody').innerHTML.includes('<div class="model-badges"><span class="pill tag">闲时 &lt;T&gt;</span><span class="pill warn">辅助模型</span><span class="pill ok">视觉理解</span></div>'));
 assert(ids.get('modelsBody').innerHTML.includes('<td class="mono">plain-model<div class="model-badges"><span class="pill">纯文本</span><span class="pill warn">外置视觉理解</span></div></td>'), '视觉转述启用时，纯文本后要挂黄色「外置视觉理解」徽标');
 configFixture.vision_bridge.enabled = false;
 context.location.hash = '#models'; events.hashchange(); await tick(); await tick();
 assert(!ids.get('modelsBody').innerHTML.includes('外置视觉理解'), '视觉转述关闭后不得显示外置视觉徽标');
 configFixture.vision_bridge.enabled = true;
 ids.get('refreshModelsButton').onclick(); await tick(); await tick();
 assert(ids.get('modelsBody').innerHTML.includes('外置视觉理解'), '视觉转述重新启用后应恢复外置视觉徽标');
 assert.equal(ids.get('themeIconSun').hidden, false); assert.equal(ids.get('themeIconMoon').hidden, true);
 ids.get('themeButton').onclick();
 assert.equal(ids.get('themeIconSun').hidden, true); assert.equal(ids.get('themeIconMoon').hidden, false);
 ids.get('themeButton').onclick();
 assert.equal(ids.get('themeIconSun').hidden, false); assert.equal(ids.get('themeIconMoon').hidden, true);
 context.location.hash = '#usage'; events.hashchange(); await tick(); await tick();
 assert(ids.get('usageChart').innerHTML.includes('<svg'));
 // 用量页：卡片、趋势、模型汇总都用服务端按窗口聚合的结果（window=1），前端不再逐条合并记录。
 assert.equal(ids.get('usageCompleted').textContent, '128');
 assert.equal(ids.get('usageSuccess').textContent, '98.4%');
 assert.equal(ids.get('usageOutcomes').textContent, '成功 126 · 失败 2');
 assert.equal(ids.get('usageDuration').textContent, '4.2 s');
 assert.equal(ids.get('usageTTFT').textContent, '620 ms');
 assert.equal(ids.get('usageCacheHit').textContent, '80.0%');
 assert.equal(ids.get('usageCacheNote').textContent, '命中 800 · 未命中 200');
 assert.equal(ids.get('usagePromptTokens').textContent, '1,000');
 assert.equal(ids.get('usageCompletionTokens').textContent, '200');
 assert.equal(ids.get('usageTokens').textContent, '1,200');
 assert(ids.get('usageCompletedNote').textContent.includes('近 30 天') && ids.get('usageCompletedNote').textContent.includes('归档'), '卡片注释要写明范围与归档: ' + ids.get('usageCompletedNote').textContent);
 assert(ids.get('usageChartNote').textContent.includes('归档'));
 assert(ids.get('usageModelsBody').innerHTML.includes('test-model'), '模型汇总来自服务端聚合');
 // 汇总表：默认按模型（含平均 TTFT 与输出 TPS），切到按账号换列且不重新请求。
 assert.equal(ids.get('summaryMode').value, 'model', '默认按模型汇总');
 const modelHead = ids.get('usageSummaryHead').innerHTML;
 assert(modelHead.includes('<th>模型</th>') && modelHead.includes('<th>输入</th>') && modelHead.includes('<th>平均TTFT</th>') && modelHead.includes('<th>输出TPS</th>'), '按模型的表头: ' + modelHead);
 const modelRows = ids.get('usageModelsBody').innerHTML;
 assert(modelRows.includes('<td class="mono">test-model</td>'), '按模型首列是模型 ID');
 assert(modelRows.includes('<td>71.4%</td>'), '缓存命中率＝命中/(命中+未命中): ' + modelRows);
 assert(modelRows.includes('<td>1.8 s</td>'), '平均 TTFT 列: ' + modelRows);
 assert(modelRows.includes('<td>42.5</td>'), '输出 TPS 列: ' + modelRows);
 assert(modelRows.includes('<td>—</td>'), '没有首字样本的模型显示 —: ' + modelRows);
 const beforeSwitch = requests.length;
 ids.get('summaryMode').value = 'account'; ids.get('summaryMode').onchange();
 assert.equal(requests.length, beforeSwitch, '切换汇总口径不该重新请求：两套汇总在同一次响应里');
 assert.equal(ids.get('usageSummaryTitle').textContent, '按账号汇总');
 const accountHead = ids.get('usageSummaryHead').innerHTML;
 assert(accountHead.includes('<th>账号</th>') && !accountHead.includes('<th>输出TPS</th>'), '按账号的表头: ' + accountHead);
 const accountRows = ids.get('usageModelsBody').innerHTML;
 assert(accountRows.includes('<td class="mono">alice(uid00001)</td>') && accountRows.includes('<td>66.7%</td>'), '按账号的行: ' + accountRows);
 assert.equal((accountRows.match(/<tr>/g) || []).length, 2, '两个账号两行');
 ids.get('summaryMode').value = 'model'; ids.get('summaryMode').onchange();
 assert(ids.get('usageModelsBody').innerHTML.includes('test-model'), '切回模型口径要恢复模型行');
 // 查询串：默认近 30 天必须带 window=1 与 from/to，服务端才按窗口扫归档。
 const windowQueries = () => requests.filter(r => r.url.includes('/admin/api/usage') && r.url.includes('window=1'));
 assert(windowQueries().length >= 1, '用量页要请求窗口聚合');
 const defaultQuery = decodeURIComponent(windowQueries().at(-1).url);
 assert(/window=1&from=[^&]+&to=[^&]+/.test(defaultQuery), '默认范围要带 from/to: ' + defaultQuery);
 // 预设范围选中即重新查询：口径在服务端，换范围必须重新拉一次。
 let beforeRange = requests.length;
 ids.get('usageRange').value = '7d'; ids.get('usageRange').onchange(); await tick(); await tick();
 assert(requests.length > beforeRange, '换预设范围要重新查询');
 assert(windowQueries().at(-1).url !== windowQueries().at(-2).url, '不同范围的查询串不能相同');
 assert(ids.get('usageCompletedNote').textContent.startsWith('近 7 天'), '注释要跟当前范围: ' + ids.get('usageCompletedNote').textContent);
 // 自定义：先露出两个 datetime-local 和查询按钮，点「查询」才发新请求（填到一半不扫归档）。
 assert.equal(ids.get('usageCustom').hidden, true, '预设范围下不显示自定义输入');
 ids.get('usageRange').value = 'custom'; ids.get('usageRange').onchange(); await tick(); await tick();
 assert.equal(ids.get('usageCustom').hidden, false, '选自定义要露出时间输入与查询按钮');
 assert(ids.get('usageFrom').value && ids.get('usageTo').value, '切到自定义时输入框要有默认值');
 const localStamp = ms => { const d = new Date(ms), pad = n => String(n).padStart(2, '0'); return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes()); };
 ids.get('usageFrom').value = localStamp(Date.now() - 864e5);
 ids.get('usageTo').value = localStamp(Date.now() + 36e5);
 const beforeQuery = requests.length;
 ids.get('usageQuery').onclick(); await tick(); await tick();
 assert.equal(requests.length, beforeQuery + 1, '点「查询」只发一次请求');
 const customQuery = decodeURIComponent(windowQueries().at(-1).url);
 assert(customQuery.includes('from=' + new Date(ids.get('usageFrom').value).toISOString()), '自定义时间要按本地时刻换算成绝对时刻: ' + customQuery);
 ids.get('usageRange').value = '30d'; ids.get('usageRange').onchange(); await tick(); await tick();
 assert(ids.get('usageCompletedNote').textContent.startsWith('近 30 天'), '切回预设范围要恢复');
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 // 运行日志页仍走非窗口接口：进程记录 + 归档合并去重后展示（窗口聚合只管用量页）。
 const archivedRows = [{ request_id: 'arch-1', time: new Date(Date.now() - 11 * 60e3).toISOString(), model: 'archived-a', ok: true, status: 200, attempts: 2, total_tokens: 10, duration_ms: 100 }];
 usage.history = { recent: archivedRows };
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 assert(ids.get('requestLogsBody').innerHTML.includes('archived-a'), 'archive rows must feed the request log');
 assert.equal((ids.get('requestLogsBody').innerHTML.match(/class="request-queue"/g) || []).length, 1, '没有 queue_ms 的历史重试记录不显示排队时长');
 delete usage.history;
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 assert(ids.get('runtimeLogs').innerHTML.includes('&lt;script&gt;'));
 assert(ids.get('requestLogsBody').innerHTML.includes('upstream &lt;failed&gt; &amp; unavailable'));
 assert(!ids.get('requestLogsBody').innerHTML.includes('upstream <failed>'));
 // 尝试次数列：重试过的请求把最后一次上游错误挂进原生 tooltip，一次就成功的没有 tooltip。
 assert(html.replace(/>\s+</g, '><').includes('<th>时间</th><th>模型</th><th>账户</th><th>结果</th><th>尝试次数</th><th>TTFT</th><th>耗时</th><th>缓存</th><th>总Token</th>'), '最近请求表头及顺序');
  assert(ids.get('requestLogsBody').innerHTML.includes('<td>300 ms<small class="request-queue">含排队 10.1s</small></td><td>500 ms</td><td>800</td><td>1,200</td>'), 'TTFT、耗时、缓存和总Token列的值及顺序');
  assert.equal((ids.get('requestLogsBody').innerHTML.match(/class="request-queue"/g) || []).length, 1, '只有尝试次数大于 1 的请求显示排队时长');
  const attemptsCells = [...ids.get('requestLogsBody').innerHTML.matchAll(/<td class="number"(?: title="([^"]*)")?>(\d+)<\/td>/g)];
 assert.equal(attemptsCells.length, 2, '每条请求都要有尝试次数');
 const retriedCell = attemptsCells.find(cell => cell[2] === '3');
 const firstTryCell = attemptsCells.find(cell => cell[2] === '1');
 assert(retriedCell && retriedCell[1] === 'upstream 429: TPM &lt;limit&gt; &amp; retry later', '重试请求要用原生 tooltip 显示最后一次上游错误');
 assert(firstTryCell && firstTryCell[1] === undefined, '没有错误的请求不应有 tooltip');
 const logPoll = intervals.find(timer => timer.ms === 5000);
 assert(logPoll, 'logs must have a five-second refresh timer');
 context.location.hash = '#accounts'; events.hashchange(); await tick(); await tick();
 let beforePoll = requests.length;
 logPoll.cb(); await tick();
 assert.equal(requests.length, beforePoll, 'polling must not run outside the logs page');
 usage.recent.push({ time: new Date().toISOString(), model: 'live-failure', ok: false, outcome: 'stream_error', status: 200, error: 'live stream failed' });
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 beforePoll = requests.length;
 logPoll.cb(); await tick(); await tick();
 assert.equal(requests.length, beforePoll + 2, 'each visible log poll fetches logs and usage once');
 assert(ids.get('requestLogsBody').innerHTML.includes('live stream failed'));
 // 最近请求的结果筛选只过滤已取回的这一批记录，不发新请求。
 const outcome = ids.get('requestOutcome'), beforeFilter = requests.length;
 outcome.value = 'ok'; outcome.onchange();
 assert(ids.get('requestLogsBody').innerHTML.includes('test-model'), '成功筛选必须保留成功记录');
 assert(!ids.get('requestLogsBody').innerHTML.includes('live stream failed'), '成功筛选必须隐藏失败记录');
 outcome.value = 'fail'; outcome.onchange();
 assert(ids.get('requestLogsBody').innerHTML.includes('live stream failed') && !ids.get('requestLogsBody').innerHTML.includes('test-model'), '失败筛选只显示失败记录');
 outcome.value = ''; outcome.onchange();
 assert(ids.get('requestLogsBody').innerHTML.includes('test-model') && ids.get('requestLogsBody').innerHTML.includes('live stream failed'), '全部筛选恢复所有记录');
 assert.equal(requests.length, beforeFilter, '筛选不得触发额外请求');
 assert(html.includes('<option id="requestOutcomeAbnormal" value="abnormal">异常</option>'), '结果筛选要有「异常」选项');
 // 失败原因徽标：四个已知分支黄色，「异常」红色；异常筛选只留没归类的失败。
 const at = Date.now();
 usage.recent.push(
   { request_id: 'req-cancel', time: new Date(at - 5e3).toISOString(), model: 'cancel-model', ok: false, outcome: 'http_error', status: 503, error: 'Post "https://upstream": context canceled' },
   { request_id: 'req-missing', time: new Date(at - 4e3).toISOString(), model: 'missing-model', ok: false, outcome: 'http_error', status: 404, error: 'model test1 is not in the model catalog' },
   { request_id: 'req-no-account', time: new Date(at - 3e3).toISOString(), model: 'no-account-model', ok: false, outcome: 'http_error', status: 503, error: 'all accounts unavailable (disabled/cooldown)' },
   { request_id: 'req-capability', time: new Date(at - 2e3).toISOString(), model: 'capability-model', ok: false, outcome: 'http_error', status: 400, error: 'upstream rejected request params: InferHub.001001020.406 The request model is not multimodal' },
   // 11:48:11 那条真实记录：queue_max_attempts=1 时 TM.00001041 一次都没重试就 503，
   // 本质是上游并发已满（排队/限流），不能归进红色「异常」。
   { request_id: 'req-queue-limit', time: new Date(at - 1.8e3).toISOString(), model: 'queue-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream attempts exhausted after 1 attempt(s) on 1 account(s): codearts api code=400 http=400 path=/api/v2/chat/completions msg={"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}' },
   { request_id: 'req-upstream-timeout', time: new Date(at - 1.75e3).toISOString(), model: 'timeout-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream attempts exhausted after 1 attempt(s) on 1 account(s): codearts api code=504 http=504 path=/api/v2/chat/completions msg={"error_msg":"Backend timeout","error_code":"APIG.0203","request_id":"example"}' },
   { request_id: 'req-retry-exhausted', time: new Date(at - 1.6e3).toISOString(), model: 'exhausted-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream attempts exhausted after 3 attempt(s) on 3 account(s): codearts api code=500 http=500 msg=internal error' },
   { request_id: 'req-auth', time: new Date(at - 1.55e3).toISOString(), model: 'auth-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream attempts exhausted after 2 attempt(s) on 1 account(s): codearts api code=401 http=401 msg=unauthorized' },
   { request_id: 'req-param', time: new Date(at - 1.45e3).toISOString(), model: 'param-model', ok: false, outcome: 'http_error', status: 400, error: 'upstream rejected request params: InferHub.001001005.400 Request param is invalid' },
   { request_id: 'req-quota', time: new Date(at - 1.35e3).toISOString(), model: 'quota-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream attempts exhausted after 1 attempt(s) on 1 account(s): codearts api code=502 http=502 msg=InferHub.4291.200 benefit exhausted' },
   { request_id: 'req-legacy-upstream', time: new Date(at - 1.25e3).toISOString(), model: 'legacy-model', ok: false, outcome: 'http_error', status: 503, error: 'all accounts unavailable (disabled/cooldown): codearts api code=500 http=500 msg=internal error' },
   { request_id: 'req-no-accounts', time: new Date(at - 1.4e3).toISOString(), model: 'unconfigured-model', ok: false, outcome: 'http_error', status: 503, error: 'no accounts configured' },
   { request_id: 'req-token-invalid', time: new Date(at - 1.2e3).toISOString(), model: 'token-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream attempts exhausted after 0 attempt(s) on 1 account(s): token invalid' },
   { request_id: 'req-abnormal', time: new Date(at - 1e3).toISOString(), model: 'abnormal-model', ok: false, outcome: 'http_error', status: 502, error: 'Post "https://upstream": EOF' }
 );
 logPoll.cb(); await tick(); await tick();
 const classified = ids.get('requestLogsBody').innerHTML;
 const rowOf = model => classified.split('<tr>').find(tr => tr.includes(model)) || '';
 for (const label of ['客户端取消', '模型不存在', '无可用账号', '模型能力不符', '上游排队/限流', '上游超时', '上游服务异常', '账号鉴权失败', '请求参数错误', '福利额度耗尽']) {
   assert(classified.includes('<span class="pill warn">' + label + '</span>'), '失败原因徽标缺少「' + label + '」：' + classified);
 }
 assert(classified.includes('<span class="pill bad">异常</span>'), '没归类的失败要挂红色「异常」：' + classified);
 assert(rowOf('queue-model').includes('上游排队/限流') && !rowOf('queue-model').includes('异常'), '排队/限流不能归进「异常」：' + rowOf('queue-model'));
 assert(rowOf('exhausted-model').includes('上游服务异常') && !rowOf('exhausted-model').includes('重试耗尽'), '耗尽只是现象，要显示 500 这个真实原因：' + rowOf('exhausted-model'));
 assert(rowOf('timeout-model').includes('上游超时') && !rowOf('timeout-model').includes('重试耗尽'), 'APIG.0203 要按上游超时分类，不能叫重试耗尽：' + rowOf('timeout-model'));
 assert(rowOf('auth-model').includes('账号鉴权失败'), '401 要按鉴权失败分类，不能被耗尽文案覆盖：' + rowOf('auth-model'));
 assert(rowOf('param-model').includes('请求参数错误'), 'InferHub.001001005 要按参数错误分类：' + rowOf('param-model'));
 assert(rowOf('quota-model').includes('福利额度耗尽') && !rowOf('quota-model').includes('排队'), 'InferHub.4291.200 不是 HTTP 429，不能误归限流：' + rowOf('quota-model'));
 assert(rowOf('legacy-model').includes('上游服务异常') && !rowOf('legacy-model').includes('无可用账号'), '旧文案的底层 500 不能被外层无账号覆盖：' + rowOf('legacy-model'));
 assert(rowOf('token-model').includes('账号鉴权失败'), 'token invalid 是账号鉴权问题，不能只按 0 次尝试归类：' + rowOf('token-model'));
 assert(rowOf('unconfigured-model').includes('无可用账号'), '未配置账号也是无可用账号：' + rowOf('unconfigured-model'));
 // 下拉选项带条数：描述的是已取回的窗口（进程 100 + 归档 200），不是全量历史。
 // 17 条 = 既有 3 条（成功 / 失败 / 流式失败）+ 本次 14 条分类样例；成功 1，异常 3（failed-model、live-failure、abnormal-model）。
 assert.equal(ids.get('requestOutcomeAll').textContent, '全部 (17)', '全部选项要带窗口内的总条数');
 assert.equal(ids.get('requestOutcomeOk').textContent, '成功 (1)', '成功选项要带成功条数');
 assert.equal(ids.get('requestOutcomeFail').textContent, '失败 (16)', '失败选项要带失败条数');
 assert.equal(ids.get('requestOutcomeAbnormal').textContent, '异常 (3)', '异常选项要带未归类失败条数');
 outcome.value = 'abnormal'; outcome.onchange();
 const abnormalOnly = ids.get('requestLogsBody').innerHTML;
 assert(abnormalOnly.includes('abnormal-model'), '异常筛选要保留未归类失败');
 for (const model of ['cancel-model', 'missing-model', 'no-account-model', 'capability-model', 'queue-model', 'timeout-model', 'exhausted-model', 'auth-model', 'param-model', 'quota-model', 'legacy-model', 'unconfigured-model', 'token-model']) {
   assert(!abnormalOnly.includes(model), '异常筛选要隐藏已归类失败：' + model);
 }
 assert(!abnormalOnly.includes('test-model'), '异常筛选不得包含成功记录');
 outcome.value = ''; outcome.onchange();
 document.hidden = true; beforePoll = requests.length; logPoll.cb(); await tick();
 assert.equal(requests.length, beforePoll, 'hidden tabs must not poll');
 document.hidden = false;
 // 配置页：默认表单模式；切到 Raw 会重新读取配置文件；保存按当前模式取数据。
 assert.equal(ids.get('configForm').hidden, false, 'config page must default to the form');
 assert.equal(ids.get('configRaw').hidden, true);
 assert.equal(ids.get('cfgListen').value, ':7866');
 for (const [id, value] of multimodalControls) {
   assert.equal(typeof value === 'boolean' ? ids.get(id).checked : ids.get(id).value, typeof value === 'boolean' ? value : String(value), 'multimodal config did not load: ' + id);
 }
 const beforeMode = requests.length;
 ids.get('configModeRaw').onclick(); await tick(); await tick();
 assert(requests.length > beforeMode, 'switching mode must re-read the config file');
 assert.equal(ids.get('configForm').hidden, true); assert.equal(ids.get('configRaw').hidden, false);
 assert(ids.get('configEditor').value.includes('"listen": ":7866"'));
 const rawConfig = { listen: ':9000', custom_keep: true, max_body_mb: 96, image_shrink: { enabled: false, quality: 62, raw_extra: 'keep' }, vision_bridge: { enabled: true, prompt: 'Raw\nprompt', raw_extra: { keep: true } } };
 ids.get('configEditor').value = JSON.stringify(rawConfig);
 await ids.get('saveConfigButton').onclick();
 const rawSave = JSON.parse(requests.filter(r => r.opts.method === 'PUT').at(-1).opts.body);
 assert.equal(rawSave.listen, ':9000', 'raw mode must save the editor content');
 assert.equal(rawSave.custom_keep, true, 'raw mode must keep unknown fields');
 assert.equal(rawSave.max_body_mb, 96);
 assert.deepEqual(rawSave.image_shrink, rawConfig.image_shrink, 'Raw mode must save image config without form overrides');
 assert.deepEqual(rawSave.vision_bridge, rawConfig.vision_bridge, 'Raw mode must keep vision config and nested extensions');
 ids.get('configModeForm').onclick(); await tick(); await tick();
 assert.equal(ids.get('configForm').hidden, false);
 assert.equal(ids.get('cfgListen').value, ':7866', 'switching back must re-read the file, not the raw edit');
 for (const [id, value, edited] of multimodalControls) {
   const input = ids.get(id);
   assert.equal(typeof value === 'boolean' ? input.checked : input.value, typeof value === 'boolean' ? value : String(value), 'switching back must reload multimodal config: ' + id);
   if (typeof edited === 'boolean') { input.checked = edited; input.onchange(); }
   else { input.value = String(edited); input.oninput(); }
   assert.equal(ids.get('configEditor').dataset.dirty, 'true', 'editing must mark config dirty: ' + id);
 }
 ids.get('cfgPollMinutes').value = '15';
 ids.get('cfgCheckinHours').value = '8, 20';
 ids.get('cfgRequestLogRetentionDays').value = '14';
 await ids.get('saveConfigButton').onclick();
 const formSave = JSON.parse(requests.filter(r => r.opts.method === 'PUT').at(-1).opts.body);
 assert.equal(formSave.watch.poll_minutes, 15, 'form mode must save the form values');
 assert.deepEqual(formSave.checkin_hours, [8, 20]);
 assert.equal(formSave.listen, ':7866', 'untouched form fields keep the loaded value');
 assert.equal(formSave.max_body_mb, 64);
 const savedImageShrink = { enabled: false, trigger_body_mb: 5, target_image_mb: 2, target_total_mb: 4, max_dimension: 1800, quality: 82, extension: { preserved: 'image' } };
 const savedVisionBridge = { enabled: false, model: 'kimi-k2.6-vl', max_images: 4, max_chars: 8000, max_tokens: 2000, timeout_seconds: 80, total_timeout_seconds: 100, prompt: '请按顺序描述。\n保留表格。', extension: { preserved: 'vision' } };
 assert.deepEqual(formSave.image_shrink, savedImageShrink, 'form must save every image field and keep nested extensions');
 assert.deepEqual(formSave.vision_bridge, savedVisionBridge, 'form must save every vision field, multiline prompt and nested extensions');
 assert.deepEqual(formSave.extension, { preserved: true }, 'form must keep top-level extensions');
 assert.equal(ids.get('cfgImageShrinkEnabled').checked, false, 'saved image switch must remain off');
 assert.equal(ids.get('cfgVisionBridgeEnabled').checked, false, 'saved vision switch must remain off');
 assert.equal(ids.get('configEditor').dataset.dirty, 'false', 'saving must clear the dirty flag');
 assert(!ids.get('configEditor').value.includes('server-secret')); assert(!ids.get('configEditor').value.includes('api-secret'));
 ids.get('cfgImageShrinkEnabled').checked = true; ids.get('cfgImageShrinkEnabled').onchange();
 ids.get('cfgVisionBridgeEnabled').checked = true; ids.get('cfgVisionBridgeEnabled').onchange();
 await ids.get('saveConfigButton').onclick();
 const reenabledSave = JSON.parse(requests.filter(r => r.opts.method === 'PUT').at(-1).opts.body);
 assert.deepEqual(reenabledSave.image_shrink, { ...savedImageShrink, enabled: true }, 're-enabling image shrink must keep the edited policy');
 assert.deepEqual(reenabledSave.vision_bridge, { ...savedVisionBridge, enabled: true }, 're-enabling vision bridge must keep the edited policy');
 assert.equal(ids.get('cfgImageShrinkEnabled').checked, true);
 assert.equal(ids.get('cfgVisionBridgeEnabled').checked, true);
 ids.get('newAdminKey').value = 'rotated-secret'; ids.get('newAPIKey').value = 'new-api-secret';
 await ids.get('saveConfigButton').onclick();
 assert.equal(ids.get('newAdminKey').value, ''); assert.equal(ids.get('newAPIKey').value, ''); assert(!ids.get('configEditor').value.includes('rotated-secret')); assert(ids.get('configNotice').textContent.includes('需重启'), '保存后横幅要说明有字段需重启: ' + ids.get('configNotice').textContent);
 // 只有这四项需要重启，标记写在各自 label 后面（非强调色），横幅不再罗列字段名。
for (const [id, text] of [['cfgListen', '监听地址'], ['cfgAuthDir', '凭据目录'], ['cfgStateFile', '状态文件'], ['cfgTimeoutSeconds', '上游超时（秒）']]) {
  assert(html.includes('<label for="' + id + '">' + text + ' <span class="muted">（需重启）</span></label>'), id + ' must be labelled 需重启');
}
assert(!html.includes('listen、auth_dir、state_file'), '横幅不得再罗列重启字段');
assert(!script.includes("restart.length + ' 项'"), '保存成功的 toast 不得再带数量');
assert(!/<label for="cfg(DefaultModel|SoftRate|ErrThreshold|ErrCooldown|MaxConcurrent)">[^<]*<span class="muted">（需重启）/.test(html), '热生效字段不得标需重启');
 // 密钥热改后本次会话立刻换新 key，旧 key 只出现在保存之前/保存当次的请求里。
 for (const req of requests) { assert(!req.url.includes('secret')); assert(['Bearer test-admin-secret', 'Bearer rotated-secret'].includes(req.opts.headers.get('Authorization'))); }
 context.location.hash = '#models'; events.hashchange(); await tick(); await tick();
 const beforeModelRefresh = requests.length;
 await ids.get('refreshModelsButton').onclick();
 assert.equal(requests.length, beforeModelRefresh + 2);
 const modelRefreshRequest = requests.slice(beforeModelRefresh).find(r => r.url.endsWith('/models?refresh=1'));
 assert(modelRefreshRequest);
 assert.equal(modelRefreshRequest.opts.headers.get('Authorization'), 'Bearer rotated-secret', '热改管理密钥后必须立即改用新密钥');
 assert.equal(ids.get('refreshModelsButton').disabled, false); assert.equal(ids.get('refreshModelsButton').textContent, '重新获取');
 assert(ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 ids.get('addAccountButton').onclick();
 assert(ids.get('addDialog').open);
 assert.equal(ids.get('oauthStart').textContent, '生成授权链接');
 assert.equal(ids.get('oauthCopy').hidden, true); assert.equal(ids.get('oauthLink').hidden, true);
 for (let parent = ids.get('callbackURL'); parent && parent.id !== 'addDialog'; parent = parent.parentElement) {
   assert.equal(parent.hidden, false, 'callback field must remain visible before generating a URL');
   assert.notEqual(parent.tagName, 'details', 'callback field must not require expanding a disclosure');
   assert.notEqual(parent.id, 'oauthReady', 'callback field must not depend on generated-link visibility');
 }
 await ids.get('oauthStart').onclick();
 const firstAuthURL = 'https://codearts.huaweicloud.com/portal/authorize?ticket_id=test-1';
 assert.equal(ids.get('oauthStart').textContent, '重新生成授权链接');
 assert.equal(ids.get('oauthStart').disabled, false);
 assert.equal(ids.get('oauthCopy').hidden, false); assert.equal(ids.get('oauthLink').hidden, false);
 assert.equal(ids.get('oauthLink').href, firstAuthURL);
 assert.equal(ids.get('oauthLink').target, '_blank'); assert(ids.get('oauthLink').rel.split(/\s+/).includes('noopener'));
 await ids.get('oauthCopy').onclick(); assert.equal(copiedURLs.at(-1), firstAuthURL);
 await ids.get('oauthStart').onclick();
 const secondAuthURL = 'https://codearts.huaweicloud.com/portal/authorize?ticket_id=test-2';
 assert.equal(ids.get('oauthLink').href, secondAuthURL);
 await ids.get('oauthCopy').onclick(); assert.equal(copiedURLs.at(-1), secondAuthURL);
 const originalFetch = context.fetch;
 let rejectOldPoll;
 context.fetch = (url, options) => url.endsWith('/oauth/poll') ? new Promise((resolve, reject) => { rejectOldPoll = reject; }) : originalFetch(url, options);
 const oldPoll = ids.get('oauthPoll').onclick();
 await tick();
 await ids.get('oauthStart').onclick();
 rejectOldPoll(new Error('old session expired')); await oldPoll;
 assert.equal(ids.get('addError').hidden, true, 'an old failed poll must not invalidate the regenerated session');
 context.fetch = originalFetch;
 await ids.get('oauthPoll').onclick();
 assert.equal(JSON.parse(requests.at(-1).opts.body).session_id, 'session-3');
 clipboardError = true;
 const beforeClipboardError = ids.get('toasts').children.length;
 await ids.get('oauthCopy').onclick();
 assert.equal(copiedURLs.length, 2, 'clipboard failure must not report another copied URL');
 assert.equal(ids.get('toasts').children.length, beforeClipboardError + 1);
 assert(ids.get('toasts').children.at(-1).className.includes('error'));
 assert(ids.get('toasts').children.at(-1).textContent.includes('复制'), 'clipboard failure needs visible feedback');
 clipboardError = false;
 oauthURLOverride = 'javascript:alert(1)';
 await ids.get('oauthStart').onclick();
 assert(ids.get('addError').textContent.length > 0, 'unsafe authorization URL must be rejected');
 assert(!String(ids.get('oauthLink').href).startsWith('javascript:'), 'unsafe authorization URL reached the link');
 oauthURLOverride = '';
 ids.get('addDialog').close(); ids.get('addAccountButton').onclick();
 assert.equal(ids.get('oauthStart').textContent, '生成授权链接');
 assert.equal(ids.get('oauthCopy').hidden, true); assert.equal(ids.get('oauthLink').hidden, true);
 assert.equal(ids.get('oauthLink').href, undefined);
 ids.get('addDialog').close();
 const beforeDelete = requests.length;
 await ids.get('accountsBody').onclick({ target: { closest: () => ({ dataset: { accountAction: 'delete', uid: 'u1' } }) } });
 assert(ids.get('deleteDialog').open); assert.equal(requests.length, beforeDelete, 'opening delete dialog must not delete');
 await ids.get('confirmDelete').onclick(); assert.equal(requests.filter(r => r.url.endsWith('/accounts/delete')).length, 1);
 // 零账号：模型目录返回空列表时，面板显示空态而不是残留旧数据。
 context.fetch = (url, opts) => url.replace('/admin/api/', '').startsWith('models') ? { ok: true, status: 200, json: async () => ({ data: [] }) } : originalFetch(url, opts);
 context.location.hash = '#accounts'; events.hashchange(); await tick(); await tick();
 context.location.hash = '#models'; events.hashchange(); await tick(); await tick();
 assert.equal(ids.get('modelCount').textContent, 0);
 assert(ids.get('modelsBody').innerHTML.includes('模型目录为空'));
 assert(!ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 context.fetch = originalFetch;
 // 账号池状态懒刷新：账号页可见时只拉 overview；后台标签页/其它页/对话框打开/搜索中都不拉。
 const overviewPoll = intervals.find(timer => timer.ms === 10000);
 assert(overviewPoll, 'accounts page must have a ten-second overview refresh timer');
 context.location.hash = '#accounts'; events.hashchange(); await tick(); await tick();
 document.hidden = false;
 let beforeOverview = requests.length;
 overviewPoll.cb(); await tick(); await tick();
 assert.equal(requests.length, beforeOverview + 1, 'visible accounts page polls overview once');
 assert(requests[requests.length - 1].url.endsWith('/overview'), 'the lazy poll must be a plain overview read');
 document.hidden = true;
 beforeOverview = requests.length;
 overviewPoll.cb(); await tick();
 assert.equal(requests.length, beforeOverview, 'hidden tab must not poll the pool');
 document.hidden = false;
 context.location.hash = '#models'; events.hashchange(); await tick(); await tick();
 beforeOverview = requests.length;
 overviewPoll.cb(); await tick();
 assert.equal(requests.length, beforeOverview, 'other pages must not poll the pool');
 context.location.hash = '#accounts'; events.hashchange(); await tick(); await tick();
 ids.get('addDialog').open = true;
 beforeOverview = requests.length;
 overviewPoll.cb(); await tick();
 assert.equal(requests.length, beforeOverview, 'open dialogs must pause the lazy refresh');
 ids.get('addDialog').open = false;
 ids.get('accountSearch').value = 'busy';
 beforeOverview = requests.length;
 overviewPoll.cb(); await tick();
 assert.equal(requests.length, beforeOverview, 'an active search must not be disturbed');
 ids.get('accountSearch').value = '';
 ids.get('logoutButton').onclick(); assert(ids.get('loginDialog').open); assert.equal(context.sessionStorage.getItem('codearts2api-panel.admin-key'), undefined);
 console.log('PASS: six views, balance history charts, live log polling, escaped recent failures, OAuth controls, session-key login, secret redaction and deletion gate.');
})().catch(error => { console.error(error); process.exitCode = 1; });

