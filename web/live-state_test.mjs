import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import { createLiveState, retryDelay, CHANGES_PATH, OLD_SERVER_RETRY_MS, MAX_BACKOFF_MS } from './live-state.js';

const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
const httpError = status => Object.assign(new Error(`HTTP ${status}`), { status });

function fixture({ active = () => true } = {}) {
  const requests = [], sleeps = [];
  let changes = 0;
  const live = createLiveState({
    request: (path, options) => {
      const reply = deferred();
      requests.push({ path, signal: options.signal, ...reply });
      return reply.promise;
    },
    onChange: () => { changes++; },
    active: () => active(),
    sleep: ms => { const wake = deferred(); sleeps.push({ ms, ...wake }); return wake.promise; },
  });
  return { live, requests, sleeps, changes: () => changes };
}

test('a change reads the state at once and the page waits again from the new tag', async () => {
  const { live, requests, changes } = fixture();
  live.start();
  live.start(); // already waiting
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, `${CHANGES_PATH}?since=`);
  requests[0].resolve({ etag: 'tag/1', changed: true });
  await settle();
  assert.equal(changes(), 1);
  assert.equal(requests[1].path, `${CHANGES_PATH}?since=tag%2F1`);
  requests[1].resolve({ etag: 'tag/1', changed: false });
  await settle();
  assert.equal(changes(), 1, 'a wait that timed out reads nothing');
  assert.equal(requests.length, 3);
  requests[2].resolve({ etag: 'tag/2', changed: true });
  await settle();
  assert.equal(changes(), 2);
  assert.equal(live.etag, 'tag/2');
  live.stop();
});

test('a hidden page holds no wait open and keeps the tag it saw', async () => {
  let shown = true;
  const { live, requests } = fixture({ active: () => shown });
  live.start();
  requests[0].resolve({ etag: 'seen', changed: true });
  await settle();
  shown = false;
  live.stop();
  assert.equal(requests[1].signal.aborted, true, 'hiding the page must end the wait');
  requests[1].reject(Object.assign(new Error('aborted'), { name: 'AbortError' }));
  await settle();
  assert.equal(requests.length, 2);
  live.start();
  assert.equal(requests.length, 2, 'a hidden page must not wait');
  shown = true;
  live.start();
  assert.equal(requests[2].path, `${CHANGES_PATH}?since=seen`, 'shown again, it asks about changes since what it saw');
  live.stop();
});

test('an older server without the route is asked again only after a while', async () => {
  const { live, requests, sleeps, changes } = fixture();
  live.start();
  requests[0].reject(httpError(404));
  await settle();
  assert.deepEqual(sleeps.map(s => s.ms), [OLD_SERVER_RETRY_MS]);
  assert.equal(requests.length, 1);
  sleeps[0].resolve();
  await settle();
  assert.equal(requests.length, 2);
  requests[1].resolve({ etag: 'updated', changed: true });
  await settle();
  assert.equal(changes(), 1);
  live.stop();
});

test('errors back off, odd answers count as errors, and an answer resets the backoff', async () => {
  const { live, requests, sleeps, changes } = fixture();
  live.start();
  const failures = [httpError(503), new Error('network down'), { etag: 7, changed: true }, httpError(500)];
  for (const [i, failure] of failures.entries()) {
    if (failure instanceof Error) requests[i].reject(failure); else requests[i].resolve(failure);
    await settle();
    sleeps[i].resolve();
    await settle();
  }
  assert.deepEqual(sleeps.map(s => s.ms), [1000, 2000, 4000, 8000]);
  assert.equal(changes(), 0);
  requests[4].resolve({ etag: 'back', changed: true });
  await settle();
  requests[5].reject(httpError(502));
  await settle();
  assert.equal(sleeps.at(-1).ms, 1000, 'an answer resets the backoff');
  assert.equal(retryDelay(40), MAX_BACKOFF_MS);
  live.stop();
  sleeps.at(-1).resolve();
  await settle();
  assert.equal(requests.length, 6, 'a stopped page does not ask again after its backoff');
});

test('a page that was signed out stops waiting', async () => {
  let signedIn = true;
  const { live, requests, changes } = fixture({ active: () => signedIn });
  live.start();
  signedIn = false;
  requests[0].resolve({ etag: 'late', changed: true });
  await settle();
  assert.equal(requests.length, 1);
  assert.equal(live.running, false);
  assert.equal(changes(), 1, 'refreshState itself refuses once signed out');
});

// The dashboard wires the wait to its own request, refresh and visibility.
test('the dashboard waits with a long deadline and only while shown and signed in', async () => {
  const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const start = source.indexOf('const liveState = createLiveState({');
  const end = source.indexOf('\n/* ===', start);
  assert.ok(start >= 0 && end > start, 'the live state wiring must remain findable');
  let options, starts = 0, stops = 0, refreshes = 0, visibility;
  const calls = [];
  const context = vm.createContext({
    createLiveState: given => { options = given; return { start: () => starts++, stop: () => stops++ }; },
    api: (path, opts) => { calls.push({ path, opts }); return Promise.resolve({}); },
    refreshState: () => { refreshes++; },
    document: { hidden: false, addEventListener: (name, fn) => { if (name === 'visibilitychange') visibility = fn; } },
    authState: { locked: false },
    ensureAuthReady: () => Promise.resolve(),
  });
  vm.runInContext(source.slice(start, end), context);
  await settle();
  assert.equal(starts, 1, 'the wait starts once signed in');
  const signal = new AbortController().signal;
  await options.request('/api/state/changes?since=x', { signal });
  assert.equal(calls[0].path, '/api/state/changes?since=x');
  assert.ok(calls[0].opts.timeoutMs > 25000, 'the deadline must outlast the server wait');
  assert.equal(calls[0].opts.signal, signal);
  options.onChange();
  assert.equal(refreshes, 1);
  assert.equal(options.active(), true);
  context.document.hidden = true;
  assert.equal(options.active(), false);
  visibility();
  assert.equal(stops, 1);
  context.document.hidden = false;
  visibility();
  assert.equal(starts, 2);
  context.authState.locked = true;
  assert.equal(options.active(), false);
});
