// The share card: a picture of your activity to post, like Codex's. It
// shows the last 26 weeks as a heatmap and four numbers, with Switcher's
// name and a link to its repository; never an account, email or computer
// name. It is drawn on a canvas in the page, so nothing leaves this
// computer until you post or save it.

import { switcherMark } from './brand.js';
import { tokens, levels, plural, esc } from './insights.js';

export const REPO = 'https://github.com/00xmario/Switcher';
export const REPO_LABEL = 'github.com/00xmario/Switcher';
export const WEEKS = 26;
const TOOL_NAMES = { codex: 'Codex', claude: 'Claude Code', grok: 'Grok Build', opencode: 'OpenCode' };
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function dayKey(d) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function parseDay(s) {
  const [y, m, d] = String(s).split('-').map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

// cardWeeks lays out the last weeks up to the last day of the insights as
// columns of seven days, Monday on top; each cell is the day's tokens and
// shade (0 to 4), or null after the last day. Days without data are empty.
export function cardWeeks(insights, weeks = WEEKS) {
  const days = insights?.days || [];
  const byDay = new Map(days.map(d => [d.day, Number(d.tokens) || 0]));
  const last = parseDay(insights?.until || days[days.length - 1]?.day || dayKey(new Date()));
  const monday = new Date(last.getFullYear(), last.getMonth(), last.getDate() - ((last.getDay() + 6) % 7));
  const start = new Date(monday.getFullYear(), monday.getMonth(), monday.getDate() - 7 * (weeks - 1));
  const cols = [];
  const values = [];
  for (let w = 0; w < weeks; w++) {
    const col = [];
    for (let r = 0; r < 7; r++) {
      const d = new Date(start.getFullYear(), start.getMonth(), start.getDate() + w * 7 + r);
      if (d > last) { col.push(null); continue; }
      const v = byDay.get(dayKey(d)) || 0;
      values.push(v);
      col.push({ day: dayKey(d), tokens: v });
    }
    cols.push(col);
  }
  const level = levels(values);
  for (const col of cols) for (const c of col) if (c) c.level = level(c.tokens);
  return cols;
}

// cardStats are the four numbers under the heatmap.
export function cardStats(insights) {
  const all = insights?.range === 'all';
  return [
    [tokens(insights?.totals?.tokens), all ? 'lifetime tokens' : 'tokens'],
    [insights?.peak_day ? tokens(insights.peak_day.tokens) : '0', 'peak day'],
    [plural(insights?.streaks?.current || 0, 'day'), 'current streak'],
    [plural(insights?.streaks?.longest || 0, 'day'), 'longest streak'],
  ];
}

// cardSubtitle says which tools the usage is from, and since when.
export function cardSubtitle(insights) {
  const tools = (insights?.providers || []).filter(p => p.tokens > 0).map(p => TOOL_NAMES[p.provider] || p.provider);
  const parts = [];
  if (tools.length) parts.push(tools.slice(0, 3).join(' · '));
  if (insights?.first_day) {
    const d = parseDay(insights.first_day);
    parts.push(`since ${MONTHS[d.getMonth()]} ${d.getFullYear()}`);
  }
  return parts.join(' · ');
}

// shareText is what a post says with the picture.
export function shareText(insights) {
  const total = tokens(insights?.totals?.tokens);
  const longest = insights?.streaks?.longest || 0;
  return `My AI coding activity: ${total} tokens${longest > 1 ? `, a ${longest}-day streak` : ''}. Tracked with Switcher`;
}

// intentURL opens a site's own "new post" page with the text and the link;
// the picture is copied first, to paste there.
export function intentURL(site, text) {
  const e = encodeURIComponent;
  switch (site) {
    case 'x': return `https://x.com/intent/post?text=${e(text)}&url=${e(REPO)}`;
    case 'linkedin': return `https://www.linkedin.com/sharing/share-offsite/?url=${e(REPO)}`;
    case 'reddit': return `https://www.reddit.com/submit?url=${e(REPO)}&title=${e(text)}`;
  }
  return REPO;
}

const THEMES = {
  light: { bg: '#eef1ee', glow: 'rgba(63,185,111,.16)', card: '#ffffff', border: 'rgba(16,20,26,.07)', text: '#17191d', dim: '#6f7680', faint: '#9aa1ab', empty: '#ebedf0', accent: [47, 158, 95], rule: '#e7e8ec', shadow: 'rgba(16,20,26,.10)' },
  dark: { bg: '#060807', glow: 'rgba(63,185,111,.20)', card: '#141615', border: 'rgba(255,255,255,.07)', text: '#f4f6f5', dim: '#a3aaa6', faint: '#6c736f', empty: '#232624', accent: [63, 185, 111], rule: '#262928', shadow: 'rgba(0,0,0,.5)' },
};

function shade(theme, level) {
  if (!level) return theme.empty;
  const mix = [0, 0.3, 0.52, 0.76, 1][level];
  const hex = theme.empty.replace('#', '');
  const base = [0, 2, 4].map(i => parseInt(hex.slice(i, i + 2), 16));
  const c = base.map((b, i) => Math.round(b + (theme.accent[i] - b) * mix));
  return `rgb(${c[0]},${c[1]},${c[2]})`;
}

function roundRect(ctx, x, y, w, h, r) {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

export const CARD = { width: 1000, height: 640 };
// Inter ships with Switcher (web/fonts, see style.css), so the card looks
// the same on every computer; the system font stands in until it loads.
export const FONT = '"Switcher Inter", system-ui, -apple-system, "Segoe UI", sans-serif';

// fontsReady waits for the card's faces, briefly: a card in the fallback
// font is better than none.
export function fontsReady() {
  if (!document.fonts?.load) return Promise.resolve();
  const faces = ['400 21px "Switcher Inter"', '600 34px "Switcher Inter"', 'italic 400 18px "Switcher Inter"'];
  return Promise.race([Promise.all(faces.map(f => document.fonts.load(f))), new Promise(r => setTimeout(r, 1500))]).catch(() => {});
}

function spaced(ctx, px) { if ('letterSpacing' in ctx) ctx.letterSpacing = `${px}px`; }

// drawCard draws the card for insights on a canvas, at scale times its
// size. mark is the Switcher icon as a loaded image, or null.
export function drawCard(canvas, insights, { theme = 'light', mark = null, scale = 2 } = {}) {
  const t = THEMES[theme] || THEMES.light;
  const { width: W, height: H } = CARD;
  canvas.width = W * scale;
  canvas.height = H * scale;
  const ctx = canvas.getContext('2d');
  ctx.setTransform(scale, 0, 0, scale, 0, 0);

  // The backdrop: a soft wash with a green glow, like the app's.
  ctx.fillStyle = t.bg;
  ctx.fillRect(0, 0, W, H);
  const glow = ctx.createRadialGradient(W * 0.82, H * 0.12, 0, W * 0.82, H * 0.12, W * 0.6);
  glow.addColorStop(0, t.glow);
  glow.addColorStop(1, 'rgba(0,0,0,0)');
  ctx.fillStyle = glow;
  ctx.fillRect(0, 0, W, H);

  const cx = 56, cy = 50, cw = W - 112, ch = H - 100, pad = 52;
  ctx.save();
  ctx.shadowColor = t.shadow;
  ctx.shadowBlur = 40;
  ctx.shadowOffsetY = 12;
  roundRect(ctx, cx, cy, cw, ch, 40);
  ctx.fillStyle = t.card;
  ctx.fill();
  ctx.restore();
  roundRect(ctx, cx + 0.5, cy + 0.5, cw - 1, ch - 1, 40);
  ctx.strokeStyle = t.border;
  ctx.lineWidth = 1;
  ctx.stroke();

  // Header: what this is on the left, Switcher on the right.
  const left = cx + pad, right = cx + cw - pad;
  ctx.textBaseline = 'alphabetic';
  ctx.fillStyle = t.text;
  ctx.font = `600 33px ${FONT}`;
  spaced(ctx, -0.6);
  ctx.textAlign = 'left';
  ctx.fillText('My AI coding activity', left, cy + 80);
  ctx.fillStyle = t.dim;
  ctx.font = `400 20px ${FONT}`;
  spaced(ctx, -0.1);
  ctx.fillText(cardSubtitle(insights) || 'Every computer, every coding tool', left, cy + 112);

  ctx.font = `600 31px ${FONT}`;
  spaced(ctx, -0.6);
  ctx.fillStyle = t.text;
  ctx.textAlign = 'right';
  ctx.fillText('Switcher', right, cy + 88);
  const nameWidth = ctx.measureText('Switcher').width;
  if (mark) ctx.drawImage(mark, right - nameWidth - 56, cy + 52, 46, 46);

  // The heatmap: 26 weeks, Monday on top.
  const cols = cardWeeks(insights);
  const gridTop = cy + 150, gridW = cw - pad * 2;
  const pitch = gridW / cols.length, cell = pitch - 5;
  cols.forEach((col, x) => col.forEach((c, y) => {
    if (!c) return;
    roundRect(ctx, left + x * pitch + 2.5, gridTop + y * pitch, cell, cell, 6);
    ctx.fillStyle = shade(t, c.level);
    ctx.fill();
  }));

  // The four numbers, divided by thin rules.
  const stats = cardStats(insights);
  const statsTop = gridTop + 7 * pitch + 30, colW = gridW / stats.length;
  stats.forEach(([value, label], i) => {
    const mid = left + colW * i + colW / 2;
    ctx.textAlign = 'center';
    ctx.fillStyle = t.text;
    ctx.font = `600 34px ${FONT}`;
    spaced(ctx, -0.8);
    ctx.fillText(value, mid, statsTop + 38);
    ctx.fillStyle = t.dim;
    ctx.font = `400 19px ${FONT}`;
    spaced(ctx, 0);
    ctx.fillText(label, mid, statsTop + 68);
    if (i > 0) {
      ctx.fillStyle = t.rule;
      ctx.fillRect(left + colW * i, statsTop + 8, 1, 66);
    }
  });

  // The link, small and subtle.
  ctx.textAlign = 'center';
  ctx.fillStyle = t.faint;
  ctx.font = `italic 400 17px ${FONT}`;
  spaced(ctx, 0);
  ctx.fillText(REPO_LABEL, cx + cw / 2, cy + ch - 28);
  return canvas;
}

// markImage loads the Switcher icon for drawing; it resolves to null when
// it cannot, and the card is drawn without it.
export function markImage() {
  return new Promise(resolve => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => resolve(null);
    img.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(switcherMark({ size: 96, tile: true, id: 'share-mark' })
      .replace('<svg ', '<svg xmlns="http://www.w3.org/2000/svg" '));
  });
}

const ICONS = {
  x: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M18.9 2H22l-6.77 7.74L23.2 22h-6.23l-4.88-6.38L6.5 22H3.4l7.05-8.05L1.6 2h6.4l4.41 5.83L18.9 2Zm-1.1 18.13h1.73L7.02 3.77H5.3l12.5 16.36Z"/></svg>',
  linkedin: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M4.98 3.5a2.5 2.5 0 1 1 0 5 2.5 2.5 0 0 1 0-5ZM3 9.75h4v11.5H3V9.75Zm6.5 0h3.83v1.57h.06c.53-1.01 1.84-2.07 3.79-2.07 4.05 0 4.8 2.67 4.8 6.13v5.87h-4v-5.2c0-1.24-.02-2.84-1.73-2.84-1.73 0-2 1.35-2 2.75v5.29h-4V9.75Z"/></svg>',
  reddit: '<svg viewBox="0 0 24 24" aria-hidden="true"><g fill="currentColor"><circle cx="18.5" cy="4.6" r="1.7"/><path d="M12.3 3.2c.3-.1.6 0 .8.3l1.6 3.5 3.4.8-.4 1.5-4.1-1-1.4-3.1-.9 3.3c1.9.1 3.6.7 4.9 1.6a2.3 2.3 0 1 1 2.5 3.6c0 .2.1.4.1.6 0 3.3-3.6 5.9-8 5.9s-8-2.6-8-5.9c0-.2 0-.4.1-.6a2.3 2.3 0 1 1 2.5-3.6c1.3-.9 3-1.5 4.8-1.6l1.1-4c.1-.3.4-.6.8-.7Zm-3.4 9.6a1.3 1.3 0 1 0 0 2.6 1.3 1.3 0 0 0 0-2.6Zm6.2 0a1.3 1.3 0 1 0 0 2.6 1.3 1.3 0 0 0 0-2.6Zm-5.6 4.1-.8.8c.9.8 2 1.2 3.3 1.2s2.4-.4 3.3-1.2l-.8-.8c-.6.5-1.5.9-2.5.9s-1.9-.4-2.5-.9Z"/></g></svg>',
  copy: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>',
  save: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/></svg>',
  share: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3v13"/><path d="m7 8 5-5 5 5"/><path d="M5 13v6a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-6"/></svg>',
  close: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6 6 18"/></svg>',
};
export const SHARE_ICON = ICONS.share;

export function dialogHTML(theme, nativeShare) {
  const action = (id, label) => `<button type="button" class="share-action" data-share="${id}"><span class="share-action-icon">${ICONS[id] || ICONS.share}</span><span>${esc(label)}</span></button>`;
  return `<div class="share-dialog" role="dialog" aria-modal="true" aria-labelledby="share-title" tabindex="-1">
    <button type="button" class="share-close" data-share="close" aria-label="Close">${ICONS.close}</button>
    <h2 id="share-title">Share your activity</h2>
    <div class="share-preview"><img data-share-img alt="Your AI coding activity: a heatmap of the last 26 weeks, lifetime tokens, peak day and streaks, with Switcher's name"></div>
    <div class="usage-window share-theme" role="radiogroup" aria-label="Card theme">
      <button type="button" data-share-theme="light" class="${theme === 'light' ? 'active' : ''}" role="radio" aria-checked="${theme === 'light'}">Light</button>
      <button type="button" data-share-theme="dark" class="${theme === 'dark' ? 'active' : ''}" role="radio" aria-checked="${theme === 'dark'}">Dark</button>
    </div>
    <div class="share-actions">
      ${action('x', 'X')}${action('linkedin', 'LinkedIn')}${action('reddit', 'Reddit')}${action('copy', 'Copy')}${action('save', 'Save')}${nativeShare ? action('share', 'More') : ''}
    </div>
    <p class="share-status" data-share-status role="status" aria-live="polite"></p>
  </div>`;
}

// openShare shows the share dialog for insights (all time). It returns a
// function that closes it.
export async function openShare(insights, { prefersDark = false } = {}) {
  let theme = prefersDark ? 'dark' : 'light';
  try { theme = localStorage.getItem('switcher.share.theme') || theme; } catch { /* private window */ }
  if (!['light', 'dark'].includes(theme)) theme = 'light';
  const [mark] = await Promise.all([markImage(), fontsReady()]);
  const canvas = document.createElement('canvas');
  const fileFor = blob => new File([blob], 'switcher-activity.png', { type: 'image/png' });
  const nativeShare = typeof navigator.canShare === 'function' && (() => {
    try { return navigator.canShare({ files: [new File([new Uint8Array(1)], 'a.png', { type: 'image/png' })] }); } catch { return false; }
  })();
  const overlay = document.createElement('div');
  overlay.className = 'share-overlay';
  overlay.innerHTML = dialogHTML(theme, nativeShare);
  document.body.append(overlay);
  const img = overlay.querySelector('[data-share-img]');
  const status = overlay.querySelector('[data-share-status]');
  const returnFocus = document.activeElement;
  let blob = null;
  const draw = () => {
    drawCard(canvas, insights, { theme, mark });
    img.src = canvas.toDataURL('image/png');
    blob = new Promise(resolve => canvas.toBlob(resolve, 'image/png'));
  };
  draw();
  const say = text => { status.textContent = text; };
  const copy = async () => {
    // The picture is put on the clipboard as it is made, within the click.
    await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })]);
  };
  const close = () => {
    overlay.remove();
    document.removeEventListener('keydown', onKey, true);
    returnFocus?.focus?.({ preventScroll: true });
  };
  const onKey = e => { if (e.key === 'Escape') { e.preventDefault(); close(); } };
  document.addEventListener('keydown', onKey, true);
  overlay.addEventListener('click', async event => {
    if (event.target === overlay) { close(); return; }
    const button = event.target.closest('button');
    if (!button) return;
    if (button.dataset.shareTheme) {
      theme = button.dataset.shareTheme;
      try { localStorage.setItem('switcher.share.theme', theme); } catch { /* private window */ }
      overlay.querySelectorAll('[data-share-theme]').forEach(b => { b.classList.toggle('active', b === button); b.setAttribute('aria-checked', String(b === button)); });
      draw();
      return;
    }
    const what = button.dataset.share;
    if (what === 'close') { close(); return; }
    if (what === 'save') {
      const url = URL.createObjectURL(await blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = 'switcher-activity.png';
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 10_000);
      say('Saved as switcher-activity.png.');
      return;
    }
    if (what === 'share') {
      try {
        await navigator.share({ files: [fileFor(await blob)], text: shareText(insights), url: REPO });
        say('');
      } catch (err) {
        if (err?.name !== 'AbortError') say('Sharing did not work here; use Copy or Save.');
      }
      return;
    }
    let copied = true;
    try { await copy(); } catch { copied = false; }
    if (what === 'copy') {
      say(copied ? 'Copied the picture.' : 'Could not copy here; use Save instead.');
      return;
    }
    const url = intentURL(what, shareText(insights));
    const site = { x: 'X', linkedin: 'LinkedIn', reddit: 'Reddit' }[what];
    const opened = window.open(url, '_blank', 'noopener');
    status.replaceChildren();
    status.append(copied ? `Picture copied: paste it into your ${site} post. ` : 'Save the picture to add it to your post. ');
    if (!opened) {
      const link = document.createElement('a');
      link.href = url;
      link.target = '_blank';
      link.rel = 'noopener';
      link.textContent = `Open ${site}`;
      status.append(link);
    }
  });
  overlay.querySelector('.share-dialog').focus({ preventScroll: true });
  return close;
}
