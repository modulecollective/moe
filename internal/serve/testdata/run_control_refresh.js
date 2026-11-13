const assert = require('node:assert/strict');
const fs = require('node:fs');
const fixtures = JSON.parse(fs.readFileSync(0, 'utf8'));
const script = [...fixtures.page.matchAll(/<script>([\s\S]*?)<\/script>/g)]
  .map(m => m[1]).find(s => s.includes("getElementById('run-controls')"));
assert.ok(script, 'served page must include the control refresher');

async function exercise(source) {
  const nodes = {};
  let replacements = 0, requests = [], pending, interval, visibility;
  const note = {value: 'unsaved note'};
  const document = {
    hidden: false, activeElement: note,
    getElementById: id => nodes[id],
    addEventListener: (event, callback) => { assert.equal(event, 'visibilitychange'); visibility = callback; },
    createElement: tag => {
      assert.equal(tag, 'div');
      return {set innerHTML(html) { this.parts = parse(html); }, querySelector(selector) { return this.parts[selector.slice(1)]; }};
    }
  };
  const window = {scrollX: 4, scrollY: 500, scrollTo(x, y) { this.scrollX = x; this.scrollY = y; }};
  function parse(html) {
    const parts = {};
    for (const [id, tag] of [['run-status', 'section'], ['run-controls', 'div']]) {
      const match = html.match(new RegExp('<' + tag + '[^>]*id="' + id + '"[^>]*>[\\s\\S]*?</' + tag + '>'));
      if (!match) continue;
      const body = match[0];
      const receipt = body.includes('class="control-requests"') ? {open: body.includes('class="control-requests" open')} : null;
      parts[id] = {
        body, dataset: {refresh: (body.match(/data-refresh="([^"]+)"/) || [])[1]},
        contains: element => element && element.parent === id,
        querySelector: selector => { assert.equal(selector, '.control-requests'); return receipt; },
        replaceWith(next) { nodes[id] = next; replacements++; window.scrollY = 0; }
      };
    }
    return parts;
  }
  Object.assign(nodes, parse(fixtures.page));
  new Function('document', 'window', 'fetch', 'setInterval', 'clearInterval', source)(
    document, window,
    (url, options) => {
      requests.push(url);
      assert.equal(options.cache, 'no-store');
      assert.equal(options.credentials, 'same-origin');
      return new Promise(resolve => { pending = resolve; });
    },
    (callback, delay) => { assert.equal(delay, 5000); interval = callback; return 1; },
    () => { interval = null; }
  );
  const tick = interval;
  const flush = () => new Promise(setImmediate);
  async function reply(html) { pending({ok: true, text: () => Promise.resolve(html)}); await flush(); }
  function focus(id) { document.activeElement = {parent: id}; }
  // Both live slots suppress polling before fetch.
  for (const id of ['run-status', 'run-controls']) {
    focus(id); tick(); assert.equal(requests.length, 0);
  }
  document.activeElement = note;
  nodes['run-status'].querySelector('.control-requests').open = true;
  tick(); await reply(fixtures.resume);
  assert.equal(requests[0], '/run/alpha/fix-it/status');
  assert.equal(replacements, 2);
  assert.match(nodes['run-controls'].body, /name="action" value="resume"/);
  assert.match(nodes['run-controls'].body, /name="revision" value="7"/);
  assert.match(nodes['run-controls'].body, /name="id" value="web-2"/);
  assert.doesNotMatch(nodes['run-controls'].body, /value="pause"/);
  assert.equal(nodes['run-status'].querySelector('.control-requests').open, true);
  assert.equal(window.scrollY, 500); assert.equal(window.scrollX, 4);
  assert.equal(document.activeElement, note); assert.equal(note.value, 'unsaved note');
  // Focus acquired after fetch blocks both replacements.
  for (const id of ['run-status', 'run-controls']) {
    tick(); focus(id); await reply(fixtures.empty); assert.equal(replacements, 2);
    document.activeElement = note;
  }
  // A page hidden while fetch is in flight also rejects the response.
  tick(); document.hidden = true; visibility();
  assert.equal(interval, null);
  await reply(fixtures.empty); assert.equal(replacements, 2);
  const count = requests.length; tick(); assert.equal(requests.length, count);
  document.hidden = false; visibility();
  assert.ok(interval); await reply(fixtures.empty);
  assert.equal(replacements, 4);
  assert.doesNotMatch(nodes['run-controls'].body, /<form/);
  // The empty control slot survives and can acquire fresh controls later.
  tick(); await reply(fixtures.active);
  assert.equal(replacements, 6);
  assert.match(nodes['run-controls'].body, /name="action" value="pause"/);
  assert.match(nodes['run-controls'].body, /name="revision" value="8"/);
  assert.match(nodes['run-controls'].body, /name="id" value="web-4"/);
  // An incomplete response cannot partially refresh the page.
  tick(); await reply('<section id="run-status"></section>');
  assert.equal(replacements, 6);
  assert.equal(note.value, 'unsaved note'); assert.equal(window.scrollY, 500);
}

(async () => {
  await exercise(script);
  // Discriminate the response-time focus check from the initial check.
  const weakened = script.replace('|| occupied(now, actions)', '');
  assert.notEqual(weakened, script);
  await assert.rejects(() => exercise(weakened));
})().catch(err => { console.error(err); process.exitCode = 1; });
