const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

async function harness(t, technocore) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: 'http://piso.local/#/projects', runScripts: 'outside-only', pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window;
  w.scrollTo = () => {};
  w.EventSource = class { addEventListener() {} };
  w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  w.HTMLDialogElement.prototype.close = function () { this.open = false; };
  w.AbortSignal.timeout = () => new w.AbortController().signal;
  w.setInterval = () => 0;
  w.fetch = async (path) => {
    let result = [];
    if (path.endsWith('/health')) result = {};
    else if (path.endsWith('/technocore')) result = technocore;
    else if (path.endsWith('/asks')) result = { asks: [] };
    return { ok: true, status: 200, json: async () => result };
  };
  w.eval(script);
  await tick();
  return w;
}

const rail = w => w.document.querySelector('#pairing');

test('an unpaired gateway says it is not connected, with no sync age', async t => {
  const w = await harness(t, { status: 'unpaired', placement: 'local' });
  const el = rail(w);
  assert.equal(el.textContent, 'Technocore: not connected');
  assert.doesNotMatch(el.textContent, /synced|739905|unpaired/);
  assert.match(el.title, /run locally/);
});

// Older gateways serialized Go's zero time as 0001-01-01; the label must not
// turn that into "synced 739905d ago".
test('a zero timestamp never renders as a sync age', async t => {
  const w = await harness(t, { status: 'unpaired', lastSync: '0001-01-01T00:00:00Z' });
  assert.equal(rail(w).textContent, 'Technocore: not connected');
});

test('an active gateway reports the sync age', async t => {
  const lastSync = new Date(Date.now() - 2 * 60000).toISOString();
  const w = await harness(t, { status: 'active', lastSync, fingerprint: 'abc123def456' });
  assert.equal(rail(w).textContent, 'Technocore: paired · synced 2m ago');
  assert.equal(rail(w).title, 'abc123def456');
});

test('an active gateway that never synced says so instead of "just now"', async t => {
  const w = await harness(t, { status: 'active' });
  assert.equal(rail(w).textContent, 'Technocore: paired · awaiting first sync');
});

test('a pending gateway links to the pairing page', async t => {
  const pairUrl = 'https://tc.example/pair/pair-1/';
  const w = await harness(t, { status: 'pending', pairUrl });
  const el = rail(w);
  assert.equal(el.querySelector('a').getAttribute('href'), pairUrl);
  assert.match(el.textContent, /awaiting approval/);
  assert.equal(el.querySelector('a').textContent, 'Open pairing page');
});

test('a failed identity read reports an error, not a pairing state', async t => {
  const w = await harness(t, { status: 'error', lastError: 'signing key: bad seed' });
  assert.equal(rail(w).textContent, 'Technocore: error');
  assert.equal(rail(w).title, 'signing key: bad seed');
});
