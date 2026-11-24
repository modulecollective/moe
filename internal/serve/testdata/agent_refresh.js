const assert = require('node:assert/strict');
const fs = require('node:fs');
const page = fs.readFileSync(0, 'utf8');
const source = [...page.matchAll(/<script>([\s\S]*?)<\/script>/g)]
  .map(m => m[1]).find(s => s.includes("getElementById('agent-default')"));
assert.ok(source);
const summary = {textContent: 'Default: stylesheet'};
const error = {textContent: ''};
const revision = {value: '0'};
const button = {};
const form = {querySelector: s => ({
  '[data-agent-summary]': summary,
  '[data-agent-error]': error,
  '[name="revision"]': revision
})[s]};
const document = {
  hidden: false, activeElement: button,
  getElementById: id => { assert.equal(id, 'agent-default'); return form; },
  querySelectorAll: () => [],
  addEventListener: () => {}
};
let tick, reply;
new Function('document', 'fetch', 'setInterval', 'clearInterval', source)(
  document,
  (url, opts) => {
    assert.equal(url, '/run-status'); assert.equal(opts.cache, 'no-store');
    return new Promise(resolve => { reply = resolve; });
  },
  callback => { tick = callback; return 1; },
  () => {}
);
async function refresh(text, rev, err = '') {
  tick();
  reply({ok: true, json: async () => ({
    __agent_summary: text, __agent_revision: String(rev), __agent_error: err
  })});
  await new Promise(setImmediate);
  assert.equal(summary.textContent, text);
  assert.equal(revision.value, String(rev));
  assert.equal(error.textContent, err);
  assert.equal(document.activeElement, button, 'focused control was replaced');
}
(async () => {
  await refresh('Default: Codex until tomorrow', 1);
  await refresh('Default: stylesheet', 1); // expiry has no new revision
  await refresh('Default: unavailable', 0, 'malformed record');
})().catch(err => { console.error(err); process.exitCode = 1; });
