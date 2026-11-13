import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// The Usage page's breakdown by computer and its note on the host's part,
// run from app.js itself.
const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const slice = (from, to) => {
  const start = source.indexOf(from);
  const end = source.indexOf(to, start);
  assert.ok(start >= 0 && end > start, `app.js has ${from}`);
  return source.slice(start, end);
};

function element() {
  return {
    children: [], hidden: false, className: '', textContent: '', _html: '',
    get innerHTML() { return this._html + this.children.map(c => c.textContent || c.innerHTML).join(''); },
    set innerHTML(v) { this._html = v; this.children = []; },
    appendChild(c) { this.children.push(c); return c; },
  };
}

function page() {
  const usageDevices = element();
  const context = vm.createContext({ usageDevices, document: { createElement: element }, Date, Math, Number, String });
  vm.runInContext([
    slice('function escapeHTML(s) {', '\n// fmtRemaining'),
    slice('function fmtTokens(n) {', '\nfunction renderUsage() {'),
    slice('function sigRound(', '\n}\n') + '\n}',
    slice('// usageSharingNote says', '\n/* ---------- daily chart'),
    'this.renderUsageDevices = renderUsageDevices; this.usageSharingNote = usageSharingNote;',
  ].join('\n'), context);
  return { context, usageDevices };
}

test('each computer shows its share, its tags and its name escaped, the largest first', () => {
  const { context, usageDevices } = page();
  const hourAgo = Date.now() - 3 * 60 * 60 * 1000;
  context.renderUsageDevices({
    total_cost_usd: 10, total_tokens: 1000,
    devices: [
      { name: 'Mini <b>', cost_usd: 2, tokens: 800, updated_ms: hourAgo },
      { name: 'k12', host: true, cost_usd: 7, tokens: 150, updated_ms: Date.now() },
      { name: 'MacBook', this: true, cost_usd: 1, tokens: 50, updated_ms: hourAgo },
    ],
  }, true);
  assert.equal(usageDevices.hidden, false);
  const rows = usageDevices.children.slice(1).map(r => r.innerHTML);
  assert.equal(usageDevices.children[0].textContent, 'By computer');
  assert.match(rows[0], /k12[\s\S]*usage-device-tag">host<[\s\S]*\$7\.00[\s\S]*70\.0% of cost/);
  assert.match(rows[1], /Mini &lt;b&gt;[\s\S]*as of 3 h ago/);
  assert.doesNotMatch(rows[1], /<b>/);
  assert.match(rows[2], /MacBook[\s\S]*this computer/);
  assert.doesNotMatch(rows[2], /as of/, 'this computer is always current');
  // By tokens, the order follows the tokens.
  context.renderUsageDevices({ total_cost_usd: 10, total_tokens: 1000, devices: [
    { name: 'k12', cost_usd: 7, tokens: 150 }, { name: 'Mini', cost_usd: 2, tokens: 800 },
  ] }, false);
  assert.match(usageDevices.children[1].innerHTML, /Mini/);
});

test('one computer alone shows no breakdown', () => {
  const { context, usageDevices } = page();
  context.renderUsageDevices({ total_cost_usd: 1, total_tokens: 1, devices: [{ name: 'k12', cost_usd: 1, tokens: 1 }] }, true);
  assert.equal(usageDevices.hidden, true);
  assert.equal(usageDevices.children.length, 0);
});

test('the note says when the host is behind, and why nothing is added up', () => {
  const { context } = page();
  const note = context.usageSharingNote;
  assert.equal(note(null), '');
  assert.equal(note({ host: 'k12', updated_ms: Date.now() }), '');
  assert.equal(note({ host: 'k12' }), "reading k12's usage");
  assert.equal(note({ host: 'k12', error: 'could not reach the host', reason: 'unreachable' }), "k12's usage could not be read yet");
  assert.equal(note({ host: 'k12', updated_ms: Date.now() - 2 * 60 * 60 * 1000, error: 'x', reason: 'unreachable' }), "k12's usage as of 2 h ago");
  assert.equal(note({ host: 'k12', updated_ms: Date.now(), error: 'the host does not combine usage yet; update it', reason: 'old_host' }),
    'k12 does not add up usage yet; update Switcher there');
  assert.equal(note({ host: 'k12', error: 'the host refused: it no longer accepts this computer; pair again', reason: 'rejected' }),
    'k12 no longer accepts this computer; pair again to add up usage');
  assert.equal(note({ host: 'k12', updated_ms: Date.now(), error: 'the host refused: full', reason: 'refused' }),
    "k12 did not take this computer's usage (the host refused: full)");
});
