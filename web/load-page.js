// The load page: how busy the computer doing the work is, over the last two
// minutes, and what is using it, by application. It is this computer, or,
// while this one uses a paired host's accounts, the host. It reads
// GET /api/system/apps every two seconds, only while it is open.
import { level, gigabytes } from './load-card.js';

const POLL_MS = 2000;
const HISTORY = 60; // two minutes of readings
const ROWS = 12;

const METRICS = [
  { id: 'cpu', label: 'CPU', sort: 'Processor' },
  { id: 'memory', label: 'Memory', sort: 'Memory' },
  { id: 'gpu', label: 'GPU', sort: 'Graphics' },
];

// bytes says a size in the unit that reads best.
export function bytes(n) {
  if (!(n > 0)) return '0 MB';
  if (n >= 2 ** 30) return `${gigabytes(n)} GB`;
  return `${Math.max(1, Math.round(n / 2 ** 20))} MB`;
}

// share says a percentage, with a decimal below ten.
export function share(v) {
  if (!Number.isFinite(v)) return '–';
  return v < 10 ? `${(Math.round(v * 10) / 10).toFixed(1)}%` : `${Math.round(v)}%`;
}

// value is one metric of the reading, or NaN without one.
function value(view, id) {
  if (!view?.available) return NaN;
  if (id === 'gpu') return Number.isFinite(view.gpu) ? view.gpu : NaN;
  return view[id];
}

function detail(view, id) {
  if (!view?.available) return '';
  if (id === 'cpu') return view.cores > 0 ? `${view.cores} cores` : '';
  if (id === 'memory') return view.memory_total > 0 ? `${gigabytes(view.memory_used)} of ${gigabytes(view.memory_total)} GB` : '';
  if (!Number.isFinite(view.gpu)) return 'No reading';
  const vram = (view.apps || []).reduce((sum, a) => sum + (a.gpu_memory || 0), 0);
  return vram > 0 ? `${bytes(vram)} graphics memory in use` : '';
}

// sparkPath draws the history as a line and the area under it, newest on
// the right, in a 120 by 40 box. A missed reading breaks the line.
export function sparkPath(points) {
  const x = i => (120 * (i + HISTORY - points.length)) / (HISTORY - 1);
  const y = p => 38 - (Math.max(0, Math.min(100, p)) / 100) * 34;
  const runs = [];
  let run = [];
  points.forEach((p, i) => {
    if (Number.isFinite(p)) run.push([i, p]);
    else if (run.length) { runs.push(run); run = []; }
  });
  if (run.length) runs.push(run);
  let line = '', area = '';
  for (const r of runs.filter(r => r.length > 1)) {
    const path = r.map(([i, p], n) => `${n ? 'L' : 'M'}${x(i).toFixed(2)} ${y(p).toFixed(2)}`).join('');
    line += path;
    area += `${path}L${x(r[r.length - 1][0]).toFixed(2)} 40L${x(r[0][0]).toFixed(2)} 40Z`;
  }
  return { line, area };
}

function metricHTML(m) {
  return `<article class="load-metric" data-metric="${m.id}">
      <div class="load-metric-head">
        <span class="load-metric-label">${m.label}</span>
        <span class="load-ring" aria-hidden="true"><svg viewBox="0 0 48 48"><circle class="ring-track" cx="24" cy="24" r="19" pathLength="100"/><circle class="ring-arc" cx="24" cy="24" r="19" pathLength="100" style="stroke-dasharray:0 100"/></svg></span>
      </div>
      <p class="load-metric-value" role="meter" aria-label="${m.label}" aria-valuemin="0" aria-valuemax="100">–</p>
      <p class="load-metric-detail"></p>
      <svg class="load-spark" viewBox="0 0 120 40" preserveAspectRatio="none" aria-hidden="true">
        <defs><linearGradient id="spark-${m.id}" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="currentColor" stop-opacity=".22"/><stop offset="1" stop-color="currentColor" stop-opacity="0"/></linearGradient></defs>
        <path class="load-spark-base" d="M0 39.5H120" vector-effect="non-scaling-stroke"/>
        <path class="load-spark-area" fill="url(#spark-${m.id})" d=""/>
        <path class="load-spark-line" d="" vector-effect="non-scaling-stroke"/>
      </svg>
    </article>`;
}

function shellHTML() {
  return `<div class="usage-head load-head-row">
      <p class="usage-crumb">Load <span class="crumb-sep">/</span> <span data-load-machine>This computer</span> <span class="load-host-chip" data-load-host hidden>host</span></p>
      <p class="load-live-note"><span class="load-live-dot" aria-hidden="true"></span><span data-load-status>Connecting…</span></p>
    </div>
    <p class="load-off-note" data-load-off hidden></p>
    <div class="load-metrics">${METRICS.map(metricHTML).join('')}</div>
    <div class="usage-breakdown load-apps">
      <div class="usage-breakdown-head">
        <h2>What's using it</h2>
        <div class="usage-mode" role="radiogroup" aria-label="Sort by">
          ${METRICS.map((m, i) => `<button type="button" role="radio" data-load-sort="${m.id}" class="${i ? '' : 'active'}" aria-checked="${i ? 'false' : 'true'}">${m.sort}</button>`).join('')}
        </div>
      </div>
      <p class="load-apps-note" data-load-apps-note></p>
      <ol class="load-app-list" data-load-list></ol>
      <button type="button" class="load-more" data-load-more hidden>Show more</button>
    </div>
    <p class="load-foot">Grouped by app, named after the program: no arguments, files or users. Live while this page is open.</p>`;
}

// rank orders the apps by the chosen metric, and says each one's share of
// the machine and the value to show.
export function rank(view, sort) {
  const apps = [...(view?.apps || [])];
  const total = view?.memory_total || 0;
  const pick = {
    cpu: a => ({ n: a.cpu || 0, bar: a.cpu || 0, text: share(a.cpu || 0) }),
    memory: a => ({ n: a.memory || 0, bar: total > 0 ? (100 * (a.memory || 0)) / total : 0, text: bytes(a.memory) }),
    gpu: a => ({ n: (a.gpu || 0) * 1e15 + (a.gpu_memory || 0), bar: a.gpu || 0, text: share(a.gpu || 0) }),
  }[sort];
  return apps.map(a => ({ app: a, ...pick(a) })).sort((x, y) => y.n - x.n || x.app.name.localeCompare(y.app.name));
}

function metaText(app, sort) {
  const parts = [`${app.processes} process${app.processes === 1 ? '' : 'es'}`];
  if (sort !== 'cpu') parts.push(`${share(app.cpu || 0)} CPU`);
  if (sort !== 'memory') parts.push(bytes(app.memory));
  if (sort === 'gpu' && app.gpu_memory > 0) parts.push(`${bytes(app.gpu_memory)} graphics memory`);
  return parts.join(' · ');
}

function monogram(name) {
  const letter = [...String(name).replace(/^[^\p{L}\p{N}]+/u, '')][0] || '?';
  return letter.toUpperCase();
}

export function createLoadPage({ api, root, onVisible = () => {} }) {
  let active = false, timer = null, inFlight = false, view = null, machine = null, lastAt = 0;
  let sort = 'cpu', expanded = false, failures = 0;
  const history = { cpu: [], memory: [], gpu: [] };
  root.innerHTML = shellHTML();
  const $ = sel => root.querySelector(sel);

  function bindControls() {
    sort = 'cpu';
    expanded = false;
    root.querySelectorAll('[data-load-sort]').forEach(button => button.addEventListener('click', () => {
      if (button.disabled) return;
      sort = button.dataset.loadSort;
      root.querySelectorAll('[data-load-sort]').forEach(b => {
        b.classList.toggle('active', b === button);
        b.setAttribute('aria-checked', String(b === button));
      });
      renderApps(true);
    }));
    $('[data-load-more]').addEventListener('click', () => { expanded = !expanded; renderApps(true); });
  }
  bindControls();

  function renderMetrics() {
    for (const m of METRICS) {
      const card = root.querySelector(`[data-metric="${m.id}"]`);
      const v = value(view, m.id);
      const known = Number.isFinite(v);
      card.className = `load-metric is-${known ? level(v) : 'unknown'}`;
      const out = card.querySelector('.load-metric-value');
      out.innerHTML = known ? `${Math.round(v)}<small>%</small>` : '–';
      if (known) out.setAttribute('aria-valuenow', String(Math.round(v)));
      else out.removeAttribute('aria-valuenow');
      card.querySelector('.load-metric-detail').textContent = known ? detail(view, m.id) : (view?.available ? 'No reading' : '');
      card.querySelector('.ring-arc').style.strokeDasharray = `${known ? Math.max(0, Math.min(100, v)) : 0} 100`;
      const { line, area } = sparkPath(history[m.id]);
      card.querySelector('.load-spark-line').setAttribute('d', line);
      card.querySelector('.load-spark-area').setAttribute('d', area);
    }
  }

  function renderApps(fromClick = false) {
    const list = $('[data-load-list]');
    const note = $('[data-load-apps-note]');
    const gpuButton = root.querySelector('[data-load-sort="gpu"]');
    const gpuApps = !!view?.gpu_apps;
    gpuButton.disabled = !gpuApps;
    gpuButton.title = gpuApps ? '' : `${view?.remote ? view.machine || 'The host' : 'This computer'} does not report graphics use per app`;
    if (sort === 'gpu' && !gpuApps) {
      sort = 'cpu';
      root.querySelectorAll('[data-load-sort]').forEach(b => { b.classList.toggle('active', b.dataset.loadSort === 'cpu'); b.setAttribute('aria-checked', String(b.dataset.loadSort === 'cpu')); });
    }
    const ranked = view?.available ? rank(view, sort) : [];
    note.textContent = !view?.available ? ''
      : view.apps_unavailable ? 'Could not list the apps this time; trying again.'
      : ranked.length === 0 ? 'No apps to show yet.'
      : `${view.processes} processes running${view.remote ? ` on ${view.machine}` : ''}`;
    const shown = expanded ? ranked : ranked.slice(0, ROWS);
    const more = $('[data-load-more]');
    more.hidden = ranked.length <= ROWS;
    more.textContent = expanded ? 'Show fewer' : `Show ${ranked.length - ROWS} more`;
    // Rows are kept by app and moved, so their bars ease to new values.
    const existing = new Map([...list.children].map(li => [li.dataset.app, li]));
    const keep = new Set();
    const top = Math.max(1, ...shown.map(r => r.bar));
    shown.forEach((row, index) => {
      const name = row.app.name;
      keep.add(name);
      let li = existing.get(name);
      if (!li) {
        li = document.createElement('li');
        li.className = 'load-app';
        li.dataset.app = name;
        li.innerHTML = `<span class="load-app-mark" aria-hidden="true"></span>
          <span class="load-app-main"><span class="load-app-name"></span><span class="load-app-meta"></span></span>
          <span class="load-app-bar" aria-hidden="true"><i></i></span>
          <span class="load-app-value"></span>`;
        li.querySelector('.load-app-mark').textContent = monogram(name);
        li.querySelector('.load-app-name').textContent = name;
        li.querySelector('.load-app-bar i').style.width = '0%';
      }
      li.querySelector('.load-app-meta').textContent = metaText(row.app, sort);
      li.querySelector('.load-app-value').textContent = row.text;
      // Bars are drawn against the leader, so small shares stay readable;
      // the value says the share itself.
      const width = Math.max(row.bar > 0 ? 1.5 : 0, (100 * row.bar) / top);
      li.style.setProperty('--app-tone', `var(--load-${level(row.bar)})`);
      li.setAttribute('aria-label', `${name}: ${row.text}, ${metaText(row.app, sort)}`);
      if (list.children[index] !== li) list.insertBefore(li, list.children[index] || null);
      requestAnimationFrame(() => { li.querySelector('.load-app-bar i').style.width = `${width}%`; });
    });
    for (const [name, li] of existing) if (!keep.has(name)) li.remove();
    if (fromClick) list.scrollTop = 0;
  }

  function renderHead() {
    const name = view?.remote ? (view.machine || 'Host') : (view?.machine || 'This computer');
    $('[data-load-machine]').textContent = name;
    $('[data-load-host]').hidden = !view?.remote;
    const status = $('[data-load-status]');
    const off = $('[data-load-off]');
    root.classList.toggle('is-offline', !view?.available);
    if (!view) { status.textContent = failures ? 'Not reachable' : 'Connecting…'; off.hidden = true; return; }
    if (view.available) {
      status.textContent = view.remote ? `Live from ${name}` : 'Live';
      off.hidden = true;
    } else {
      status.textContent = 'No reading';
      off.hidden = false;
      off.textContent = view.reason === 'unreachable' ? `Can't reach ${name} right now. Readings return when it answers.`
        : view.reason === 'update' ? `Update ${name} to Switcher 1.28.0 or later to see what is using it.`
        : 'Load readings are not available on this computer.';
    }
  }

  async function poll() {
    if (!active || inFlight) return;
    inFlight = true;
    // The history is two minutes of time, one slot per two-second step:
    // steps with no reading (a failure, a hidden tab, a slow answer) are
    // gaps, and after a gap longer than the history it starts again.
    const push = next => {
      const now = Date.now();
      const missed = lastAt ? Math.round((now - lastAt) / POLL_MS) - 1 : 0;
      lastAt = now;
      for (const m of METRICS) {
        if (missed >= HISTORY) history[m.id] = [];
        else for (let i = 0; i < missed; i++) history[m.id].push(NaN);
        history[m.id].push(value(next, m.id));
        history[m.id].splice(0, Math.max(0, history[m.id].length - HISTORY));
      }
    };
    try {
      const next = await api('/api/system/apps');
      if (!active) return;
      failures = 0;
      // Another computer's history is not this one's.
      const id = `${next.remote ? 'host' : 'here'}:${next.machine}`;
      if (id !== machine) {
        machine = id;
        for (const k of Object.keys(history)) history[k] = [];
      }
      view = next;
      push(view);
    } catch {
      if (!active) return;
      failures++;
      push(null);
      if (failures > 2) view = null;
    } finally {
      inFlight = false;
    }
    if (!active || document.hidden) return;
    renderHead();
    renderMetrics();
    renderApps();
    clearTimeout(timer);
    timer = setTimeout(poll, failures ? Math.min(POLL_MS * 2 ** failures, 15000) : POLL_MS);
  }

  function setActive(on) {
    if (on === active) return;
    active = on;
    clearTimeout(timer);
    if (on) {
      onVisible();
      poll();
    }
  }

  // A hidden tab stops reading; showing it again picks up at once.
  document.addEventListener('visibilitychange', () => {
    if (!active) return;
    clearTimeout(timer);
    if (!document.hidden) poll();
  });

  // reset forgets every reading, as on logout.
  function reset() {
    setActive(false);
    view = null;
    machine = null;
    lastAt = 0;
    failures = 0;
    for (const k of Object.keys(history)) history[k] = [];
    root.innerHTML = shellHTML();
    bindControls();
  }

  return { setActive, reset, get active() { return active; } };
}
