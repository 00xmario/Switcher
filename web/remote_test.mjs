import assert from 'node:assert/strict';
import test from 'node:test';
import { createRemote } from './remote.js';

const now = Date.parse('2026-10-07T12:00:00Z');
const host = (extra = {}) => ({ enabled: false, listening: false, name: 'Studio Mac', port: 8788, lan: ['studio.local'], tailscale: ['100.101.2.3'], devices: [], ...extra });

async function render(status, found) {
  const calls = [];
  const remote = createRemote({ api: async (path, options = {}) => { calls.push([path, options.method || 'GET']); return path === '/api/remote/discover' ? { hosts: found || [] } : status; }, now: () => now });
  await remote.load();
  return { html: remote.html(), calls };
}

test('an idle Mac can share itself or use another Switcher', async () => {
  const { html } = await render({ host: host(), client: { connected: false } });
  assert.match(html, /Share this Switcher/);
  assert.match(html, /Use another Switcher/);
  assert.match(html, /data-remote-form/);
});

test('a sharing Mac shows its addresses, pairing code and paired Macs', async () => {
  const { html } = await render({ host: host({ enabled: true, listening: true, pairing: { code: 'K7QF-M2XP', expires_at: '2026-10-07T12:09:30Z' },
    devices: [{ id: 'd1', name: 'MacBook Air', created_at: '2026-10-06T10:00:00Z', last_seen: '2026-10-07T11:55:00Z' }] }), client: { connected: false } });
  assert.match(html, /data-remote-copy="studio\.local"[^>]*>[\s\S]*?<code>studio\.local<\/code>/);
  assert.match(html, /title="Tailscale: click to copy">[\s\S]*?<code>100\.101\.2\.3<\/code>/);
  assert.match(html, /K7QF-M2XP/);
  assert.match(html, /Valid for 9:30/);
  assert.match(html, /MacBook Air<\/strong><span class="dim">last used 5 min ago/);
  assert.match(html, /data-remote-action="revoke" data-remote-id="d1"/);
  assert.doesNotMatch(html, /Use another Switcher/, 'a sharing Mac cannot also use another one');
});

test('a connected Mac shows its host and how to disconnect', async () => {
  const { html } = await render({ host: host(), client: { connected: true, host_name: 'Studio Mac', address: 'studio.local', reachable: false, error: 'the Switcher host is unreachable' } });
  assert.match(html, /Using Studio Mac/);
  assert.match(html, /Unreachable/);
  assert.match(html, /data-remote-action="disconnect"/);
  assert.doesNotMatch(html, /Share this Switcher/);
});

test('away from home: off, signing in, and running on Tailscale', async () => {
  const base = { host: host(), client: { connected: false } };
  let { html } = await render({ ...base, tailnet: { installed: false, enabled: false, running: false, peers: [] } });
  assert.match(html, /Away from home/);
  assert.match(html, /downloads a 19 MB add-on/);
  assert.doesNotMatch(html, /Remove add-on/);
  ({ html } = await render({ ...base, tailnet: { installed: true, enabled: true, running: true, state: 'NeedsLogin', auth_url: 'https://login.tailscale.com/a/abc', peers: [] } }));
  assert.match(html, /href="https:\/\/login\.tailscale\.com\/a\/abc"[^>]*>Sign in with Tailscale/);
  ({ html } = await render({ ...base, tailnet: { installed: true, enabled: true, running: true, state: 'Running', dns_name: 'switcher-studio.tail1234.ts.net', peers: [] } }));
  assert.match(html, /On Tailscale as <code>switcher-studio\.tail1234\.ts\.net<\/code>/);
  assert.match(html, /Remove add-on and sign out/);
});

test('an action error stays visible after the status reloads', async () => {
  const status = { host: host(), client: { connected: false } };
  const remote = createRemote({ api: async () => status, now: () => now });
  await remote.act('connect', async () => { throw new Error('pairing failed: check the code shown on the other Mac'); });
  assert.match(remote.html(), /role="alert">pairing failed: check the code/);
  await remote.load();
  assert.match(remote.html(), /pairing failed/, 'a status poll does not hide it');
  await remote.act('scan', async () => {});
  assert.doesNotMatch(remote.html(), /pairing failed/, 'the next action clears it');
});

test('a paired Mac can stop relying on its host and stay paired', async () => {
  const paired = { connected: true, host_name: 'Studio Mac', address: 'studio.local', reachable: true };
  let { html } = await render({ host: host(), client: paired });
  assert.match(html, /<input type="checkbox" data-remote-action="use-host" aria-label="Use the accounts of Studio Mac" checked/);
  assert.match(html, /Turn off if Studio Mac stops working/);
  ({ html } = await render({ host: host(), client: { ...paired, local_only: true, reachable: false, error: 'the Switcher host is unreachable' } }));
  assert.match(html, /Not in use/);
  assert.match(html, /Paired with Studio Mac/);
  assert.match(html, /data-remote-action="use-host" aria-label="Use the accounts of Studio Mac" (?!checked)/);
  assert.match(html, /uses its own accounts/);
  assert.doesNotMatch(html, /remote-error/, 'an unreachable host is no error while it is not in use');
  assert.match(html, /data-remote-action="disconnect"/, 'unpairing stays possible');
});

test('a Linux Switcher calls itself a computer', async () => {
  const { html } = await render({ platform: 'linux', host: host(), client: { connected: true, host_name: 'Studio Mac', address: 'studio.local', reachable: true, local_only: true } });
  assert.match(html, /this computer uses its own accounts/);
  assert.doesNotMatch(html, /this Mac uses its own accounts/);
});

test('a paired Mac shows its host\'s Switcher and offers to update it', async () => {
  const status = { host: host(), client: { connected: true, host_name: 'Studio <Mac>', address: 'studio.local', reachable: true } };
  let update = { host: 'Studio <Mac>', current: '1.21.0', latest: 'v1.22.0', update_available: true, remote_update: true };
  let clock = now;
  const remote = createRemote({ api: async path => path === '/api/remote/host-update' ? { host_update: update } : status, now: () => clock });
  await remote.load();
  let html = remote.html();
  assert.match(html, /Switcher on Studio &lt;Mac&gt;<\/strong><span class="dim">Switcher 1\.21\.0 · 1\.22\.0 is out/);
  assert.match(html, /data-remote-action="host-update"[^>]*>Update to 1\.22\.0/);

  update = { ...update, install: { step: 'downloading', fraction: 0.42 } };
  clock += 61000; // the host is asked at most once a minute unless it updates
  await remote.load();
  html = remote.html();
  assert.match(html, /Downloading 42%/);
  assert.match(html, /data-remote-action="host-update" disabled>Updating…/);

  update = { host: 'Studio <Mac>', current: '1.22.0', latest: 'v1.22.0', update_available: false, remote_update: true };
  await remote.load();
  assert.match(remote.html(), /up to date[^]*data-remote-action="host-update-check"/);
});

test('an older host is updated on it once; a Mac working without its host asks only on Check', async () => {
  const calls = [];
  let status = { host: host(), client: { connected: true, host_name: 'Studio', address: 'studio.local', reachable: true } };
  const update = { host: 'Studio', current: '1.19.0', update_available: false, remote_update: false };
  const remote = createRemote({ api: async path => { calls.push(path); return path === '/api/remote/host-update' ? { host_update: update } : status; }, now: () => now });
  await remote.load();
  assert.match(remote.html(), /Switcher 1\.19\.0\. Update it on Studio once/);
  assert.doesNotMatch(remote.html(), /data-remote-action="host-update"/);

  status = { host: host(), client: { connected: true, local_only: true, host_name: 'Studio', address: 'studio.local', reachable: false } };
  const local = createRemote({ api: async path => { calls.push(path); return path === '/api/remote/host-update' ? { host_update: update } : status; }, now: () => now });
  calls.length = 0;
  await local.load();
  assert.equal(calls.filter(p => p === '/api/remote/host-update').length, 0, 'no host request while working without it');
  assert.match(local.html(), /data-remote-action="host-update-check"/);
});
