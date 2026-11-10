// The load card: how busy the computer doing the work is, as three rings for
// the processor, memory and graphics. It is this computer, or, while it uses
// a paired host's accounts, the host. The Linux tray's panel draws it from
// GET /api/system, the same reading the Mac menu draws natively.
import { esc } from './account-cards.js';

// Load at or above these shares reads as busy, then as strained.
export const BUSY = 70;
export const STRAINED = 90;

export function level(percent) {
  return percent >= STRAINED ? 'strained' : percent >= BUSY ? 'busy' : 'calm';
}

export function gigabytes(bytes) {
  const gb = bytes / 2 ** 30;
  return gb >= 100 ? gb.toFixed(0) : gb.toFixed(1);
}

// title names the computer: this one, or the host by the name it was paired
// with.
export function title(view, place = 'This computer') {
  return view?.remote ? view.machine || 'Host' : place;
}

// detailText is the small line under a ring.
function detailText(kind, view) {
  if (kind === 'cpu') return view.cores > 0 ? `${view.cores} cores` : '';
  if (kind === 'memory') return view.memory_total > 0 ? `${gigabytes(view.memory_used)} of ${gigabytes(view.memory_total)} GB` : '';
  return '';
}

// A ring is a circle whose stroke is the share; pathLength 100 makes the
// dash the percentage itself.
function ringHTML(kind, label, value, view) {
  const known = Number.isFinite(value);
  const v = known ? Math.max(0, Math.min(100, Math.round(value))) : 0;
  const detail = known ? detailText(kind, view) : 'No reading';
  return `<div class="gauge is-${known ? level(v) : 'unknown'}" data-gauge="${kind}" role="meter" aria-label="${esc(label)}"
      aria-valuemin="0" aria-valuemax="100" ${known ? `aria-valuenow="${v}"` : ''} aria-valuetext="${known ? `${v}%` : 'no reading'}">
      <span class="ring">
        <svg viewBox="0 0 48 48" aria-hidden="true">
          <circle class="ring-track" cx="24" cy="24" r="20" pathLength="100"/>
          <circle class="ring-arc" cx="24" cy="24" r="20" pathLength="100" style="stroke-dasharray:${v} 100"/>
        </svg>
        <span class="ring-value">${known ? `${v}<small>%</small>` : '–'}</span>
      </span>
      <span class="gauge-label">${esc(label)}</span>
      <span class="gauge-detail">${esc(detail)}</span>
    </div>`;
}

// reasonText says why there is no reading.
function reasonText(view) {
  switch (view.reason) {
    case 'unreachable': return `Can't reach ${view.machine || 'the host'} right now`;
    case 'update': return `Update ${view.machine || 'the host'} to see its load`;
    default: return 'Load readings are not available here';
  }
}

export function loadHTML(view, place = 'This computer') {
  if (!view) return '';
  const name = title(view, place);
  const head = `<h2 class="provider-head load-head"><span class="logo load-logo" aria-hidden="true">${view.remote ? SERVER : CHIP}</span>${esc(name)}
      ${view.remote ? '<span class="load-sub">host</span>' : ''}<span class="load-live${view.available ? '' : ' is-off'}" aria-hidden="true"></span></h2>`;
  const body = view.available
    ? `<div class="gauges">${ringHTML('cpu', 'CPU', view.cpu, view)}${ringHTML('memory', 'Memory', view.memory, view)}${ringHTML('gpu', 'GPU', view.gpu ?? NaN, view)}</div>`
    : `<p class="load-off">${esc(reasonText(view))}</p>`;
  return `<section class="provider load" aria-label="Load on ${esc(name)}">${head}<div class="card load-card">${body}</div></section>`;
}

const CHIP = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="6" y="6" width="12" height="12" rx="2.5"/><rect x="9.5" y="9.5" width="5" height="5" rx="1"/><path d="M9 2.5V6M15 2.5V6M9 18v3.5M15 18v3.5M2.5 9H6M2.5 15H6M18 9h3.5M18 15h3.5"/></svg>';
const SERVER = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="4" width="17" height="7" rx="2"/><rect x="3.5" y="13" width="17" height="7" rx="2"/><path d="M7.5 7.5h.01M7.5 16.5h.01M11 7.5h5M11 16.5h5"/></svg>';

// update moves the rings of a card already on screen to a new reading, so
// they ease from one value to the next instead of redrawing. It answers
// false when the card has to be drawn anew (it changed shape).
export function update(root, view) {
  // A card that says why there is no reading stays as it is.
  if (view && !view.available) return Boolean(root?.querySelector?.('.load-card .load-off'));
  const card = root?.querySelector?.('.load-card .gauges');
  if (!card) return false;
  for (const [kind, value] of [['cpu', view.cpu], ['memory', view.memory], ['gpu', view.gpu ?? NaN]]) {
    const gauge = card.querySelector(`[data-gauge="${kind}"]`);
    if (!gauge || gauge.classList.contains('is-unknown') !== !Number.isFinite(value)) return false;
    if (!Number.isFinite(value)) continue;
    const v = Math.max(0, Math.min(100, Math.round(value)));
    gauge.className = `gauge is-${level(v)}`;
    gauge.setAttribute('aria-valuenow', String(v));
    gauge.setAttribute('aria-valuetext', `${v}%`);
    gauge.querySelector('.ring-arc').style.strokeDasharray = `${v} 100`;
    gauge.querySelector('.ring-value').innerHTML = `${v}<small>%</small>`;
    gauge.querySelector('.gauge-detail').textContent = detailText(kind, view);
  }
  return true;
}
