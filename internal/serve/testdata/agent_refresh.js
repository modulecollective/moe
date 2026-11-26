const assert = require('node:assert/strict');
const fs = require('node:fs');
const page = fs.readFileSync(0, 'utf8');
const scripts = [...page.matchAll(/<script>([\s\S]*?)<\/script>/g)]
  .map(m => m[1]).filter(s => s.includes("fetch('/run-status'"));
assert.equal(scripts.length, 1, 'one observation poller per page');
const summary = {textContent: 'Default: stylesheet'};
const error = {textContent: ''};
const revision = {value: '0'};
const button = {};
const pauseSummary = {textContent: ''}, pauseError = {textContent: ''}, pauseRevision = {value: '0'}, pauseButton = {}, pauseForm = {};
const pause = {querySelector: s => ({
 '[data-pause-summary]': pauseSummary, '[data-pause-error]': pauseError,
 '[name="revision"]': pauseRevision, 'button': pauseButton, 'form': pauseForm
})[s]};
const cluster = {textContent: 'serve browse-only · up 0m'};
const clock = {textContent: 'Thu 26 Nov 14:07'};
const hasCluster = page.includes('data-serve-cluster'), hasClock = page.includes('data-clock');
const rows = [...page.matchAll(/<span class="note run-status" data-key="([^"]+)">([^<]*)<\/span>/g)]
  .map(m => ({dataset: {key: m[1]}, textContent: m[2]}));
const badges = [...page.matchAll(/<span class="badge blocked" data-blocked-key="([^"]+)"( hidden)?>blocked<\/span>/g)]
  .map(m => ({dataset: {blockedKey: m[1]}, hidden: !!m[2]}));
assert.equal(rows.length, badges.length);
const form = {querySelector: s => ({
  '[data-agent-summary]': summary,
  '[data-agent-error]': error,
  '[name="revision"]': revision
})[s]};
let visibility, timer, reply, polls = 0, starts = 0;
const document = {
  hidden: false, activeElement: button,
  getElementById: id => { if (id === 'serve-pause') return page.includes('id="serve-pause"') ? pause : null; assert.equal(id, 'agent-default'); return page.includes('id="agent-default"') ? form : null; },
  querySelector: s => {
    if (s === '[data-clock]') return hasClock ? clock : null;
    assert.equal(s, '[data-serve-cluster]'); return hasCluster ? cluster : null;
  },
  querySelectorAll: s => {
    if (s === '.run-status[data-key]') return rows;
    assert.equal(s, '[data-blocked-key]'); return badges;
  },
  addEventListener: (name, cb) => { assert.equal(name, 'visibilitychange'); visibility = cb; }
};
new Function('document', 'fetch', 'setInterval', 'clearInterval', scripts[0])(
  document,
  (url, opts) => {
    assert.equal(url, '/run-status'); assert.equal(opts.cache, 'no-store'); polls++;
    return new Promise(resolve => { reply = resolve; });
  },
  (callback, delay) => { assert.equal(delay, 5000); timer = callback; starts++; return 1; },
  () => { timer = null; }
);
const flush = () => new Promise(setImmediate);
async function refresh(text, rev, err = '', status = 'serve armed · up 4m · 3 unpushed', blocked = true, present = true) {
  timer();
  reply({ok: true, json: async () => ({
    __pause_summary: 'new automatic sweeps ' + (rev % 2 ? 'paused until tomorrow' : 'unpaused'),
    __pause_revision: String(rev), __pause_error: err, __pause_active: rev % 2 ? 'true' : 'false',
    __agent_summary: text, __agent_revision: String(rev), __agent_error: err,
    __serve_cluster: status, __clock: 'Thu 26 Nov 14:0' + rev, ...(present ? Object.fromEntries(rows.map(row => [row.dataset.key, {summary: '<working>', stage_blocked: blocked}])) : {})
  })});
  await flush();
  if (page.includes('id="agent-default"')) {
    assert.equal(summary.textContent, text);
    assert.equal(revision.value, String(rev));
    assert.equal(error.textContent, err);
  }
  if (page.includes('id="serve-pause"')) {
    assert.equal(pauseSummary.textContent, 'new automatic sweeps ' + (rev % 2 ? 'paused until tomorrow' : 'unpaused'));
    assert.equal(pauseError.textContent, err);
    assert.equal(pauseRevision.value, String(rev));
    assert.equal(pauseButton.textContent, rev % 2 ? 'Unpause' : 'Pause for 24 hours');
    assert.equal(pauseButton.disabled, !!err);
    assert.equal(pauseForm.action, rev % 2 ? '/serve/unpause' : '/serve/pause');
  }
  if (hasCluster) assert.equal(cluster.textContent, status);
  if (hasClock) assert.equal(clock.textContent, 'Thu 26 Nov 14:0' + rev);
  rows.forEach(row => assert.equal(row.textContent, present ? '<working>' : ''));
  badges.forEach(badge => assert.equal(badge.hidden, !(present && blocked)));
  assert.equal(document.activeElement, button, 'focused control was replaced');
}
(async () => {
  assert.equal(starts, 1);
  await refresh('Default: Codex until tomorrow', 1);
  await refresh('Default: stylesheet', 1, '', undefined, false); // expiry has no new revision
  await refresh('Default: unavailable', 0, 'malformed record', 'serve armed · up 4m · push failing 12m · 3 unpushed');
  await refresh('Default: Claude until tomorrow', 2, '', 'serve armed · up 4m', true);
  await refresh('Default: Claude until tomorrow', 2, '', 'serve armed · up 4m', true, false);
  timer(); reply({ok: false}); await flush();
  assert.equal(cluster.textContent, 'serve armed · up 4m', 'failure erased last good status');
  timer(); // A response arriving after the tab hides must not paint.
  document.hidden = true; visibility(); assert.equal(timer, null);
  reply({ok: true, json: async () => ({__serve_cluster: 'wrong'})}); await flush();
  assert.equal(cluster.textContent, 'serve armed · up 4m');
  const before = polls;
  document.hidden = false; visibility();
  assert.equal(polls, before + 1, 'return to tab must observe immediately');
  assert.equal(starts, 2);
  reply({ok: false}); await flush();
})().catch(err => { console.error(err); process.exitCode = 1; });
