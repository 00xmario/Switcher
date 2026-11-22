// Switcher on your phone. Served only through the Tailscale add-on to the
// owner's devices, and only to phones approved on the Mac. The page can read
// accounts, what the coding tools spent and how busy the computer is,
// refresh them, switch accounts and spend banked resets.
//
// It draws accounts the way the menu bar panel does, with the account cards
// it shares with the Linux tray's panel (account-cards.js).
import { switcherMark, playMark } from './brand.js';
import * as cards from './account-cards.js';
import { PROVIDERS, OUTCOMES, esc, resetAsk } from './account-cards.js';
import { update as updateRings } from './load-card.js';
import { tabsHTML, usageHTML, loadViewHTML, updateHTML, TABS, PERIODS } from './phone-views.js';

export { duration, esc, windowHTML } from './account-cards.js';

// The phone marks the proxy's account, the cards' default, and names the
// computer running Switcher in their hints.
export const accountHTML = (a, now) => cards.accountHTML(a, now, { place: yourMac() });
export const stateHTML = (state, now) => cards.stateHTML(state, now, { place: yourMac() });

const PHONE = '<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="6.5" y="2.5" width="11" height="19" rx="2.6"/><path d="M10.5 18.5h3"/></svg>';

export function footerHTML(device) {
  return `<footer class="foot">
    <button type="button" class="button wide" id="sign-out">Sign this phone out</button>
    <p>${device ? `This phone: ${esc(device)}` : 'This phone'} · only your Tailscale devices reach this page</p>
  </footer>`;
}

// codeHTML shows the pairing code in two groups of three, like the boxes the
// Mac asks it to be typed into.
function codeHTML(code) {
  const digits = code.split('');
  const group = part => `<span class="code-group">${part.map(d => `<b>${esc(d)}</b>`).join('')}</span>`;
  return `<div class="code" role="img" aria-label="Code ${esc(digits.join(' '))}">${group(digits.slice(0, 3))}<i class="code-dash"></i>${group(digits.slice(3))}</div>`;
}

export function pairHTML(pairing, now = Date.now() / 1000) {
  const tile = `<span class="tile tile-phone">${PHONE}</span>`;
  if (!pairing) {
    return `<section class="panel">
      ${tile}
      <h1>Use Switcher on this phone</h1>
      <p>${yourMac().replace('your', 'Your')} approves each phone once. This phone will show a code; you type it into Switcher on ${yourMac()}.</p>
      <button type="button" class="button primary wide" id="pair-start">Pair this phone</button>
    </section>`;
  }
  const code = String(pairing.code || '');
  const left = Math.max(0, Math.round(pairing.expires_at - now));
  return `<section class="panel">
    ${tile}
    <h1>Approve this phone</h1>
    <p>On ${yourMac()}, open Switcher, go to <strong>Settings → Phone</strong> and enter this code.</p>
    ${codeHTML(code)}
    <p class="small">Expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}. Or in a terminal on ${theMac()}:</p>
    <code class="command">switcher phone approve ${esc(code)}</code>
    <div class="waiting"><span class="spinner" aria-hidden="true"></span>Waiting for ${yourMac()}</div>
  </section>`;
}

export function messageHTML(title, text) {
  return `<section class="panel">
    <span class="tile">${switcherMark({ tile: true, size: 44, id: 'msg-mark' })}</span>
    <h1>${esc(title)}</h1><p>${esc(text)}</p>
    <button type="button" class="button wide" id="retry">Try again</button>
  </section>`;
}

/* ---------- the page ---------- */

const app = {
  csrf: '', state: null, pairing: null, timer: 0, mac: '', device: '', busy: false, platform: '', paired: false,
  tab: 'accounts', tabTimer: 0, usage: { days: 30, metric: 'cost', answer: null }, load: null,
  // The version of Switcher that served this page, and a newer one the
  // server runs now: an app on the Home Screen cannot be reloaded by hand.
  pageVersion: '', newVersion: '', versionTimer: 0,
};
const LOAD_MS = 3000;
// What the computer running Switcher is called: a Linux one is no Mac.
const yourMac = () => app.platform === 'linux' ? 'your computer' : 'your Mac';
const theMac = () => app.platform === 'linux' ? 'the computer' : 'the Mac';

class HttpError extends Error {
  constructor(status, body) { super(body?.error || `HTTP ${status}`); this.status = status; this.body = body; }
}

async function api(path, { method = 'GET', body } = {}) {
  const headers = {};
  if (method !== 'GET') {
    headers['X-Switcher-CSRF'] = app.csrf;
    headers['Content-Type'] = 'application/json';
  }
  const response = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined, credentials: 'same-origin', cache: 'no-store' });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new HttpError(response.status, data);
  return data;
}

const $ = id => document.getElementById(id);

function show(html) {
  $('view').innerHTML = (app.newVersion ? updateHTML(app.newVersion) : '') + html;
}

// render draws the signed-in page: the tabs and the one that is open.
function render() {
  let body;
  if (app.tab === 'usage') body = usageHTML(app.usage.answer, app.usage);
  else if (app.tab === 'load') body = loadViewHTML(app.load, app.mac || 'This computer');
  else body = app.state ? stateHTML(app.state) + footerHTML(app.device) : '<p class="empty"><span class="spinner" aria-hidden="true"></span>Loading…</p>';
  show(tabsHTML(app.tab) + `<div class="tab-body" role="tabpanel">${body}</div>`);
  $('refresh').hidden = app.tab === 'load';
}

function remember(key, value) {
  try { localStorage.setItem(`switcher-phone-${key}`, String(value)); } catch { /* private mode: not kept */ }
}
function recall(key) {
  try { return localStorage.getItem(`switcher-phone-${key}`); } catch { return null; }
}

function toast(text) {
  const el = $('toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.hidden = true; }, 3200);
}

function confirmSheet(title, text, action, { danger = false } = {}) {
  return new Promise(resolve => {
    $('sheet-title').textContent = title;
    $('sheet-text').textContent = text;
    $('sheet-confirm').textContent = action;
    $('sheet-confirm').className = `button ${danger ? 'danger' : 'primary'}`;
    $('sheet').hidden = false;
    const done = value => {
      $('sheet').hidden = true;
      $('sheet-confirm').onclick = $('sheet-cancel').onclick = $('sheet').onclick = null;
      resolve(value);
    };
    $('sheet-confirm').onclick = () => done(true);
    $('sheet-cancel').onclick = () => done(false);
    $('sheet').onclick = event => { if (event.target === $('sheet')) done(false); };
  });
}

function schedule(fn, ms) {
  clearTimeout(app.timer);
  app.timer = setTimeout(fn, ms);
}

function handleError(error) {
  if (error.status === 401) {
    app.state = null;
    app.paired = false;
    clearTimeout(app.tabTimer);
    $('refresh').hidden = true;
    show(pairHTML(null));
    return;
  }
  if (error.status === 503 && error.body?.enabled === false) {
    app.paired = false;
    clearTimeout(app.tabTimer);
    $('refresh').hidden = true;
    show(messageHTML('Phone access is off', `Turn it on in Switcher on ${yourMac()}, under Settings → Phone.`));
    return;
  }
  if (app.paired) {
    toast(error.message || 'Switcher did not answer');
    return;
  }
  show(messageHTML('Switcher did not answer', error.message || `Check that ${yourMac()} is awake and Tailscale is connected.`));
}

async function loadState() {
  try {
    app.state = await api('/api/state');
    app.paired = true;
    if (!app.busy && app.tab === 'accounts') render();
    schedule(loadState, document.visibilityState === 'visible' && app.tab === 'accounts' ? 15000 : 60000);
  } catch (error) {
    handleError(error);
  }
}

// loadUsage reads what the tools spent; while Switcher is still reading
// their logs it asks again shortly.
async function loadUsage() {
  if (app.tab !== 'usage') return;
  clearTimeout(app.tabTimer);
  const days = app.usage.days;
  let again = 60000;
  try {
    const answer = await api(`/api/tokens?days=${days}`);
    if (app.tab !== 'usage' || app.usage.days !== days) return;
    app.usage.answer = answer;
    if (!app.busy) render();
    if (answer.scanning) again = 2500;
  } catch (error) {
    if (error.status === 401 || error.status === 503) return handleError(error);
    if (app.tab !== 'usage') return;
    app.usage.answer = { error: error.message || 'Switcher did not answer' };
    if (!app.busy) render();
    again = 15000;
  }
  // Hidden, the page waits: becoming visible reads again.
  if (app.tab === 'usage' && document.visibilityState === 'visible') app.tabTimer = setTimeout(loadUsage, again);
}

// loadLoad reads the load every few seconds while the Load tab is open; the
// rings ease to each new reading instead of being drawn again.
async function loadLoad() {
  if (app.tab !== 'load') return;
  clearTimeout(app.tabTimer);
  try {
    const view = await api('/api/system/apps');
    if (app.tab !== 'load') return;
    // Rings ease in place only while the card says the same thing around
    // them: the same computer, reachable or not for the same reason.
    const shape = v => v ? JSON.stringify([v.available, v.remote, v.machine, v.reason]) : '';
    const same = shape(app.load) === shape(view);
    app.load = view;
    const root = $('view');
    if (same && view.available && updateRings(root, view)) {
      const fresh = document.createElement('div');
      fresh.innerHTML = loadViewHTML(view, app.mac || 'This computer');
      const next = fresh.querySelector('.block'), current = root.querySelector('.tab-body .block');
      if (next && current) current.replaceWith(next);
    } else if (!app.busy) {
      render();
    }
  } catch (error) {
    if (error.status === 401 || error.status === 503) return handleError(error);
    // No reading: say so rather than leave old rings looking live.
    if (app.tab === 'load' && app.load?.reason !== 'unreachable') {
      app.load = { available: false, reason: 'unreachable', machine: app.mac };
      if (!app.busy) render();
    }
  }
  if (app.tab === 'load' && document.visibilityState === 'visible') app.tabTimer = setTimeout(loadLoad, LOAD_MS);
}

function openTab(tab) {
  if (!TABS.some(([id]) => id === tab)) tab = 'accounts';
  app.tab = tab;
  remember('tab', tab);
  clearTimeout(app.tabTimer);
  if (!app.busy) render();
  if (tab === 'usage') loadUsage();
  else if (tab === 'load') loadLoad();
  else loadState();
}

// checkVersion asks which Switcher serves this phone now; a newer one than
// the one that served the page shows the update bar.
async function checkVersion() {
  clearTimeout(app.versionTimer);
  try {
    const session = await api('/api/session');
    const now = session.version || '';
    if (!app.pageVersion) app.pageVersion = now;
    const offer = now && now !== app.pageVersion ? now : '';
    if (offer !== app.newVersion) {
      app.newVersion = offer;
      if (app.paired && !app.busy) render();
      else if (!app.paired) {
        document.querySelector('.update')?.remove();
        if (offer) $('view').insertAdjacentHTML('afterbegin', updateHTML(offer));
      }
    }
  } catch { /* asked again below */ }
  app.versionTimer = setTimeout(checkVersion, 60000);
}

async function poll() {
  try {
    const answer = await api('/api/pair');
    if (answer.status === 'approved') {
      app.csrf = answer.csrf;
      app.pairing = null;
      playMark($('top-mark'));
      toast('This phone is approved');
      app.paired = true;
      openTab(app.tab);
      return;
    }
    if (answer.status === 'expired') {
      app.pairing = null;
      show(pairHTML(null));
      toast('The code expired; ask for a new one');
      return;
    }
    app.pairing = answer;
    show(pairHTML(answer));
    schedule(poll, 2000);
  } catch (error) {
    handleError(error);
  }
}

async function startPairing() {
  try {
    app.pairing = await api('/api/pair', { method: 'POST' });
    show(pairHTML(app.pairing));
    schedule(poll, 2000);
  } catch (error) {
    if (error.status === 429) toast(error.message);
    else handleError(error);
  }
}

async function boot() {
  $('top-mark').innerHTML = switcherMark({ tile: true, size: 34, id: 'top-mark-svg' });
  app.pageVersion = document.querySelector('meta[name="switcher-version"]')?.content || '';
  const tab = recall('tab');
  if (TABS.some(([id]) => id === tab)) app.tab = tab;
  const days = Number(recall('days'));
  if (PERIODS.some(([d]) => d === days)) app.usage.days = days;
  if (recall('metric') === 'tokens') app.usage.metric = 'tokens';
  try {
    const session = await api('/api/session');
    app.mac = session.mac || '';
    app.platform = session.platform || '';
    app.device = session.device || '';
    if (!app.pageVersion) app.pageVersion = session.version || '';
    clearTimeout(app.versionTimer);
    app.versionTimer = setTimeout(checkVersion, 60000);
    $('top-mac').textContent = app.mac ? `on ${app.mac}` : `on ${yourMac()}`;
    if (session.paired) {
      app.csrf = session.csrf;
      app.paired = true;
      if (app.tab !== 'accounts') loadState();
      openTab(app.tab);
    } else {
      show(pairHTML(null));
    }
  } catch (error) {
    handleError(error);
  }
}

async function act(button, run) {
  if (app.busy) return;
  app.busy = true;
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  try {
    await run();
  } catch (error) {
    if (error.status === 401 || error.status === 503) handleError(error);
    else toast(error.message || 'That did not work');
  } finally {
    app.busy = false;
    await loadState();
    if (app.paired && app.tab !== 'accounts') render();
  }
}

function account(id) {
  return app.state?.accounts?.find(a => a.id === id);
}

if (typeof document !== 'undefined') {
  document.addEventListener('click', event => {
    const target = event.target.closest('button');
    if (!target) return;
    if (target.id === 'update-page') { target.disabled = true; location.reload(); }
    else if (target.dataset.tab) openTab(target.dataset.tab);
    else if (target.dataset.days) {
      app.usage.days = Number(target.dataset.days);
      app.usage.answer = null;
      remember('days', app.usage.days);
      if (!app.busy) render();
      loadUsage();
    } else if (target.dataset.metric) {
      app.usage.metric = target.dataset.metric === 'tokens' ? 'tokens' : 'cost';
      remember('metric', app.usage.metric);
      if (!app.busy) render();
    }
    else if (target.id === 'pair-start') startPairing();
    else if (target.id === 'retry') boot();
    else if (target.id === 'sign-out') {
      confirmSheet('Sign this phone out?', `To use Switcher here again, you approve this phone on ${yourMac()} once more.`, 'Sign out', { danger: true }).then(async ok => {
        if (!ok) return;
        try {
          await api('/api/logout', { method: 'POST' });
        } catch { /* signed out either way below */ }
        clearTimeout(app.timer);
        clearTimeout(app.tabTimer);
        app.state = null;
        app.paired = false;
        $('refresh').hidden = true;
        show(pairHTML(null));
      });
    }
    else if (target.id === 'refresh' && app.tab === 'usage') {
      target.disabled = true;
      api('/api/tokens/refresh', { method: 'POST' })
        .then(() => {
          toast('Reading the sessions again');
          clearTimeout(app.tabTimer);
          app.tabTimer = setTimeout(loadUsage, 1500);
        }, error => toast(error.message || 'That did not work'))
        .finally(() => { target.disabled = false; });
    } else if (target.id === 'refresh') {
      act(target, async () => {
        await api('/api/usage/refresh', { method: 'POST' });
        toast('Refreshing usage');
      }).finally(() => { target.disabled = false; target.removeAttribute('aria-busy'); });
    } else if (target.dataset.use) {
      const a = account(target.dataset.use);
      act(target, async () => {
        const answer = await api(`/api/accounts/${encodeURIComponent(target.dataset.use)}/use`, { method: 'POST' });
        playMark($('top-mark'));
        toast(answer.antigravity_switched ? `agy and the Antigravity app on your Mac now use ${a?.email || 'that account'}`
          : answer.claude_code_switched ? `Claude and Claude Code now use ${a?.email || 'that account'}`
          : `${PROVIDERS[a?.provider] || 'Switcher'} now uses ${a?.email || 'that account'}`);
      });
    } else if (target.dataset.reset) {
      const a = account(target.dataset.reset);
      const ask = resetAsk(a, target);
      confirmSheet(ask.title, ask.message, 'Use reset')
        .then(ok => {
          if (!ok || !ask.credit) return;
          act(target, async () => {
            const answer = await api(`/api/accounts/${encodeURIComponent(a.id)}/reset`, { method: 'POST', body: { credit_id: ask.credit } });
            if (answer.outcome === 'reset') playMark($('top-mark'));
            toast(OUTCOMES[answer.outcome] || `The provider answered ${answer.outcome}`);
          });
        });
    }
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible') return;
    checkVersion();
    if (!app.paired) return;
    if (app.tab === 'accounts') loadState();
    else if (app.tab === 'usage') loadUsage();
    else loadLoad();
  });
  boot();
}
