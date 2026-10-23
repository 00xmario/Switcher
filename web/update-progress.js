// Install progress as the server reports it (update.Progress): the step, a
// 0–1 fraction and the bytes downloaded. The update banners draw it as a bar.
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// The bar follows the server, but nothing moves it off the restart: a read
// answered before the install finished can arrive after it.
export function mergeInstallProgress(current, next) {
  if (!next || typeof next.fraction !== 'number' || !Number.isFinite(next.fraction)) return current;
  if (current?.step === 'restarting') return current;
  return next;
}

export function installPercent(progress) {
  return Math.round(Math.min(Math.max(progress?.fraction || 0, 0), 1) * 100);
}

export function installStepText(progress) {
  switch (progress?.step) {
    case 'downloading': {
      if (!(progress.total > 0)) return 'Downloading…';
      const megabytes = bytes => (bytes / 1e6).toFixed(1);
      return `Downloading · ${megabytes(progress.done || 0)} of ${megabytes(progress.total)} MB`;
    }
    case 'checking': return 'Checking the new version';
    case 'installing': return 'Installing';
    case 'restarting': return 'Restarting Switcher…';
    default: return 'Starting…';
  }
}

export function installProgressHTML(progress, release) {
  const percent = installPercent(progress);
  const step = installStepText(progress);
  const label = release ? `Installing Switcher ${release}` : 'Installing Switcher';
  return `<div class="update-progress" role="progressbar" aria-label="${escape(label)}"
      aria-valuemin="0" aria-valuemax="100" aria-valuenow="${percent}" aria-valuetext="${escape(`${step}, ${percent}%`)}">
    <div class="update-progress-track"><div class="update-progress-fill" style="width: ${percent}%"></div></div>
    <div class="update-progress-meta"><span class="update-progress-step">${escape(step)}</span><span class="update-progress-percent">${percent}%</span></div>
  </div>`;
}

// Painting the existing bar in place lets its width transition smoothly.
export function paintInstallProgress(node, progress) {
  const percent = installPercent(progress);
  const step = installStepText(progress);
  node.querySelector('.update-progress-fill').style.width = `${percent}%`;
  node.querySelector('.update-progress-step').textContent = step;
  node.querySelector('.update-progress-percent').textContent = `${percent}%`;
  node.setAttribute('aria-valuenow', String(percent));
  node.setAttribute('aria-valuetext', `${step}, ${percent}%`);
}
