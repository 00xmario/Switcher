import test from 'node:test';
import assert from 'node:assert/strict';
import { tabsHTML, usageHTML, loadViewHTML, updateHTML, chartHTML, tokens, money } from './phone-views.js';

const text = html => html.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim()
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');

const summary = {
  total_cost_usd: 1234.5, total_tokens: 12_900_000_000, sessions: 42, unpriced_share: 0,
  cache_savings_usd: 88.1,
  totals: { uncached_input_tokens: 1e6, cached_input_tokens: 9e9, cache_creation_tokens: 0, output_tokens: 3e6 },
  providers: [
    { provider: 'claude', sessions: 30, cost_usd: 1000, tokens: 9e9, cost_share: 0.81, token_share: 0.7 },
    { provider: 'codex', sessions: 12, cost_usd: 234.5, tokens: 3.9e9, cost_share: 0.19, token_share: 0.3 },
  ],
  models: [{ provider: 'claude', model: 'claude-fable-5-2', cost_usd: 900, tokens: 8e9 }, { provider: 'codex', model: 'gpt-x<b>', cost_usd: 0, tokens: 1, unpriced: true }],
  days: [{ day: '2026-11-01', providers: { claude: { cost_usd: 10, tokens: 5 } } }, { day: '2026-11-14', providers: { claude: { cost_usd: 30, tokens: 9 }, codex: { cost_usd: 5, tokens: 1 } } }],
  devices: [{ name: 'k12', host: true, cost_usd: 1000, tokens: 9e9 }, { name: 'MacBook <Air>', this: true, cost_usd: 234.5, tokens: 3.9e9 }],
};

test('the tabs mark the open one', () => {
  const html = tabsHTML('usage');
  assert.match(html, /data-tab="usage" aria-selected="true"/);
  assert.match(html, /data-tab="load" aria-selected="false"/);
});

test('numbers read the way the dashboard writes them', () => {
  assert.equal(tokens(12_900_000_000), '12.9B');
  assert.equal(tokens(580_000_000), '580M');
  assert.equal(tokens(62_900), '62.9K');
  assert.equal(tokens(1_000_000), '1M');
  assert.equal(tokens(512), '512');
  assert.equal(money(1234.5), '$1,234.50');
});

test('usage shows the total, each tool, each computer and the top models', () => {
  const html = usageHTML({ summary }, { days: 30, metric: 'cost' });
  const t = text(html);
  assert.match(t, /\$1,234\.50/);
  assert.match(t, /42 sessions · across 2 computers/);
  assert.match(t, /Claude Code \$1,000\.00/);
  assert.match(t, /By computer/);
  assert.match(t, /claude-fable-5-2/);
  assert.match(html, /data-days="30" class="is-on"/);
  assert.match(html, /data-metric="cost" class="is-on"/);
  // Names from other computers and models are text, never markup.
  assert.doesNotMatch(html, /<Air>|gpt-x<b>/);
  assert.match(html, /MacBook &lt;Air&gt;/);
  const byTokens = text(usageHTML({ summary }, { days: 30, metric: 'tokens' }));
  assert.match(byTokens, /12\.9B/);
});

test('usage says when it is still reading, or what went wrong', () => {
  assert.match(text(usageHTML({ scanning: true })), /Reading the coding tools' sessions/);
  assert.match(text(usageHTML(null)), /Loading usage/);
  assert.match(text(usageHTML({ error: 'Switcher <did> not answer' })), /Switcher <did> not answer/);
  assert.doesNotMatch(usageHTML({ error: '<img>' }), /<img>/);
});

test('the chart draws a bar per day and needs at least two days', () => {
  assert.equal(chartHTML(summary.days.slice(0, 1), true), '');
  const html = chartHTML(summary.days, true);
  assert.equal((html.match(/<rect /g) || []).length, 3);
  assert.match(text(html), /peak \$35\.00/);
  assert.match(text(html), /1 Nov 14 Nov/);
});

test('load shows the rings and the apps using the most processor', () => {
  const view = {
    available: true, machine: 'k12', cpu: 23, memory: 61, cores: 12, memory_used: 2 ** 33, memory_total: 2 ** 34, processes: 200,
    apps: [{ name: 'node', processes: 3, cpu: 12.5, memory: 2 ** 30 }, { name: '<evil>', processes: 1, cpu: 40, memory: 2 ** 20 }],
  };
  const html = loadViewHTML(view, 'k12');
  assert.match(html, /data-gauge="cpu"/);
  const t = text(html);
  assert.ok(t.indexOf('<evil>') < t.indexOf('node'), 'apps are ordered by processor');
  assert.match(t, /200 processes/);
  assert.doesNotMatch(html, /<evil>/);
  assert.match(text(loadViewHTML({ available: false, reason: 'unreachable', machine: 'k12', remote: true })), /Can't reach k12/);
  assert.match(text(loadViewHTML(null)), /Reading the load/);
});

test('the update bar names the new version and escapes it', () => {
  const html = updateHTML('1.34.0');
  assert.match(text(html), /Switcher 1\.34\.0 is ready/);
  assert.match(html, /id="update-page"/);
  assert.doesNotMatch(updateHTML('<x>'), /<x>/);
});
