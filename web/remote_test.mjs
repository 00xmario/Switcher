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

// remoteAPI answers like the server: connecting is refused with needs_code
// while refuse is set.
function remoteAPI(status, hosts) {
  const calls = [];
  const state = { refuse: false, status };
  const api = async (path, options = {}) => {
    calls.push({ path, body: options.body ? JSON.parse(options.body) : null });
    if (path === '/api/remote/discover') return { hosts };
    if (path === '/api/remote/connect') {
      if (state.refuse) {
        const e = new Error(`${calls.at(-1).body.address} did not let this computer in without a code. Enter the code shown on it.`);
        e.status = 400;
        e.body = { error: e.message, needs_code: true };
        throw e;
      }
      return { client: { connected: true } };
    }
    return state.status;
  };
  return { api, calls, state, connects: () => calls.filter(c => c.path === '/api/remote/connect').map(c => c.body) };
}

const studioPeer = { name: 'switcher-studio (Tailscale)', address: 'switcher-studio.tail1.ts.net', port: 8788, tailscale: true };

test('a Switcher found over Tailscale connects without a code and asks for one only when refused', async () => {
  const server = remoteAPI({ host: host(), client: { connected: false } }, [studioPeer]);
  const changes = [];
  const remote = createRemote({ api: server.api, onConnectionChange: c => changes.push(c), now: () => now });
  await remote.load();
  await remote.scan();
  assert.match(remote.html(), /data-remote-action="connect-found" data-remote-address="switcher-studio\.tail1\.ts\.net" data-remote-port="8788"[^>]*>Connect</);
  assert.match(remote.html(), /Your own devices on Tailscale connect without a code/);
  assert.doesNotMatch(remote.html(), /name="code"[^>]*required/, 'the code is optional until a host asks for one');

  server.state.refuse = true;
  assert.equal(await remote.connect(studioPeer), false);
  assert.deepEqual(server.connects()[0], { address: 'switcher-studio.tail1.ts.net', port: 8788 }, 'the first try sends no code');
  let html = remote.html();
  assert.match(html, /role="alert">switcher-studio\.tail1\.ts\.net did not let this computer in without a code/);
  assert.match(html, /Enter the code shown on switcher-studio \(Tailscale\)/);
  assert.match(html, /name="code"[^>]*required/);
  assert.match(html, /value="switcher-studio\.tail1\.ts\.net"/);
  assert.match(html, />Connect to switcher-studio \(Tailscale\)</);
  assert.deepEqual(changes, []);

  server.state.refuse = false;
  assert.equal(await remote.connect({ ...studioPeer, needsCode: true }, 'K7QF-M2XP'), true);
  assert.deepEqual(server.connects()[1], { address: 'switcher-studio.tail1.ts.net', port: 8788, code: 'K7QF-M2XP' });
  assert.deepEqual(changes, [true]);
});

test('a connected Mac offers its other Switchers and switches only once confirmed', async () => {
  const server = remoteAPI({ host: host(), client: { connected: true, host_name: 'Studio', address: 'studio.local', reachable: true } },
    [{ name: 'Studio', address: 'studio.local', port: 8788, current: true }, { ...studioPeer, name: 'switcher-mini (Tailscale)', address: 'switcher-mini.tail1.ts.net' }]);
  const asked = [];
  let answer = false;
  const remote = createRemote({ api: server.api, confirm: async o => { asked.push(o); return answer; }, now: () => now });
  await remote.load();
  await remote.scan();
  const html = remote.html();
  assert.match(html, /Other Switchers/);
  assert.match(html, /data-remote-action="switch" data-remote-address="switcher-mini\.tail1\.ts\.net"[^>]*>Use this one</);
  assert.doesNotMatch(html, /data-remote-action="switch" data-remote-address="studio\.local"/, 'the host in use is not offered');

  const mini = { name: 'switcher-mini (Tailscale)', address: 'switcher-mini.tail1.ts.net', port: 8788 };
  assert.equal(await remote.switchTo(mini), false);
  assert.equal(asked[0].title, 'Use switcher-mini (Tailscale) instead of Studio?');
  assert.match(asked[0].message, /connects to switcher-mini \(Tailscale\) first and stops using Studio only once that works/);
  assert.deepEqual(server.connects(), [], 'nothing happens without confirming');

  answer = true;
  server.state.refuse = true;
  assert.equal(await remote.switchTo(mini), false);
  assert.deepEqual(server.connects()[0], { address: 'switcher-mini.tail1.ts.net', port: 8788, switch: true });
  assert.match(remote.html(), /data-remote-form[^]*Use switcher-mini \(Tailscale\)/, 'a code is asked for, to switch with');
  server.state.refuse = false;
  assert.equal(await remote.connect({ ...mini, switching: true, needsCode: true }, 'K7QF-M2XP'), true);
  assert.deepEqual(server.connects()[1], { address: 'switcher-mini.tail1.ts.net', port: 8788, code: 'K7QF-M2XP', switch: true });
});

test('a sharing Mac lets its own Tailscale devices in without a code and says why it uses no other host', async () => {
  const server = remoteAPI({ host: host({ enabled: true, listening: true, tailnet_pairing: true,
    devices: [{ id: 'd1', name: 'MacBook Air', tailscale: 'switcher-air.tail1.ts.net', last_seen: '2026-10-07T11:55:00Z' }, { id: 'd2', name: 'Mini' }] }), client: { connected: false } }, []);
  const remote = createRemote({ api: server.api, now: () => now });
  await remote.load();
  let html = remote.html();
  assert.match(html, /data-remote-action="tailnet-pairing" aria-label="Let my own Tailscale devices connect without a code" checked/);
  assert.match(html, /title="Paired via Tailscale as switcher-air\.tail1\.ts\.net">Paired via Tailscale · last used 5 min ago/);
  assert.doesNotMatch(html, /Mini<\/strong><span class="dim"[^>]*>Paired via Tailscale/);
  assert.match(html, /While this Mac shares, it cannot use another Switcher/);
  server.state.status = { host: host({ enabled: true, tailnet_pairing: false }), client: { connected: false } };
  await remote.load();
  html = remote.html();
  assert.match(html, /data-remote-action="tailnet-pairing" aria-label="Let my own Tailscale devices connect without a code" (?!checked)/);
  assert.match(html, /Off: every Mac needs a code, also your own/);
});
