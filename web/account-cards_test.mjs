import test from 'node:test';
import assert from 'node:assert/strict';
import { accountHTML, stateHTML } from './account-cards.js';

const now = 1_800_000_000;
const claude = {
  id: 'claude-a', provider: 'claude', email: 'a@example.com', plan: 'claude_max_20x',
  active: true, native_switch_available: true, native_active: false,
  usage: { windows: [{ label: 'Weekly', used_percent: 69, resets_at: now + 2 * 86400 + 8 * 3600 }] },
};

test('without a view the cards are the phone\'s: the proxy\'s account, your Mac, bars', () => {
  const html = accountHTML(claude, now);
  assert.match(html, /class="account is-active"/);
  assert.match(html, /class="lines"/);
  assert.match(stateHTML({ accounts: [] }, now), /Add them in Switcher on your Mac\./);
});

test('a view picks the account in use, the place in hints and the bars', () => {
  const view = { place: 'this computer', selected: a => a.native_switch_available ? a.native_active : a.active, bars: false };
  const html = accountHTML(claude, now, view);
  assert.doesNotMatch(html, /is-active/);
  assert.match(html, /<button type="button" class="row-main" data-use="claude-a">/);
  assert.match(html, /Use also switches Claude Code on this computer\./);
  assert.match(html, /class="lines no-bars"/);
  assert.match(accountHTML({ ...claude, native_active: true }, now, view), /class="account is-active"/);
  assert.match(stateHTML({ accounts: [claude] }, now, view), /data-use="claude-a"/);
  assert.match(stateHTML({ accounts: [] }, now, view), /Add them in Switcher on this computer\./);
});

test('the menu bar\'s badges show as notes', () => {
  const relogin = accountHTML({ ...claude, health: { condition: 'needs_relogin' } }, now);
  assert.match(relogin, /<span class="note is-out">Sign in again<\/span>/);
  const stale = accountHTML({ ...claude, health: { condition: 'usage_stale' } }, now);
  assert.match(stale, /<span class="note">Usage stale<\/span>/);
  // With no numbers on screen there is nothing to call stale.
  assert.doesNotMatch(accountHTML({ ...claude, usage: { windows: [] }, health: { condition: 'usage_unavailable' } }, now), /Usage stale/);
  assert.doesNotMatch(accountHTML({ ...claude, health: { condition: 'usage_current' } }, now), /Sign in again|Usage stale/);
});
