const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

// A view of the dashboard with a fixed pairing snapshot. `paths` records every
// API call so a test can prove an unpaired gateway never asks for a coordinator
// surface in the first place.
async function harness(t, technocore, hash) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: `http://piso.local/#/${hash}`, runScripts: 'outside-only', pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window;
  const paths = [];
  w.scrollTo = () => {};
  w.EventSource = class { addEventListener() {} };
  w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  w.HTMLDialogElement.prototype.close = function () { this.open = false; };
  w.AbortSignal.timeout = () => new w.AbortController().signal;
  w.setInterval = () => 0;
  w.fetch = async (path) => {
    paths.push(path);
    let result = [];
    if (path.endsWith('/health')) result = {};
    else if (path.endsWith('/technocore')) result = technocore;
    else if (path.endsWith('/asks')) result = { asks: [] };
    else if (path.includes('/directives')) result = [];
    else if (path.includes('/tasks')) result = { tasks: [] };
    return { ok: true, status: 200, json: async () => result };
  };
  w.eval(script);
  await tick();
  await tick();
  return { w, paths };
}

const content = w => w.document.querySelector('#content');
const banner = w => w.document.querySelector('#content .notice.info');

test('an unpaired gateway explains the Directives view instead of failing it', async t => {
  const { w, paths } = await harness(t, { status: 'unpaired', placement: 'local' }, 'directives');
  const notice = banner(w);
  assert.ok(notice, 'no info banner rendered');
  assert.match(notice.textContent, /Directives need a paired gateway/);
  assert.doesNotMatch(notice.textContent, /error|failed|unavailable/i);
  // Nothing was requested that could only 503, and no refresh error is shown.
  assert.ok(!paths.some(p => p.includes('/directives')), paths.join(','));
  assert.equal(w.document.querySelector('#errors').textContent, '');
  assert.equal(w.document.querySelector('[data-action="directive-new"]').disabled, true);
});

test('an unpaired gateway disables task creation and says why Tasks is empty', async t => {
  const { w, paths } = await harness(t, { status: 'unpaired' }, 'tasks');
  assert.match(banner(w).textContent, /Tasks need a paired gateway/);
  assert.ok(!paths.some(p => p.includes('/tasks')), paths.join(','));
  assert.equal(w.document.querySelector('[data-action="task-new"]').disabled, true);
  assert.equal(w.document.querySelector('#errors').textContent, '');
});

test('the rail marks coordinator destinations without hiding them', async t => {
  const { w } = await harness(t, { status: 'unpaired' }, 'tasks');
  for (const name of ['tasks', 'directives']) {
    const link = w.document.querySelector(`nav a[data-view="${name}"]`);
    assert.ok(link.classList.contains('is-gated'), name);
    // The gated link is described by the rail's pairing words, which live on
    // the Live connection button now that the old pairing paragraph is gone.
    assert.equal(link.getAttribute('aria-describedby'), 'live');
    assert.equal(link.getAttribute('href'), `#/${name}`);
  }
  assert.ok(!w.document.querySelector('nav a[data-view="projects"]').classList.contains('is-gated'));
});

test('a pending gateway names the approval step and links to it', async t => {
  const { w } = await harness(t, { status: 'pending', pairUrl: 'https://tc.example/pair/abc/' }, 'directives');
  const notice = banner(w);
  assert.match(notice.textContent, /load once this gateway is approved/);
  const link = notice.querySelector('a');
  assert.equal(link.getAttribute('href'), 'https://tc.example/pair/abc/');
});

test('an unreadable identity is reported as such, not as a pairing state', async t => {
  const { w } = await harness(t, { status: 'error', lastError: 'identity json: bad key' }, 'tasks');
  assert.match(banner(w).textContent, /identity is unreadable/);
  assert.match(banner(w).textContent, /identity json: bad key/);
});

// After the merge the phone approval is only intent: the dashboard still has
// to confirm, so the banner must point at the panel that can do it rather than
// at a pairing page the phone already used.
test('a phone-approved gateway points the banner at the confirm step', async t => {
  const { w } = await harness(t, {
    status: 'pending', awaitingConfirm: true, pairApprover: 'alice',
    pairUrl: 'https://tc.example/pair/abc/',
  }, 'tasks');
  const notice = banner(w);
  assert.match(notice.textContent, /unlock once you connect this gateway/);
  assert.match(notice.textContent, /nothing has been shared yet/);
  const link = notice.querySelector('a');
  assert.equal(link.getAttribute('href'), '#/live');
  assert.equal(link.getAttribute('target'), null);
});

test('a paired gateway loads the gated views and shows no banner', async t => {
  const { w, paths } = await harness(t, { status: 'active', fingerprint: 'abc123def456' }, 'directives');
  assert.equal(banner(w), null);
  assert.ok(paths.some(p => p.includes('/directives')), paths.join(','));
  assert.equal(w.document.querySelector('[data-action="directive-new"]').disabled, false);
  assert.ok(!w.document.querySelector('nav a[data-view="directives"]').classList.contains('is-gated'));
});
