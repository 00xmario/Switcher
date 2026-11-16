import test from 'node:test';
import assert from 'node:assert/strict';
import { cardWeeks, cardStats, cardSubtitle, shareText, intentURL, dialogHTML, REPO, WEEKS } from './share-card.js';

const insights = {
  range: 'all', until: '2026-11-16', first_day: '2026-04-01',
  totals: { tokens: 2_600_000_000 },
  peak_day: { day: '2026-10-09', tokens: 447_100_000 },
  streaks: { current: 7, longest: 14 },
  providers: [{ provider: 'claude', tokens: 2e9 }, { provider: 'codex', tokens: 6e8 }, { provider: 'grok', tokens: 0 }],
  days: [
    { day: '2026-11-16', tokens: 100 }, { day: '2026-11-15', tokens: 400_000_000 },
    { day: '2026-10-09', tokens: 447_100_000 }, { day: '2025-01-01', tokens: 5 },
  ],
};

test('the card shows the last 26 weeks, Monday on top, ending today', () => {
  const cols = cardWeeks(insights);
  assert.equal(cols.length, WEEKS);
  const last = cols[cols.length - 1];
  assert.equal(last[0].day, '2026-11-16'); // a Monday
  assert.equal(last[1], null, 'no days after today');
  assert.equal(cols[0][0].day, '2026-05-25');
  const all = cols.flat().filter(Boolean);
  assert.equal(all.length, 25 * 7 + 1);
  assert.ok(all.find(c => c.day === '2026-10-09').level > 0);
  assert.equal(all.find(c => c.day === '2026-06-01').level, 0);
  assert.ok(!all.some(c => c.day === '2025-01-01'), 'older days are not on the card');
});

test('the four numbers, the tools and the post text', () => {
  assert.deepEqual(cardStats(insights), [['2.6B', 'lifetime tokens'], ['447M', 'peak day'], ['7 days', 'current streak'], ['14 days', 'longest streak']]);
  assert.equal(cardSubtitle(insights), 'Claude Code · Codex · since Apr 2026');
  assert.equal(shareText(insights), 'My AI coding activity: 2.6B tokens, a 14-day streak. Tracked with Switcher');
});

test('each site gets the text and the repository, encoded', () => {
  const text = shareText(insights);
  assert.equal(intentURL('x', text), `https://x.com/intent/post?text=${encodeURIComponent(text)}&url=${encodeURIComponent(REPO)}`);
  assert.match(intentURL('reddit', text), /^https:\/\/www\.reddit\.com\/submit\?url=https%3A%2F%2Fgithub\.com%2F00xmario%2FSwitcher&title=My%20AI/);
  assert.equal(intentURL('linkedin', text), 'https://www.linkedin.com/sharing/share-offsite/?url=https%3A%2F%2Fgithub.com%2F00xmario%2FSwitcher');
});

test('the dialog offers every way to share, and the system sheet only where there is one', () => {
  const html = dialogHTML('light', false);
  for (const id of ['x', 'linkedin', 'reddit', 'copy', 'save']) assert.match(html, new RegExp(`data-share="${id}"`));
  assert.doesNotMatch(html, /data-share="share"/);
  assert.match(dialogHTML('dark', true), /data-share="share"/);
  assert.match(html, /data-share-theme="light" class="active"/);
});
