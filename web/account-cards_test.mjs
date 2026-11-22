import test from 'node:test';
import assert from 'node:assert/strict';
import { accountHTML, stateHTML, resetAsk, endingNote } from './account-cards.js';

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

test('Claude\'s kinds of banked reset get a button each, and the question names the kind', () => {
  const a = { ...claude, reset_credits: { count: 3, next_id: '1-0011aabb-weekly', kinds: [
    { kind: 'Weekly reset', count: 1, next_id: '1-0011aabb-weekly' },
    { kind: 'Fable reset', count: 2, next_id: '2-0011aabb-fable', unusable: 'Usable once you reach a limit it refills' },
  ] } };
  const html = accountHTML(a, now);
  assert.match(html, /⚡ 3 banked/);
  assert.match(html, /title="1 × Weekly reset, 2 × Fable reset"/);
  assert.match(html, /data-credit="1-0011aabb-weekly"[^>]*>Use weekly reset · 1</);
  assert.match(html, /data-credit="2-0011aabb-fable"[^>]*title="Usable once you reach a limit it refills">Use Fable reset · 2</);
  const button = { dataset: { kind: 'Fable reset', credit: '2-0011aabb-fable', count: '2' } };
  const ask = resetAsk(a, button);
  assert.equal(ask.title, 'Use a Fable reset?');
  assert.equal(ask.credit, '2-0011aabb-fable');
  assert.match(ask.message, /^Usable once you reach a limit it refills\. a@example\.com has 2 left/);
});

test('a Codex account keeps its single banked reset button', () => {
  const a = { ...claude, provider: 'codex', reset_credits: { count: 2, next_id: 'credit-1' } };
  const html = accountHTML(a, now);
  assert.match(html, />Use banked reset</);
  const ask = resetAsk(a, { dataset: {} });
  assert.equal(ask.credit, 'credit-1');
  assert.equal(ask.title, 'Use a banked reset?');
});

test('an end date shows as a note with the days to go; a paid-through day without one', () => {
  const at = new Date(2026, 10, 22, 15).getTime() / 1000;
  assert.match(endingNote({ end_date: '2026-11-29', ending: true }, at), /<span class="note">Ends Nov 29 · in 7 days · used first<\/span>/);
  assert.match(endingNote({ end_date: '2026-11-24', ending: true }, at), /class="note is-out">Ends Nov 24 · in 2 days/);
  assert.match(endingNote({ end_date: '2026-11-22', ending: true }, at), />Ends today · used first</);
  assert.match(endingNote({ end_date: '2026-11-20' }, at), />Subscription ended Nov 20</);
  assert.equal(endingNote({ end_date: '"><img src=x>', ending: true }, at), '');
  assert.equal(endingNote({ end_date: '2026-02-30', ending: true }, at), '');
  assert.match(endingNote({ paid_through: new Date(2026, 11, 20).getTime() / 1000 }, at), />Paid through Dec 20</);
  assert.equal(endingNote({ paid_through: at - 60 }, at), '');
  assert.match(accountHTML({ ...claude, end_date: '2026-11-29', ending: true }, at), /Ends Nov 29/);
});
