import test from 'node:test';
import assert from 'node:assert/strict';
import { loadHTML, level, gigabytes, title } from './load-card.js';

const text = html => html.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
const reading = { machine: 'k12', remote: false, platform: 'linux', available: true, cpu: 23, cores: 16, memory: 72,
  memory_used: 22.9 * 2 ** 30, memory_total: 31.8 * 2 ** 30, gpu: 94 };

test('three rings: processor, memory and graphics, each with its share', () => {
  const html = loadHTML(reading);
  assert.match(text(html), /^This computer 23% CPU 16 cores/);
  assert.match(html, /data-gauge="cpu"[^>]*aria-valuenow="23"/);
  assert.match(html, /stroke-dasharray:23 100/);
  assert.match(text(html), /16 cores/);
  assert.match(text(html), /22\.9 of 31\.8 GB/);
  assert.match(html, /class="gauge is-busy" data-gauge="memory"/);
  assert.match(html, /class="gauge is-strained" data-gauge="gpu"/);
  assert.match(html, /class="gauge is-calm" data-gauge="cpu"/);
});

test('a missing graphics reading says so instead of showing zero', () => {
  const html = loadHTML({ ...reading, gpu: undefined });
  assert.match(html, /class="gauge is-unknown" data-gauge="gpu"/);
  assert.match(text(html), /GPU No reading/);
});

test('the host is named while its accounts are used, and why its load is missing', () => {
  assert.match(text(loadHTML({ ...reading, remote: true, machine: 'k12' })), /^k12 host/);
  assert.match(text(loadHTML({ machine: 'k12', remote: true, available: false, reason: 'update' })), /Update k12 to see its load/);
  assert.match(text(loadHTML({ machine: 'k12', remote: true, available: false, reason: 'unreachable' })), /Can&#39;t reach k12 right now/);
  assert.equal(title({ remote: false, machine: 'k12' }), 'This computer');
});

test('names and numbers from the server are escaped and bounded', () => {
  const html = loadHTML({ ...reading, remote: true, machine: '<img src=x onerror=alert(1)>', cpu: 250, memory: -4 });
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /data-gauge="cpu"[^>]*aria-valuenow="100"/);
  assert.match(html, /data-gauge="memory"[^>]*aria-valuenow="0"/);
  assert.equal(loadHTML(null), '');
});

test('levels and sizes', () => {
  assert.deepEqual([0, 69, 70, 89, 90, 100].map(level), ['calm', 'calm', 'busy', 'busy', 'strained', 'strained']);
  assert.equal(gigabytes(16 * 2 ** 30), '16.0');
  assert.equal(gigabytes(128 * 2 ** 30), '128');
});

test('a card that says why there is no reading is kept, not redrawn', async () => {
  const { update } = await import('./load-card.js');
  const off = { querySelector: s => (s === '.load-card .load-off' ? {} : null) };
  assert.equal(update(off, { available: false, reason: 'unreachable' }), true);
  assert.equal(update({ querySelector: () => null }, { available: false }), false);
});

test('with a link, an available card opens the load page', async () => {
  const { loadHTML } = await import('./load-card.js');
  const view = { machine: 'k12', remote: true, available: true, cpu: 5, memory: 40, cores: 8 };
  assert.match(loadHTML(view, 'This computer', '/#load'), /<a class="card load-card is-link" href="\/#load" target="_blank" rel="noopener" title="See what is using k12">/);
  assert.doesNotMatch(loadHTML({ ...view, available: false, reason: 'unreachable' }, 'This computer', '/#load'), /<a /);
  assert.doesNotMatch(loadHTML(view), /<a /);
});
