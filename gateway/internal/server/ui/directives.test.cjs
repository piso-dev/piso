const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

// The directive proxy unwraps the coordinator's {"directives": [...]} envelope
// before it answers the dashboard, so `/api/v1/directives` returns a bare array
// while `/api/v1/asks` still returns its envelope. `state.list` is that array.
async function harness(t, state = {list: [], onCreate: null}) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: 'http://piso.local/#/directives', runScripts: 'outside-only', pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window, calls = [];
  w.scrollTo = () => {};
  w.EventSource = class { addEventListener() {} };
  w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  w.HTMLDialogElement.prototype.close = function () { this.open = false; };
  w.AbortSignal.timeout = () => new w.AbortController().signal;
  w.setInterval = () => 0;
  w.fetch = async (path, opts = {}) => {
    let result;
    if (path.startsWith('/api/v1/directives')) {
      const call = {path, method: opts.method || 'GET', body: opts.body && JSON.parse(opts.body)};
      calls.push(call);
      result = call.method === 'GET' ? state.list : await state.onCreate(call);
    } else if (path.endsWith('/health')) result = {};
    else if (path.endsWith('/technocore')) result = {status: 'active'};
    else if (path.endsWith('/asks')) result = {asks: []};
    else result = [];
    return {ok: true, status: 201, json: async () => result};
  };
  w.eval(script + '\nwindow.directiveTest = {directiveForm, renderContent};');
  await tick();
  await tick();
  return { w, doc: w.document, calls };
}

const directive = (over = {}) => ({
  id: 'd1', code: 'D1', title: 'No git operations', body: 'Ask before any git command.',
  kind: 'policy', labels: {}, status: 'active', priority: 0, completionCriteria: '',
  createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-02T00:00:00Z', ...over,
});

const content = doc => doc.querySelector('#content');
const submit = (w, doc) => doc.querySelector('#actionForm').dispatchEvent(
  new w.Event('submit', {bubbles: true, cancelable: true}));

test('the bare array from the proxy is what the Directives page renders', async t => {
  const { doc } = await harness(t, {list: [
    directive(),
    directive({id: 'd2', code: 'D2', title: 'Local records', labels: {repo: 'piso'},
      completionCriteria: 'All checks green.'}),
  ]});
  const page = content(doc);
  assert.match(page.textContent, /D1 · No git operations/);
  assert.match(page.textContent, /D2 · Local records/);
  assert.match(page.textContent, /Ask before any git command\./);
  assert.doesNotMatch(page.textContent, /No directives yet/);
  assert.equal(doc.querySelector('#resultCount').textContent, '2 directives');
  // Scope and completion criteria are part of the record, not decoration.
  assert.match(page.textContent, /repo=piso/);
  assert.match(page.textContent, /All checks green\./);
});

test('an empty collection says so rather than stalling on a blank page', async t => {
  const { doc } = await harness(t, {list: []});
  assert.match(content(doc).textContent, /No directives yet/);
  assert.equal(doc.querySelector('#resultCount').textContent, '0 directives');
});

test('creating a directive lands on the directives page with the new code', async t => {
  const state = {list: [], onCreate: call => {
    const row = directive({id: call.body.id, code: 'D5', title: call.body.title,
      body: call.body.body, kind: call.body.kind, completionCriteria: call.body.completionCriteria});
    state.list = [row];
    return row;
  }};
  const { w, doc, calls } = await harness(t, state);
  doc.querySelector('[data-action="directive-new"]').click();
  await tick();
  doc.querySelector('#f-title').value = 'Ship it';
  doc.querySelector('#f-body').value = 'Verify before you claim done.';
  doc.querySelector('#f-completionCriteria').value = 'Tests pass.';
  submit(w, doc);
  await tick();
  await tick();
  const created = calls.find(c => c.method === 'POST');
  assert.equal(created.body.title, 'Ship it');
  assert.equal(created.body.completionCriteria, 'Tests pass.');
  // The code is the confirmation, and the page is where it is visible.
  assert.equal(w.location.hash, '#/directives');
  assert.match(content(doc).textContent, /D5 · Ship it/);
  assert.equal(doc.querySelector('#toast').hidden, false);
  assert.match(doc.querySelector('#toast').textContent, /Created D5/);
});

test('creating from another view navigates to the directives page', async t => {
  const state = {list: [], onCreate: () => {
    const row = directive({id: 'd9', code: 'D9', title: 'From elsewhere'});
    state.list = [row];
    return row;
  }};
  const { w, doc } = await harness(t, state);
  w.location.hash = '#/projects';
  await tick();
  w.directiveTest.directiveForm('');
  await tick();
  doc.querySelector('#f-title').value = 'From elsewhere';
  doc.querySelector('#f-body').value = 'Still lands on the list.';
  submit(w, doc);
  await tick();
  await tick();
  assert.equal(w.location.hash, '#/directives');
  assert.match(content(doc).textContent, /D9 · From elsewhere/);
});
