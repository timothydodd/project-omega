import { lineChart } from './chart.js';

// ---------- Helpers ----------
const $ = (sel, root = document) => root.querySelector(sel);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
};

const nf = new Intl.NumberFormat();
const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
const fmtNum = n => (n >= 10_000 ? compact.format(n) : nf.format(Math.round(n ?? 0)));
const fmtPct = r => `${Math.round((r ?? 0) * 100)}%`;
function fmtDur(ms) {
  const s = Math.round((ms ?? 0) / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}
const fmtDate = ts => new Date(ts).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
const fmtDateTime = ts => new Date(ts).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
const fmtClock = ts => new Date(ts).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', second: '2-digit' });
function ago(ts) {
  const s = (Date.now() - ts) / 1000;
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  if (s < 7 * 86400) return `${Math.round(s / 86400)}d ago`;
  return fmtDate(ts);
}
let regionNames;
function countryName(code) {
  if (!code || code === 'Unknown') return 'Unknown';
  try { regionNames ??= new Intl.DisplayNames(undefined, { type: 'region' }); return regionNames.of(code) ?? code; } catch { return code; }
}
function ordinal(n) {
  const s = ['th', 'st', 'nd', 'rd'], v = n % 100;
  return n + (s[(v - 20) % 10] || s[v] || s[0]);
}

const RANGES = [
  ['today', 'Today', 'yesterday so far'], ['24h', '24h', 'previous 24 hours'], ['3d', '3d', 'previous 3 days'],
  ['7d', '7d', 'previous 7 days'], ['30d', '30d', 'previous 30 days'], ['90d', '90d', 'previous 90 days'],
];
const WINDOW_LABELS = { '24h': ['Past 24 hours', 'previous 24 hours'], '3d': ['Past 3 days', 'previous 3 days'], '7d': ['Past 7 days', 'previous 7 days'], '30d': ['Past 30 days', 'previous 30 days'] };

function delta(cur, prev, label, { invert = false } = {}) {
  if (!prev) return `<div class="delta flat">No data for the ${esc(label)}</div>`;
  const pct = Math.round(((cur - prev) / prev) * 100);
  if (pct === 0) return `<div class="delta flat">Same as the ${esc(label)}</div>`;
  const good = invert ? pct < 0 : pct > 0;
  return `<div class="delta ${good ? 'up' : 'down'}">${pct > 0 ? '↑' : '↓'} ${Math.abs(pct)}% <span class="muted">vs ${esc(label)}</span></div>`;
}

const AVATAR_COLORS = ['#2a78d6', '#eb6834', '#1baf7a', '#c98500', '#d55181', '#008300', '#4a3aa7', '#e34948'];
function avatar(seed, name) {
  let h = 0;
  for (const ch of String(seed)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  const initials = String(name).replace(/^Visitor /, '').split(/[\s@._-]+/).filter(Boolean).slice(0, 2).map(w => w[0].toUpperCase()).join('');
  return `<span class="avatar" aria-hidden="true" style="background:${AVATAR_COLORS[h % AVATAR_COLORS.length]}">${esc(initials || '?')}</span>`;
}
function personName(traits, userId, visitorId) {
  const t = traits || {};
  return t.name || t.email || userId || `Visitor ${String(visitorId).slice(0, 5)}`;
}
function person(traits, userId, visitorId, sub = '') {
  const name = personName(traits, userId, visitorId);
  return `<div class="person">${avatar(userId || visitorId, name)}<div><div>${esc(name)}</div>${sub ? `<div class="sub">${sub}</div>` : ''}</div></div>`;
}

// ---------- API ----------
class AuthError extends Error {}
async function api(path, { method = 'GET', body } = {}) {
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new Error("Can't reach the Omega server. Check that it's running, then reload.");
  }
  if (res.status === 401 && path !== '/api/login') {
    state.user = null;
    renderAuth(false);
    throw new AuthError('Signed out');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data;
}

// ---------- State ----------
const state = {
  user: null,
  sites: [],
  siteId: Number(store.get('omega.site')) || null,
  range: store.get('omega.range') || '7d',
  metric: 'visitors',
  settings: { publicUrl: '' },
  live: { count: 0, visitors: [], now: Date.now() },
};
let liveSource = null;
const liveListeners = new Set();
let viewCleanups = [];
let renderToken = 0;

function site() { return state.sites.find(s => s.id === state.siteId); }

function connectLive() {
  liveSource?.close();
  state.live = { count: 0, visitors: [], now: Date.now() };
  if (!state.siteId) return;
  liveSource = new EventSource(`/api/live/stream?site=${state.siteId}`);
  liveSource.onmessage = e => {
    state.live = JSON.parse(e.data);
    const badge = $('#live-count');
    if (badge) badge.textContent = state.live.count ? fmtNum(state.live.count) : '';
    for (const fn of liveListeners) fn(state.live);
  };
}
function onLive(fn) {
  liveListeners.add(fn);
  viewCleanups.push(() => liveListeners.delete(fn));
}

// ---------- Routing ----------
function route() {
  const [path, qs] = location.hash.replace(/^#/, '').split('?');
  return { parts: (path || '/overview').split('/').filter(Boolean).map(decodeURIComponent), params: new URLSearchParams(qs || '') };
}
function hrefWith(params) {
  const r = route();
  const p = new URLSearchParams(r.params);
  for (const [k, v] of Object.entries(params)) v == null ? p.delete(k) : p.set(k, v);
  const qs = p.toString();
  return `#/${r.parts.map(encodeURIComponent).join('/')}${qs ? `?${qs}` : ''}`;
}
addEventListener('hashchange', () => render());

// ---------- Shell ----------
const ICONS = {
  overview: '<path d="M3 13h4v5H3zM8.5 8h4v10h-4zM14 3h4v15h-4z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/>',
  live: '<circle cx="10" cy="10" r="2.2" fill="currentColor"/><path d="M5.8 5.8a6 6 0 0 0 0 8.4M14.2 5.8a6 6 0 0 1 0 8.4M3.3 3.3a9.5 9.5 0 0 0 0 13.4M16.7 3.3a9.5 9.5 0 0 1 0 13.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
  sessions: '<path d="M3 5h14M3 10h14M3 15h9" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
  visitors: '<circle cx="10" cy="7" r="3.2" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M3.5 17c.8-3.2 3.4-5 6.5-5s5.7 1.8 6.5 5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
  sites: '<circle cx="10" cy="10" r="7" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M3 10h14M10 3c2 2 3 4.4 3 7s-1 5-3 7c-2-2-3-4.4-3-7s1-5 3-7z" fill="none" stroke="currentColor" stroke-width="1.6"/>',
  settings: '<circle cx="10" cy="10" r="2.6" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M10 2.5v2.2M10 15.3v2.2M2.5 10h2.2M15.3 10h2.2M4.7 4.7l1.6 1.6M13.7 13.7l1.6 1.6M4.7 15.3l1.6-1.6M13.7 6.3l1.6-1.6" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>',
};
const LOGO = `<svg class="brand-mark" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="#2a78d6"/><path d="M9 23h4.5v-2.2A7 7 0 1 1 18.5 20.8V23H23" fill="none" stroke="#fff" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>`;

function renderShell(view) {
  const nav = [['overview', 'Overview'], ['live', 'Live'], ['sessions', 'Sessions'], ['visitors', 'Visitors'], ['sites', 'Sites'], ['settings', 'Settings']];
  $('#app').innerHTML = `
    <div class="shell">
      <aside class="rail">
        <div class="brand">${LOGO}<span>Omega</span></div>
        ${state.sites.length ? `
        <div class="site-picker">
          <label class="sr-only" for="site-select">Site</label>
          <select id="site-select">${state.sites.map(s => `<option value="${s.id}" ${s.id === state.siteId ? 'selected' : ''}>${esc(s.name)}</option>`).join('')}</select>
        </div>` : ''}
        <nav class="nav" aria-label="Main">
          ${nav.map(([key, label]) => `
            <a href="#/${key}" ${view === key ? 'aria-current="page"' : ''}>
              <svg viewBox="0 0 20 20" aria-hidden="true">${ICONS[key]}</svg>${label}
              ${key === 'live' ? `<span class="count" id="live-count">${state.live.count ? fmtNum(state.live.count) : ''}</span>` : ''}
            </a>`).join('')}
        </nav>
        <div class="rail-foot">
          <span class="muted">${esc(state.user?.email)}</span>
          <button type="button" id="sign-out">Sign out</button>
        </div>
      </aside>
      <main id="main"></main>
    </div>`;
  $('#site-select')?.addEventListener('change', e => {
    state.siteId = Number(e.target.value);
    store.set('omega.site', state.siteId);
    connectLive();
    const r = route();
    // Detail pages belong to the previous site, so go back to the list.
    location.hash = `#/${r.parts[0] === 'sites' ? 'sites' : r.parts[0] || 'overview'}`;
    render();
  });
  $('#sign-out').addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' }).catch(() => {});
    liveSource?.close();
    state.user = null;
    renderAuth(false);
  });
}

function pageHead(title, right = '', crumb = '') {
  return `<header class="page-head">${crumb}<h1>${esc(title)}</h1><span class="spacer"></span>${right}</header>`;
}

function rangeControl() {
  return `<div class="segmented" role="group" aria-label="Date range">${RANGES.map(([key, label]) =>
    `<button type="button" data-range="${key}" aria-pressed="${state.range === key}">${label}</button>`).join('')}</div>`;
}
function bindRange(main) {
  main.querySelectorAll('[data-range]').forEach(b => b.addEventListener('click', () => {
    state.range = b.dataset.range;
    store.set('omega.range', state.range);
    render();
  }));
}

// Row links: any <tr data-href> is clickable and keyboard reachable.
document.addEventListener('click', e => {
  const tr = e.target.closest?.('tr[data-href]');
  if (tr && !e.target.closest('a, button')) location.hash = tr.dataset.href;
});
document.addEventListener('keydown', e => {
  if (e.key === 'Enter' && e.target.matches?.('tr[data-href]')) location.hash = e.target.dataset.href;
});

// ---------- Views ----------
async function render() {
  viewCleanups.forEach(fn => fn());
  viewCleanups = [];
  const token = ++renderToken;
  const alive = () => token === renderToken;
  if (!state.user) return;

  const r = route();
  let view = r.parts[0] || 'overview';
  if (!VIEWS[view]) view = 'overview';
  if (!state.sites.length && view !== 'sites' && view !== 'settings') { location.hash = '#/sites'; return; }
  renderShell(view);
  const main = $('#main');
  try {
    await VIEWS[view](main, r, alive);
  } catch (err) {
    if (err instanceof AuthError || !alive()) return;
    main.innerHTML = `${pageHead('Something went wrong')}<div class="card card-body"><p class="error">${esc(err.message)}</p><button class="btn" onclick="location.reload()">Reload</button></div>`;
  }
}

const VIEWS = {
  overview: overviewView,
  live: liveView,
  sessions: (main, r, alive) => (r.parts[1] ? sessionDetailView(main, r.parts[1], alive) : sessionsView(main, r, alive)),
  visitors: (main, r, alive) => (r.parts[1] ? visitorDetailView(main, r.parts[1], alive) : visitorsView(main, r, alive)),
  sites: sitesView,
  settings: settingsView,
};

// ----- Overview -----
async function overviewView(main, r, alive) {
  const path = r.params.get('path');
  const source = r.params.get('source');
  const country = r.params.get('country');
  const bots = r.params.get('bots') === '1';
  const chips = [
    path && ['path', `Page is ${path}`],
    source && ['source', `Source is ${source}`],
    country && ['country', `Country is ${countryName(country)}`],
    bots && ['bots', 'Including likely bots'],
  ].filter(Boolean);
  main.innerHTML = pageHead('Overview', rangeControl()) +
    (chips.length ? `<div class="chips">${chips.map(([k, label]) => `<span class="chip">${esc(label)}<button type="button" data-clear="${k}" aria-label="Remove filter: ${esc(label)}">×</button></span>`).join('')}</div>` : '') +
    '<div id="ov"><div class="loading">Loading…</div></div>';
  bindRange(main);
  main.querySelectorAll('[data-clear]').forEach(b => b.addEventListener('click', () => { location.hash = hrefWith({ [b.dataset.clear]: null }); }));

  const q = new URLSearchParams({ site: state.siteId, range: state.range, tz: -new Date().getTimezoneOffset() });
  if (path) q.set('path', path);
  if (source) q.set('source', source);
  if (country) q.set('country', country);
  if (bots) q.set('bots', '1');
  const d = await api(`/api/stats?${q}`);
  if (!alive()) return;
  const prevLabel = RANGES.find(x => x[0] === state.range)[2];
  const ov = $('#ov');

  const noData = d.kpis.sessions === 0 && d.windows.every(w => w.visitors === 0) && !path && !source;
  const botNote = !bots && d.botSessions > 0
    ? `<p class="footnote">Hiding ${fmtNum(d.botSessions)} ${d.botSessions === 1 ? 'session' : 'sessions'} in this period that ${d.botSessions === 1 ? 'looks' : 'look'} like ${d.botSessions === 1 ? 'a bot' : 'bots'}. <a href="${hrefWith({ bots: '1' })}">Include them</a></p>`
    : '';
  ov.innerHTML = `
    ${noData ? `<div class="card card-body" style="margin-bottom:16px">No visits recorded yet. <a href="#/sites">Add the tracking script</a> to ${esc(site()?.name)}, then open a page on the site. You'll show up on the Live view within a few seconds.</div>` : ''}
    <section class="windows" aria-label="Unique visitors">
      <div class="window live" role="link" tabindex="0" id="live-cell">
        <h3>Live now</h3>
        <div class="big num"><span class="pulse" aria-hidden="true"></span><span id="live-big">${fmtNum(state.live.count)}</span></div>
        <div class="delta soft">Visitors on the site right now</div>
      </div>
      ${d.windows.map(w => `
        <div class="window">
          <h3>${WINDOW_LABELS[w.key][0]}</h3>
          <div class="big num">${w.estimated ? '<span title="Estimate">≈</span>' : ''}${fmtNum(w.visitors)}</div>
          ${delta(w.visitors, w.previous, WINDOW_LABELS[w.key][1])}
        </div>`).join('')}
    </section>
    ${botNote}
    ${d.windows.some(w => w.estimated) || d.kpis.visitorsEstimated ? `<p class="footnote">≈ Includes cookieless visitors, who are recognised for one day at a time. Someone who comes back on several days is counted once per day, so multi-day totals run high.</p>` : ''}
    <section class="card" aria-label="Trend">
      <div class="metrics" role="tablist">
        ${[
          ['visitors', 'Unique visitors', (d.kpis.visitorsEstimated ? '≈' : '') + fmtNum(d.kpis.visitors), delta(d.kpis.visitors, d.previous.visitors, prevLabel)],
          ['pageviews', 'Page views', fmtNum(d.kpis.pageviews), delta(d.kpis.pageviews, d.previous.pageviews, prevLabel)],
          ['sessions', 'Sessions', fmtNum(d.kpis.sessions), delta(d.kpis.sessions, d.previous.sessions, prevLabel)],
          ['bounce', 'Bounce rate', fmtPct(d.kpis.bounceRate), delta(d.kpis.bounceRate, d.previous.bounceRate, prevLabel, { invert: true })],
          ['duration', 'Avg. visit', fmtDur(d.kpis.avgDuration), delta(d.kpis.avgDuration, d.previous.avgDuration, prevLabel)],
        ].map(([key, label, value, dl]) => {
          const chartable = ['visitors', 'pageviews', 'sessions'].includes(key);
          return `<${chartable ? `button type="button" role="tab" aria-selected="${state.metric === key}" data-metric="${key}"` : 'div'} class="metric">
            <div class="label">${label}</div><div class="value num">${value}</div>${dl}
          </${chartable ? 'button' : 'div'}>`;
        }).join('')}
      </div>
      <div class="chart" id="chart"></div>
    </section>
    <div class="grid-2" id="panels"></div>`;

  const drawChart = () => {
    const label = { visitors: 'Unique visitors', pageviews: 'Page views', sessions: 'Sessions' }[state.metric];
    const hourly = d.series.bucket === 'hour';
    const multiDay = d.until - d.since > 26 * 3_600_000;
    viewCleanups.push(lineChart($('#chart'), {
      label,
      points: d.series.points.map(p => ({ t: p.t, value: p[state.metric] })),
      formatValue: fmtNum,
      formatTick: t => hourly
        ? new Date(t).toLocaleString(undefined, multiDay ? { weekday: 'short', hour: 'numeric' } : { hour: 'numeric' })
        : fmtDate(t),
      formatTitle: t => hourly
        ? new Date(t).toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric' })
        : new Date(t).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' }),
    }));
  };
  drawChart();
  ov.querySelectorAll('[data-metric]').forEach(b => b.addEventListener('click', () => {
    state.metric = b.dataset.metric;
    ov.querySelectorAll('[data-metric]').forEach(x => x.setAttribute('aria-selected', String(x === b)));
    drawChart();
  }));

  const liveCell = $('#live-cell');
  liveCell.addEventListener('click', () => { location.hash = '#/live'; });
  liveCell.addEventListener('keydown', e => { if (e.key === 'Enter') location.hash = '#/live'; });
  onLive(l => { const n = $('#live-big'); if (n) n.textContent = fmtNum(l.count); });

  const filterPath = row => { location.hash = hrefWith({ path: row.name }); };
  const filterSource = row => { location.hash = hrefWith({ source: row.name }); };
  const visitorsCol = { key: 'visitors', label: 'Visitors', fmt: fmtNum };
  const panels = $('#panels');
  panels.append(
    breakdownPanel('Pages', [
      { label: 'Top pages', rows: d.pages, nameLabel: 'Page', cols: [visitorsCol, { key: 'pageviews', label: 'Views', fmt: fmtNum }, { key: 'avg_time', label: 'Time on page', fmt: fmtDur }], onClick: filterPath },
      { label: 'Entry', rows: d.entryPages, nameLabel: 'Landing page', cols: [visitorsCol, { key: 'sessions', label: 'Entrances', fmt: fmtNum }], onClick: filterPath },
      { label: 'Exit', rows: d.exitPages, nameLabel: 'Exit page', cols: [visitorsCol, { key: 'sessions', label: 'Exits', fmt: fmtNum }], onClick: filterPath },
    ]),
    breakdownPanel('Where visitors came from', [
      { label: 'Sources', rows: d.sources, nameLabel: 'Source', cols: [visitorsCol, { key: 'sessions', label: 'Sessions', fmt: fmtNum }], onClick: filterSource },
      { label: 'Referrers', rows: d.referrers, nameLabel: 'Referring page', cols: [visitorsCol], display: r => r.name.replace(/^https?:\/\/(www\.)?/, '').replace(/\/$/, ''), empty: 'No referring pages in this period.' },
      { label: 'Campaigns', rows: d.campaigns, nameLabel: 'utm_campaign', cols: [visitorsCol, { key: 'sessions', label: 'Sessions', fmt: fmtNum }], empty: 'No campaign traffic. Add ?utm_campaign=… to links you share.' },
    ]),
    breakdownPanel('Devices', [
      { label: 'Device', rows: d.devices, nameLabel: 'Device', cols: [visitorsCol] },
      { label: 'Browser', rows: d.browsers, nameLabel: 'Browser', cols: [visitorsCol] },
      { label: 'OS', rows: d.os, nameLabel: 'Operating system', cols: [visitorsCol] },
    ]),
    breakdownPanel('Countries', [
      {
        label: 'Countries', rows: d.countries, nameLabel: 'Country', display: r => countryName(r.name),
        cols: [visitorsCol, { key: 'sessions', label: 'Sessions', fmt: fmtNum }],
        onClick: row => { location.hash = hrefWith({ country: row.name }); },
        // DB-IP's licence (CC BY 4.0) asks for this attribution wherever its data is shown.
        footer: d.geoip ? '<p class="attribution"><a href="https://db-ip.com" target="_blank" rel="noopener">IP Geolocation by DB-IP</a></p>' : '',
        empty: d.geoip ? 'No visits in this period.' : 'Country lookup is off. Run "omega geoip-update" on the server (the container image already includes it).',
      },
    ]),
    breakdownPanel('Events', [
      { label: 'Events', rows: d.events, nameLabel: 'Event', cols: [visitorsCol, { key: 'count', label: 'Count', fmt: fmtNum }], empty: 'No custom events yet. Call omega.track("Signup") or add data-omega-event="Signup" to a button.' },
    ]),
  );
}

function breakdownPanel(title, tabs) {
  const card = document.createElement('section');
  card.className = 'card';
  let active = 0;
  let expanded = false;
  const draw = () => {
    const t = tabs[active];
    const rows = expanded ? t.rows : t.rows.slice(0, 8);
    const metric = t.cols[0].key;
    const max = Math.max(1, ...t.rows.map(r => r[metric]));
    card.innerHTML = `
      <div class="card-head"><h2>${esc(title)}</h2><span class="spacer"></span>
        ${tabs.length > 1 ? `<div class="tabs" role="tablist">${tabs.map((x, i) => `<button type="button" role="tab" aria-selected="${i === active}" data-tab="${i}">${esc(x.label)}</button>`).join('')}</div>` : ''}
      </div>
      <div class="card-body">
        ${rows.length ? `
          <table class="bars">
            <thead><tr><th>${esc(t.nameLabel)}</th>${t.cols.map(c => `<th class="r">${esc(c.label)}</th>`).join('')}</tr></thead>
            <tbody>${rows.map((r, i) => `
              <tr ${t.onClick ? `class="clickable" tabindex="0" data-row="${i}" title="Filter by ${esc(r.name)}"` : ''}>
                <td class="name"><span class="fill" style="width:${(r[metric] / max) * 100}%"></span><span class="label">${esc(t.display ? t.display(r) : r.name)}</span></td>
                ${t.cols.map(c => `<td class="r num">${c.fmt(r[c.key])}</td>`).join('')}
              </tr>`).join('')}
            </tbody>
          </table>
          ${t.rows.length > 8 ? `<button type="button" class="btn small more" data-more>${expanded ? 'Show fewer' : `Show all ${t.rows.length}`}</button>` : ''}`
        : `<div class="empty">${esc(t.empty || 'No data for this period.')}</div>`}
        ${t.footer ?? ''}
      </div>`;
    card.querySelectorAll('[data-tab]').forEach(b => b.addEventListener('click', () => { active = Number(b.dataset.tab); expanded = false; draw(); }));
    card.querySelector('[data-more]')?.addEventListener('click', () => { expanded = !expanded; draw(); });
    card.querySelectorAll('[data-row]').forEach(tr => {
      const go = () => t.onClick(rows[Number(tr.dataset.row)]);
      tr.addEventListener('click', go);
      tr.addEventListener('keydown', e => { if (e.key === 'Enter') go(); });
    });
  };
  draw();
  return card;
}

// ----- Live -----
async function liveView(main) {
  main.innerHTML = `${pageHead('Live')}<div id="live"></div>`;
  const draw = () => {
    const l = state.live;
    const now = Date.now();
    const pages = new Map();
    for (const v of l.visitors) {
      const p = pages.get(v.path) ?? { path: v.path, title: v.title, count: 0 };
      p.count++;
      pages.set(v.path, p);
    }
    const byPage = [...pages.values()].sort((a, b) => b.count - a.count);
    $('#live').innerHTML = `
      <section class="card live-head" aria-live="polite">
        <span class="pulse" aria-hidden="true"></span>
        <div><div class="big num">${fmtNum(l.count)}</div></div>
        <div><div>${l.count === 1 ? 'person' : 'people'} on ${esc(site()?.name)} right now</div><div class="muted">Anyone active in the last minute. Updates every few seconds.${l.bots ? ` Not counting ${l.bots} likely ${l.bots === 1 ? 'bot' : 'bots'}.` : ''}</div></div>
      </section>
      <div class="live-grid" style="margin-top:16px">
        <section class="card">
          <div class="card-head"><h2>Pages being viewed</h2></div>
          <div class="card-body">
            ${byPage.length ? `<div class="live-pages">${byPage.map(p => `
              <div class="live-page">
                <div class="path" title="${esc(p.title || '')}">${esc(p.path)}</div>
                <div class="num soft">${p.count}</div>
                <div class="live-dots" aria-hidden="true">${'<span></span>'.repeat(Math.min(p.count, 40))}</div>
              </div>`).join('')}</div>` : '<div class="empty">Nobody is on the site right now.</div>'}
          </div>
        </section>
        <section class="card">
          <div class="card-head"><h2>Visitors</h2></div>
          <div class="card-body" style="padding:8px 0 0">
            ${l.visitors.length ? `<div class="table-wrap"><table class="table">
              <thead><tr><th>Visitor</th><th>Current page</th><th>On page</th><th>Came from</th><th>Country</th><th>Device</th><th>Visit so far</th></tr></thead>
              <tbody>${l.visitors.map(v => `
                <tr data-href="#/sessions/${esc(v.sessionId)}" tabindex="0">
                  <td>${person(v.userName ? { name: v.userName } : null, v.userId, v.visitorId)}</td>
                  <td><div class="path">${esc(v.path)}</div>${v.title ? `<div class="sub">${esc(v.title)}</div>` : ''}</td>
                  <td class="num">${fmtDur(now - v.pageStartedAt)}</td>
                  <td>${esc(v.source)}</td>
                  <td>${esc(countryName(v.country))}</td>
                  <td>${esc(v.device)}<div class="sub">${esc(v.browser)}</div></td>
                  <td class="num">${fmtDur(now - v.startedAt)}<div class="sub">${v.pageviews} ${v.pageviews === 1 ? 'page' : 'pages'}</div></td>
                </tr>`).join('')}</tbody>
            </table></div>` : '<div class="empty" style="padding-bottom:28px">Visitors appear here as soon as they open a page.</div>'}
          </div>
        </section>
      </div>`;
  };
  draw();
  onLive(draw);
  const timer = setInterval(draw, 5000);
  viewCleanups.push(() => clearInterval(timer));
}

// ----- Sessions -----
function sessionRows(sessions, { showVisitor = true } = {}) {
  return sessions.map(s => `
    <tr data-href="#/sessions/${esc(s.id)}" tabindex="0">
      ${showVisitor ? `<td>${person(s.traits, s.user_id, s.visitor_id, s.bot ? '<span class="tag bot">Likely bot</span>' : '')}</td>` : ''}
      <td>${esc(s.source)}${s.utm_campaign ? `<div class="sub">${esc(s.utm_campaign)}</div>` : ''}</td>
      <td><div class="path">${esc(s.entry_path)}</div>${s.pageviews > 1 ? `<div class="sub">then ${s.pageviews - 1} more ${s.pageviews === 2 ? 'page' : 'pages'}, left from ${esc(s.exit_path)}</div>` : '<div class="sub">Single page</div>'}</td>
      <td class="num">${fmtDur(s.last_seen - s.started_at)}</td>
      <td>${esc(s.device)}<div class="sub">${esc(s.browser)}, ${esc(s.os)}</div></td>
      <td>${esc(countryName(s.country))}</td>
      <td class="num" title="${esc(new Date(s.started_at).toLocaleString())}">${ago(s.started_at)}</td>
    </tr>`).join('');
}

async function sessionsView(main, r, alive) {
  const bots = r.params.get('bots') === '1';
  main.innerHTML = `${pageHead('Sessions', `
    <div class="segmented" role="group" aria-label="Show">
      <button type="button" data-bots="0" aria-pressed="${!bots}">People</button>
      <button type="button" data-bots="1" aria-pressed="${bots}">Include likely bots</button>
    </div>`)}<section class="card"><div class="loading">Loading…</div></section>`;
  main.querySelectorAll('[data-bots]').forEach(b => b.addEventListener('click', () => { location.hash = hrefWith({ bots: b.dataset.bots === '1' ? '1' : null }); }));
  const base = `/api/sessions?site=${state.siteId}${bots ? '&bots=1' : ''}`;
  let rows = await api(base);
  if (!alive()) return;
  const card = main.querySelector('.card');
  const draw = (hasMore) => {
    card.innerHTML = rows.length ? `
      <div class="table-wrap"><table class="table">
        <thead><tr><th>Visitor</th><th>Came from</th><th>Pages</th><th>Duration</th><th>Device</th><th>Country</th><th>Started</th></tr></thead>
        <tbody>${sessionRows(rows)}</tbody>
      </table></div>
      ${hasMore ? '<div class="card-body"><button type="button" class="btn small" id="more">Load older sessions</button></div>' : ''}`
      : '<div class="empty">No sessions yet.</div>';
    $('#more')?.addEventListener('click', async () => {
      const older = await api(`${base}&before=${rows.at(-1).started_at}`);
      if (!alive()) return;
      rows = rows.concat(older);
      draw(older.length === 50);
    });
  };
  draw(rows.length === 50);
}

async function sessionDetailView(main, id, alive) {
  main.innerHTML = `${pageHead('Session', '', '<a class="crumb" href="#/sessions">Sessions /</a>')}<div class="loading">Loading…</div>`;
  const { session: s, pageviews, events } = await api(`/api/sessions/${encodeURIComponent(id)}?site=${state.siteId}`);
  if (!alive()) return;
  const name = personName(s.traits, s.user_id, s.visitor_id);
  const steps = [
    ...pageviews.map((p, i) => ({ kind: 'page', ts: p.ts, i, p })),
    ...events.map(e => ({ kind: 'event', ts: e.ts, e })),
  ].sort((a, b) => a.ts - b.ts);
  const liveNow = state.live.visitors.some(v => v.sessionId === s.id);

  main.innerHTML = `
    ${pageHead(`${name}, ${fmtDateTime(s.started_at)}`, liveNow ? '<span class="chip"><span class="pulse" aria-hidden="true" style="width:8px;height:8px"></span>Live now</span>' : '', '<a class="crumb" href="#/sessions">Sessions /</a>')}
    <div class="split">
      <section class="card">
        <div class="card-head"><h2>Journey</h2><span class="spacer"></span><span class="muted">${pageviews.length} ${pageviews.length === 1 ? 'page' : 'pages'}, ${fmtDur(s.last_seen - s.started_at)}</span></div>
        <div class="card-body">
          <p class="soft" style="margin:0 0 8px">Arrived from <b>${esc(s.source)}</b>${s.referrer ? ` (<span title="${esc(s.referrer)}">${esc(s.referrer.replace(/^https?:\/\//, ''))}</span>)` : ''}</p>
          <ol class="journey">
            ${steps.map(step => step.kind === 'page' ? `
              <li>
                <span class="node">${step.i + 1}</span>
                <div class="what"><b>${esc(step.p.path)}</b>${step.p.title ? `<div class="sub muted">${esc(step.p.title)}</div>` : ''}</div>
                <div class="when">${fmtClock(step.ts)}<div>${step.p.duration ? `${fmtDur(step.p.duration)} on page` : ''}</div></div>
              </li>` : `
              <li class="event">
                <span class="node" aria-hidden="true"></span>
                <div class="what">Event: <b>${esc(step.e.name)}</b>${step.e.props ? `<div class="props">${Object.entries(step.e.props).map(([k, v]) => `${esc(k)}: ${esc(typeof v === 'object' ? JSON.stringify(v) : v)}`).join(', ')}</div>` : ''}</div>
                <div class="when">${fmtClock(step.ts)}</div>
              </li>`).join('')}
          </ol>
        </div>
      </section>
      <div class="stack" style="gap:16px">
      <section class="card">
        <div class="card-head"><h2>Details</h2></div>
        <div class="card-body">
          <dl class="facts" style="grid-template-columns:1fr 1fr">
            <div style="grid-column:1/-1"><dt>Visitor</dt><dd><a href="#/visitors/${esc(s.visitor_id)}">${esc(name)}</a>${s.user_id ? ` <span class="tag id">${esc(s.user_id)}</span>` : ''}</dd></div>
            <div><dt>Visit</dt><dd>${ordinal(s.visit_number)}</dd></div>
            <div><dt>First seen</dt><dd>${fmtDate(s.visitor_first_seen)}</dd></div>
            <div><dt>Source</dt><dd>${esc(s.source)}</dd></div>
            ${s.utm_campaign ? `<div><dt>Campaign</dt><dd>${esc(s.utm_campaign)}${s.utm_medium ? ` (${esc(s.utm_medium)})` : ''}</dd></div>` : ''}
            <div><dt>Device</dt><dd>${esc(s.device)}</dd></div>
            <div><dt>Browser</dt><dd>${esc(s.browser)}, ${esc(s.os)}</dd></div>
            ${s.screen ? `<div><dt>Screen</dt><dd>${esc(s.screen)}</dd></div>` : ''}
            ${s.country ? `<div><dt>Country</dt><dd>${esc(countryName(s.country))}</dd></div>` : ''}
            ${s.language ? `<div><dt>Language</dt><dd>${esc(s.language)}</dd></div>` : ''}
            ${s.timezone ? `<div><dt>Time zone</dt><dd>${esc(s.timezone)}</dd></div>` : ''}
            ${s.network ? `<div style="grid-column:1/-1"><dt>Network</dt><dd>${esc(s.network)}</dd></div>` : ''}
          </dl>
        </div>
      </section>
      <section class="card">
        <div class="card-head"><h2>Bot check</h2><span class="spacer"></span>${s.bot ? '<span class="tag bot">Likely bot</span>' : '<span class="tag">Looks like a person</span>'}</div>
        <div class="card-body">
          <p class="soft" style="margin:0 0 8px">Score ${s.bot_score} (${s.bot_threshold} or more counts as a bot).${s.interacted ? ' The visitor scrolled, clicked, tapped or typed.' : ''}</p>
          ${s.bot_reasons.length ? `<ul class="reasons">${s.bot_reasons.map(r => `<li>${esc(r)}</li>`).join('')}</ul>` : '<p class="muted" style="margin:0">No bot signals.</p>'}
        </div>
      </section>
      </div>
    </div>`;
}

// ----- Visitors -----
async function visitorsView(main, r, alive) {
  const qText = r.params.get('q') || '';
  const identified = r.params.get('identified') === '1';
  main.innerHTML = `
    ${pageHead('Visitors', `
      <form id="search" role="search"><label class="sr-only" for="q">Search visitors</label><input class="input" id="q" type="search" placeholder="Search by name, email or user ID" value="${esc(qText)}" style="width:280px"></form>
      <div class="segmented" role="group" aria-label="Show">
        <button type="button" data-ident="0" aria-pressed="${!identified}">Everyone</button>
        <button type="button" data-ident="1" aria-pressed="${identified}">Signed-in users</button>
      </div>`)}
    <section class="card"><div class="loading">Loading…</div></section>`;
  $('#search').addEventListener('submit', e => { e.preventDefault(); location.hash = hrefWith({ q: $('#q').value.trim() || null }); });
  main.querySelectorAll('[data-ident]').forEach(b => b.addEventListener('click', () => { location.hash = hrefWith({ identified: b.dataset.ident === '1' ? '1' : null }); }));

  const base = `/api/visitors?site=${state.siteId}${qText ? `&q=${encodeURIComponent(qText)}` : ''}${identified ? '&identified=1' : ''}`;
  let rows = await api(base);
  if (!alive()) return;
  const card = main.querySelector('section.card');
  const draw = hasMore => {
    card.innerHTML = rows.length ? `
      <div class="table-wrap"><table class="table">
        <thead><tr><th>Visitor</th><th>Sessions</th><th>Page views</th><th>First came from</th><th>Last device</th><th>Country</th><th>Last seen</th><th>First seen</th></tr></thead>
        <tbody>${rows.map(v => `
          <tr data-href="#/visitors/${esc(v.id)}" tabindex="0">
            <td>${person(v.traits, v.user_id, v.id, v.user_id ? `<span class="tag id">${esc(v.user_id)}</span>` : v.cookieless ? '<span class="tag" title="Recognised for one day only. Visits on other days appear as different visitors.">Cookieless</span>' : '')}</td>
            <td class="num">${fmtNum(v.sessions)}</td>
            <td class="num">${fmtNum(v.pageviews ?? 0)}</td>
            <td>${esc(v.first_source ?? '')}</td>
            <td>${esc(v.last_device ?? '')}</td>
            <td>${esc(countryName(v.last_country))}</td>
            <td class="num">${ago(v.last_seen)}</td>
            <td class="num">${fmtDate(v.first_seen)}</td>
          </tr>`).join('')}</tbody>
      </table></div>
      ${hasMore ? '<div class="card-body"><button type="button" class="btn small" id="more">Load more</button></div>' : ''}`
      : `<div class="empty">${qText || identified ? 'No visitors match.' : 'No visitors yet.'}${identified && !qText ? ' Call omega.identify(userId, { name, email }) after sign-in to link visitors to your users.' : ''}</div>`;
    $('#more')?.addEventListener('click', async () => {
      const older = await api(`${base}&before=${rows.at(-1).last_seen}`);
      if (!alive()) return;
      rows = rows.concat(older);
      draw(older.length === 50);
    });
  };
  draw(rows.length === 50);
}

async function visitorDetailView(main, id, alive) {
  main.innerHTML = `${pageHead('Visitor', '', '<a class="crumb" href="#/visitors">Visitors /</a>')}<div class="loading">Loading…</div>`;
  const { visitor: v, devices, sessions, totals } = await api(`/api/visitors/${encodeURIComponent(id)}?site=${state.siteId}`);
  if (!alive()) return;
  const name = personName(v.traits, v.user_id, v.id);
  const traits = Object.entries(v.traits || {});
  const liveNow = state.live.visitors.some(x => x.visitorId === v.id);
  main.innerHTML = `
    ${pageHead(name, liveNow ? '<span class="chip"><span class="pulse" aria-hidden="true" style="width:8px;height:8px"></span>On the site now</span>' : '', '<a class="crumb" href="#/visitors">Visitors /</a>')}
    <section class="card card-body" style="margin-bottom:16px">
      <dl class="facts">
        <div><dt>User ID</dt><dd>${v.user_id ? esc(v.user_id) : '<span class="muted">Not identified</span>'}</dd></div>
        ${traits.map(([k, val]) => `<div><dt>${esc(k)}</dt><dd>${esc(typeof val === 'object' ? JSON.stringify(val) : val)}</dd></div>`).join('')}
        <div><dt>Sessions</dt><dd class="num">${fmtNum(totals.sessions)}</dd></div>
        <div><dt>Page views</dt><dd class="num">${fmtNum(totals.pageviews ?? 0)}</dd></div>
        <div><dt>Total time</dt><dd class="num">${fmtDur(totals.time)}</dd></div>
        <div><dt>First seen</dt><dd>${fmtDateTime(v.first_seen)}</dd></div>
        <div><dt>Last seen</dt><dd>${ago(v.last_seen)}</dd></div>
        ${devices.length > 1 ? `<div><dt>Browsers linked</dt><dd>${devices.length}</dd></div>` : ''}
      </dl>
    </section>
    <section class="card">
      <div class="card-head"><h2>Sessions</h2></div>
      <div class="table-wrap" style="padding-top:8px"><table class="table">
        <thead><tr><th>Came from</th><th>Pages</th><th>Duration</th><th>Device</th><th>Country</th><th>Started</th></tr></thead>
        <tbody>${sessionRows(sessions, { showVisitor: false })}</tbody>
      </table></div>
    </section>`;
}

// ----- Sites -----
const PRIVACY = {
  cookieless: {
    label: 'Cookieless',
    title: 'Cookieless (no consent banner needed)',
    help: 'Nothing is stored in visitors\' browsers. Each visitor is recognised for one day at a time, so unique counts over several days are estimates and returning visitors can\'t be told apart.',
  },
  consent: {
    label: 'Cookies after consent',
    title: 'Cookies after consent',
    help: 'Cookieless until your consent banner calls omega.consent(true). After that, a first-party cookie recognises the visitor on later visits.',
  },
  cookies: {
    label: 'Always cookies',
    title: 'Always use cookies',
    help: 'A first-party cookie from the first page view. Most accurate, but in the EU and UK this usually needs consent.',
  },
};

/** The address sites load the tracker from: the saved public address, or this page's origin until one is set. */
function trackerBase() {
  return state.settings.publicUrl || location.origin;
}

function snippet(s, base = trackerBase()) {
  const attr = { cookieless: '', consent: ' data-cookies="consent"', cookies: ' data-cookies="always"' }[s.privacy] ?? '';
  return `<script defer src="${base}/t.js" data-site="${s.key}"${attr}></script>`;
}

// Nudge to set the public address, shown where snippets appear.
function addressNotice() {
  if (state.settings.publicUrl) return '';
  return `<div class="notice">Snippets below use the address you opened the dashboard on (<b>${esc(location.origin)}</b>). If your sites reach this server at a different address, <a href="#/settings">set it in Settings</a>.</div>`;
}

async function sitesView(main, r, alive) {
  state.sites = await api('/api/sites');
  if (!alive()) return;
  const justCreated = Number(r.params.get('created'));
  const editing = Number(r.params.get('edit'));
  const adding = r.params.get('add') === '1' || state.sites.length === 0;

  const siteForm = (s) => `
    <form class="stack" data-form="${s ? s.id : 'new'}">
      <div class="field"><label for="name-${s?.id ?? 'new'}">Name</label><input class="input" id="name-${s?.id ?? 'new'}" name="name" required value="${esc(s?.name ?? '')}" placeholder="Marketing site"></div>
      <div class="field">
        <label for="domains-${s?.id ?? 'new'}">Domains</label>
        <input class="input" id="domains-${s?.id ?? 'new'}" name="domains" value="${esc(s?.domains.join(', ') ?? '')}" placeholder="example.com, app.example.com">
        <span class="hint">Only pages on these domains (and their subdomains) can send data. Separate with commas. Leave empty to accept any domain.</span>
      </div>
      <fieldset class="choices">
        <legend>Privacy</legend>
        ${Object.entries(PRIVACY).map(([key, p]) => `
          <label class="choice">
            <input type="radio" name="privacy" value="${key}" ${(s?.privacy ?? 'cookieless') === key ? 'checked' : ''}>
            <span><b>${esc(p.title)}</b><span class="hint">${esc(p.help)}</span></span>
          </label>`).join('')}
        ${s ? '<span class="hint">If you change this, update the snippet on your site too.</span>' : ''}
      </fieldset>
      <p class="error" hidden></p>
      <div class="row"><button class="btn primary" type="submit">${s ? 'Save changes' : 'Add site'}</button>${state.sites.length ? `<a class="btn" href="#/sites">Cancel</a>` : ''}</div>
    </form>`;

  main.innerHTML = `
    ${pageHead('Sites', adding ? '' : '<a class="btn primary" href="#/sites?add=1">Add site</a>')}
    ${state.sites.length ? addressNotice() : ''}
    ${adding ? `<section class="card" style="margin-bottom:16px"><div class="card-head"><h2>${state.sites.length ? 'Add a site' : 'Add your first site'}</h2></div><div class="card-body">${siteForm(null)}</div></section>` : ''}
    ${state.sites.length ? `<section class="card">${state.sites.map(s => `
      <div class="site-row" id="site-${s.id}">
        ${editing === s.id ? `<div style="grid-column:1/-1">${siteForm(s)}</div>` : `
        <div class="stack" style="gap:10px;min-width:0">
          <div><h2 style="margin:0;font-size:16px">${esc(s.name)}</h2>
            <div class="soft">${s.domains.length ? s.domains.map(esc).join(', ') : 'Any domain'}</div>
            <div style="margin-top:6px"><span class="tag" title="${esc(PRIVACY[s.privacy].help)}">${esc(PRIVACY[s.privacy].label)}</span></div></div>
          ${justCreated === s.id ? '<p style="margin:0">Paste this into the <code>&lt;head&gt;</code> of every page you want to track:</p>' : ''}
          <pre class="snippet">${esc(snippet(s))}</pre>
          <div class="row"><button type="button" class="btn small" data-copy="${s.id}">Copy snippet</button><span class="muted" data-copied="${s.id}" aria-live="polite"></span></div>
          ${s.privacy === 'consent' ? `<p style="margin:0" class="soft">Then, in your consent banner:</p>
          <pre class="snippet">omega.consent(true);   // visitor accepted analytics cookies
omega.consent(false);  // visitor declined or withdrew: cookies are deleted</pre>` : ''}
        </div>
        <div class="row" style="align-items:flex-start">
          <button type="button" class="btn small" data-view="${s.id}">View stats</button>
          <a class="btn small" href="#/sites?edit=${s.id}">Edit</a>
          <button type="button" class="btn small danger" data-delete="${s.id}">Delete</button>
        </div>`}
      </div>`).join('')}</section>
    <section class="card card-body" style="margin-top:16px">
      <h2 style="margin:0 0 8px;font-size:14.5px">Custom events and signed-in users</h2>
      <pre class="snippet">// Count a conversion or action
omega.track('Signup', { plan: 'pro' });

// Or track clicks without code
&lt;button data-omega-event="Download" data-omega-file="guide.pdf"&gt;Download&lt;/button&gt;

// After a user signs in, link this browser to them
omega.identify(user.id, { name: user.name, email: user.email });

// Stop counting your own visits (run once in your browser's console on the site)
localStorage.setItem('omega_ignore', '1');</pre>
    </section>` : ''}`;

  main.querySelectorAll('form[data-form]').forEach(form => form.addEventListener('submit', async e => {
    e.preventDefault();
    const err = form.querySelector('.error');
    const body = { name: form.name.value, domains: form.domains.value, privacy: form.privacy.value };
    try {
      const isNew = form.dataset.form === 'new';
      const s = await api(isNew ? '/api/sites' : `/api/sites/${form.dataset.form}`, { method: isNew ? 'POST' : 'PUT', body });
      state.sites = isNew ? [...state.sites, s] : state.sites.map(x => (x.id === s.id ? s : x));
      if (isNew) {
        state.siteId = s.id;
        store.set('omega.site', s.id);
        connectLive();
      }
      location.hash = isNew ? `#/sites?created=${s.id}` : '#/sites';
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    }
  }));
  main.querySelectorAll('[data-copy]').forEach(b => b.addEventListener('click', async () => {
    const s = state.sites.find(x => x.id === Number(b.dataset.copy));
    const note = main.querySelector(`[data-copied="${s.id}"]`);
    try { await navigator.clipboard.writeText(snippet(s)); note.textContent = 'Copied'; } catch { note.textContent = 'Select the snippet and copy it'; }
  }));
  main.querySelectorAll('[data-view]').forEach(b => b.addEventListener('click', () => {
    state.siteId = Number(b.dataset.view);
    store.set('omega.site', state.siteId);
    connectLive();
    location.hash = '#/overview';
  }));
  main.querySelectorAll('[data-delete]').forEach(b => b.addEventListener('click', async () => {
    const s = state.sites.find(x => x.id === Number(b.dataset.delete));
    if (!confirm(`Delete ${s.name} and all of its analytics data? This can't be undone.`)) return;
    await api(`/api/sites/${s.id}`, { method: 'DELETE' });
    state.sites = state.sites.filter(x => x.id !== s.id);
    if (state.siteId === s.id) { state.siteId = state.sites[0]?.id ?? null; store.set('omega.site', state.siteId ?? ''); connectLive(); }
    render();
  }));
}

// ----- Settings -----
async function settingsView(main, r, alive) {
  state.settings = await api('/api/settings');
  if (!alive()) return;
  const st = state.settings;
  const example = state.sites[0] ?? { key: 'site_xxxxxxxx', privacy: 'cookieless' };
  const lookupRow = (label, info, use) => `
    <div><dt>${label}</dt><dd>${info.ready ? `On, database from ${esc(fmtDate(Date.parse(`${info.built}T12:00:00Z`)))}` : '<span class="muted">Off: no database yet</span>'}</dd>
    <dd class="muted" style="font-weight:400;margin-top:2px">${use}</dd></div>`;

  main.innerHTML = `
    ${pageHead('Settings')}
    <section class="card" style="margin-bottom:16px">
      <div class="card-head"><h2>Analytics address</h2></div>
      <div class="card-body">
        <form class="stack" id="address-form">
          <div class="field">
            <label for="public-url">Public address of this server</label>
            <input class="input" id="public-url" name="publicUrl" value="${esc(st.publicUrl)}" placeholder="https://analytics.example.com" ${st.publicUrlFromEnv ? 'disabled' : ''} autocomplete="off" spellcheck="false">
            <span class="hint">${st.publicUrlFromEnv
              ? 'Set on the server with OMEGA_PUBLIC_URL. Change it there.'
              : 'The address your websites use to reach Omega. It goes into every tracking snippet. Leave empty to use the address you open the dashboard on.'}</span>
          </div>
          <div>
            <p class="soft" style="margin:0 0 6px">Your sites' snippet will look like this:</p>
            <pre class="snippet" id="snippet-preview"></pre>
          </div>
          <p class="error" hidden></p>
          ${st.publicUrlFromEnv ? '' : `<div class="row">
            <button class="btn primary" type="submit">Save address</button>
            <button class="btn" type="button" id="use-current">Use ${esc(location.origin)}</button>
            <span class="muted" id="saved" aria-live="polite"></span>
          </div>`}
        </form>
      </div>
    </section>
    <section class="card">
      <div class="card-head"><h2>IP lookups</h2></div>
      <div class="card-body">
        <dl class="facts" style="grid-template-columns:1fr 1fr">
          ${lookupRow('Country lookup', st.countryLookup, 'Which country visitors are in.')}
          ${lookupRow('Network lookup', st.networkLookup, 'Spots visits from data centres, a strong sign of bots.')}
        </dl>
        <p class="muted" style="margin:14px 0 0">${st.autoUpdate
          ? 'New monthly releases download automatically; no restart needed.'
          : 'Automatic updates are off (OMEGA_IP_DB_UPDATE=off). Run "omega geoip-update" to update.'}
          IP addresses are only used for the lookup and never stored.
          <a href="https://db-ip.com" target="_blank" rel="noopener">IP Geolocation by DB-IP</a>.</p>
      </div>
    </section>`;

  const form = $('#address-form');
  const input = $('#public-url');
  const preview = () => {
    const typed = input.value.trim().replace(/\/+$/, '');
    const base = typed ? (/^https?:\/\//i.test(typed) ? typed : `https://${typed}`) : location.origin;
    $('#snippet-preview').textContent = snippet(example, base);
  };
  preview();
  input.addEventListener('input', preview);
  $('#use-current')?.addEventListener('click', () => { input.value = location.origin; preview(); input.focus(); });
  form.addEventListener('submit', async e => {
    e.preventDefault();
    const err = form.querySelector('.error');
    err.hidden = true;
    try {
      state.settings = await api('/api/settings', { method: 'PUT', body: { publicUrl: input.value } });
      input.value = state.settings.publicUrl;
      preview();
      $('#saved').textContent = state.settings.publicUrl ? 'Saved' : 'Saved: snippets will use the dashboard address';
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    }
  });
}

// ---------- Auth ----------
function renderAuth(needsSetup) {
  liveSource?.close();
  $('#app').innerHTML = `
    <div class="auth">
      <form class="card" id="auth-form">
        ${LOGO}
        <h1>${needsSetup ? 'Create your account' : 'Sign in to Omega'}</h1>
        <p>${needsSetup ? 'This is the first account on this server. You can add sites once you are in.' : 'Your analytics dashboard.'}</p>
        <div class="stack">
          <div class="field"><label for="email">Email</label><input class="input" id="email" name="email" type="email" autocomplete="email" required></div>
          <div class="field"><label for="password">Password</label><input class="input" id="password" name="password" type="password" autocomplete="${needsSetup ? 'new-password' : 'current-password'}" minlength="${needsSetup ? 8 : 1}" required>
            ${needsSetup ? '<span class="hint">At least 8 characters.</span>' : ''}</div>
          <p class="error" hidden></p>
          <button class="btn primary" type="submit">${needsSetup ? 'Create account' : 'Sign in'}</button>
        </div>
      </form>
    </div>`;
  const form = $('#auth-form');
  form.addEventListener('submit', async e => {
    e.preventDefault();
    const err = form.querySelector('.error');
    try {
      await api(needsSetup ? '/api/setup' : '/api/login', { method: 'POST', body: { email: form.email.value, password: form.password.value } });
      await boot();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    }
  });
}

async function boot() {
  const { user, needsSetup } = await api('/api/auth');
  if (!user) return renderAuth(needsSetup);
  state.user = user;
  [state.sites, state.settings] = await Promise.all([api('/api/sites'), api('/api/settings')]);
  if (!state.sites.some(s => s.id === state.siteId)) state.siteId = state.sites[0]?.id ?? null;
  connectLive();
  render();
}

boot();
