// The phone's Usage and Load tabs, and its update banner: plain functions
// from what Switcher answers to HTML, so they are tested like the cards.
// Usage is what the coding tools spent, from GET /api/tokens, as the
// dashboard's Usage page shows it; Load is how busy the computer doing the
// work is and what is using it, from GET /api/system/apps.
import { PROVIDERS, esc } from './account-cards.js';
import { loadHTML } from './load-card.js';
import { rank, bytes, share } from './load-page.js';

export const TABS = [['accounts', 'Accounts'], ['usage', 'Usage'], ['load', 'Load']];
export const PERIODS = [[1, '24h'], [7, '7 days'], [30, '30 days']];
const TOOL_NAMES = { codex: 'Codex', claude: 'Claude Code', grok: 'Grok Build', opencode: 'OpenCode' };
const TOOL_COLORS = { claude: '#d97757', codex: 'var(--text)', grok: 'var(--faint)', opencode: 'var(--accent)' };
const APP_ROWS = 8;
const MODEL_ROWS = 5;

export function tabsHTML(current) {
  return `<nav class="tabs" role="tablist" aria-label="Switcher">${TABS.map(([id, label]) =>
    `<button type="button" role="tab" class="tab${id === current ? ' is-on' : ''}" data-tab="${id}" aria-selected="${id === current}">${label}</button>`).join('')}</nav>`;
}

function segmentHTML(name, choices, current, label) {
  return `<div class="segment" role="radiogroup" aria-label="${esc(label)}">${choices.map(([value, text]) =>
    `<button type="button" role="radio" data-${name}="${esc(value)}" class="${String(value) === String(current) ? 'is-on' : ''}" aria-checked="${String(value) === String(current)}">${esc(text)}</button>`).join('')}</div>`;
}

// tokens: three significant digits inside the unit, like 12.9B, 580M, 62.9K.
export function tokens(n) {
  n = Number(n) || 0;
  const units = [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
  for (const [size, unit] of units) {
    if (Math.abs(n) >= size) {
      const m = n / size;
      const text = m >= 100 ? m.toFixed(0) : m >= 10 ? m.toFixed(1) : m.toFixed(2);
      return (text.includes('.') ? text.replace(/0+$/, '').replace(/\.$/, '') : text) + unit;
    }
  }
  return String(Math.round(n));
}

export function money(n) {
  n = Number(n) || 0;
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

const pct = x => `${((Number(x) || 0) * 100).toFixed(x >= 0.1 ? 0 : 1)}%`;
const toolName = p => TOOL_NAMES[p] || PROVIDERS[p] || p;
const toolDot = p => `<span class="dot" style="background:${TOOL_COLORS[p] || 'var(--dim)'}"></span>`;

// shareRow is one line of a breakdown: a name, the value, a bar for its
// share and a small line under it.
function shareRow(name, value, part, meta) {
  const width = Math.max(0, Math.min(100, (Number(part) || 0) * 100));
  return `<div class="share-row">
      <div class="share-top"><span class="share-name">${name}</span><span class="share-value">${esc(value)}</span></div>
      <span class="bar"><i style="width:${width.toFixed(1)}%"></i></span>
      ${meta ? `<span class="share-meta">${esc(meta)}</span>` : ''}
    </div>`;
}

// chartHTML draws the days as stacked bars, one colour per tool.
export function chartHTML(days, isCost) {
  if (!Array.isArray(days) || days.length < 2) return '';
  const value = (d, p) => isCost ? d.providers?.[p]?.cost_usd || 0 : d.providers?.[p]?.tokens || 0;
  const tools = [...new Set(days.flatMap(d => Object.keys(d.providers || {})))]
    .sort((a, b) => days.reduce((s, d) => s + value(d, b), 0) - days.reduce((s, d) => s + value(d, a), 0));
  const totals = days.map(d => tools.reduce((s, p) => s + value(d, p), 0));
  const top = Math.max(...totals, 0);
  if (top <= 0) return '';
  const w = 300, h = 96, gap = days.length > 40 ? 1 : 2;
  const bw = Math.max(1, (w - gap * (days.length - 1)) / days.length);
  let bars = '';
  days.forEach((d, i) => {
    let y = h;
    for (const p of tools) {
      const v = value(d, p);
      if (v <= 0) continue;
      const bh = (v / top) * (h - 2);
      y -= bh;
      bars += `<rect x="${(i * (bw + gap)).toFixed(2)}" y="${y.toFixed(2)}" width="${bw.toFixed(2)}" height="${bh.toFixed(2)}" rx="${Math.min(2, bw / 2).toFixed(2)}" style="fill:${TOOL_COLORS[p] || 'var(--dim)'}"/>`;
    }
  });
  const first = days[0].day, last = days[days.length - 1].day;
  return `<section class="block"><h2 class="block-head">${isCost ? 'Daily cost' : 'Daily tokens'}<span>peak ${esc(isCost ? money(top) : tokens(top))}</span></h2>
      <div class="card chart-card"><svg class="chart" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img" aria-label="${esc(isCost ? 'Cost' : 'Tokens')} per day from ${esc(first)} to ${esc(last)}">${bars}</svg>
      <div class="chart-axis"><span>${esc(shortDay(first))}</span><span>${esc(shortDay(last))}</span></div></div></section>`;
}

function shortDay(day) {
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(String(day || ''));
  if (!m) return String(day || '');
  const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  return `${Number(m[3])} ${months[Number(m[2]) - 1] || ''}`;
}

// usageHTML is the Usage tab: what the tools spent over the chosen days.
export function usageHTML(answer, { days = 30, metric = 'cost' } = {}) {
  const controls = `<div class="controls">${segmentHTML('days', PERIODS, days, 'Period')}${segmentHTML('metric', [['cost', 'Cost'], ['tokens', 'Tokens']], metric, 'Show')}</div>`;
  if (!answer) return `${controls}<p class="empty"><span class="spinner" aria-hidden="true"></span>Loading usage…</p>`;
  if (answer.error) return `${controls}<p class="empty">${esc(answer.error)}</p>`;
  const s = answer.summary;
  if (!s || answer.scanning) return `${controls}<p class="empty"><span class="spinner" aria-hidden="true"></span>Reading the coding tools' sessions…</p>`;
  const isCost = metric === 'cost';
  const sub = [`${s.sessions || 0} session${s.sessions === 1 ? '' : 's'}`];
  if (s.devices?.length > 1) sub.push(`across ${s.devices.length} computers`);
  if (s.unpriced_share > 0.001) sub.push(`${pct(s.unpriced_share)} not priced`);
  const head = `<section class="card hero">
      <span class="hero-label">${isCost ? 'Cost at API prices' : 'Tokens processed'}</span>
      <strong class="hero-value">${esc(isCost ? money(s.total_cost_usd) : tokens(s.total_tokens))}</strong>
      <span class="hero-sub">${esc(sub.join(' · '))}</span>
    </section>`;

  const providers = (s.providers || []).map(p => shareRow(
    `${toolDot(p.provider)}${esc(toolName(p.provider))}`,
    isCost ? money(p.cost_usd) : tokens(p.tokens),
    isCost ? p.cost_share : p.token_share,
    `${p.sessions} session${p.sessions === 1 ? '' : 's'} · ${isCost ? `${tokens(p.tokens)} tokens` : money(p.cost_usd)}`)).join('');

  const total = isCost ? s.total_cost_usd : s.total_tokens;
  const devices = (s.devices || []).length > 1
    ? `<section class="block"><h2 class="block-head">By computer</h2><div class="card list">${[...s.devices]
      .sort((a, b) => (isCost ? b.cost_usd - a.cost_usd : b.tokens - a.tokens))
      .map(d => shareRow(`${esc(d.name || 'A computer')}${d.host ? '<span class="tag">host</span>' : ''}${d.this ? '<span class="tag">this one</span>' : ''}`,
        isCost ? money(d.cost_usd) : tokens(d.tokens), total > 0 ? (isCost ? d.cost_usd : d.tokens) / total : 0,
        isCost ? `${tokens(d.tokens)} tokens` : money(d.cost_usd))).join('')}</div></section>`
    : '';

  const models = [...(s.models || [])].sort((a, b) => (isCost ? b.cost_usd - a.cost_usd : b.tokens - a.tokens)).slice(0, MODEL_ROWS);
  const modelTotal = isCost ? s.total_cost_usd : s.total_tokens;
  const modelsBlock = models.length
    ? `<section class="block"><h2 class="block-head">Top models</h2><div class="card list">${models.map(m => shareRow(
      `${toolDot(m.provider)}<span class="mono">${esc(m.model || 'unknown')}</span>`,
      isCost ? (m.unpriced ? 'not priced' : money(m.cost_usd)) : tokens(m.tokens),
      modelTotal > 0 ? (isCost ? m.cost_usd : m.tokens) / modelTotal : 0, '')).join('')}</div></section>`
    : '';

  const t = s.totals || {};
  const processed = (t.uncached_input_tokens || 0) + (t.cached_input_tokens || 0) + (t.cache_creation_tokens || 0) + (t.output_tokens || 0);
  const facts = `<section class="block"><div class="facts">
      ${[['Processed', tokens(processed)], ['Cached input', tokens(t.cached_input_tokens)], ['Output', tokens(t.output_tokens)], ['Cache savings', money(s.cache_savings_usd)]]
        .map(([label, value]) => `<div class="fact"><span>${label}</span><strong>${esc(value)}</strong></div>`).join('')}
    </div></section>`;

  return `${controls}${head}${chartHTML(s.days, isCost)}
    ${providers ? `<section class="block"><h2 class="block-head">By tool</h2><div class="card list">${providers}</div></section>` : ''}
    ${devices}${modelsBlock}${facts}
    <p class="fine">From the coding tools' own session logs, priced at API rates.${answer.stale ? ' Updating in the background.' : ''}</p>`;
}

function monogram(name) {
  return ([...String(name || '').replace(/^[^\p{L}\p{N}]+/u, '')][0] || '?').toUpperCase();
}

// loadViewHTML is the Load tab: the three rings and the apps using the most
// processor, as the dashboard's Load page lists them.
export function loadViewHTML(view, place = 'This computer') {
  if (!view) return `<p class="empty"><span class="spinner" aria-hidden="true"></span>Reading the load…</p>`;
  const rings = loadHTML(view, place);
  if (!view.available) return rings;
  const rows = rank(view, 'cpu').slice(0, APP_ROWS);
  const top = Math.max(1, ...rows.map(r => r.bar));
  const apps = view.apps_unavailable
    ? '<p class="empty">Could not list the apps this time; trying again.</p>'
    : rows.length
      ? `<div class="card list apps">${rows.map(r => `<div class="app-row">
          <span class="app-mark" aria-hidden="true">${esc(monogram(r.app.name))}</span>
          <span class="app-main"><span class="app-name">${esc(r.app.name)}</span>
            <span class="bar"><i style="width:${Math.min(100, (100 * r.bar) / top).toFixed(1)}%"></i></span></span>
          <span class="app-nums"><strong>${esc(share(r.app.cpu || 0))}</strong><span>${esc(bytes(r.app.memory))}</span></span>
        </div>`).join('')}</div>`
      : '<p class="empty">No apps to show yet.</p>';
  return `${rings}<section class="block"><h2 class="block-head">Using the most processor<span>${view.processes ? `${esc(view.processes)} processes` : ''}</span></h2>${apps}</section>
    <p class="fine">Live while this tab is open. Grouped by app, by program name only.</p>`;
}

// updateHTML is the bar that offers the new version of this page once
// Switcher has been updated: an app added to the Home Screen has no reload.
export function updateHTML(version) {
  return `<div class="update" role="status">
      <span class="update-text"><strong>Switcher ${esc(version)} is ready</strong><span>Update this page to get the new version.</span></span>
      <button type="button" class="button primary" id="update-page">Update</button>
    </div>`;
}
