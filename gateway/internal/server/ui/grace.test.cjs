const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

// A task in its grace period: visible on the board, invisible to workers. The
// page must show the countdown, offer "make available now", and — while the
// detail page is open — beat every five seconds so the task is not released
// from under the person reading it.
const task = (over = {}) => ({
  id: 't1', title: 'Wire the grace period', body: 'Add the countdown.', status: 'open',
  priority: 0, labels: {}, artifacts: [], updates: [], graceHeld: 'timer',
  availableAt: new Date(Date.now() + 21000).toISOString(), releasedAt: null,
  viewedAt: null, viewedBy: null, pausedAt: null, createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z', ...over,
});

async function harness(t, {hash, tasks = [], detail = null, release = null}) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: 'http://piso.local/' + hash, runScripts: 'outside-only', pretendToBeVisual: true,
  });
  t.after(() => dom.window.close());
  const w = dom.window, calls = [], timers = new Map();
  // The board is a live read of the queue: releasing a task changes what the
  // next list request answers, exactly as the coordinator would.
  const state = {tasks: [...tasks]};
  let nextTimer = 1;
  w.scrollTo = () => {};
  w.EventSource = class { addEventListener() {} };
  w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  w.HTMLDialogElement.prototype.close = function () { this.open = false; };
  w.AbortSignal.timeout = () => new w.AbortController().signal;
  w.setInterval = (fn, ms) => { const id = nextTimer++; timers.set(id, {fn, ms}); return id; };
  w.clearInterval = id => timers.delete(id);
  w.fetch = async (path, opts = {}) => {
    const method = opts.method || 'GET';
    const body = opts.body ? JSON.parse(opts.body) : undefined;
    if (path.startsWith('/api/v1/tasks')) calls.push({path, method, body});
    let result = [];
    if (path.endsWith('/health')) result = {};
    else if (path.endsWith('/technocore')) result = {status: 'active'};
    else if (path.endsWith('/asks')) result = {asks: []};
    else if (path.match(/\/tasks\/t1\/view$/)) result = {id: 't1', graceHeld: 'viewing'};
    else if (path.match(/\/tasks\/t1\/available$/)) {
      result = release || task({graceHeld: null, availableAt: null, releasedAt: new Date().toISOString()});
      state.tasks = state.tasks.map(row => (row.id === 't1' ? result : row));
    } else if (path.match(/^\/api\/v1\/tasks\/t1$/)) result = detail || task();
    else if (path.includes('group=active')) result = {tasks: state.tasks};
    else if (path.includes('group=finished')) result = {tasks: []};
    return {ok: true, status: method === 'POST' ? 201 : 200, text: async () => '', json: async () => result};
  };
  w.eval(script + '\nwindow.graceTest = {tickGrace, renderTasks, taskUI, graceRemaining, clockText};');
  await tick();
  await tick();
  return {w, doc: w.document, calls, timers, tickGrace: w.graceTest.tickGrace};
}

const content = doc => doc.querySelector('#content');
const beatTimers = timers => [...timers.entries()].filter(([, v]) => v.ms === 5000);
const viewCalls = calls => calls.filter(c => c.path.endsWith('/t1/view'));

test('a held card shows one green panel: clock top left, release bottom right', async t => {
  const {doc, calls, w} = await harness(t, {hash: '#/tasks', tasks: [task()]});
  const panel = content(doc).querySelector('.card .grace-panel');
  assert.ok(panel, 'no grace panel on the card');
  assert.doesNotMatch(panel.className, /held/);
  // One panel, not a chip plus a loose button: the state and its action are the
  // same object, so they cannot drift apart in the kicker or the verb row.
  assert.equal(content(doc).querySelectorAll('.grace-panel').length, 1);
  const state = panel.firstElementChild;
  assert.match(state.className, /grace-state/, 'the clock is not the first line of the panel');
  assert.match(state.textContent, /0:2[01]/);
  const action = panel.lastElementChild;
  assert.match(action.className, /grace-action/, 'the button is not the last row of the panel');
  const release = action.querySelector('button[data-action="task-available"]');
  assert.ok(release, 'no "make available now" control in the panel');
  assert.match(release.textContent, /Make available now/);
  // ...and it is not duplicated in the card's verb row.
  assert.equal([...content(doc).querySelectorAll('button[data-action="task-available"]')].length, 1);

  release.click();
  await tick();
  await tick();
  assert.ok(calls.some(c => c.method === 'POST' && c.path === '/api/v1/tasks/t1/available'),
    'the button did not post the release');
  // The row is re-read rather than patched: the board shows the coordinator's
  // answer, not the browser's guess about the clock.
  assert.equal(content(doc).querySelector('.grace-panel'), null);
  assert.match(doc.querySelector('#toast').textContent, /available to workers now/i);
});

test('a viewed task holds open instead of counting down', async t => {
  const {doc} = await harness(t, {
    hash: '#/tasks',
    tasks: [task({graceHeld: 'viewing', availableAt: new Date(Date.now() - 1000).toISOString(),
      viewedBy: 'Ada'})],
  });
  const panel = content(doc).querySelector('.grace-panel.held');
  assert.ok(panel, 'no held-open panel on the card');
  assert.match(panel.querySelector('.grace-state').textContent, /Held open by Ada/);
  assert.doesNotMatch(panel.querySelector('.grace-state').textContent, /\d+:\d\d/);
});

test('the detail page beats every five seconds and says goodbye when it closes', async t => {
  const {w, doc, calls, timers} = await harness(t, {
    hash: '#/tasks/t1',
    detail: task({graceHeld: 'viewing', viewedBy: 'Ada'}),
  });
  // The detail page carries the same panel as the card it came from.
  const panel = content(doc).querySelector('.task-detail > .grace-panel');
  assert.ok(panel, 'no grace panel on the detail page');
  assert.match(panel.textContent, /It stays out of the queue while you have it open/);
  assert.equal(viewCalls(calls).filter(c => c.method === 'POST').length, 1, 'no beat on open');
  const beats = beatTimers(timers);
  assert.equal(beats.length, 1, 'expected exactly one 5s heartbeat timer');
  const [, timer] = beats[0];
  timer.fn();
  timer.fn();
  await tick();
  assert.equal(viewCalls(calls).filter(c => c.method === 'POST').length, 3, 'heartbeat stopped early');

  // Leaving the detail view is the person saying they are done: the hold lifts
  // now rather than after the stale window.
  w.location.hash = '#/tasks';
  w.dispatchEvent(new w.Event('hashchange'));
  await tick();
  await tick();
  assert.ok(viewCalls(calls).some(c => c.method === 'DELETE'), 'leaving did not release the view hold');
  assert.equal(beatTimers(timers).length, 0, 'the heartbeat kept running after leaving');
});

test('the countdown ticks in place and asks the server once when it hits zero', async t => {
  const {doc, calls, tickGrace} = await harness(t, {
    hash: '#/tasks',
    tasks: [task({availableAt: new Date(Date.now() - 1000).toISOString()})],
  });
  const chip = content(doc).querySelector('.grace-text');
  assert.equal(chip.textContent, '0:00');
  const before = calls.filter(c => c.method === 'GET' && c.path.includes('group=active')).length;
  tickGrace();
  tickGrace();
  await tick();
  const after = calls.filter(c => c.method === 'GET' && c.path.includes('group=active')).length;
  assert.equal(after - before, 1, 'the expiry should trigger one refresh, not one per tick');
});

test('a ten-minute default renders as minutes, not a bare seconds count', async t => {
  const {doc} = await harness(t, {
    hash: '#/tasks',
    tasks: [task({availableAt: new Date(Date.now() + 600000).toISOString()})],
  });
  const state = content(doc).querySelector('.grace-state');
  assert.equal(state.textContent.trim(), '◷10:00');
});

test('a task with no grace period has no grace chrome at all', async t => {
  const {doc} = await harness(t, {
    hash: '#/tasks',
    tasks: [task({graceHeld: null, availableAt: null})],
  });
  assert.equal(content(doc).querySelector('.grace'), null);
  assert.equal([...content(doc).querySelectorAll('button')]
    .filter(b => b.dataset.action === 'task-available').length, 0);
});
