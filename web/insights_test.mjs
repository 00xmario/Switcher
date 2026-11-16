import test from 'node:test';
import assert from 'node:assert/strict';
import {
  tokens, money, moneyShort, duration, levels, weeks, change, rate, within, statsHTML, heatmapHTML, weekdayHTML,
  hoursHTML, modelsHTML, recordsHTML, bodyHTML, limitsHTML, poolChartHTML, accountsHTML, eventsHTML, controlsHTML,
} from './insights.js';

const text = html => html.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim()
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');

const day = (d, tokens, cost = 0, providers = {}) => ({ day: d, tokens, cost_usd: cost, sessions: tokens ? 1 : 0, providers });
const insights = {
  range: 'all', since: '2026-11-02', until: '2026-11-15', first_day: '2026-11-02', time_zone: 'UTC',
  totals: { tokens: 3_000_000, cost_usd: 1234.5, active_days: 3, days: 14, sessions: 4, cached_share: 0.8, cache_savings_usd: 10, avg_tokens_per_active_day: 1e6, avg_cost_per_active_day: 411.5 },
  streaks: { current: 2, longest: 2, longest_from: '2026-11-14', longest_to: '2026-11-15' },
  peak_day: day('2026-11-14', 2_000_000, 1000), peak_cost_day: day('2026-11-14', 2_000_000, 1000),
  peak_hour: { start_ms: new Date(2026, 10, 14, 15).getTime(), tokens: 900_000, cost_usd: 400 },
  longest_session: { provider: 'claude', minutes: 525, day: '2026-11-14' },
  sessions: { count: 4, avg_minutes: 90, median_minutes: 60, per_active_day: 1.3 },
  days: Array.from({ length: 14 }, (_, i) => {
    const d = `2026-11-${String(i + 2).padStart(2, '0')}`;
    return d === '2026-11-14' ? day(d, 2_000_000, 1000, { claude: { tokens: 2_000_000, cost_usd: 1000 } })
      : d === '2026-11-15' ? day(d, 900_000, 200) : d === '2026-11-02' ? day(d, 100_000, 34.5) : day(d, 0);
  }),
  weekdays: ['Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday', 'Sunday'].map((w, i) => ({
    weekday: w, tokens: [100_000, 0, 0, 0, 0, 2_000_000, 900_000][i], days: 2, active_days: i === 0 || i > 4 ? 1 : 0,
    avg_tokens: [50_000, 0, 0, 0, 0, 1_000_000, 450_000][i], avg_cost_usd: 0, cost_usd: 0,
  })),
  hours: Array.from({ length: 24 }, (_, h) => ({ hour: h, tokens: h === 15 ? 2_000_000 : h === 16 ? 1_000_000 : 0, cost_usd: 0 })),
  grid: Array.from({ length: 7 }, () => Array(24).fill(0)),
  months: [{ month: '2026-11', tokens: 3_000_000, cost_usd: 1234.5, active_days: 3, sessions: 4 }],
  models: [
    { provider: 'claude', model: 'claude-fable-5-2', tokens: 2_500_000, cost_usd: 1200, token_share: 0.83, cost_share: 0.97, first_day: '2026-11-02', last_day: '2026-11-15', active_days: 3 },
    { provider: 'codex', model: '<img src=x onerror=alert(1)>', tokens: 500_000, cost_usd: 34.5, token_share: 0.17, cost_share: 0.03, first_day: '2026-11-15', last_day: '2026-11-15', active_days: 1, unpriced: true },
  ],
  providers: [{ provider: 'claude', tokens: 2_500_000, cost_usd: 1200, sessions: 3, active_days: 3, first_day: '2026-11-02' }],
  devices: [{ name: 'Studio <b>', host: true, tokens: 2_000_000, cost_usd: 900 }, { name: 'MacBook', this: true, tokens: 1_000_000, cost_usd: 334.5 }],
  compare: { this_week: { tokens: 2_900_000, cost_usd: 1200 }, last_week: { tokens: 100_000, cost_usd: 34.5 }, this_month: { tokens: 3e6 }, last_month: { tokens: 0 } },
};

test('numbers read the way the Usage page writes them', () => {
  assert.equal(tokens(12_900_000_000), '12.9B');
  assert.equal(tokens(580_000_000), '580M');
  assert.equal(tokens(1_000_000), '1M');
  assert.equal(tokens(999_999), '1M');
  assert.equal(tokens(512), '512');
  assert.equal(money(1234.5), '$1,234.50');
  assert.equal(moneyShort(519_745.67), '$520K');
  assert.equal(moneyShort(12.5), '$12.50');
  assert.equal(duration(525), '8h 45m');
  assert.equal(duration(45), '45m');
  assert.equal(duration(60 * 50), '2d 2h');
  assert.equal(change(150, 100), '+50%');
  assert.equal(change(50, 100), '-50%');
  assert.equal(change(5, 0), 'new');
  assert.equal(rate(3.04), '3.0%/h');
  assert.equal(within(1000 + 14 * 3600 + 120, 1000), 'in 14h 2m');
  assert.equal(within(1000 - 3 * 3600, 1000), '3h ago');
});

test('the heatmap has a column a week, Monday on top, and shades by quartile', () => {
  const cols = weeks(insights.days);
  assert.equal(cols[0].length, 7); // 2 Nov 2026 is a Monday
  assert.equal(cols.length, 2);
  const level = levels([0, 1, 2, 3, 4, 100]);
  assert.equal(level(0), 0);
  assert.equal(level(100), 4);
  assert.ok(level(1) <= level(3));
  const html = heatmapHTML(insights, 'tokens');
  assert.equal((html.match(/class="ins-heat-cell l\d"/g) || []).length, 14 + 5);
  assert.match(html, /data-tip="2M on Nov 14/);
});

test('the headline numbers and the busiest weekday and hour', () => {
  const stats = text(statsHTML(insights, 'tokens'));
  assert.match(stats, /3M Lifetime tokens \$1.23K at API prices/);
  assert.match(stats, /8h 45m Longest session Claude Code/);
  assert.match(stats, /2 days Current streak/);
  assert.match(text(weekdayHTML(insights, 'tokens')), /Busiest on Saturdays ?, 1M on an average one; 4 days of the week without any/);
  assert.match(text(hoursHTML(insights, 'tokens')), /Most active 15:00–16:00 ?; 100% of it falls between/);
});

test('names from other computers and models are text, never markup', () => {
  const html = bodyHTML(insights, { metric: 'cost', activity: 'daily' }, {});
  assert.doesNotMatch(html, /<img src=x/);
  assert.doesNotMatch(html, /Studio <b>/);
  assert.match(html, /Studio &lt;b&gt;/);
  assert.match(text(modelsHTML(insights, 'cost')), /no price/);
  assert.match(text(html), /Compared with before/);
  assert.match(text(recordsHTML(insights, 'tokens')), /Busiest hour 900K tokens/);
});

const report = {
  now: 1_000_000, since: 1_000_000 - 7 * 86400, days: 7, burn_hours: 3, recording_since: 1_000_000 - 2 * 86400,
  pools: [
    { provider: 'claude', window: 'Weekly', accounts: 2, capacity: 200, remaining: 122, empty_now: 0,
      recorded: [{ t: 1_000_000 - 86400, remaining: 140, capacity: 200 }, { t: 1_000_000, remaining: 122, capacity: 200 }],
      projected: [{ t: 1_000_000, remaining: 122, capacity: 200 }, { t: 1_000_000 + 86400, remaining: 50, capacity: 200 }],
      resets: [{ t: 1_000_000 + 3600, account: 'a', name: 'work@<x>', gain: 58, projected: true }],
      burn: { '1h': 3, '3h': 3, '6h': 3, '24h': 1 }, first_empty: { t: 1_000_000 + 14 * 3600, account: 'a', name: 'work@<x>' },
      pool_empty: 1_000_000 + 40 * 3600, next_reset: { t: 1_000_000 + 3600, account: 'a', name: 'work@<x>', gain: 58 }, limit_hits: 2 },
    { provider: 'claude', window: 'Session', accounts: 2, capacity: 200, remaining: 150, recorded: [], projected: [], burn: {} },
  ],
  accounts: [{ id: 'a', provider: 'claude', name: 'work@<x>', active: true, limit_hits: 2, last_hit: 1_000_000 - 3600,
    windows: [{ label: 'Weekly', used: 58, known: true, resets_at: 1_000_000 + 3600, series: [[1_000_000 - 86400, 40], [1_000_000, 58]], burn: 3, active_burn: 6, empty_in: 14 * 3600, cycles: 2, hits: 1, avg_hours_to_empty: 50, fastest_hours_to_empty: 50 }] }],
  events: [{ t: 1_000_000 - 3600, kind: 'switch', provider: 'claude', account: 'a', name: 'work@<x>', to: 'b', to_name: 'home@example.com' },
    { t: 1_000_000 - 7200, kind: 'limit', window: 'Weekly', account: 'a', name: 'work@<x>' }],
};

test('limits: the pool, when it runs out, and the next reset', () => {
  const html = limitsHTML(report, { windows: {} }, {});
  const t = text(html);
  assert.match(t, /122% of 200% left/);
  assert.match(t, /Pool empty in 40h/);
  assert.match(t, /First empty in 14h work@<x>/);
  assert.match(t, /Next reset \+58% in 1h/);
  assert.match(t, /Burn, last 3h 3\.0%\/h/);
  assert.doesNotMatch(html, /work@<x>/);
  // The window chosen shows its own pool.
  assert.match(text(limitsHTML(report, { windows: { claude: 'Session' } }, {})), /150% of 200% left/);
  assert.match(poolChartHTML(report.pools[0], report), /class="ins-pool-proj"/);
  assert.match(poolChartHTML(report.pools[0], report), /\+58%/);
});

test('limits: accounts drain, and what happened', () => {
  const t = text(accountsHTML(report));
  assert.match(t, /work@<x> in use/);
  assert.match(t, /Weekly 42% left/);
  assert.match(t, /3\.0%\/h 6\.0%\/h in use 14h/);
  assert.match(t, /2d 2h 1 of 2 windows/);
  const ev = text(eventsHTML(report));
  assert.match(ev, /Switched Claude Code work@<x> → home@example.com/);
  assert.match(ev, /Reached its Weekly limit/);
  assert.match(text(eventsHTML({ ...report, events: [] })), /Nothing yet/);
});

test('the controls follow the view', () => {
  assert.match(controlsHTML({ view: 'usage', metric: 'tokens', range: 'all' }), /data-ins-range="all"/);
  const limits = controlsHTML({ view: 'limits', days: '7', burn: '3' });
  assert.match(limits, /data-ins-burn="3" class="active"/);
  assert.doesNotMatch(limits, /data-ins-range/);
});
