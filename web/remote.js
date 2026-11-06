// Settings card for sharing accounts between Macs: share this Switcher with
// paired Macs, or use another Mac's Switcher.
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function ago(value, now) {
  const ms = Date.parse(value || '');
  if (!ms || ms < 0) return 'never';
  const minutes = Math.floor(Math.max(0, now - ms) / 60000);
  return minutes < 1 ? 'just now' : minutes < 60 ? `${minutes} min ago` : minutes < 1440 ? `${Math.floor(minutes / 60)} hr ago` : new Date(ms).toLocaleDateString();
}

function remaining(value, now) {
  const seconds = Math.max(0, Math.round((Date.parse(value || '') - now) / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

const HOME_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M4 11l8-6.5 8 6.5v8a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 19z"/><path d="M10 20.5v-5h4v5"/></svg>';
const NET_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="currentColor"><circle cx="6" cy="6" r="2"/><circle cx="12" cy="6" r="2"/><circle cx="18" cy="6" r="2"/><circle cx="6" cy="12" r="2" opacity=".35"/><circle cx="12" cy="12" r="2"/><circle cx="18" cy="12" r="2"/><circle cx="6" cy="18" r="2" opacity=".35"/><circle cx="12" cy="18" r="2" opacity=".35"/><circle cx="18" cy="18" r="2" opacity=".35"/></svg>';
const SHARE_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="M8.2 10.8l7.6-3.6M8.2 13.2l7.6 3.6"/></svg>';
const LINK_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/></svg>';

// The pairing code's countdown as a ring around the seconds left.
function ring(value, now, total = 600) {
  const seconds = Math.max(0, Math.round((Date.parse(value || '') - now) / 1000));
  const share = Math.min(1, seconds / total);
  return `<svg class="countdown" viewBox="0 0 36 36" aria-hidden="true"><circle cx="18" cy="18" r="15.5" pathLength="1"/><circle class="countdown-left" cx="18" cy="18" r="15.5" pathLength="1" style="stroke-dasharray:${share.toFixed(3)} 1"/></svg>`;
}

const MAC_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="5" width="16" height="10.5" rx="1.8"/><path d="M2 18.5h20"/></svg>';

// UPDATE_STEPS names a host's install steps.
const UPDATE_STEPS = { downloading: 'Downloading', checking: 'Checking', installing: 'Installing', restarting: 'Restarting', updating: 'Updating' };

export function createRemote({ api, confirm = async () => true, onConnectionChange = () => {}, now = () => Date.now() }) {
  // error belongs to the last action and stays until the next one; loadError
  // is the status poll's own.
  let view = null, status = null, found = null, scanning = false, busy = '', error = '', loadError = '', target = null, timer = null;
  // hostUpdate is the paired host's Switcher version and update, if known;
  // it is asked for apart from the status, at most once a minute unless an
  // update runs, so a slow host never holds up this page.
  let hostUpdate = null, hostFetchedAt = 0, hostFetching = false;

  async function load() {
    try {
      status = await api('/api/remote');
      loadError = '';
    } catch (e) {
      loadError = e.message || 'Could not load sharing status.';
    }
    render();
    await loadHostUpdate();
  }

  // loadHostUpdate asks the host about its Switcher while this Mac uses it
  // and reaches it; one working without its host asks only on Check.
  async function loadHostUpdate() {
    const client = status?.client;
    if (!client?.connected) {
      hostUpdate = null;
      return;
    }
    const updating = busy === 'host-update' || hostUpdate?.install;
    if (client.local_only || !client.reachable || hostFetching || (!updating && now() - hostFetchedAt < 60000)) return;
    hostFetching = true;
    try {
      hostUpdate = (await api('/api/remote/host-update')).host_update || null;
      hostFetchedAt = now();
    } catch { /* the host may be restarting; keep what was known */ } finally {
      hostFetching = false;
    }
    render();
  }

  async function act(kind, work) {
    if (busy) return false;
    busy = kind;
    error = '';
    render();
    try {
      await work();
      return true;
    } catch (e) {
      error = e.message || 'That did not work.';
      return false;
    } finally {
      busy = '';
      await load();
    }
  }

  async function scan() {
    if (scanning) return;
    scanning = true;
    render();
    try {
      found = (await api('/api/remote/discover')).hosts || [];
    } catch {
      found = [];
    } finally {
      scanning = false;
      render();
    }
  }

  const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
  // What this machine is called: a Linux Switcher is not on a Mac.
  const mac = () => status?.platform === 'linux' ? 'computer' : 'Mac';

  function hostHTML(host) {
    const chips = [...(host.lan || []).map(a => ({ a, icon: HOME_ICON, kind: 'Home network' })), ...(host.tailscale || []).map(a => ({ a, icon: NET_ICON, kind: 'Tailscale' }))];
    const tailnet = host.tailnet_pairing !== false;
    return `<div class="settings-card remote-section">
      <div class="settings-row set-row-icon">
        <span class="set-icon">${SHARE_ICON}</span>
        <div><strong>Share this Switcher</strong><span class="dim">Your other Macs use the accounts on ${escape(host.name)}</span></div>
        <label class="switch-wrap"><input type="checkbox" data-remote-action="share" aria-label="Share this Switcher with my other Macs" ${host.enabled ? 'checked' : ''} ${busy ? 'disabled' : ''}><span class="switch-visual"></span></label>
      </div>
      ${host.enabled ? `
        ${host.error ? `<p class="remote-error">${escape(host.error)}</p>` : ''}
        ${chips.length ? `<div class="set-chips" aria-label="Addresses of this ${mac()}">${chips.map(c => `<button type="button" class="set-chip" data-remote-copy="${escape(c.a)}" title="${c.kind}: click to copy">${c.icon}<code>${escape(c.a)}</code></button>`).join('')}</div>` : ''}
        <div class="settings-row">
          <div><strong>Let my own Tailscale devices connect without a code</strong><span class="dim">${tailnet
            ? `Your ${mac()}s signed in to the same Tailscale account connect from their list of Switchers. Everything else needs a code.`
            : `Off: every ${mac()} needs a code, also your own.`}</span></div>
          <label class="switch-wrap"><input type="checkbox" data-remote-action="tailnet-pairing" aria-label="Let my own Tailscale devices connect without a code" ${tailnet ? 'checked' : ''} ${busy ? 'disabled' : ''}><span class="switch-visual"></span></label>
        </div>
        ${host.pairing ? `<div class="remote-code" role="status">
            ${ring(host.pairing.expires_at, now())}
            <div><span class="remote-code-label">Enter this code on the other Mac</span>
            <strong class="remote-code-value">${escape(host.pairing.code)}</strong>
            <span class="remote-code-time">Valid for ${remaining(host.pairing.expires_at, now())}</span></div>
          </div>` : `<div><button type="button" class="primary" data-remote-action="pair" ${busy ? 'disabled' : ''}>${LINK_ICON}Pair a Mac</button></div>`}
        ${host.devices.length ? `<ul class="remote-devices">${host.devices.map(d => `<li>
            <span class="set-icon">${MAC_ICON}</span>
            <span><strong>${escape(d.name)}</strong><span class="dim"${d.tailscale ? ` title="Paired via Tailscale as ${escape(d.tailscale)}"` : ''}>${d.tailscale ? 'Paired via Tailscale · ' : ''}last used ${escape(ago(d.last_seen, now()))}</span></span>
            <button type="button" class="quiet danger" data-remote-action="revoke" data-remote-id="${escape(d.id)}" ${busy ? 'disabled' : ''}>Remove</button>
          </li>`).join('')}</ul>` : `<p class="set-foot">No Macs paired yet. Pair one with a code${tailnet ? ', or connect from your own Tailscale devices' : ''}; it stays paired until you remove it.</p>`}
        <p class="set-foot">While this ${mac()} shares, it cannot use another Switcher. Turn off sharing to use one.</p>` : ''}
    </div>`;
  }

  // foundHTML lists Switchers to connect to, or, while connected, to use
  // instead of the current host.
  function foundHTML(hosts, action, label) {
    return `<ul class="remote-found">${hosts.map(h => `<li>
        <span><strong>${escape(h.name)}</strong><span class="dim">${escape(h.address)}</span></span>
        <button type="button" data-remote-action="${action}" data-remote-address="${escape(h.address)}" data-remote-port="${escape(h.port)}" data-remote-name="${escape(h.name)}"${h.tailnet ? ` data-remote-tailnet="${escape(h.tailnet)}"` : ''} ${busy ? 'disabled' : ''}>${busy === 'connect' && target?.address === h.address ? 'Connecting…' : label}</button>
      </li>`).join('')}</ul>`;
  }

  // codeFormHTML pairs by address, with a code unless the host is one of
  // the user's own Tailscale devices.
  function codeFormHTML() {
    const name = target ? escape(target.name) : '';
    return `<form class="remote-connect" data-remote-form>
        <input name="address" placeholder="Address, e.g. marios-mac.local or 100.101.2.3" autocomplete="off" value="${escape(target?.address || '')}" ${busy ? 'disabled' : ''} required>
        <input name="code" placeholder="Code, e.g. K7QF-M2XP" autocomplete="off" autocapitalize="characters" spellcheck="false" maxlength="9" ${busy ? 'disabled' : ''} ${target?.needsCode ? 'required' : ''}>
        <button type="submit" class="primary" ${busy ? 'disabled' : ''}>${busy === 'connect' ? 'Connecting…' : target?.switching ? `Use ${name}` : target ? `Connect to ${name}` : 'Connect'}</button>
      </form>
      <p class="set-foot">${target?.needsCode ? `Enter the code shown on ${name} (Settings → Share between Macs → Pair a Mac).` : 'Your own devices on Tailscale connect without a code.'}</p>`;
  }

  // hostUpdateHTML offers to update the paired host's Switcher from here.
  function hostUpdateHTML(name) {
    const u = hostUpdate;
    if (!u) {
      // Working without the host: it is asked only when you check.
      return status?.client?.local_only ? `<div class="settings-row">
          <div><strong>Switcher on ${name}</strong><span class="dim">Check whether an update is out for it</span></div>
          <button type="button" data-remote-action="host-update-check" ${busy ? 'disabled' : ''}>${busy === 'host-update-check' ? 'Checking…' : 'Check'}</button>
        </div>` : '';
    }
    const latest = escape((u.latest || '').replace(/^v/, ''));
    const version = u.current ? `Switcher ${escape(u.current)}` : 'Switcher';
    if (!u.remote_update) {
      return `<div class="settings-row">
          <div><strong>Switcher on ${name}</strong><span class="dim">${version}. Update it on ${name} once; after that you can update it from here.</span></div>
        </div>`;
    }
    const progress = u.install ? ` · ${escape(UPDATE_STEPS[u.install.step] || 'Updating')}${u.install.step === 'downloading' ? ` ${Math.round((u.install.fraction || 0) * 100)}%` : ''}` : '';
    return `<div class="settings-row">
        <div><strong>Switcher on ${name}</strong><span class="dim">${version}${u.update_available ? ` · ${latest} is out` : ' · up to date'}${progress}</span></div>
        ${u.update_available
          ? `<button type="button" class="primary" data-remote-action="host-update" ${busy || u.install ? 'disabled' : ''}>${busy === 'host-update' || u.install ? 'Updating…' : `Update to ${latest}`}</button>`
          : `<button type="button" data-remote-action="host-update-check" ${busy ? 'disabled' : ''}>${busy === 'host-update-check' ? 'Checking…' : 'Check'}</button>`}
      </div>`;
  }

  function clientHTML(client) {
    if (client.connected) {
      const local = !!client.local_only, name = escape(client.host_name);
      const state = local ? ['warn', 'Not in use'] : client.reachable ? ['ok', 'Connected'] : ['warn', 'Unreachable'];
      return `<div class="settings-card remote-section">
        <div class="remote-connected">
          <span class="dr-status ${state[0]}">${state[1]}</span>
          <div><strong>${local ? 'Paired with' : 'Using'} ${name}</strong><span class="dim">via ${escape(client.address)}</span></div>
          <button type="button" data-remote-action="disconnect" ${busy ? 'disabled' : ''}>${busy === 'disconnect' ? 'Disconnecting…' : 'Disconnect'}</button>
        </div>
        <div class="settings-row">
          <div><strong>Use the accounts of ${name}</strong><span class="dim">${local
            ? `Off: this ${mac()} uses its own accounts and stays paired. Turn on to use ${name} again.`
            : `Turn off if ${name} stops working: this ${mac()} then works on its own and stays paired.`}</span></div>
          <label class="switch-wrap"><input type="checkbox" data-remote-action="use-host" aria-label="Use the accounts of ${name}" ${local ? '' : 'checked'} ${busy ? 'disabled' : ''}><span class="switch-visual"></span></label>
        </div>
        ${hostUpdateHTML(name)}
        ${!local && !client.reachable && client.error ? `<p class="remote-error">${escape(client.error)}</p>` : ''}
        <p class="settings-sub">${local
          ? `Nothing goes to ${name} now: this ${mac()}'s own accounts, usage checks, Codex and Claude routing all run here.`
          : `Accounts, usage, Codex, the T3 Code hub, and Claude Code and Claude Desktop (once connected below) all run through ${name}. Requests leave from that ${mac()}.`}</p>
      </div>
      ${switchHTML(name)}`;
    }
    const hosts = found || [];
    return `<div class="settings-card remote-section">
      <div class="settings-row set-row-icon"><span class="set-icon">${LINK_ICON}</span><div><strong>Use another Switcher</strong><span class="dim">Run on the accounts of another Mac</span></div>
        <button type="button" data-remote-action="scan" ${scanning || busy ? 'disabled' : ''}>${scanning ? 'Looking…' : 'Look again'}</button></div>
      ${hosts.length ? foundHTML(hosts, 'connect-found', 'Connect') : `<p class="settings-sub">${scanning ? 'Looking for Switchers on your network…' : found ? 'No shared Switcher found on this network. Turn on sharing on the other Mac, or enter its address (for example its Tailscale name).' : ''}</p>`}
      ${codeFormHTML()}
    </div>`;
  }

  // switchHTML offers the other Switchers found, to use one instead of the
  // current host. The current host stays until the new one is paired.
  function switchHTML(name) {
    const others = (found || []).filter(h => !h.current);
    return `<div class="settings-card remote-section">
      <div class="settings-row set-row-icon"><span class="set-icon">${LINK_ICON}</span><div><strong>Other Switchers</strong><span class="dim">Use another one instead of ${name}</span></div>
        <button type="button" data-remote-action="scan" ${scanning || busy ? 'disabled' : ''}>${scanning ? 'Looking…' : 'Look again'}</button></div>
      ${others.length ? foundHTML(others, 'switch', 'Use this one') : `<p class="settings-sub">${scanning ? 'Looking for Switchers…' : found ? 'No other shared Switcher found.' : ''}</p>`}
      ${target?.switching && target.needsCode ? codeFormHTML() : ''}
    </div>`;
  }

  // "Away from home": the optional Tailscale add-on, downloaded on demand.
  function tailnetHTML(t) {
    if (!t) return '';
    const on = t.enabled || t.downloading;
    let detail = '';
    if (t.downloading) detail = '<p class="settings-sub">Downloading the Tailscale add-on…</p>';
    else if (t.enabled && t.error) detail = `<p class="remote-error">${escape(t.error)}</p>`;
    else if (t.enabled && !t.running) detail = '<p class="settings-sub">Starting…</p>';
    else if (t.enabled && t.state === 'NeedsLogin' && /^https:\/\//.test(t.auth_url || '')) detail = `<div class="remote-tailnet-login">
        <p class="settings-sub">Sign in once to add this ${mac()} to your Tailscale network. Use the same Tailscale account on your other Macs.</p>
        <a class="button primary" href="${escape(t.auth_url)}" target="_blank" rel="noopener noreferrer">Sign in with Tailscale</a>
      </div>`;
    else if (t.enabled && t.state === 'Running') detail = `<p class="set-live"><span class="set-live-dot"></span>On Tailscale as <code>${escape(t.dns_name || t.name)}</code>${t.tailnet ? ` in ${escape(t.tailnet)}` : ''}. Your Macs find each other from anywhere.</p>`;
    else if (t.enabled) detail = '<p class="settings-sub">Connecting to Tailscale…</p>';
    return `<div class="settings-card remote-section remote-tailnet">
      <div class="settings-row set-row-icon">
        <span class="set-icon">${NET_ICON}</span>
        <div><strong>Away from home</strong><span class="dim">Reach your Macs over Tailscale from anywhere${t.installed ? '' : '; downloads a 19 MB add-on'}</span></div>
        <label class="switch-wrap"><input type="checkbox" data-remote-action="tailnet" aria-label="Use Switcher away from home with Tailscale" ${on ? 'checked' : ''} ${busy || t.downloading ? 'disabled' : ''}><span class="switch-visual"></span></label>
      </div>
      ${detail}
      ${t.installed && !t.downloading ? `<div class="set-foot-actions"><button type="button" class="quiet danger" data-remote-action="tailnet-remove" ${busy ? 'disabled' : ''}>Remove add-on and sign out</button></div>` : ''}
    </div>`;
  }

  // heroHTML draws which Mac keeps the accounts and which use them.
  function heroHTML() {
    const { host, client } = status;
    const me = escape(status.device_name || host?.name || 'This Mac');
    const tailnet = status.tailnet?.state === 'Running' ? '<em class="flow-tag">Tailscale</em>' : '';
    const node = (name, sub, keeps) => `<div class="flow-node ${keeps ? 'is-host' : ''}"><span class="flow-icon">${MAC_ICON}</span><strong>${name}</strong><small>${sub}</small></div>`;
    let flow, pill;
    if (client.connected && client.local_only) {
      flow = `<div class="flow is-off" aria-hidden="true">${node(me, 'uses its own accounts', true)}<div class="flow-wire"><span></span></div>${node(escape(client.host_name), 'paired, not in use')}</div>`;
      pill = '<span class="dr-status warn">Not in use</span>';
    } else if (client.connected) {
      flow = `<div class="flow ${client.reachable ? 'is-on' : 'is-warn'}" aria-hidden="true">${node(me, `this ${mac()}`)}<div class="flow-wire"><span></span>${tailnet}</div>${node(escape(client.host_name), 'keeps the accounts', true)}</div>`;
      pill = client.reachable ? '<span class="dr-status ok">Connected</span>' : '<span class="dr-status warn">Unreachable</span>';
    } else if (host?.enabled) {
      const n = host.devices.length;
      flow = `<div class="flow is-on" aria-hidden="true">${node(n ? `${n} paired Mac${n === 1 ? '' : 's'}` : 'Your other Macs', n ? 'use these accounts' : 'pair one below')}<div class="flow-wire"><span></span>${tailnet}</div>${node(escape(host.name || status.device_name || `This ${mac()}`), `this ${mac()} keeps the accounts`, true)}</div>`;
      pill = '<span class="dr-status ok">Sharing</span>';
    } else {
      flow = `<div class="flow is-off" aria-hidden="true">${node(me, `this ${mac()}`)}<div class="flow-wire"><span></span></div>${node('Your other Macs', 'not connected')}</div>`;
      pill = '<span class="dr-status warn">Off</span>';
    }
    return `<div class="settings-card set-hero remote-hero">
      <div class="set-hero-status">${pill}</div>
      ${flow}
      <p class="set-hero-lead">One Mac keeps the accounts and tokens; your other Macs use them through it. Requests to Claude and Codex leave from that Mac.</p>
    </div>`;
  }

  function html() {
    if (!status) return `<div class="settings-card"><p class="settings-sub">${escape(error || loadError || 'Loading…')}</p></div>`;
    const { host, client } = status;
    return `${heroHTML()}
      ${error || loadError ? `<p class="remote-error" role="alert">${escape(error || loadError)}</p>` : ''}
      ${client.connected ? clientHTML(client) : `${host ? hostHTML(host) : ''}${host?.enabled ? '' : clientHTML(client)}`}
      ${tailnetHTML(status.tailnet)}`;
  }

  let shown = '';
  function render() {
    if (!view) return;
    // The status poll often changes nothing; leave the card and its inputs alone.
    const markup = html();
    if (markup === shown) return;
    shown = markup;
    const focused = view.contains(document.activeElement) ? document.activeElement : null;
    const keep = focused?.name ? { name: focused.name, value: focused.value } : null;
    const values = Object.fromEntries([...view.querySelectorAll('[data-remote-form] input')].map(i => [i.name, i.value]));
    view.innerHTML = markup;
    for (const [name, value] of Object.entries(values)) {
      const input = view.querySelector(`[data-remote-form] input[name="${name}"]`);
      if (input && value) input.value = value;
    }
    if (keep) view.querySelector(`[name="${keep.name}"]`)?.focus({ preventScroll: true });
  }

  function bind(root) {
    if (root.dataset.remoteBound) return;
    root.dataset.remoteBound = '1';
    root.addEventListener('change', event => {
      if (event.target.matches('[data-remote-action="share"]')) {
        const enabled = event.target.checked;
        act('share', () => post('/api/remote/host', { enabled }));
      }
      if (event.target.matches('[data-remote-action="tailnet"]')) {
        const enabled = event.target.checked;
        act('tailnet', () => post('/api/remote/tailnet', { enabled }));
      }
      if (event.target.matches('[data-remote-action="tailnet-pairing"]')) {
        const enabled = event.target.checked;
        act('tailnet-pairing', () => post('/api/remote/host/tailnet', { enabled }));
      }
      if (event.target.matches('[data-remote-action="use-host"]')) {
        const enabled = event.target.checked;
        act('use-host', () => post('/api/remote/use-host', { enabled })).then(ok => { if (ok) onConnectionChange(enabled); });
      }
    });
    root.addEventListener('click', async event => {
      const chip = event.target.closest('[data-remote-copy]');
      if (chip) {
        navigator.clipboard?.writeText(chip.dataset.remoteCopy).then(() => {
          chip.classList.add('is-copied');
          setTimeout(() => chip.classList.remove('is-copied'), 1200);
        }, () => {});
        return;
      }
      const button = event.target.closest('button[data-remote-action]');
      if (!button || button.disabled) return;
      switch (button.dataset.remoteAction) {
        case 'pair': return act('pair', () => post('/api/remote/host/pairing'));
        case 'scan': return scan();
        case 'revoke':
          if (!await confirm({ title: 'Remove this Mac?', message: 'It can no longer use this Switcher until you pair it again.', confirmLabel: 'Remove', danger: true })) return;
          return act('revoke', () => api(`/api/remote/host/devices/${encodeURIComponent(button.dataset.remoteId)}`, { method: 'DELETE' }));
        case 'connect-found':
          return connect(chosen(button));
        case 'switch':
          return switchTo(chosen(button));
        case 'tailnet-remove':
          if (!await confirm({ title: 'Remove the Tailscale add-on?', message: `This ${mac()} signs out of Tailscale and can only reach your other Macs at home again.`, confirmLabel: 'Remove', danger: true })) return;
          return act('tailnet-remove', () => post('/api/remote/tailnet/remove'));
        case 'host-update-check':
          return act('host-update-check', async () => {
            hostUpdate = (await post('/api/remote/host-update', { action: 'check' })).host_update || hostUpdate;
            hostFetchedAt = now();
          });
        case 'host-update': {
          const name = status?.client?.host_name || 'the host';
          const latest = (hostUpdate?.latest || '').replace(/^v/, '');
          if (!await confirm({ title: `Update ${name} to Switcher ${latest}?`, confirmLabel: 'Update',
            message: `${name} downloads the release, installs it and restarts. Its tools, and every computer that uses it, pause for a moment. Nothing changes on this ${mac()}.` })) return;
          return act('host-update', () => post('/api/remote/host-update', { action: 'install' }));
        }
        case 'disconnect':
          if (!await confirm({ title: `Stop using ${status?.client?.host_name || 'the other Mac'}?`, message: `This ${mac()} goes back to its own accounts.`, confirmLabel: 'Disconnect' })) return;
          if (await act('disconnect', () => post('/api/remote/disconnect'))) onConnectionChange(false);
          return;
      }
    });
    root.addEventListener('submit', async event => {
      if (!event.target.matches('[data-remote-form]')) return;
      event.preventDefault();
      const form = event.target.elements;
      const address = form.address.value.trim();
      const next = target && target.address === address ? target : { address, port: 0, name: address };
      await connect(next, form.code.value.trim());
    });
  }

  const chosen = button => ({ address: button.dataset.remoteAddress, port: Number(button.dataset.remotePort) || 0, name: button.dataset.remoteName,
    ...(button.dataset.remoteTailnet ? { tailnet: button.dataset.remoteTailnet } : {}) });

  // connect pairs with a host: without a code first, which works for the
  // user's own Tailscale devices, and with a code once the host asks for
  // one. A host found nearby that is in the tailnet too is tried over
  // Tailscale for that, and over its local address with a code. Switching
  // keeps the current host until the new one is paired.
  async function connect(next, code = '') {
    target = { ...next, needsCode: !!(code || next.needsCode) };
    const [address, port] = !code && next.tailnet ? [next.tailnet, 0] : [next.address, next.port || 0];
    let needsCode = false;
    const ok = await act('connect', async () => {
      try {
        await post('/api/remote/connect', { address, port, ...(code ? { code } : {}), ...(next.switching ? { switch: true } : {}) });
      } catch (e) {
        needsCode = !!e.body?.needs_code;
        throw e;
      }
    });
    if (ok) {
      target = null;
      render();
      onConnectionChange(true);
      return true;
    }
    if (needsCode) {
      target = { ...next, needsCode: true };
      render();
      view?.querySelector('[data-remote-form] input[name="code"]')?.focus();
    }
    return false;
  }

  // switchTo uses another host instead of the current one, once confirmed.
  async function switchTo(next) {
    const current = status?.client?.host_name || 'the current host';
    if (!await confirm({ title: `Use ${next.name} instead of ${current}?`, confirmLabel: `Use ${next.name}`,
      message: `This ${mac()} connects to ${next.name} first and stops using ${current} only once that works, so it never ends up with neither. If ${next.name} asks for a code, you can enter it next.` })) return false;
    return connect({ ...next, switching: true });
  }

  function mount(root) {
    view = root;
    shown = '';
    error = '';
    bind(root);
    render();
    // Connected or not, the Switchers around are listed: to connect to one,
    // or to use one instead of the current host. A Mac that shares uses none.
    load().then(() => { if (!status?.host?.enabled && found === null) scan(); });
    clearInterval(timer);
    timer = setInterval(() => {
      if (!view?.isConnected) { clearInterval(timer); timer = null; return; }
      const t = status?.tailnet;
      if (status?.host?.pairing || status?.host?.enabled || status?.client?.connected || t?.downloading || (t?.enabled && t?.state !== 'Running')) load();
    }, 3000);
  }

  return { mount, html, load, act, scan, connect, switchTo };
}
