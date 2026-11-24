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
const cluster = {textContent: 'serve browse-only · up 0m'};
const row = {dataset: {key: 'p/r'}, textContent: ''};
const form = {querySelector: s => ({
  '[data-agent-summary]': summary,
  '[data-agent-error]': error,
  '[name="revision"]': revision
})[s]};
let visibility, timer, reply, polls = 0, starts = 0;
const document = {
  hidden: false, activeElement: button,
  getElementById: id => { assert.equal(id, 'agent-default'); return page.includes('id="agent-default"') ? form : null; },
  querySelector: s => { assert.equal(s, '[data-serve-cluster]'); return cluster; },
  querySelectorAll: () => [row],
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
async function refresh(text, rev, err = '', status = 'serve armed · up 4m · 3 unpushed') {
  timer();
  reply({ok: true, json: async () => ({
    __agent_summary: text, __agent_revision: String(rev), __agent_error: err,
    __serve_cluster: status, 'p/r': 'working'
  })});
  await flush();
  if (page.includes('id="agent-default"')) {
    assert.equal(summary.textContent, text);
    assert.equal(revision.value, String(rev));
    assert.equal(error.textContent, err);
  }
  assert.equal(cluster.textContent, status);
  assert.equal(row.textContent, 'working');
  assert.equal(document.activeElement, button, 'focused control was replaced');
}
(async () => {
  assert.equal(starts, 1);
  await refresh('Default: Codex until tomorrow', 1);
  await refresh('Default: stylesheet', 1); // expiry has no new revision
  await refresh('Default: unavailable', 0, 'malformed record', 'serve armed · up 4m · push failing 12m · 3 unpushed');
  await refresh('Default: Claude until tomorrow', 2, '', 'serve armed · up 4m');
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
