import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { selected, viewOf, subtitle, noticeHTML, pageHTML, messageHTML, switchedText, watch } from './panel.js';

const now = 1_800_000_000;
const text = html => html.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
const state = {
  version: '1.21.0', order: ['claude', 'codex'], hidden: [],
  accounts: [
    { id: 'claude-a', provider: 'claude', email: 'work@example.com', plan: 'claude_max_20x', active: true,
      native_switch_available: true, native_active: false,
      usage: { windows: [{ label: 'Weekly', used_percent: 69, resets_at: now + 2 * 86400 + 8 * 3600 }, { label: 'Fable', used_percent: 40 }] } },
    { id: 'claude-b', provider: 'claude', email: 'home@example.com', plan: 'claude_max_5x', active: false,
      native_switch_available: true, native_active: true },
    { id: 'codex-a', provider: 'codex', email: 'x@example.com', plan: 'prolite', active: true,
      exhausted_until: now + 3600, reset_credits: { count: 1, next_id: 'credit_1' } },
  ],
};

test('the panel marks the account the Mac menu marks: Claude Code\'s login for Claude', () => {
  assert.equal(selected(state.accounts[0]), false);
  assert.equal(selected(state.accounts[1]), true);
  assert.equal(selected(state.accounts[2]), true);
  const html = pageHTML(state, now);
  assert.match(html, /<article class="account is-active" data-account="claude-b">/);
  assert.match(html, /<button type="button" class="row-main" data-use="claude-a">/);
  assert.match(html, /Use also switches Claude Code on this computer\./);
  assert.match(html, /<article class="account is-active is-out" data-account="codex-a">/);
});

test('usage reads as on the phone and in the Mac menu: what is left and the reset', () => {
  const html = pageHTML(state, now);
  assert.match(text(html), /Weekly: 31% left ↻ , resets in 2d 8h/);
  assert.match(text(html), /Fable: 60% left/);
  assert.match(html, /Out of usage · back in 1h 0m/);
  assert.match(html, /data-reset="codex-a"/);
  assert.match(html, /class="lines"/);
  // Bars follow the menu's setting.
  assert.equal(viewOf({ menu_usage_bars: false }).bars, false);
  assert.equal(viewOf({}).bars, true);
  assert.match(pageHTML({ ...state, menu_usage_bars: false }, now), /class="lines no-bars"/);
  assert.match(html, /<a href="\/" target="_blank" rel="noopener">Open dashboard<\/a>/);
});

test('the header names the version, or the host whose accounts these are', () => {
  assert.equal(subtitle(state), 'v1.21.0');
  assert.equal(subtitle({ ...state, remote: { role: 'client', host: 'Studio', connected: true } }), 'Using the accounts of Studio');
  assert.equal(subtitle({ ...state, remote: { role: 'client', host: 'Studio', connected: false } }), "Can't reach Studio");
  assert.equal(subtitle(null), '');
});

test('notices say what changes how the accounts are used, escaped', () => {
  assert.equal(noticeHTML(state), '');
  assert.match(noticeHTML({ handed_back: true }), /handed back to their providers/);
  assert.match(noticeHTML({ remote: { role: 'local', host: 'Studio' } }), /Paired with <strong>Studio<\/strong>, using this computer's own accounts/);
  const hostile = noticeHTML({ remote: { role: 'client', host: '<img src=x>', connected: false } });
  assert.doesNotMatch(hostile, /<img/);
  assert.match(hostile, /Can't reach <strong>&lt;img src=x&gt;<\/strong>/);
  assert.doesNotMatch(messageHTML('<x>', '<y>'), /<x>|<y>/);
});

test('a switch says whether Claude Code moved too', () => {
  assert.equal(switchedText(state.accounts[0], { native: { changed: true } }), 'Claude and Claude Code now use work@example.com');
  assert.equal(switchedText(state.accounts[2], { status: 'ok' }), 'Codex now uses x@example.com');
  assert.equal(switchedText(undefined, {}), 'Switcher now uses that account');
});

const fail = status => Object.assign(new Error(`HTTP ${status}`), { status });

// A scripted server: each request takes the next answer, which must be for
// the path asked. When the script ends, the watch is stopped.
function scripted(script) {
  const asked = [], states = [], errors = [], sleeps = [];
  let watcher;
  const get = async path => {
    asked.push(path);
    const step = script.shift();
    if (!step) {
      watcher.stop();
      throw new Error('script ended');
    }
    assert.equal(path, step[0]);
    if (step[1] instanceof Error) throw step[1];
    return step[1];
  };
  return { asked, states, errors, sleeps, start(options = {}) {
    watcher = watch({ get, onState: s => states.push(s), onError: e => errors.push(e.status), sleep: async ms => { sleeps.push(ms); }, ...options });
    return watcher.running;
  } };
}

test('the panel reads the state when the server reports a change, and only then', async () => {
  const s = scripted([
    ['/api/state/changes?since=', { etag: 'a1', changed: true }],
    ['/api/state', { version: 'one' }],
    ['/api/state/changes?since=a1', { etag: 'a1', changed: false }],
    ['/api/state/changes?since=a1', { etag: 'a1', changed: false }],
    ['/api/state/changes?since=a1', { etag: 'b2', changed: true }],
    ['/api/state', { version: 'two' }],
  ]);
  await s.start();
  assert.deepEqual(s.states, [{ version: 'one' }, { version: 'two' }]);
  assert.deepEqual(s.sleeps, [], 'waiting on the server needs no timer');
  assert.deepEqual(s.errors, []);
});

test('a server without the changes feed is read every four seconds, then asked again', async () => {
  const t = scripted([
    ['/api/state/changes?since=', fail(404)],
    ['/api/state', { n: 1 }],
    ['/api/state', { n: 2 }],
    ['/api/state/changes?since=', fail(404)],
    ['/api/state', { n: 3 }],
  ]);
  await t.start({ rounds: 2 });
  assert.deepEqual(t.states, [{ n: 1 }, { n: 2 }, { n: 3 }]);
  // Each read waits its turn, including the one the script ended on.
  assert.equal(t.asked.at(-1), '/api/state');
  assert.deepEqual(t.sleeps, [4000, 4000, 4000, 4000]);
  assert.deepEqual(t.errors, [], 'an older server is no error');
});

test('a failed wait is reported and retried after a pause, from the start', async () => {
  const t = scripted([
    ['/api/state/changes?since=', { etag: 'a1', changed: true }],
    ['/api/state', { n: 1 }],
    ['/api/state/changes?since=a1', fail(502)],
    ['/api/state/changes?since=', { etag: 'a1', changed: true }],
    ['/api/state', { n: 2 }],
    // A server that reports a change under the same tag is not asked in a
    // tight loop.
    ['/api/state/changes?since=a1', { etag: 'a1', changed: true }],
    ['/api/state', { n: 3 }],
  ]);
  await t.start();
  assert.deepEqual(t.errors, [502]);
  assert.deepEqual(t.states, [{ n: 1 }, { n: 2 }, { n: 3 }]);
  assert.deepEqual(t.sleeps, [4000, 4000]);
});

test('the page keeps the strict CSP: no inline scripts or handlers', () => {
  const page = readFileSync(new URL('./panel.html', import.meta.url), 'utf8');
  const scripts = [...page.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)];
  assert.equal(scripts.length, 1);
  assert.match(scripts[0][1], /type="module" src="\/panel.js"/);
  assert.equal(scripts[0][2].trim(), '');
  assert.doesNotMatch(page, /\son[a-z]+=/i);
  assert.match(page, /href="\/phone.css"/);
  assert.match(page, /href="\/panel.css"/);
});

test('usage limits: the panel offers them, marks the bar and says when an account reached its limit', () => {
  const limited = { ...state, accounts: [{ ...state.accounts[0], usage_limit: 30, at_limit: true }, state.accounts[1], state.accounts[2]] };
  const html = pageHTML(limited, now);
  assert.match(html, /<select data-limit="claude-a"/);
  assert.match(html, /<option value="30" selected>keep 30%<\/option>/);
  assert.match(html, /class="limit-tick" style="left:30%"/);
  assert.equal((html.match(/limit-tick/g) || []).length, 1, 'only the weekly window carries the line');
  assert.match(text(html), /At its 30% limit · Switcher uses another account/);
  assert.doesNotMatch(html, /data-limit="claude-b"/, 'an account without a weekly window offers no limit');
  assert.doesNotMatch(pageHTML({ ...limited, handed_back: true }, now), /<select data-limit/);
});

test('the load card sits above the footer', () => {
  const html = pageHTML(state, now, { machine: 'k12', remote: true, available: true, cpu: 5, memory: 40, cores: 8 });
  assert.ok(html.indexOf('class="provider load"') > html.indexOf('data-account="codex-a"'));
  assert.ok(html.indexOf('class="provider load"') < html.indexOf('class="foot"'));
  assert.doesNotMatch(pageHTML(state, now), /class="provider load"/);
});

test('a usage limit that is not a whole share is never printed', () => {
  const evil = { ...state, accounts: [{ ...state.accounts[0], usage_limit: '"><img src=x onerror=alert(1)>', at_limit: true }] };
  const html = pageHTML(evil, now);
  assert.doesNotMatch(html, /<img/);
  assert.doesNotMatch(html, /At its/);
  assert.match(html, /<option value="0" selected>off<\/option>/);
});
