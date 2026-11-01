import assert from 'node:assert/strict';
import test from 'node:test';
import { autostartHTML, createHandback, handbackBannerHTML, machineWord, remoteBannerHTML, stepsHTML } from './toggles.js';

test('the banner offers to work without an unreachable host', () => {
  const offline = remoteBannerHTML({ role: 'client', host: 'Studio Mac', connected: false });
  assert.match(offline, /Can't reach <strong>Studio Mac<\/strong>/);
  assert.match(offline, /data-use-host="off"/);
  const online = remoteBannerHTML({ role: 'client', host: 'Studio Mac', connected: true });
  assert.match(online, /Using the accounts on <strong>Studio Mac<\/strong>/);
  assert.doesNotMatch(online, /data-use-host/);
  assert.equal(remoteBannerHTML(undefined), '');
});

test('a host not in use can be used again from the banner', () => {
  const local = remoteBannerHTML({ role: 'local', host: 'Studio <Mac>' }, 'linux');
  assert.match(local, /Paired with <strong>Studio &lt;Mac&gt;<\/strong>, using this computer's own accounts/);
  assert.match(local, /data-use-host="on"/);
  assert.equal(machineWord('darwin'), 'Mac');
});

test('handed back tools are announced until Switcher is set up again', () => {
  assert.equal(handbackBannerHTML(false), '');
  assert.match(handbackBannerHTML(true), /data-handback="off"/);
});

test('each tool says what happened to it', () => {
  const html = stepsHTML([
    { tool: 'codex', outcome: 'done', message: 'Codex uses its previous provider again.' },
    { tool: 'claude-desktop', outcome: 'failed', message: 'Desktop settings <changed>' },
  ]);
  assert.match(html, /class="is-done"[\s\S]*✓[\s\S]*<strong>Codex<\/strong> Codex uses its previous provider again\./);
  assert.match(html, /class="is-failed"[\s\S]*✗[\s\S]*Claude Desktop<\/strong> Desktop settings &lt;changed&gt;/);
  assert.equal(stepsHTML([]), '');
});

function handbackAPI(initial) {
  const calls = [];
  let status = initial;
  const api = async (path, options = {}) => {
    calls.push([path, options.method || 'GET', options.body]);
    if (options.method === 'POST') {
      const on = JSON.parse(options.body).enabled;
      status = { handed_back: on, steps: [{ tool: 'switcher', outcome: 'done', message: on ? 'Switcher no longer uses its accounts.' : 'Switcher uses its accounts again.' }] };
      return { handback: status };
    }
    return status;
  };
  return { api, calls };
}

test('handing back asks first and reports the steps', async () => {
  const { api, calls } = handbackAPI({ handed_back: false, steps: [] });
  let asked = 0;
  const changes = [];
  const card = createHandback({ api, confirm: async () => { asked++; return asked > 1; }, onChange: on => changes.push(on) });
  await card.load();
  assert.match(card.html(), /Before you remove Switcher/);
  assert.match(card.html(), /data-handback-switch[^>]*aria-label="Hand every tool back to its provider" (?!checked)/);

  assert.equal(await card.set(true), false, 'declining the question changes nothing');
  assert.equal(calls.filter(c => c[1] === 'POST').length, 0);

  assert.equal(await card.set(true), true);
  assert.deepEqual(calls.find(c => c[1] === 'POST'), ['/api/handback', 'POST', '{"enabled":true}']);
  assert.match(card.html(), /checked/);
  assert.match(card.html(), /Switcher no longer uses its accounts/);
  assert.match(card.html(), /until you restart it/);

  assert.equal(await card.set(false), true, 'setting up again needs no question');
  assert.equal(asked, 2);
  assert.deepEqual(changes, [true, false]);
});

test('a refused hand-back shows why and stays off', async () => {
  const api = async (path, options = {}) => {
    if (options.method === 'POST') throw new Error("codex: Codex's config.toml changed outside Switcher; nothing was handed back");
    return { handed_back: false, steps: [] };
  };
  const card = createHandback({ api });
  await card.load();
  assert.equal(await card.set(true), false);
  assert.match(card.html(), /role="alert">codex: Codex&#39;s config.toml changed outside Switcher/);
  assert.doesNotMatch(card.html(), /aria-label="Hand every tool back to its provider" checked/);
});

test('an interrupted hand-back says how to finish it', async () => {
  const card = createHandback({ api: async () => ({ handed_back: true, incomplete: true, steps: [] }) });
  await card.load();
  assert.match(card.html(), /Stopped halfway/);
});

test('start at login shows only where Switcher manages it', () => {
  assert.equal(autostartHTML({ supported: false, message: 'On a Mac, Start at login is in the Switcher menu.' }), '');
  const on = autostartHTML({ supported: true, enabled: true, service: true });
  assert.match(on, /id="autostart"[^>]*checked/);
  assert.match(on, /switcher\.service/);
  assert.match(autostartHTML({ supported: true, enabled: true, tray: true, message: 'Switcher starts with your desktop session' }), /With your desktop session/);
});
