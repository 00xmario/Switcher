// Insights: the long view of usage, every computer's added up, for all
// time or the last days chosen. The numbers come from /api/insights; this
// module draws them. Everything shown from another computer (names, model
// names) is escaped: it is text, never markup.

export const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

export const RANGES = [['7', '7 days'], ['30', '30 days'], ['90', '90 days'], ['365', 'Year'], ['all', 'All time']];
export const TOOL_NAMES = { codex: 'Codex', claude: 'Claude Code', grok: 'Grok Build', opencode: 'OpenCode' };
const WEEKDAYS_SHORT = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// tokens: three significant digits inside the unit, like the Usage page:
// 12.9B, 580M, 62.9K.
export function tokens(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(Math.round(n));
  const units = [[1e12, 'T'], [1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
  for (const [size, unit] of units) {
    if (n >= size * 0.9995) {
      const v = n / size;
      const s = v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
      return (s.includes('.') ? s.replace(/0+$/, '').replace(/\.$/, '') : s) + unit;
    }
  }
  return String(Math.round(n));
}

export function money(n) {
  n = Number(n) || 0;
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

// moneyShort: "$520K", "$1.24M"; under a thousand, to the cent.
export function moneyShort(n) {
  n = Number(n) || 0;
  return n < 1000 ? money(n) : '$' + tokens(n);
}

export function metricValue(x, metric) { return metric === 'cost' ? (x?.cost_usd || 0) : (x?.tokens || 0); }
export function metricText(v, metric) { return metric === 'cost' ? money(v) : tokens(v); }

// duration: minutes as "8h 45m", "45m", "2d 3h".
export function duration(minutes) {
  minutes = Math.max(0, Math.round(Number(minutes) || 0));
  if (minutes < 60) return `${minutes}m`;
  const h = Math.floor(minutes / 60), m = minutes % 60;
  if (h < 48) return m ? `${h}h ${m}m` : `${h}h`;
  const d = Math.floor(h / 24), rh = h % 24;
  return rh ? `${d}d ${rh}h` : `${d}d`;
}

function parseDay(day) {
  const [y, m, d] = String(day).split('-').map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

// day: "Oct 9", with the year when it is not this one.
export function day(dayStr, now = new Date()) {
  if (!dayStr) return '';
  const d = parseDay(dayStr);
  const s = `${MONTHS[d.getMonth()]} ${d.getDate()}`;
  return d.getFullYear() === now.getFullYear() ? s : `${s}, ${d.getFullYear()}`;
}

export function monthLabel(month) {
  const [y, m] = String(month).split('-').map(Number);
  return `${MONTHS[(m || 1) - 1]} ’${String(y).slice(2)}`;
}

export function hourLabel(h) { return `${String(h).padStart(2, '0')}:00`; }

export function plural(n, one, many = one + 's') { n = Number(n) || 0; return `${n} ${n === 1 ? one : many}`; }

// num is a number from an answer, as a number whatever was sent: values
// the host sends a paired computer are drawn as numbers, never markup.
export const num = v => Number(v) || 0;

// change: how much a number moved against the one before, as "+34%".
export function change(now, before) {
  if (!before) return now ? 'new' : '–';
  const pct = Math.round(((now - before) / before) * 100);
  return `${pct > 0 ? '+' : ''}${pct}%`;
}

// levels: five shades for the heatmap, from the quartiles of the days with
// usage, so a few huge days do not wash out the rest.
export function levels(values) {
  const used = values.filter(v => v > 0).sort((a, b) => a - b);
  if (!used.length) return () => 0;
  const q = p => used[Math.min(used.length - 1, Math.floor(p * used.length))];
  const cuts = [q(0.25), q(0.5), q(0.75)];
  return v => v <= 0 ? 0 : v <= cuts[0] ? 1 : v <= cuts[1] ? 2 : v <= cuts[2] ? 3 : 4;
}

// weeks lays the days out as columns of a week each, Monday on top; the
// first column starts with blanks before the range's first day.
export function weeks(days) {
  const out = [];
  let col = [];
  days.forEach((d, i) => {
    const wd = (parseDay(d.day).getDay() + 6) % 7;
    if (i === 0) for (let k = 0; k < wd; k++) col.push(null);
    col.push(d);
    if (wd === 6) { out.push(col); col = []; }
  });
  if (col.length) out.push(col);
  return out;
}

// tip is what a cell or bar says on hover: a title and lines, as data
// attributes; the page sets them as text.
function tip(title, ...lines) {
  return `data-tip="${esc(title)}" data-tip-lines="${esc(lines.filter(Boolean).join('\n'))}"`;
}

// ---- sections ------------------------------------------------------------

export function statsHTML(ins, metric) {
  const t = ins.totals || {};
  const all = ins.range === 'all';
  const peak = metric === 'cost' ? ins.peak_cost_day : ins.peak_day;
  const cells = [
    [metric === 'cost' ? money(t.cost_usd) : tokens(t.tokens), all ? (metric === 'cost' ? 'Lifetime cost' : 'Lifetime tokens') : (metric === 'cost' ? 'Cost' : 'Tokens'),
      metric === 'cost' ? `${tokens(t.tokens)} tokens` : `${moneyShort(t.cost_usd)} at API prices`],
    [peak ? metricText(metricValue(peak, metric), metric) : '–', 'Peak day', peak ? day(peak.day) : ''],
    [String(t.active_days || 0), 'Active days', t.days ? `of ${plural(t.days, 'day')}` : ''],
    [plural(ins.streaks?.current || 0, 'day'), 'Current streak', ''],
    [plural(ins.streaks?.longest || 0, 'day'), 'Longest streak', ins.streaks?.longest ? `${day(ins.streaks.longest_from)} – ${day(ins.streaks.longest_to)}` : ''],
    [ins.longest_session ? duration(ins.longest_session.minutes) : '–', 'Longest session', ins.longest_session ? `${TOOL_NAMES[ins.longest_session.provider] || ins.longest_session.provider} · ${day(ins.longest_session.day)}` : ''],
  ];
  return `<div class="ins-stats">${cells.map(([v, label, sub]) => `
    <div class="ins-stat"><p class="ins-stat-value">${esc(v)}</p><p class="ins-stat-label">${esc(label)}</p>${sub ? `<p class="ins-stat-sub">${esc(sub)}</p>` : ''}</div>`).join('')}
  </div>`;
}

export function heatmapHTML(ins, metric) {
  const days = ins.days || [];
  const level = levels(days.map(d => metricValue(d, metric)));
  const cols = weeks(days);
  let lastMonth = -1;
  const monthsRow = cols.map(col => {
    const first = col.find(Boolean);
    const m = first ? parseDay(first.day).getMonth() : -1;
    const label = m !== lastMonth && first && parseDay(first.day).getDate() <= 7 ? MONTHS[m] : '';
    if (label) lastMonth = m;
    return `<span>${label}</span>`;
  }).join('');
  const grid = cols.map(col => `<div class="ins-heat-col">${Array.from({ length: 7 }, (_, r) => {
    const d = col[r];
    if (!d) return '<span class="ins-heat-cell is-blank"></span>';
    const v = metricValue(d, metric);
    const lines = [metric === 'cost' ? `${tokens(d.tokens)} tokens` : `${money(d.cost_usd)} at API prices`];
    if (d.sessions) lines.push(plural(d.sessions, 'session'));
    for (const [p, c] of Object.entries(d.providers || {}).sort((a, b) => metricValue(b[1], metric) - metricValue(a[1], metric))) {
      lines.push(`${TOOL_NAMES[p] || p}: ${metricText(metricValue(c, metric), metric)}`);
    }
    return `<span class="ins-heat-cell l${level(v)}" ${tip(`${v ? metricText(v, metric) : 'No usage'} on ${day(d.day)}`, ...(v ? lines : []))}></span>`;
  }).join('')}</div>`).join('');
  // Short ranges get bigger cells: a year fits at 13px a week.
  const cell = Math.max(13, Math.min(26, Math.floor(780 / Math.max(1, cols.length))));
  return `<div class="ins-heat" style="--weeks:${cols.length};--cell:${cell}px">
      <div class="ins-heat-days">${WEEKDAYS_SHORT.map((w, i) => `<span>${i % 2 === 0 ? w : ''}</span>`).join('')}</div>
      <div class="ins-heat-scroll"><div class="ins-heat-months">${monthsRow}</div><div class="ins-heat-grid">${grid}</div></div>
    </div>
    <div class="ins-heat-legend"><span>Less</span>${[0, 1, 2, 3, 4].map(l => `<span class="ins-heat-cell l${l}"></span>`).join('')}<span>More</span></div>`;
}

// bars draws a bar chart: one bar per item, the highlighted ones in the
// accent colour, labels under some of them.
export function barsHTML(items, { metric, label, highlight = new Set(), height = 140, tipFor, labelEvery = 1 }) {
  const top = Math.max(0, ...items.map(i => i.value));
  return `<div class="ins-bars" style="--count:${items.length};--h:${height}px">${items.map((it, idx) => {
    const pct = top > 0 ? Math.max(it.value > 0 ? 2 : 0, (it.value / top) * 100) : 0;
    return `<div class="ins-bar${highlight.has(idx) ? ' is-hi' : ''}" ${tipFor ? tipFor(it, idx) : tip(label(it, idx), metricText(it.value, metric))}>
      <span class="ins-bar-plot"><span class="ins-bar-fill" style="height:${pct.toFixed(1)}%"></span></span>
      <span class="ins-bar-label">${idx % labelEvery === 0 ? esc(label(it, idx)) : ''}</span>
    </div>`;
  }).join('')}</div>`;
}

export function weeklyHTML(ins, metric) {
  const out = [];
  for (const col of weeks(ins.days || [])) {
    const real = col.filter(Boolean);
    out.push({ from: real[0].day, to: real[real.length - 1].day, value: real.reduce((a, d) => a + metricValue(d, metric), 0) });
  }
  const every = Math.max(1, Math.ceil(out.length / 12));
  return barsHTML(out, { metric, height: 150, labelEvery: every, label: w => day(w.from),
    tipFor: w => tip(`Week of ${day(w.from)}`, metricText(w.value, metric)) });
}

export function cumulativeHTML(ins, metric) {
  const days = ins.days || [];
  if (days.length < 2) return '<p class="ins-empty">Not enough days yet.</p>';
  let run = 0;
  const pts = days.map(d => (run += metricValue(d, metric)));
  const W = 900, H = 150, top = Math.max(1, run);
  const xy = pts.map((v, i) => [(i / (pts.length - 1)) * W, H - (v / top) * (H - 6) - 2]);
  const line = xy.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join('');
  const area = `${line}L${W},${H}L0,${H}Z`;
  return `<div class="ins-cumulative">
    <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true"><path class="ins-cum-area" d="${area}"/><path class="ins-cum-line" d="${line}" vector-effect="non-scaling-stroke"/></svg>
    <div class="ins-axis"><span>${esc(day(days[0].day))}</span><span>${esc(metricText(run, metric))} in all</span><span>${esc(day(days[days.length - 1].day))}</span></div>
  </div>`;
}

export function weekdayHTML(ins, metric) {
  const items = (ins.weekdays || []).map(w => ({ ...w, value: metric === 'cost' ? w.avg_cost_usd : w.avg_tokens }));
  if (!items.length) return '';
  const order = items.map((w, i) => i).sort((a, b) => items[b].value - items[a].value);
  const busiest = items[order[0]], quietest = items[order[order.length - 1]];
  const idle = items.filter(w => !w.value).length;
  let note = 'No usage yet.';
  if (busiest.value > 0) {
    const times = quietest.value > 0 ? busiest.value / quietest.value : 0;
    note = `Busiest on <strong>${esc(busiest.weekday)}s</strong>, ${esc(metricText(busiest.value, metric))} on an average one` +
      (times >= 1.2 ? `, ${times >= 10 ? Math.round(times) : times.toFixed(1)}× ${esc(quietest.weekday)}s`
        : quietest.value === 0 ? (idle > 1 ? `; ${idle} days of the week without any` : `; ${esc(quietest.weekday)}s are off`) : '') + '.';
  }
  return `<p class="ins-note">${note}</p>` + barsHTML(items, {
    metric, height: 150, highlight: new Set(busiest.value > 0 ? [order[0]] : []), label: w => w.weekday.slice(0, 3),
    tipFor: w => tip(`${w.weekday}s`, `${metricText(w.value, metric)} on average`, `${metricText(metric === 'cost' ? w.cost_usd : w.tokens, metric)} in all`, `active on ${w.active_days} of ${w.days}`),
  });
}

export function hoursHTML(ins, metric) {
  const items = (ins.hours || []).map(h => ({ ...h, value: metricValue(h, metric) }));
  if (!items.length) return '';
  const top = items.reduce((b, h, i) => h.value > items[b].value ? i : b, 0);
  const total = items.reduce((a, h) => a + h.value, 0);
  // The busiest run of four hours, for "you work most from 14:00 to 18:00".
  let best = 0, bestAt = 0;
  for (let i = 0; i < 24; i++) {
    let s = 0;
    for (let k = 0; k < 4; k++) s += items[(i + k) % 24].value;
    if (s > best) { best = s; bestAt = i; }
  }
  const note = total > 0
    ? `Most active <strong>${hourLabel(top)}–${hourLabel((top + 1) % 24)}</strong>; ${Math.round((best / total) * 100)}% of it falls between ${hourLabel(bestAt)} and ${hourLabel((bestAt + 4) % 24)}.`
    : 'No usage yet.';
  return `<p class="ins-note">${note}</p>` + barsHTML(items, {
    metric, height: 150, highlight: new Set(total > 0 ? [top] : []), labelEvery: 3, label: h => String(h.hour).padStart(2, '0'),
    tipFor: h => tip(`${hourLabel(h.hour)}–${hourLabel((h.hour + 1) % 24)}`, `${metricText(h.value, metric)}`, total > 0 ? `${((h.value / total) * 100).toFixed(1)}% of all` : ''),
  });
}

export function punchcardHTML(ins) {
  const grid = ins.grid || [];
  const top = Math.max(0, ...grid.flat());
  const rows = grid.map((row, r) => `<div class="ins-punch-row"><span class="ins-punch-day">${WEEKDAYS_SHORT[r]}</span>${row.map((v, h) => {
    const s = top > 0 && v > 0 ? 0.25 + 0.75 * Math.sqrt(v / top) : 0;
    return `<span class="ins-punch-cell" ${tip(`${WEEKDAYS_SHORT[r]} ${hourLabel(h)}`, v ? `${tokens(v)} tokens` : 'No usage')}><i style="transform:scale(${s.toFixed(3)});opacity:${s ? (0.35 + 0.65 * s).toFixed(3) : 0}"></i></span>`;
  }).join('')}</div>`).join('');
  const hours = Array.from({ length: 24 }, (_, h) => `<span>${h % 3 === 0 ? String(h).padStart(2, '0') : ''}</span>`).join('');
  return `<div class="ins-punch">${rows}<div class="ins-punch-row ins-punch-hours"><span class="ins-punch-day"></span>${hours}</div></div>`;
}

export function monthsHTML(ins, metric) {
  const months = ins.months || [];
  if (!months.length) return '';
  const items = months.map(m => ({ ...m, value: metricValue(m, metric) }));
  const top = items.reduce((b, m, i) => m.value > items[b].value ? i : b, 0);
  return barsHTML(items, { metric, height: 150, labelEvery: Math.max(1, Math.ceil(items.length / 12)), highlight: new Set(items[top].value > 0 ? [top] : []),
    label: m => monthLabel(m.month),
    tipFor: m => tip(monthLabel(m.month), metricText(m.value, metric), plural(m.active_days, 'active day'), plural(m.sessions, 'session')) });
}

export function compareHTML(ins, metric) {
  const c = ins.compare || {};
  const tile = (label, now, before, beforeLabel) => {
    const a = metricValue(now, metric), b = metricValue(before, metric);
    const delta = change(a, b);
    const dir = !b ? '' : a > b ? 'is-up' : a < b ? 'is-down' : '';
    return `<div class="ins-compare">
      <p class="ins-compare-label">${esc(label)}</p>
      <p class="ins-compare-value">${esc(metricText(a, metric))} <span class="ins-delta ${dir}">${esc(delta)}</span></p>
      <p class="ins-compare-sub">${esc(beforeLabel)}: ${esc(metricText(b, metric))}</p>
    </div>`;
  };
  return `<div class="ins-compares">
    ${tile('This week so far', c.this_week, c.last_week, 'Same days last week')}
    ${tile('This month so far', c.this_month, c.last_month, 'Same days last month')}
  </div>`;
}

export function modelsHTML(ins, metric, logos = {}, limit = 12) {
  const models = ins.models || [];
  if (!models.length) return '<p class="ins-empty">No models yet.</p>';
  const top = Math.max(1e-9, ...models.map(m => metricValue(m, metric)));
  const shown = models.slice().sort((a, b) => metricValue(b, metric) - metricValue(a, metric) || b.tokens - a.tokens).slice(0, limit);
  const rows = shown.map((m, i) => {
    const v = metricValue(m, metric);
    const share = metric === 'cost' ? m.cost_share : m.token_share;
    return `<tr>
      <td class="ins-rank">${i + 1}</td>
      <td><div class="ins-model"><span class="ins-logo">${logos[m.provider] || ''}</span><span class="ins-model-name">${esc(m.model || 'unknown')}</span>${m.unpriced && metric === 'cost' ? '<span class="ins-tag">no price</span>' : ''}</div></td>
      <td class="ins-share"><span class="ins-share-track"><span class="ins-share-fill ${esc(m.provider)}" style="width:${((v / top) * 100).toFixed(1)}%"></span></span></td>
      <td class="ins-num">${esc(metricText(v, metric))}<small>${((share || 0) * 100).toFixed(1)}%</small></td>
      <td class="ins-num ins-dim">${esc(metric === 'cost' ? tokens(m.tokens) : money(m.cost_usd))}</td>
      <td class="ins-num ins-dim">${num(m.active_days)}</td>
      <td class="ins-dim ins-when">${esc(day(m.first_day))} – ${esc(day(m.last_day))}</td>
    </tr>`;
  }).join('');
  return `<div class="ins-table-wrap"><table class="ins-table">
    <thead><tr><th></th><th>Model</th><th></th><th class="ins-num">${metric === 'cost' ? 'Cost' : 'Tokens'}</th><th class="ins-num">${metric === 'cost' ? 'Tokens' : 'Cost'}</th><th class="ins-num">Days</th><th>Used</th></tr></thead>
    <tbody>${rows}</tbody></table></div>${models.length > limit ? `<p class="ins-foot">and ${plural(models.length - limit, 'more model')}</p>` : ''}`;
}

export function sharesHTML(items, metric, { name, meta, logo }) {
  if (!items.length) return '<p class="ins-empty">Nothing yet.</p>';
  const total = items.reduce((a, x) => a + metricValue(x, metric), 0);
  return `<div class="ins-shares">${items.map(x => {
    const v = metricValue(x, metric);
    const share = total > 0 ? v / total : 0;
    return `<div class="ins-share-row">
      <div class="ins-share-head"><span class="ins-share-name">${logo ? `<span class="ins-logo">${logo(x)}</span>` : ''}${esc(name(x))}</span><span class="ins-share-value">${esc(metricText(v, metric))}</span></div>
      <span class="ins-share-track"><span class="ins-share-fill ${esc(x.provider || '')}" style="width:${(share * 100).toFixed(1)}%"></span></span>
      <p class="ins-share-meta">${esc(meta(x, share))}</p>
    </div>`;
  }).join('')}</div>`;
}

export function recordsHTML(ins, metric) {
  const t = ins.totals || {};
  const tiles = [];
  if (ins.peak_day) tiles.push(['Biggest day', tokens(ins.peak_day.tokens) + ' tokens', day(ins.peak_day.day)]);
  if (ins.peak_cost_day) tiles.push(['Most expensive day', money(ins.peak_cost_day.cost_usd), day(ins.peak_cost_day.day)]);
  if (ins.peak_hour) {
    const d = new Date(ins.peak_hour.start_ms);
    const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
    tiles.push(['Busiest hour', tokens(ins.peak_hour.tokens) + ' tokens', `${day(key)}, ${hourLabel(d.getHours())}`]);
  }
  if (t.active_days) tiles.push(['Average active day', metricText(metric === 'cost' ? t.avg_cost_per_active_day : t.avg_tokens_per_active_day, metric), plural(t.active_days, 'active day')]);
  if (ins.sessions?.count) tiles.push(['Sessions', String(ins.sessions.count), `${duration(ins.sessions.median_minutes)} typical · ${(ins.sessions.per_active_day || 0).toFixed(1)} a day`]);
  if (t.tokens) tiles.push(['Read from cache', `${((t.cached_share || 0) * 100).toFixed(0)}%`, `of input · saved ${money(t.cache_savings_usd)}`]);
  if (ins.first_day) tiles.push(['First usage', day(ins.first_day), ins.range === 'all' ? 'as far back as the logs and archive go' : 'in this range']);
  return `<div class="ins-records">${tiles.map(([label, value, sub]) => `
    <div class="ins-record"><p class="ins-record-label">${esc(label)}</p><p class="ins-record-value">${esc(value)}</p><p class="ins-record-sub">${esc(sub)}</p></div>`).join('')}</div>`;
}

function card(title, body, { controls = '', note = '', wide = false } = {}) {
  return `<section class="ins-card${wide ? ' is-wide' : ''}">
    <div class="ins-card-head"><h2>${esc(title)}</h2>${controls}</div>
    ${note ? `<p class="ins-card-note">${esc(note)}</p>` : ''}
    ${body}
  </section>`;
}

function segmented(name, options, value) {
  return `<div class="usage-window ins-seg" role="radiogroup" aria-label="${esc(name)}">${options.map(([v, label]) =>
    `<button type="button" data-ins-${esc(name)}="${esc(v)}" class="${v === value ? 'active' : ''}" role="radio" aria-checked="${v === value}">${esc(label)}</button>`).join('')}</div>`;
}

// bodyHTML is the whole page below the controls, for one answer.
export function bodyHTML(ins, state, logos = {}) {
  const metric = state.metric;
  const activity = state.activity === 'weekly' ? weeklyHTML(ins, metric) : state.activity === 'cumulative' ? cumulativeHTML(ins, metric) : heatmapHTML(ins, metric);
  const providers = ins.providers || [];
  const devices = ins.devices || [];
  return `
    ${statsHTML(ins, metric)}
    ${card('Activity', activity, { wide: true, controls: segmented('activity', [['daily', 'Daily'], ['weekly', 'Weekly'], ['cumulative', 'Cumulative']], state.activity) })}
    <div class="ins-two">
      ${card('By day of the week', weekdayHTML(ins, metric))}
      ${card('By hour of the day', hoursHTML(ins, metric))}
    </div>
    ${card('When you work', punchcardHTML(ins), { wide: true, note: 'Tokens by day of the week and hour, in your time zone.' })}
    <div class="ins-two">
      ${card('Month by month', monthsHTML(ins, metric))}
      ${card('Compared with before', compareHTML(ins, metric))}
    </div>
    ${card('Models', modelsHTML(ins, metric, logos), { wide: true })}
    <div class="ins-two">
      ${card('Coding tools', sharesHTML(providers, metric, {
        name: p => TOOL_NAMES[p.provider] || p.provider, logo: p => logos[p.provider] || '',
        meta: (p, share) => `${(share * 100).toFixed(1)}% · ${plural(p.sessions, 'session')} · ${plural(p.active_days, 'day')} · since ${day(p.first_day)}`,
      }))}
      ${devices.length > 1 ? card('Computers', sharesHTML(devices, metric, {
        name: d => d.name + (d.this ? ' (this computer)' : d.host ? ' (host)' : ''),
        meta: (d, share) => `${(share * 100).toFixed(1)}% · ${metric === 'cost' ? tokens(d.tokens) + ' tokens' : money(d.cost_usd)}`,
      })) : card('Records', recordsHTML(ins, metric))}
    </div>
    ${devices.length > 1 ? card('Records', recordsHTML(ins, metric), { wide: true }) : ''}`;
}

// ---- limits ---------------------------------------------------------------

export const LIMIT_RANGES = [['1', '24 hours'], ['7', '7 days'], ['30', '30 days']];
export const BURNS = [['1', '1h'], ['3', '3h'], ['6', '6h'], ['24', '24h']];
const PROVIDER_COLORS = { claude: '#d97757', codex: 'var(--chart-1)', grok: 'var(--faint)' };

// pct: a share of an account's window, or of a pool of them, as "122%".
export function pct(n) { return `${Math.round(Number(n) || 0)}%`; }

// rate: percentage points an hour.
export function rate(n) {
  n = Number(n) || 0;
  if (n === 0) return '0%/h';
  return `${n >= 10 ? Math.round(n) : n.toFixed(1)}%/h`;
}

// within: seconds from now as "in 14h 2m"; past times as "5h ago".
export function within(t, now) {
  const s = t - now;
  if (Math.abs(s) < 60) return 'now';
  const m = Math.round(Math.abs(s) / 60);
  return s > 0 ? `in ${duration(m)}` : `${duration(m)} ago`;
}

// when: a time as "Tue 10:00", with the date when it is not this week.
export function when(t, now) {
  const d = new Date(t * 1000);
  const time = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
  const wd = WEEKDAYS_SHORT[(d.getDay() + 6) % 7];
  if (Math.abs(t - now) < 6 * 86400) return `${wd} ${time}`;
  return `${MONTHS[d.getMonth()]} ${d.getDate()}, ${time}`;
}

function poolTitle(pool) { return `${TOOL_NAMES[pool.provider] || pool.provider}`; }

export function poolChartHTML(pool, report) {
  const rec = pool.recorded || [], proj = pool.projected || [];
  if (!rec.length && !proj.length) return '<p class="ins-empty">No readings yet.</p>';
  const cap = Math.max(100, pool.capacity, ...rec.map(p => p.capacity));
  const t0 = report.since, t1 = Math.max(report.now + 1, ...proj.map(p => p.t));
  const W = 900, H = 200, X = t => ((t - t0) / (t1 - t0)) * W, Y = v => H - (v / cap) * (H - 8) - 2;
  const path = pts => pts.map((p, i) => `${i ? 'L' : 'M'}${X(p.t).toFixed(1)},${Y(p.remaining).toFixed(1)}`).join('');
  const recLine = path(rec);
  const recArea = rec.length ? `${recLine}L${X(rec[rec.length - 1].t).toFixed(1)},${H}L${X(rec[0].t).toFixed(1)},${H}Z` : '';
  const projLine = path(proj);
  const nowX = (X(report.now) / W) * 100;
  const color = PROVIDER_COLORS[pool.provider] || 'var(--accent)';
  const marks = (pool.resets || []).filter(m => m.t >= t0 && m.t <= t1).map(m => {
    const x = (X(m.t) / W) * 100;
    return `<span class="ins-reset${m.projected ? ' is-projected' : ''}" style="left:${x.toFixed(2)}%" ${tip(`${m.projected ? 'Reset' : 'Reset'} · ${m.name || 'an account'}`, when(m.t, report.now), m.gain ? `+${num(m.gain)}% back` : '')}>${m.gain ? `+${num(m.gain)}%` : ''}</span>`;
  }).join('');
  const empties = [];
  if (pool.first_empty) empties.push([pool.first_empty.t, `${pool.first_empty.name || 'An account'} runs out`]);
  if (pool.pool_empty) empties.push([pool.pool_empty, 'Every account out']);
  const emptyMarks = empties.filter(([t]) => t <= t1).map(([t, label]) => `<span class="ins-empty-mark" style="left:${((X(t) / W) * 100).toFixed(2)}%" ${tip(label, when(t, report.now), within(t, report.now))}></span>`).join('');
  // Hover targets: one per recorded or projected point, in the order drawn.
  const targets = [...rec.map(p => ({ ...p, projected: false })), ...proj.slice(1).map(p => ({ ...p, projected: true }))];
  const step = Math.max(1, Math.ceil(targets.length / 160));
  const hovers = targets.filter((_, i) => i % step === 0).map(p => `<span class="ins-pool-hover" style="left:${((X(p.t) / W) * 100).toFixed(2)}%" ${tip(`${when(p.t, report.now)}${p.projected ? ' · projected' : ''}`, `${pct(p.remaining)} left of ${pct(p.capacity)}`)}></span>`).join('');
  const days = (t1 - t0) / 86400;
  const ticks = [];
  const tickEvery = days > 20 ? 7 * 86400 : days > 6 ? 86400 : days > 2 ? 12 * 3600 : 3 * 3600;
  const startTick = Math.ceil(t0 / tickEvery) * tickEvery;
  for (let t = startTick; t <= t1; t += tickEvery) {
    const d = new Date(t * 1000);
    const label = tickEvery >= 86400 ? `${WEEKDAYS_SHORT[(d.getDay() + 6) % 7]} ${d.getDate()}` : `${String(d.getHours()).padStart(2, '0')}:00`;
    ticks.push(`<span style="left:${((X(t) / W) * 100).toFixed(2)}%">${label}</span>`);
  }
  return `<div class="ins-pool-chart" style="--pool:${color}">
    <div class="ins-pool-y"><span style="top:2px">${pct(cap)}</span><span style="top:50%">${pct(cap / 2)}</span><span style="bottom:0">0%</span></div>
    <div class="ins-pool-plot">
      <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true">
        <line class="ins-pool-grid" x1="0" x2="${W}" y1="${Y(cap / 2)}" y2="${Y(cap / 2)}" vector-effect="non-scaling-stroke"/>
        <line class="ins-pool-grid" x1="0" x2="${W}" y1="${Y(cap)}" y2="${Y(cap)}" vector-effect="non-scaling-stroke"/>
        ${recArea ? `<path class="ins-pool-area" d="${recArea}"/>` : ''}
        ${recLine ? `<path class="ins-pool-line" d="${recLine}" vector-effect="non-scaling-stroke"/>` : ''}
        ${projLine ? `<path class="ins-pool-proj" d="${projLine}" vector-effect="non-scaling-stroke"/>` : ''}
      </svg>
      <span class="ins-now" style="left:${nowX.toFixed(2)}%"><b>Now</b></span>
      ${marks}${emptyMarks}${hovers}
    </div>
    <div class="ins-pool-x">${ticks.join('')}</div>
    <div class="ins-pool-legend"><span class="is-rec">Recorded</span><span class="is-proj">Projected at the last ${num(report.burn_hours)}h's pace</span></div>
  </div>`;
}

export function poolCardHTML(pools, report, chosen, logos = {}) {
  const pool = pools.find(p => p.window === chosen) || pools[0];
  const windows = pools.map(p => [p.window, p.window]);
  const burn = num(pool.burn?.[`${num(report.burn_hours)}h`]);
  const facts = [
    ['Pool empty', num(pool.pool_empty) ? within(pool.pool_empty, report.now) : burn > 0 ? 'Not before the resets' : 'Not at this pace', pool.pool_empty ? when(pool.pool_empty, report.now) : ''],
    ['First empty', pool.first_empty ? within(pool.first_empty.t, report.now) : pool.empty_now ? `${pool.empty_now} out now` : '–', pool.first_empty ? pool.first_empty.name : ''],
    ['Next reset', pool.next_reset ? `+${num(pool.next_reset.gain)}% ${within(pool.next_reset.t, report.now)}` : '–', pool.next_reset ? `${pool.next_reset.name} · ${when(pool.next_reset.t, report.now)}` : ''],
    [`Burn, last ${num(report.burn_hours)}h`, rate(burn), `1h ${rate(pool.burn?.['1h'])} · 24h ${rate(pool.burn?.['24h'])}`],
    ['Limits hit', String(num(pool.limit_hits)), `in the last ${num(report.days) === 1 ? '24 hours' : `${num(report.days)} days`}`],
  ];
  return `<section class="ins-card is-wide ins-pool" data-pool="${esc(pool.provider)}">
    <div class="ins-card-head">
      <h2><span class="ins-logo">${logos[pool.provider] || ''}</span>${esc(poolTitle(pool))} <span class="ins-h2-sub">${plural(pool.accounts, 'account')}</span></h2>
      ${windows.length > 1 ? `<div class="usage-window ins-seg" role="radiogroup" aria-label="Window">${windows.map(([v, label]) =>
        `<button type="button" data-ins-pool="${esc(pool.provider)}" data-ins-window="${esc(v)}" class="${v === pool.window ? 'active' : ''}" role="radio" aria-checked="${v === pool.window}">${esc(label)}</button>`).join('')}</div>` : `<span class="ins-h2-sub">${esc(pool.window)}</span>`}
    </div>
    <div class="ins-pool-top">
      <p class="ins-pool-big">${pct(pool.remaining)}<small>of ${pct(pool.capacity)} left</small></p>
      <div class="ins-pool-facts">${facts.map(([label, value, sub]) => `<div><p class="ins-fact-label">${esc(label)}</p><p class="ins-fact-value">${esc(value)}</p>${sub ? `<p class="ins-fact-sub">${esc(sub)}</p>` : ''}</div>`).join('')}</div>
    </div>
    ${poolChartHTML(pool, report)}
  </section>`;
}

function spark(series, now, since) {
  if (!series?.length) return '';
  const W = 120, H = 28, t0 = since, t1 = now;
  const pts = series.map(([t, used]) => [((t - t0) / Math.max(1, t1 - t0)) * W, H - (used / 100) * (H - 2) - 1]);
  const line = pts.map(([x, y], i) => `${i ? 'L' : 'M'}${Math.max(0, x).toFixed(1)},${y.toFixed(1)}`).join('');
  return `<svg class="ins-spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true"><path d="${line}" vector-effect="non-scaling-stroke"/></svg>`;
}

export function accountsHTML(report, logos = {}) {
  const accounts = report.accounts || [];
  if (!accounts.length) return '<p class="ins-empty">No accounts yet.</p>';
  const rows = accounts.map(a => {
    const w = (a.windows || [])[0];
    const others = (a.windows || []).slice(1).map(x => `${x.label} ${x.known ? `${pct(100 - x.used)} left` : '–'}`).join(' · ');
    const drain = w?.hits ? `${duration(w.avg_hours_to_empty * 60)}${w.hits > 1 ? ` (fastest ${duration(w.fastest_hours_to_empty * 60)})` : ''}` : w?.cycles ? 'never ran out' : '–';
    return `<tr class="${a.removed ? 'is-removed' : ''}">
      <td><div class="ins-model"><span class="ins-logo">${logos[a.provider] || ''}</span><span class="ins-acct-name">${esc(a.name || 'Account')}</span>${a.active ? '<span class="ins-tag is-on">in use</span>' : ''}${a.removed ? '<span class="ins-tag">removed</span>' : ''}</div>${others ? `<p class="ins-acct-sub">${esc(others)}</p>` : ''}</td>
      <td class="ins-acct-window">${w ? `<span class="ins-dim">${esc(w.label)}</span> <b>${w.known ? pct(100 - w.used) : '–'}</b> left${w.resets_at ? `<p class="ins-acct-sub">resets ${esc(within(w.resets_at, report.now))}</p>` : ''}` : '–'}</td>
      <td>${w ? spark(w.series, report.now, report.since) : ''}</td>
      <td class="ins-num">${w ? esc(rate(w.burn)) : '–'}<small>${w ? `${esc(rate(w.active_burn))} in use` : ''}</small></td>
      <td class="ins-num">${w?.empty_in ? esc(duration(w.empty_in / 60)) : '–'}</td>
      <td class="ins-num">${esc(drain)}<small>${w?.cycles ? `${num(w.hits)} of ${plural(w.cycles, 'window')}` : ''}</small></td>
      <td class="ins-num">${num(a.limit_hits)}<small>${a.last_hit ? esc(within(a.last_hit, report.now)) : ''}</small></td>
    </tr>`;
  }).join('');
  return `<div class="ins-table-wrap"><table class="ins-table ins-accounts">
    <thead><tr><th>Account</th><th>Now</th><th>Used, ${num(report.days) === 1 ? '24h' : `${num(report.days)} days`}</th><th class="ins-num">Burn ${num(report.burn_hours)}h</th><th class="ins-num">Empty in</th><th class="ins-num">Reset to limit</th><th class="ins-num">Limits hit</th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

const EVENT_TEXT = {
  limit: e => [`Reached its ${e.window || ''} limit`, e.name],
  refused: e => ['Refused a request for its limit', e.name],
  reset: e => [`${e.window || 'Window'} reset`, e.name],
  early_reset: e => [`${e.window || 'Window'} reset early`, `${e.name} · a banked reset or the provider's`],
  switch: e => [`Switched ${TOOL_NAMES[e.provider] || e.provider || ''}`.trim(), `${e.name || 'an account'} → ${e.to_name || 'another account'}`],
};

export function eventsHTML(report) {
  const events = report.events || [];
  if (!events.length) return '<p class="ins-empty">Nothing yet: limits, resets and switches show here as they happen.</p>';
  return `<ol class="ins-events">${events.slice(0, 100).map(e => {
    const [title, sub] = (EVENT_TEXT[e.kind] || (x => [x.kind, x.name]))(e);
    return `<li class="ins-event is-${esc(e.kind)}"><span class="ins-event-dot"></span><div><p class="ins-event-title">${esc(title)}</p><p class="ins-event-sub">${esc(sub || '')}</p></div><time>${esc(when(e.t, report.now))}<small>${esc(within(e.t, report.now))}</small></time></li>`;
  }).join('')}</ol>`;
}

export function limitsHTML(report, state, logos = {}) {
  const byProvider = new Map();
  for (const p of report.pools || []) {
    if (!byProvider.has(p.provider)) byProvider.set(p.provider, []);
    byProvider.get(p.provider).push(p);
  }
  const recording = report.recording_since
    ? `Recording since ${when(report.recording_since, report.now)}${report.recording_since > report.since ? ': charts fill in as your accounts are used' : ''}.`
    : 'Switcher has just started recording your accounts. Charts fill in as they are used.';
  const pools = [...byProvider.entries()].map(([provider, pools]) => poolCardHTML(pools, report, state.windows?.[provider] || 'Weekly', logos)).join('');
  return `<p class="ins-note ins-recording">${esc(recording)}</p>
    ${pools || '<section class="ins-card is-wide"><p class="ins-empty">No usage readings yet. They come from each account\'s usage check, every minute.</p></section>'}
    ${card('Accounts', accountsHTML(report, logos), { wide: true, note: 'Burn is the share of the window used per hour; reset to limit is how long a window took from its start to running out.' })}
    ${card('Limits, resets and switches', eventsHTML(report), { wide: true })}`;
}

// ---- page -----------------------------------------------------------------

export function controlsHTML(state) {
  const view = segmented('view', [['usage', 'Usage'], ['limits', 'Limits']], state.view);
  const refresh = `<button class="ins-refresh" type="button" title="Refresh" aria-label="Refresh" data-ins-refresh>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20 3v4h-4"/></svg>
    </button>`;
  if (state.view === 'limits') {
    return `${view}${segmented('days', LIMIT_RANGES, state.days)}
      <div class="usage-window ins-seg ins-burn" role="radiogroup" aria-label="Burn rate over"><span class="ins-seg-label">Burn</span>${BURNS.map(([v, label]) =>
        `<button type="button" data-ins-burn="${v}" class="${v === state.burn ? 'active' : ''}" role="radio" aria-checked="${v === state.burn}">${label}</button>`).join('')}</div>${refresh}`;
  }
  return `${view}
    <div class="usage-metric ins-seg" role="radiogroup" aria-label="Metric">
      <button data-ins-metric="tokens" type="button" class="${state.metric === 'tokens' ? 'active' : ''}">Tokens</button>
      <button data-ins-metric="cost" type="button" class="${state.metric === 'cost' ? 'active' : ''}">Cost</button>
    </div>
    ${segmented('range', RANGES, state.range)}${refresh}`;
}

const SHARE_BUTTON = `<button class="ins-share" type="button" data-ins-share title="Share your activity as a picture"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3v13"/><path d="m7 8 5-5 5 5"/><path d="M5 13v6a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-6"/></svg>Share</button>`;

export function shellHTML(state) {
  return `
    <div class="usage-head">
      <p class="usage-crumb">Insights <span class="crumb-sep">/</span> <span data-ins-scope>All computers</span></p>
      <div class="usage-controls" data-ins-controls>${controlsHTML(state)}</div>
    </div>
    <div class="ins-subhead"><p class="ins-status" data-ins-status></p><span data-ins-share-slot></span></div>
    <div class="ins-body" data-ins-body></div>
    <div class="usage-tip ins-tip" data-ins-tip role="tooltip"></div>`;
}

function stored(key, fallback, allowed) {
  try {
    const v = localStorage.getItem(key);
    return allowed.includes(v) ? v : fallback;
  } catch { return fallback; }
}

function store(key, value) {
  try { localStorage.setItem(key, value); } catch { /* private window */ }
}

// createInsightsPage runs the page: it loads what the view and range show,
// keeps asking while the first scan runs, and redraws on every control.
export function createInsightsPage({ api, root, logos = {}, locked = () => false }) {
  const state = {
    view: stored('switcher.insights.view', 'usage', ['usage', 'limits']),
    metric: stored('switcher.insights.metric', 'tokens', ['tokens', 'cost']),
    range: stored('switcher.insights.range', 'all', RANGES.map(r => r[0])),
    activity: stored('switcher.insights.activity', 'daily', ['daily', 'weekly', 'cumulative']),
    days: stored('switcher.insights.days', '7', LIMIT_RANGES.map(r => r[0])),
    burn: stored('switcher.insights.burn', '3', BURNS.map(r => r[0])),
    windows: {},
  };
  let active = false, data = null, dataKey = null, generation = 0, timer = null, sharing = null;
  root.innerHTML = shellHTML(state);
  const $ = sel => root.querySelector(sel);
  const status = $('[data-ins-status]');
  const body = $('[data-ins-body]');
  const tipBox = $('[data-ins-tip]');
  const controls = $('[data-ins-controls]');
  const keyOf = () => state.view === 'limits' ? `limits:${state.days}:${state.burn}` : `usage:${state.range}`;

  function render() {
    if (!data) return;
    controls.innerHTML = controlsHTML(state);
    $('[data-ins-share-slot]').innerHTML = state.view === 'usage' && data.totals?.tokens ? SHARE_BUTTON : '';
    if (state.view === 'limits') {
      body.innerHTML = limitsHTML(data, state, logos);
      $('[data-ins-scope]').textContent = 'Accounts';
      status.textContent = '';
      return;
    }
    body.innerHTML = bodyHTML(data, state, logos);
    const scroll = body.querySelector('.ins-heat-scroll');
    if (scroll) scroll.scrollLeft = scroll.scrollWidth;
    const devices = data.devices || [];
    $('[data-ins-scope]').textContent = devices.length > 1 ? `${devices.length} computers` : 'This computer';
    const notes = [];
    if (sharing?.host && sharing.reason) notes.push(`${sharing.host}'s usage is not added up right now`);
    if (data.range === 'all' && data.first_day) notes.push(`Since ${day(data.first_day)}`);
    if (data.totals?.unpriced_records && state.metric === 'cost') notes.push('some models have no price and count as $0');
    status.textContent = notes.join(' · ');
  }

  async function load() {
    if (!active || locked()) return;
    const gen = ++generation;
    clearTimeout(timer);
    const key = keyOf();
    if (dataKey !== key) {
      controls.innerHTML = controlsHTML(state);
      $('[data-ins-share-slot]').innerHTML = '';
      body.innerHTML = '<p class="ins-empty ins-loading">Loading insights…</p>';
      status.textContent = '';
    }
    const again = ms => { timer = setTimeout(() => { if (gen === generation && active && !document.hidden) load(); }, ms); };
    try {
      if (state.view === 'limits') {
        const answer = await api(`/api/insights/limits?days=${encodeURIComponent(state.days)}&burn=${encodeURIComponent(state.burn)}`);
        if (gen !== generation || !active) return;
        data = answer;
        dataKey = key;
        render();
        again(60_000);
        return;
      }
      const answer = await api(`/api/insights?range=${encodeURIComponent(state.range)}`);
      if (gen !== generation || !active) return;
      if (answer.insights) {
        data = answer.insights;
        dataKey = key;
        sharing = answer.sharing || null;
        render();
        again(60_000);
      } else if (answer.scanning) {
        body.innerHTML = `<p class="ins-empty ins-loading">Reading every log on disk for the first time. This can take a minute with a long history.</p>`;
        again(2500);
      }
    } catch (err) {
      if (gen !== generation) return;
      if (dataKey !== key) body.innerHTML = `<p class="ins-empty">Insights unavailable: ${esc(err.message || 'no answer')}</p>`;
      again(15_000);
    }
  }

  // Each control changes the state and either redraws (same data) or loads.
  root.addEventListener('click', async event => {
    const button = event.target.closest('button');
    if (!button || !root.contains(button)) return;
    const d = button.dataset;
    if (d.insView) {
      if (state.view === d.insView) return;
      state.view = d.insView; store('switcher.insights.view', state.view);
      data = null; dataKey = null; load();
    } else if (d.insMetric) {
      state.metric = d.insMetric; store('switcher.insights.metric', state.metric); render();
    } else if (d.insRange) {
      state.range = d.insRange; store('switcher.insights.range', state.range); load();
    } else if (d.insActivity) {
      state.activity = d.insActivity; store('switcher.insights.activity', state.activity); render();
    } else if (d.insDays) {
      state.days = d.insDays; store('switcher.insights.days', state.days); load();
    } else if (d.insBurn) {
      state.burn = d.insBurn; store('switcher.insights.burn', state.burn); load();
    } else if (d.insWindow) {
      state.windows[d.insPool] = d.insWindow; render();
    } else if (button.hasAttribute('data-ins-share')) {
      // The card is of all time, whatever range the page shows.
      button.disabled = true;
      try {
        const all = state.view === 'usage' && state.range === 'all' && data ? data : (await api('/api/insights?range=all')).insights;
        if (!all) throw new Error('Insights are not ready yet');
        const { openShare } = await import('./share-card.js');
        await openShare(all, { prefersDark: matchMedia?.('(prefers-color-scheme: dark)').matches || document.documentElement.dataset.theme === 'dark' });
      } catch (err) {
        status.textContent = `Could not make the picture: ${err.message || 'no answer'}`;
      } finally { button.disabled = false; }
    } else if (button.hasAttribute('data-ins-refresh')) {
      button.classList.add('spinning');
      try {
        const kick = state.view === 'limits' ? api('/api/usage/refresh', { method: 'POST' }) : api('/api/tokens/refresh', { method: 'POST' });
        await Promise.all([kick, new Promise(r => setTimeout(r, 700))]);
        dataKey = null;
        await load();
      } catch { /* the next load says why */ } finally { controls.querySelector('[data-ins-refresh]')?.classList.remove('spinning'); }
    }
  });

  // One tooltip for every cell, bar and mark: its text is set as text.
  root.addEventListener('pointerover', event => {
    const el = event.target.closest('[data-tip]');
    if (!el || !root.contains(el)) { tipBox.style.opacity = '0'; return; }
    tipBox.replaceChildren();
    const title = document.createElement('div');
    title.className = 'tip-day';
    title.textContent = el.dataset.tip;
    tipBox.append(title);
    for (const line of (el.dataset.tipLines || '').split('\n').filter(Boolean)) {
      const row = document.createElement('div');
      row.className = 'tip-row';
      row.textContent = line;
      tipBox.append(row);
    }
    const box = root.getBoundingClientRect(), r = el.getBoundingClientRect();
    tipBox.style.opacity = '1';
    const w = tipBox.offsetWidth, h = tipBox.offsetHeight;
    let x = r.left - box.left + r.width / 2 - w / 2;
    x = Math.max(0, Math.min(box.width - w, x));
    let y = r.top - box.top - h - 8;
    if (y < 0) y = r.bottom - box.top + 8;
    tipBox.style.left = `${x}px`;
    tipBox.style.top = `${y}px`;
  });
  root.addEventListener('pointerleave', () => { tipBox.style.opacity = '0'; });

  document.addEventListener('visibilitychange', () => { if (active && !document.hidden) load(); });

  return {
    setActive(on) {
      if (on === active) return;
      active = on;
      if (on) load();
      else { clearTimeout(timer); generation++; tipBox.style.opacity = '0'; }
    },
    reset() { data = null; dataKey = null; generation++; clearTimeout(timer); body.innerHTML = ''; status.textContent = ''; },
  };
}
