// Two switches that change what Switcher relies on, and Start at login:
//   - working without the paired host (the banner on the Accounts tab),
//   - handing every tool back to its provider (Settings → Tools),
//   - starting at login on Linux (Settings → General; the Mac's menu bar app
//     has its own).
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// machineWord is what this Switcher's computer is called.
export const machineWord = platform => platform === 'linux' ? 'computer' : 'Mac';

// remoteBannerHTML says whose accounts this Switcher uses. An unreachable
// host offers to work without it; a host not in use offers to use it again.
export function remoteBannerHTML(remote, platform) {
  const word = machineWord(platform);
  if (remote?.role === 'local') {
    return `
    <div class="remote-banner local">
      <span class="remote-banner-dot" aria-hidden="true"></span>
      <span>Paired with <strong>${escape(remote.host)}</strong>, using this ${word}'s own accounts.</span>
      <button type="button" data-use-host="on">Use ${escape(remote.host)} again</button>
    </div>`;
  }
  if (remote?.role !== 'client') return '';
  return `
    <div class="remote-banner ${remote.connected ? '' : 'offline'}">
      <span class="remote-banner-dot" aria-hidden="true"></span>
      <span>${remote.connected
        ? `Using the accounts on <strong>${escape(remote.host)}</strong>`
        : `Can't reach <strong>${escape(remote.host)}</strong>. Showing its last known accounts; requests fail until it is back.`}</span>
      ${remote.connected ? '' : `<button type="button" data-use-host="off">Work without it</button>`}
      <button type="button" data-open-settings>Settings</button>
    </div>`;
}

// handbackBannerHTML stays on the Accounts tab while the tools are handed back.
export function handbackBannerHTML(handedBack) {
  if (!handedBack) return '';
  return `
    <div class="remote-banner handback">
      <span class="remote-banner-dot" aria-hidden="true"></span>
      <span>Your tools are handed back to their providers. Switcher uses none of its accounts until you set it up again.</span>
      <button type="button" data-handback="off">Set up again</button>
    </div>`;
}

const STEP_MARKS = { done: '✓', unchanged: '·', skipped: '–', failed: '✗' };
const TOOL_NAMES = { 'claude-code': 'Claude Code', codex: 'Codex', 'codex-login': 'Codex login', 'claude-desktop': 'Claude Desktop',
  sharing: 'Sharing', host: 'Paired host', switcher: 'Switcher' };

export function stepsHTML(steps) {
  if (!steps?.length) return '';
  return `<ul class="handback-steps">${steps.map(s => `<li class="is-${escape(s.outcome)}">
      <span class="handback-mark" aria-hidden="true">${STEP_MARKS[s.outcome] || '·'}</span>
      <span><strong>${escape(TOOL_NAMES[s.tool] || s.tool)}</strong> ${escape(s.message)}</span>
    </li>`).join('')}</ul>`;
}

// createHandback is the Settings card for handing the tools back.
export function createHandback({ api, confirm = async () => true, toast = () => {}, onChange = () => {} }) {
  let view = null, status = null, busy = false, error = '';
  const post = body => api('/api/handback', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

  async function load() {
    try {
      status = await api('/api/handback');
    } catch (e) {
      error = e.message || 'Could not load the hand-back status.';
    }
    render();
  }

  function html() {
    const on = !!status?.handed_back;
    const sub = on
      ? 'Codex, Claude Code and Claude Desktop use their providers directly, with their own logins. Switcher uses none of its accounts: nothing is routed, refreshed or switched. You can remove Switcher now, or set it up again.'
      : 'Before you remove Switcher: undo its setup so Codex, Claude Code and Claude Desktop talk to OpenAI and Anthropic directly and keep working without it. Turning this off sets everything up again.';
    return `<div class="settings-card handback-card">
      <div class="set-card-head"><h3>Hand back to the providers</h3><p class="settings-sub">${sub}</p></div>
      <div class="settings-row">
        <div><strong>Hand back</strong><span class="dim">${status?.incomplete ? 'Stopped halfway; turn it off to set Switcher up again, then hand back again' : on ? 'On' : 'Off'}</span></div>
        <label class="switch-wrap"><input type="checkbox" data-handback-switch aria-label="Hand every tool back to its provider" ${on ? 'checked' : ''} ${busy || !status ? 'disabled' : ''}><span class="switch-visual"></span></label>
      </div>
      ${busy ? `<p class="settings-sub" role="status">${on ? 'Setting Switcher up again…' : 'Handing your tools back…'}</p>` : ''}
      ${error ? `<p class="remote-error" role="alert">${escape(error)}</p>` : ''}
      ${stepsHTML(status?.steps)}
      ${on && status?.steps?.some(s => s.tool === 'claude-desktop' && s.outcome === 'done')
        ? '<p class="set-foot">Claude Desktop keeps running through Switcher, on its own login, until you restart it.</p>' : ''}
    </div>`;
  }

  function render() {
    if (view) view.innerHTML = html();
  }

  // set hands back (true) or sets Switcher up again (false).
  async function set(on) {
    if (busy) return false;
    if (on && !await confirm({ title: 'Hand your tools back?', confirmLabel: 'Hand back', danger: true,
      message: 'Codex gets its previous provider back, Claude Desktop its settings, and sharing turns off. Switcher then stops using its accounts until you set it up again.' })) {
      render();
      return false;
    }
    busy = true;
    error = '';
    render();
    try {
      const answer = await post({ enabled: on });
      status = answer.handback || status;
      if (answer.warning) error = answer.warning;
      toast(on ? 'Your tools are handed back' : 'Switcher is set up again');
      onChange(on);
      return true;
    } catch (e) {
      error = e.message || 'That did not work.';
      return false;
    } finally {
      busy = false;
      await load();
    }
  }

  function mount(root) {
    view = root;
    error = '';
    if (!root.dataset.handbackBound) {
      root.dataset.handbackBound = '1';
      root.addEventListener('change', event => {
        if (event.target.matches('[data-handback-switch]')) set(event.target.checked);
      });
    }
    render();
    load();
  }

  return { mount, html, load, set };
}

// autostartHTML is the Start at login card, for a Switcher that manages its
// own login items (Linux).
export function autostartHTML(autostart) {
  if (!autostart?.supported) return '';
  return `<div class="settings-card">
    <div class="set-card-head"><h3>Start at login</h3><p class="settings-sub">Runs Switcher when you log in, also over SSH, and shows the tray with your desktop.</p></div>
    <div class="settings-row">
      <div><strong>Start at login</strong><span class="dim">${autostart.service ? 'systemd user service switcher.service' : autostart.enabled ? 'With your desktop session' : 'Off'}</span></div>
      <label class="switch-wrap"><input type="checkbox" id="autostart" aria-label="Start Switcher at login" ${autostart.enabled ? 'checked' : ''}><span class="switch-visual"></span></label>
    </div>
    ${autostart.message ? `<p class="set-foot">${escape(autostart.message)}</p>` : ''}
  </div>`;
}
