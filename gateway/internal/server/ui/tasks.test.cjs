const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

async function harness(t, handler = () => ({ tasks: [], nextCursor: null })) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: 'http://piso.local/#/tasks', runScripts: 'outside-only', pretendToBeVisual: true,
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
    if (path.startsWith('/api/v1/tasks')) {
      const call = {path, method: opts.method || 'GET', body: opts.body && JSON.parse(opts.body)};
      calls.push(call);
      result = await handler(call);
    } else if (path.endsWith('/health')) result = {};
    else if (path.endsWith('/technocore')) result = {status: 'active'};
    else if (path.endsWith('/asks')) result = {asks: []};
    else result = [];
    if (result?.errorStatus) return {ok: false, status: result.errorStatus, text: async () => JSON.stringify({error: result.error})};
    return {ok: true, status: 200, json: async () => result};
  };
  w.eval(script + "\nwindow.taskTest = {renderContent, loadTasks};");
  await tick();
  return {w, doc: w.document, calls, eval: code => w.eval(code)};
}
const click = (doc, action) => doc.querySelector(`[data-action="${action}"]`).click();
const submit = (w, doc) => doc.querySelector('#actionForm').dispatchEvent(new w.Event('submit', {bubbles: true, cancelable: true}));

test('Tasks follows Projects; empty, active, finished and pagination are real API views', async t => {
  const h = await harness(t, ({path}) => {
    const url = new URL(path, 'http://piso.local');
    return {tasks: [{id: 'one', title: '<script>unsafe</script>', status: url.searchParams.get('group') === 'finished' ? 'done' : 'open'}], nextCursor: url.searchParams.has('cursor') ? null : 'cursor:2'};
  });
  assert.equal(h.doc.querySelector('[data-view="projects"]').nextElementSibling.dataset.view, 'tasks');
  assert.match(h.doc.querySelector('.task-row').textContent, /Queued/);
  assert.equal(h.doc.querySelector('.task-row script'), null);
  assert.equal(h.w.getComputedStyle(h.doc.querySelector('.task-pagination')).paddingTop, '24px');
  assert.equal(h.doc.querySelector('[data-action="task-prev"]').disabled, true);
  assert.equal(h.w.getComputedStyle(h.doc.querySelector('[data-action="task-prev"]')).cursor, 'default');
  assert.equal(h.w.getComputedStyle(h.doc.querySelector('[data-action="task-next"]')).cursor, 'pointer');
  click(h.doc, 'task-next'); await tick();
  assert.match(h.calls.at(-1).path, /cursor=cursor%3A2/);
  assert.match(h.doc.querySelector('#content').textContent, /Page 2/);
  assert.ok(h.doc.querySelector('.task-pagination')); // Last page still needs Previous.
  assert.equal(h.doc.querySelector('[data-action="task-next"]').disabled, true);
  assert.equal(h.w.getComputedStyle(h.doc.querySelector('[data-action="task-next"]')).cursor, 'default');
  click(h.doc, 'task-prev'); await tick();
  assert.match(h.doc.querySelector('.task-pagination').textContent, /Page 1/);
  h.doc.querySelector('[data-action="task-group"][data-id="finished"]').click(); await tick();
  assert.match(h.doc.querySelector('.task-row').textContent, /Done/);
  assert.doesNotMatch(h.calls.at(-1).path, /cursor=/);
});

test('paginator is absent for empty and single-page results', async t => {
  let tasks = [];
  const h = await harness(t, () => ({tasks, nextCursor: null}));
  assert.equal(h.doc.querySelector('.task-pagination'), null);
  tasks = [{id: 'one', title: 'Only task', status: 'open'}];
  await h.w.taskTest.loadTasks();
  assert.ok(h.doc.querySelector('.task-row'));
  assert.equal(h.doc.querySelector('.task-pagination'), null);
});

test('create submits one UUID and preserves identical payload after ambiguous failure', async t => {
  let posts = 0;
  const h = await harness(t, call => {
    if (call.method === 'POST') {
      if (++posts === 1) throw new Error('timeout');
      return {...call.body, status: 'open'};
    }
    if (/tasks\//.test(call.path)) return {id: 'one', title: 'Release notes', status: 'open', updates: []};
    return {tasks: [], nextCursor: null};
  });
  click(h.doc, 'task-new');
  h.doc.querySelector('#f-title').value = 'Release notes';
  h.doc.querySelector('#f-body').value = 'Draft notes';
  h.doc.querySelector('#f-acceptanceCriteria').value = 'Includes breaking changes';
  submit(h.w, h.doc); await tick();
  assert.equal(h.doc.querySelector('#dialog').open, true);
  assert.equal(h.doc.querySelector('#f-title').readOnly, true);
  assert.match(h.doc.querySelector('#formError').textContent, /same task/);
  submit(h.w, h.doc); await tick();
  const requests = h.calls.filter(c => c.method === 'POST');
  assert.equal(requests.length, 2);
  assert.deepEqual(requests[0].body, requests[1].body);
  assert.match(requests[0].body.id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.equal(requests[0].body.acceptanceCriteria, 'Includes breaking changes');
  assert.equal(requests[0].body.accountId, undefined);
  assert.equal(h.doc.querySelector('#dialog').open, false);
  assert.match(h.w.location.hash, /^#\/tasks\//);
});

test('validation keeps draft editable; closing asks before discarding', async t => {
  const h = await harness(t, call => call.method === 'POST' ? {errorStatus: 400, error: 'invalid title'} : {tasks: [], nextCursor: null});
  click(h.doc, 'task-new');
  assert.equal(h.doc.querySelector('#f-title').maxLength, 300);
  assert.equal(h.doc.querySelector('#f-body').maxLength, 20000);
  h.doc.querySelector('#f-title').value = 'Draft';
  submit(h.w, h.doc); await tick();
  assert.equal(h.doc.querySelector('#f-title').readOnly, false);
  assert.equal(h.doc.querySelector('#f-title').value, 'Draft');
  h.doc.querySelector('#closeDialog').click();
  assert.equal(h.doc.querySelector('#dialog').open, true);
  assert.equal(h.doc.querySelector('#taskDiscard').hidden, false);
  click(h.doc, 'task-discard'); await tick();
  assert.equal(h.doc.querySelector('#dialog').open, false);
});

test('unavailable is not an empty queue and detail escapes text and artifact URLs', async t => {
  const h = await harness(t, call => call.path.includes('/tasks/') ? {
    id: 'one', title: '<img src=x onerror=alert(1)>', status: 'waiting', waitReasons: ['review'],
    body: '<script>bad</script>', acceptanceCriteria: 'Done when tested', result: {summary: 'Ready'},
    artifacts: [{type: 'link', url: 'javascript:alert(1)'}], updates: [],
  } : {errorStatus: 503, error: 'Pair this gateway'});
  assert.match(h.doc.querySelector('#content').textContent, /Pair this gateway/);
  assert.doesNotMatch(h.doc.querySelector('#content').textContent, /No active tasks/);
  h.w.location.hash = '#/tasks/one'; await tick();
  assert.match(h.doc.querySelector('.task-detail').textContent, /Needs review/);
  assert.match(h.doc.querySelector('.task-detail').textContent, /Done when tested/);
  assert.equal(h.doc.querySelector('.task-detail img, .task-detail script, a[href^="javascript:"]'), null);
  assert.ok(h.doc.querySelector('.task-detail a[href="#/attention"]'));
});

test('submit is disabled while pending and background renders do not erase draft', async t => {
  let resolve;
  const h = await harness(t, call => call.method === 'POST' ? new Promise(r => {resolve = r;}) : {tasks: [], nextCursor: null});
  click(h.doc, 'task-new');
  h.doc.querySelector('#f-title').value = 'Keep me';
  submit(h.w, h.doc);
  assert.equal(h.doc.querySelector('#actionForm [type="submit"]').disabled, true);
  submit(h.w, h.doc); // Enter or a second submit event must not post twice.
  assert.equal(h.calls.filter(c => c.method === 'POST').length, 1);
  h.w.taskTest.renderContent();
  assert.equal(h.doc.querySelector('#f-title').value, 'Keep me');
  resolve({id: 'one', title: 'Keep me', status: 'open'}); await tick();
});
