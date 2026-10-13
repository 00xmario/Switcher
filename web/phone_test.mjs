import test from 'node:test';
import assert from 'node:assert/strict';
import { accountHTML, stateHTML, pairHTML, windowHTML, footerHTML, messageHTML, duration, esc } from './phone.js';

const now = 1_800_000_000;
const text = html => html.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();

test('a usage line shows what is left, a bar and the reset countdown', () => {
  const html = windowHTML({ label: 'Weekly', used_percent: 92, resets_at: now + 2 * 86400 + 3600 }, now);
  assert.equal(text(html), 'Weekly: 8% left ↻ , resets in 2d 1h');
  assert.match(html, /class="line is-low"/);
  assert.match(html, /<i style="width:8%"><\/i>/);
  const fresh = windowHTML({ label: 'Session', used_percent: 34, resets_at: now - 5 }, now);
  assert.equal(text(fresh), 'Session: 66% left');
  assert.doesNotMatch(fresh, /is-low/);
  assert.equal(duration(30), '1m');
  assert.equal(duration(2 * 3600 + 20 * 60), '2h 20m');
});

test('the account in use shows its plan and check mark and offers no switch', () => {
  const codex = { id: 'codex-a', provider: 'codex', email: 'a@example.com', plan: 'prolite', active: true,
    exhausted_until: now + 3600, usage: { windows: [{ label: 'Weekly', used_percent: 100 }] },
    reset_credits: { count: 2, next_id: 'credit_1' } };
  const html = accountHTML(codex, now);
  assert.match(html, /class="account is-active is-out"/);
  assert.match(html, /<span class="plan">· Pro 5x<\/span>/);
  assert.match(html, /class="check"/);
  assert.match(html, /⚡ 2 banked/);
  assert.match(html, /Out of usage · back in 1h 0m/);
  assert.match(html, /<button type="button" class="button accent" data-reset="codex-a">Use banked reset<\/button>/);
  assert.doesNotMatch(html, /data-use=/);
  assert.doesNotMatch(html, /class="use"/);
  // A banked reset needs the credit to spend.
  assert.doesNotMatch(accountHTML({ ...codex, reset_credits: { count: 1 } }, now), /data-reset=/);
  // Without running out, the reset button is a plain one.
  assert.match(accountHTML({ ...codex, exhausted_until: 0 }, now), /class="button" data-reset="codex-a"/);
});

test('another account is one row to tap, with the Claude Code hint', () => {
  const claude = { id: 'claude-b', provider: 'claude', email: 'b@example.com', plan: 'claude_max_5x', active: false, native_switch_available: true };
  const html = accountHTML(claude, now);
  assert.match(html, /<button type="button" class="row-main" data-use="claude-b">/);
  assert.match(html, /<span class="use">Use<\/span>/);
  assert.match(html, /Use also switches Claude Code on your Mac/);
  assert.match(html, /No usage reported yet/);
  assert.doesNotMatch(html, /class="check"/);
  const active = accountHTML({ ...claude, active: true }, now);
  assert.doesNotMatch(active, /data-use=/);
  assert.doesNotMatch(active, /also switches Claude Code/);
});

test('account text is escaped', () => {
  const html = accountHTML({ id: 'x"y', provider: 'codex', email: '<img src=x onerror=alert(1)>',
    usage: { windows: [{ label: '<b>Week</b>', used_percent: 10 }] } }, now);
  assert.doesNotMatch(html, /<img|<b>Week/);
  assert.match(html, /&lt;img src=x/);
  assert.match(html, /data-use="x&quot;y"/);
  assert.match(stateHTML({ accounts: [{ id: 'z', provider: '<script>', email: 'z@example.com' }] }, now), /&lt;script&gt;/);
  assert.match(footerHTML('<Phone>'), /This phone: &lt;Phone&gt;/);
  assert.doesNotMatch(messageHTML('<x>', '<y>'), /<x>|<y>/);
  assert.equal(esc(`"'&`), '&quot;&#39;&amp;');
});

test('providers follow the dashboard order, each in one card, and hidden ones stay hidden', () => {
  const accounts = [
    { id: 'c', provider: 'claude', email: 'c@example.com' },
    { id: 'x', provider: 'codex', email: 'x@example.com' },
    { id: 'y', provider: 'codex', email: 'y@example.com', active: true },
    { id: 'o', provider: 'opencode', email: 'o@example.com' },
  ];
  const hidden = stateHTML({ order: ['claude', 'codex'], hidden: ['claude'], accounts }, now);
  assert.match(hidden, /Codex/);
  assert.doesNotMatch(hidden, /c@example.com/);
  assert.match(hidden, /class="logo logo-codex"[^>]*><svg/);
  assert.equal(hidden.match(/class="card"/g).length, 2);
  // Accounts keep their order, as in the menu; the one in use does not jump.
  assert.ok(hidden.indexOf('x@example.com') < hidden.indexOf('y@example.com'));
  // Providers missing from the order come last.
  assert.ok(hidden.indexOf('Codex') < hidden.indexOf('OpenCode'));
  const both = stateHTML({ order: ['claude', 'codex'], accounts }, now);
  assert.ok(both.indexOf('Claude') < both.indexOf('Codex'));
  assert.match(stateHTML({ accounts: [] }, now), /No accounts yet/);
});

test('pairing shows the code in two groups with its expiry', () => {
  const html = pairHTML({ code: '482913', expires_at: now + 125 }, now);
  const groups = [...html.matchAll(/<span class="code-group">(.*?)<\/span>/g)].map(m => text(m[1]));
  assert.deepEqual(groups, ['482', '913']);
  assert.match(html, /aria-label="Code 4 8 2 9 1 3"/);
  assert.match(html, /Expires in 2:05/);
  assert.match(html, /switcher phone approve 482913/);
  assert.match(html, /Waiting for your Mac/);
  assert.match(pairHTML(null), /id="pair-start"/);
  const hostile = pairHTML({ code: '<i>12', expires_at: now }, now);
  assert.doesNotMatch(hostile, /<i>12/);
  assert.match(hostile, /<b>&lt;<\/b><b>i<\/b><b>&gt;<\/b>/);
});
