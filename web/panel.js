// Switcher's panel on Linux: the menu bar panel's look in a small window,
// opened from the tray. It draws the phone page's account cards
// (account-cards.js) from this computer's own dashboard API, behind the
// dashboard's login, and follows changes as they happen.
import { switcherMark, playMark } from './brand.js';
import * as cards from './account-cards.js';
import { PROVIDERS, OUTCOMES, esc } from './account-cards.js';

// The panel marks the account the Mac menu marks: Claude Code's login for
// Claude, the proxy's account otherwise.
export const selected = a => (a.native_switch_available ? a.native_active : a.active) === true;

// viewOf is how the cards draw here: on this computer, with the menu's
// usage bars unless they are turned off in Settings.
export const viewOf = state => ({ place: 'this computer', selected, bars: state.menu_usage_bars !== false });

// subtitle sits under the name, as the version does in the Mac menu; while
// this computer uses a paired host's accounts, it names the host.
export function subtitle(state) {
  const remote = state?.remote;
  if (remote?.role === 'client') return remote.connected ? `Using the accounts of ${remote.host}` : `Can't reach ${remote.host}`;
  return state?.version ? `v${state.version}` : '';
}

// noticeHTML says what changes how the accounts are used, as the dashboard's
// banners do.
export function noticeHTML(state) {
  const remote = state.remote;
  return [
    state.handed_back ? '<p class="notice is-warn">Your tools are handed back to their providers. Switcher uses none of its accounts until you set it up again in the dashboard.</p>' : '',
    remote?.role === 'client' && !remote.connected
      ? `<p class="notice is-warn">Can't reach <strong>${esc(remote.host)}</strong>. Showing its last known accounts; requests fail until it is back.</p>` : '',
    remote?.role === 'local' ? `<p class="notice">Paired with <strong>${esc(remote.host)}</strong>, using this computer's own accounts.</p>` : '',
  ].join('');
}

export function pageHTML(state, now = Date.now() / 1000) {
  return `${noticeHTML(state)}${cards.stateHTML(state, now, viewOf(state))}
    <footer class="foot"><a href="/" target="_blank" rel="noopener">Open dashboard</a></footer>`;
}

export function messageHTML(title, text) {
  return `<section class="panel">
    <span class="tile">${switcherMark({ tile: true, size: 44, id: 'msg-mark' })}</span>
    <h1>${esc(title)}</h1><p>${esc(text)}</p>
    <button type="button" class="button wide" id="retry">Try again</button>
  </section>`;
}

// switchedText is what a switch did: Claude's also moves Claude Code's login.
export function switchedText(account, answer) {
  const email = account?.email || 'that account';
  return answer?.native?.changed === true ? `Claude and Claude Code now use ${email}`
    : `${PROVIDERS[account?.provider] || 'Switcher'} now uses ${email}`;
}

const pause = ms => new Promise(resolve => setTimeout(resolve, ms));

// watch keeps the panel in step with the server: it waits on
// /api/state/changes and reads the state as soon as the server reports a
// change, so a switch made in the tray or on another computer shows at once.
// A server from before the changes feed answers 404; it is read every
// pollMs instead, and asked again after a number of rounds in case an update
// taught it. It runs until stop().
export function watch({ get, onState, onError = () => {}, sleep = pause, pollMs = 4000, rounds = 20 }) {
  let stopped = false;
  const running = (async () => {
    let tag = '', polls = 0;
    while (!stopped) {
      try {
        if (polls > 0) {
          await sleep(pollMs);
          polls--;
        } else {
          let answer;
          try {
            answer = await get(`/api/state/changes?since=${encodeURIComponent(tag)}`);
          } catch (error) {
            if (error.status !== 404) throw error;
            tag = '';
            polls = rounds;
            continue;
          }
          const same = answer.etag === tag;
          tag = answer.etag || '';
          if (!answer.changed) continue;
          // A changed state keeps its tag only on a server that is not
          // keeping its side; never ask it in a tight loop.
          if (same) await sleep(pollMs);
        }
        if (stopped) return;
        const state = await get('/api/state');
        if (!stopped) onState(state);
      } catch (error) {
        if (stopped) return;
        tag = '';
        onError(error);
        await sleep(pollMs);
      }
    }
  })();
  return { stop: () => { stopped = true; }, running };
}

/* ---------- the page ---------- */

const LOGIN = '/login?next=/panel';
const app = { csrf: '', state: null, busy: false };

class HttpError extends Error {
  constructor(status, body) { super(body?.error || `HTTP ${status}`); this.status = status; this.body = body; }
}

// api talks to the dashboard's API the way the dashboard does: its session
// cookie, and its CSRF token on changes.
async function api(path, { method = 'GET', body } = {}) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET') {
    headers['X-Switcher-CSRF'] = app.csrf;
    headers['Content-Type'] = 'application/json';
  }
  const response = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined, credentials: 'same-origin', cache: 'no-store' });
  const data = await response.json().catch(() => ({}));
  // A session that ended leaves for the login, which comes back here.
  if (response.status === 401 && data.auth_required) location.replace(LOGIN);
  if (!response.ok) throw new HttpError(response.status, data);
  return data;
}

function storedCSRF() {
  try { return localStorage.getItem('switcher-csrf') || ''; } catch { return ''; }
}

const $ = id => document.getElementById(id);

function render() {
  if (!app.state) return;
  $('top-sub').textContent = subtitle(app.state);
  if (!app.busy) $('view').innerHTML = pageHTML(app.state);
}

function toast(text) {
  const el = $('toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.hidden = true; }, 3200);
}

function confirmSheet(title, text, action) {
  return new Promise(resolve => {
    $('sheet-title').textContent = title;
    $('sheet-text').textContent = text;
    $('sheet-confirm').textContent = action;
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

function failed(error) {
  if (error.status === 401) return; // the login page is on its way
  if (app.state) {
    $('top-sub').textContent = 'Switcher is not answering';
    return;
  }
  $('view').innerHTML = messageHTML('Switcher did not answer', error.message || 'Check that Switcher is running on this computer.');
}

async function load() {
  app.state = await api('/api/state');
  $('refresh').hidden = false;
  render();
}

async function boot() {
  $('top-mark').innerHTML = switcherMark({ tile: true, size: 34, id: 'top-mark-svg' });
  try {
    const status = await api('/api/auth/status');
    app.csrf = status.csrf || storedCSRF();
    if (status.auth_enabled && !app.csrf) {
      location.replace(LOGIN);
      return;
    }
    await load();
  } catch (error) {
    failed(error);
    return;
  }
  watch({ get: path => api(path), onState: state => { app.state = state; render(); }, onError: failed });
  // Countdowns move on between changes.
  setInterval(render, 30000);
}

async function act(button, run) {
  if (app.busy) return;
  app.busy = true;
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  try {
    await run();
  } catch (error) {
    if (error.status !== 401) toast(error.message || 'That did not work');
  } finally {
    app.busy = false;
    button.disabled = false;
    button.removeAttribute('aria-busy');
    await load().catch(failed);
  }
}

const account = id => app.state?.accounts?.find(a => a.id === id);

if (typeof document !== 'undefined') {
  document.addEventListener('click', event => {
    const target = event.target.closest('button');
    if (!target) return;
    if (target.id === 'retry') location.reload();
    else if (target.id === 'refresh') {
      act(target, async () => {
        await api('/api/usage/refresh', { method: 'POST' });
        toast('Refreshing usage');
      });
    } else if (target.dataset.use) {
      const a = account(target.dataset.use);
      act(target, async () => {
        const answer = await api(`/api/accounts/${encodeURIComponent(target.dataset.use)}/activate`, { method: 'POST' });
        playMark($('top-mark'));
        toast(switchedText(a, answer));
      });
    } else if (target.dataset.reset) {
      const a = account(target.dataset.reset);
      const credits = a?.reset_credits?.count || 0;
      confirmSheet('Use a banked reset?', `${a?.email || 'This account'} gets its usage back right away. You have ${credits} banked reset${credits === 1 ? '' : 's'}; a used reset cannot be given back.`, 'Use reset')
        .then(ok => {
          if (!ok || !a?.reset_credits?.next_id) return;
          act(target, async () => {
            const answer = await api(`/api/accounts/${encodeURIComponent(a.id)}/use-reset`, { method: 'POST', body: { credit_id: a.reset_credits.next_id } });
            if (answer.outcome === 'reset') playMark($('top-mark'));
            toast(OUTCOMES[answer.outcome] || `The provider answered ${answer.outcome}`);
          });
        });
    }
  });
  boot();
}
