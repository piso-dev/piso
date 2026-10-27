const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

// The harness serves the same paths the dashboard polls and lets a test pin
// any one of them (including mutations) to a canned response.
async function harness(t, technocore, routes = {}) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: 'http://piso.local/#/projects', runScripts: 'outside-only', pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window;
  const calls = [];
  const opened = [];
  w.scrollTo = () => {};
  w.open = url => { opened.push(url); return {}; };
  w.EventSource = class { addEventListener() {} };
  w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  w.HTMLDialogElement.prototype.close = function () { this.open = false; };
  w.AbortSignal.timeout = () => new w.AbortController().signal;
  w.setInterval = () => 0;
  w.fetch = async (path, opts = {}) => {
    const method = opts.method || 'GET';
    const key = path.replace(/^\/api\/v1\//, '').split('?')[0];
    calls.push({ method, key, body: opts.body ? JSON.parse(opts.body) : undefined });
    const route = routes[`${method} ${key}`];
    if (route) {
      // `status` on these payloads is the gateway's pairing status, so a fake
      // HTTP status needs its own field.
      const out = typeof route === 'function' ? route(opts) : route;
      const status = out?.httpStatus || 200;
      const payload = out && 'body' in out ? out.body : out;
      return {
        ok: status < 400, status,
        text: async () => JSON.stringify(payload),
        json: async () => payload,
      };
    }
    let result = [];
    if (path.endsWith('/health')) result = {};
    else if (path.endsWith('/technocore')) result = typeof technocore === 'function' ? technocore() : technocore;
    else if (path.endsWith('/asks')) result = { asks: [] };
    return { ok: true, status: 200, json: async () => result };
  };
  w.eval(script);
  await tick();
  return { w, calls, opened };
}

const rail = w => w.document.querySelector('#live');
const content = w => w.document.querySelector('#content');

async function openLive(t) {
  const h = await harness(t, ...Array.prototype.slice.call(arguments, 1));
  h.w.location.hash = '#/live';
  await tick();
  return h;
}

test('an unpaired gateway says it is not connected, with no sync age', async t => {
  const { w } = await harness(t, { status: 'unpaired', placement: 'local' });
  const el = rail(w);
  assert.equal(el.textContent.trim().startsWith('Technocore: not connected'), true);
  assert.doesNotMatch(el.textContent, /synced|739905|unpaired/);
  assert.match(el.title, /run locally/);
  assert.doesNotMatch(el.querySelector('#liveDot').className, /ok/);
});

// Older gateways serialized Go's zero time as 0001-01-01; the label must not
// turn that into "synced 739905d ago".
test('a zero timestamp never renders as a sync age', async t => {
  const { w } = await harness(t, { status: 'unpaired', lastSync: '0001-01-01T00:00:00Z' });
  assert.match(rail(w).textContent, /Technocore: not connected/);
});

test('an active gateway reports the sync age', async t => {
  const lastSync = new Date(Date.now() - 2 * 60000).toISOString();
  const { w } = await harness(t, { status: 'active', lastSync, fingerprint: 'abc123def456' });
  assert.match(rail(w).textContent, /Technocore: paired · synced 2m ago/);
  assert.match(rail(w).title, /abc123def456/);
  assert.match(rail(w).querySelector('#liveDot').className, /ok/);
});

test('an active gateway that never synced says so instead of "just now"', async t => {
  const { w } = await harness(t, { status: 'active' });
  assert.match(rail(w).textContent, /Technocore: paired · awaiting first sync/);
});

test('a pending gateway links into the live panel', async t => {
  const { w } = await harness(t, { status: 'pending', pairUrl: 'https://tc.example/pair/pair-1/' });
  assert.match(rail(w).textContent, /Technocore: awaiting approval/);
  assert.match(rail(w).title, /tc\.example\/pair\/pair-1/);
});

test('an approved gateway asks for the confirm in the rail line', async t => {
  const { w } = await harness(t, { status: 'pending', awaitingConfirm: true, pairApprover: 'alice' });
  assert.match(rail(w).textContent, /Technocore: approved · confirm/);
  assert.match(rail(w).title, /Approved by alice/);
});

test('a failed identity read reports an error, not a pairing state', async t => {
  const { w } = await harness(t, { status: 'error', lastError: 'signing key: bad seed' });
  assert.match(rail(w).textContent, /Technocore: error/);
  assert.match(rail(w).title, /signing key: bad seed/);
});

test('the rail button opens the live connection view', async t => {
  const { w } = await harness(t, { status: 'unpaired', name: 'virginia' });
  rail(w).click();
  await tick();
  assert.equal(w.location.hash, '#/live');
  assert.equal(rail(w).getAttribute('aria-current'), 'page');
  assert.match(content(w).textContent, /Connect this gateway/);
  assert.equal(w.document.querySelector('#liveForm #pairServer').value, '');
});

// A URL the CLI never saved must not be invented: the placeholder used by old
// builds (https://server.com) is not a coordinator.
test('the form does not prefill the placeholder coordinator', async t => {
  const { w } = await openLive(t, { status: 'unpaired', serverUrl: 'https://server.com' });
  assert.equal(w.document.querySelector('#pairServer').value, '');
});

test('a saved coordinator is offered again after an expiry', async t => {
  const { w } = await openLive(t, {
    status: 'unpaired', serverUrl: 'https://piso.dev', lastError: 'the pairing code expired',
  });
  assert.match(content(w).textContent, /Code expired\. Show a new one\./);
  assert.equal(w.document.querySelector('#pairServer').value, 'https://piso.dev');
});

test('showing a code posts the origin and renders the QR and countdown', async t => {
  const state = { status: 'unpaired', name: 'virginia' };
  const tc = () => state;
  const { w, calls } = await openLive(t, tc, {
    'POST technocore/pair': () => {
      Object.assign(state, {
        status: 'pending',
        pairUrl: 'https://piso.dev/pair/pair-1/',
        pairExpiresAt: new Date(Date.now() + 90000).toISOString(),
        fingerprint: '79541a2e2f78',
      });
      return state;
    },
  });
  w.document.querySelector('#pairServer').value = 'https://piso.dev';
  w.document.querySelector('#pairName').value = 'virginia';
  w.document.querySelector('#liveForm').dispatchEvent(new w.Event('submit', { bubbles: true, cancelable: true }));
  await tick();

  const post = calls.find(c => c.method === 'POST');
  assert.deepEqual(post.body, { serverUrl: 'https://piso.dev', name: 'virginia' });
  const img = content(w).querySelector('.live-qr img');
  assert.match(img.getAttribute('src'), /\/api\/v1\/technocore\/pair\/qr\.svg\?v=/);
  assert.match(content(w).textContent, /Scan this with your phone/);
  // The phone page signs in before it approves, so the panel must not promise
  // a sign-in-free approval.
  assert.match(content(w).textContent, /Sign in on the coordinator/);
  assert.doesNotMatch(content(w).textContent, /no sign-in/i);
  assert.match(content(w).textContent, /Waiting for your phone/);
  assert.match(content(w).textContent, /79541a2e2f78/);
  assert.match(w.document.querySelector('[data-live-countdown]').textContent, /^1:[0-3]\d$/);
  assert.match(content(w).textContent, /nothing connects until you confirm/i);
});

// The confirm screen is the load-bearing consent step: it names who approved,
// and it is the only place that offers Connect.
test('an anonymous phone approval asks the dashboard to connect the gateway', async t => {
  const { w } = await openLive(t, {
    status: 'pending', awaitingConfirm: true, pairApprover: '', pairAccount: 'Alice',
    name: 'virginia', fingerprint: '79541a2e2f78', pairExpiresAt: new Date(Date.now() + 60000).toISOString(),
  });
  const text = content(w).textContent;
  assert.match(text, /Approved from a phone/);
  assert.match(text, /Connect virginia to Alice\?/);
  assert.match(text, /Approved by\s*a phone/);
  assert.match(text, /Nothing has been shared yet/);
  assert.equal(content(w).querySelector('img'), null);
  const buttons = [...content(w).querySelectorAll('button')].map(b => b.textContent);
  assert.deepEqual(buttons.slice(0, 2), ['Connect this gateway', 'Decline']);
});

test('an approver name is escaped and shown', async t => {
  const { w } = await openLive(t, {
    status: 'pending', awaitingConfirm: true, pairApprover: '<img src=x onerror=alert(1)>',
  });
  assert.equal(content(w).querySelector('img'), null);
  assert.match(content(w).textContent, /<img src=x onerror=alert\(1\)>/);
});

test('confirming posts the confirm and lands on the paired state', async t => {
  const state = {
    status: 'pending', awaitingConfirm: true, pairApprover: 'alice', pairAccount: 'Alice',
    name: 'virginia', fingerprint: '79541a2e2f78',
  };
  const { w, calls } = await openLive(t, () => state, {
    'POST technocore/pair/confirm': () => {
      Object.assign(state, {
        status: 'active', awaitingConfirm: false, principalId: 'principal-1',
        lastSync: new Date().toISOString(), secrets: { total: 3, published: 3, pending: 0 },
      });
      return state;
    },
  });
  [...content(w).querySelectorAll('button')].find(b => b.textContent === 'Connect this gateway').click();
  await tick();

  assert.ok(calls.some(c => c.method === 'POST' && c.key === 'technocore/pair/confirm'));
  assert.match(content(w).textContent, /Paired/);
  assert.match(content(w).textContent, /3 credentials synced/);
  assert.match(content(w).textContent, /Re-enrolling or switching coordinators is a terminal action/);
  assert.match(rail(w).textContent, /Technocore: paired/);
});

test('declining a phone approval returns to the form', async t => {
  const state = { status: 'pending', awaitingConfirm: true, pairApprover: 'alice' };
  const { w, calls } = await openLive(t, () => state, {
    'POST technocore/pair/decline': () => {
      Object.assign(state, { status: 'unpaired', awaitingConfirm: false, lastError: 'the pairing was declined' });
      return state;
    },
  });
  [...content(w).querySelectorAll('button')].find(b => b.textContent === 'Decline').click();
  await tick();
  assert.ok(calls.some(c => c.key === 'technocore/pair/decline'));
  assert.match(content(w).textContent, /Pairing declined\. Nothing was shared\./);
});

// An anonymous phone approval cannot finish on the poll token alone (whoever
// created the request holds it), so the dashboard hands over a signed-in URL.
test('a signed-in confirmation requirement becomes a link, not a dead end', async t => {
  const state = { status: 'pending', awaitingConfirm: true, name: 'virginia' };
  const { w, opened } = await openLive(t, () => state, {
    'POST technocore/pair/confirm': () => ({
      httpStatus: 409,
      body: { error: 'finish this enrollment from a signed-in browser', needsLogin: true, confirmUrl: 'https://piso.dev/pair/pair-1/' },
    }),
  });
  [...content(w).querySelectorAll('button')].find(b => b.textContent === 'Connect this gateway').click();
  await tick();
  assert.match(content(w).textContent, /approved without signing in/);
  const go = [...content(w).querySelectorAll('button')].find(b => b.textContent === 'Open Technocore to confirm');
  assert.ok(go, 'no coordinator link offered');
  assert.equal(content(w).querySelector('[role="alert"]'), null);
  go.click();
  await tick();
  assert.deepEqual(opened, ['https://piso.dev/pair/pair-1/']);
});

test('a failed mint keeps the form and shows the reason', async t => {
  const { w } = await openLive(t, { status: 'unpaired' }, {
    'POST technocore/pair': { httpStatus: 502, body: { error: 'could not reach https://nope.example' } },
  });
  w.document.querySelector('#pairServer').value = 'https://nope.example';
  w.document.querySelector('#liveForm').dispatchEvent(new w.Event('submit', { bubbles: true, cancelable: true }));
  await tick();
  assert.match(content(w).querySelector('[role="alert"]').textContent, /could not reach/);
  assert.ok(w.document.querySelector('#liveForm'), 'form disappeared');
});

test('cancelling a code returns to the unpaired form', async t => {
  const state = { status: 'pending', pairUrl: 'https://piso.dev/pair/pair-1/', pairExpiresAt: new Date(Date.now() + 60000).toISOString() };
  const { w, calls } = await openLive(t, () => state, {
    'POST technocore/pair/cancel': () => {
      Object.assign(state, { status: 'unpaired' });
      return state;
    },
  });
  [...content(w).querySelectorAll('button')].find(b => b.textContent === 'Cancel').click();
  await tick();
  assert.ok(calls.some(c => c.key === 'technocore/pair/cancel'));
  assert.ok(w.document.querySelector('#liveForm'));
});

test('the paired panel is read-only: no confirm or QR', async t => {
  const { w } = await openLive(t, {
    status: 'active', name: 'virginia', serverUrl: 'https://piso.dev', fingerprint: '79541a2e2f78',
    placement: 'local', secrets: { total: 1, published: 1, pending: 0 },
  });
  assert.equal(content(w).querySelector('input'), null);
  assert.equal(content(w).querySelector('img'), null);
  assert.doesNotMatch(content(w).textContent, /Show pairing code/);
});

test('the terminal equivalent stays one disclosure away', async t => {
  const { w } = await openLive(t, {
    status: 'pending', awaitingConfirm: true, name: 'virginia', pairApprover: 'alice',
  });
  const details = content(w).querySelector('details.live-cli');
  assert.ok(details);
  assert.match(details.textContent, /piso gateway launch/);
  assert.match(details.textContent, /--yes/);
});
