import assert from 'node:assert/strict';
import { mergeInstallProgress, installPercent, installStepText, installProgressHTML, paintInstallProgress } from './update-progress.js';

const downloading = { step: 'downloading', fraction: 0.4, done: 4_000_000, total: 8_000_000 };
assert.equal(mergeInstallProgress(null, downloading), downloading);
assert.equal(mergeInstallProgress(downloading, { step: 'downloading', fraction: 0.2 }), downloading, 'a late report must not move the bar backwards');
assert.equal(mergeInstallProgress(downloading, undefined), downloading, 'a read without an install keeps the bar');
assert.equal(mergeInstallProgress(downloading, { step: 'checking', fraction: NaN }), downloading);
const restarting = { step: 'restarting', fraction: 1 };
assert.equal(mergeInstallProgress(downloading, restarting), restarting);

assert.equal(installPercent(downloading), 40);
assert.equal(installPercent({ fraction: 1.4 }), 100);
assert.equal(installPercent({ fraction: -1 }), 0);
assert.equal(installPercent(null), 0);
assert.equal(installStepText(downloading), 'Downloading · 4.0 of 8.0 MB');
assert.equal(installStepText({ step: 'downloading', fraction: 0, done: 9 }), 'Downloading…', 'an unknown size shows no byte count');
assert.equal(installStepText({ step: 'checking', fraction: 0.8 }), 'Checking the new version');
assert.equal(installStepText({ step: 'installing', fraction: 0.9 }), 'Installing');
assert.equal(installStepText(restarting), 'Restarting Switcher…');
assert.equal(installStepText({ step: 'starting', fraction: 0 }), 'Starting…');

const html = installProgressHTML(downloading, 'v2.0.0"<x>');
assert.match(html, /role="progressbar"/);
assert.match(html, /aria-valuenow="40"/);
assert.match(html, /style="width: 40%"/);
assert.match(html, /Downloading · 4\.0 of 8\.0 MB/);
assert.match(html, /aria-label="Installing Switcher v2\.0\.0&quot;&lt;x&gt;"/, 'the release must be escaped');
assert.doesNotMatch(html, /<x>/);

class Node {
  constructor() { this.style = {}; this.textContent = ''; }
}
const parts = { '.update-progress-fill': new Node(), '.update-progress-step': new Node(), '.update-progress-percent': new Node() };
const attributes = new Map();
const bar = { querySelector: selector => parts[selector], setAttribute: (name, value) => attributes.set(name, value) };
paintInstallProgress(bar, { step: 'checking', fraction: 0.8 });
assert.equal(parts['.update-progress-fill'].style.width, '80%');
assert.equal(parts['.update-progress-step'].textContent, 'Checking the new version');
assert.equal(parts['.update-progress-percent'].textContent, '80%');
assert.equal(attributes.get('aria-valuenow'), '80');
assert.equal(attributes.get('aria-valuetext'), 'Checking the new version, 80%');
console.log('Install progress merging, step text, markup and in-place painting: OK');
