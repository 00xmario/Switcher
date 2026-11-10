import assert from 'node:assert/strict';
import test from 'node:test';
import { bytes, share, sparkPath, rank } from './load-page.js';

test('sizes and shares read well', () => {
  assert.equal(bytes(0), '0 MB');
  assert.equal(bytes(5 * 2 ** 20), '5 MB');
  assert.equal(bytes(1000), '1 MB');
  assert.equal(bytes(3.25 * 2 ** 30), '3.3 GB');
  assert.equal(share(0), '0.0%');
  assert.equal(share(4.26), '4.3%');
  assert.equal(share(12.6), '13%');
  assert.equal(share(NaN), '–');
});

test('the history draws from the right, and needs two readings', () => {
  assert.deepEqual(sparkPath([50]), { line: '', area: '' });
  const { line, area } = sparkPath([0, 100]);
  assert.match(line, /^M117\.97 38\.00L120\.00 4\.00$/);
  assert.match(area, /L120\.00 40L117\.97 40Z$/);
  assert.equal(sparkPath([NaN, NaN, 10]).line, '');
});

test('apps rank by the chosen metric, as shares of the machine', () => {
  const view = { memory_total: 1000, apps: [
    { name: 'b', cpu: 5, memory: 100, gpu: 0, gpu_memory: 50 },
    { name: 'a', cpu: 20, memory: 50, gpu: 30 },
    { name: 'c', cpu: 5, memory: 500, gpu: 0, gpu_memory: 900 },
  ] };
  assert.deepEqual(rank(view, 'cpu').map(r => r.app.name), ['a', 'b', 'c']);
  const memory = rank(view, 'memory');
  assert.deepEqual(memory.map(r => r.app.name), ['c', 'b', 'a']);
  assert.equal(memory[0].bar, 50);
  assert.equal(memory[0].text, '1 MB');
  // Graphics: busy time first, then graphics memory.
  assert.deepEqual(rank(view, 'gpu').map(r => r.app.name), ['a', 'c', 'b']);
  assert.deepEqual(rank(null, 'cpu'), []);
});
