const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { JSDOM } = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const tick = () => new Promise(resolve => setTimeout(resolve, 20));

const task = (id, status, extra = {}) => ({
  id, title: id, body: '', status, priority: 0, labels: {}, artifacts: [],
  updates: [], createdAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:00Z',
  ...extra,
});

async function harness(t, {tasks = [], finished = [], asks = [], plans = [], mcpPending = []} = {}) {
  const dom = new JSDOM(html.replace(/<script>[\s\S]*<\/script>/, ''), {
    url: 'http://piso.local/#/tasks', runScripts: 'outside-only', pretendToBeVisual: true,
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
    else if (path.endsWith('/technocore')) result = {status: 'active'};
    else if (path.endsWith('/asks')) result = {asks};
    else if (path.endsWith('/ingress/requests')) result = plans;
    else if (path.endsWith('/mcp/pending')) result = mcpPending;
    else if (path.includes('group=finished')) result = {tasks: finished, nextCursor: null};
    else if (path.includes('group=active')) result = {tasks, nextCursor: null};
    return {ok: true, status: 200, json: async () => result};
  };
  w.eval(script + "\nwindow.typeTest = {loadTasks};");
  await tick();
  await tick();
  return {w, doc: w.document};
}

const cardByTitle = (doc, title) => [...doc.querySelectorAll('.board .card')]
  .find(c => c.querySelector('.card-title')?.textContent === title);
const chipOf = card => card?.querySelector('.type-chip')?.textContent || '';

test('card colour names the item type, not its status', async t => {
  const h = await harness(t, {
    tasks: [
      task('Queued work', 'open'),
      task('Running work', 'working'),
      task('Paused work', 'open', {pausedAt: '2026-01-01T00:00:00Z'}),
      task('Proposed work', 'proposed'),
    ],
    finished: [task('Finished work', 'done'), task('Failed work', 'failed')],
  });
  // One type, many states: all five are work, so all carry the same rule and
  // no chip at all. This is the assertion the status-tinting version failed.
  const work = ['Queued work', 'Running work', 'Paused work', 'Finished work', 'Failed work'];
  for (const title of work) {
    assert.equal(cardByTitle(h.doc, title).dataset.type, 'work', title);
    assert.equal(chipOf(cardByTitle(h.doc, title)), '', `${title} needs no chip`);
  }
  // A proposed task is a different kind of thing even though it is also a task.
  const proposal = cardByTitle(h.doc, 'Proposed work');
  assert.equal(proposal.dataset.type, 'proposal');
  assert.equal(chipOf(proposal), 'Proposal');
});

test('each request type is named and chipped from the ask it carries', async t => {
  const h = await harness(t, {
    tasks: [
      task('Review me', 'waiting', {waitReasons: ['review']}),
      task('Answer me', 'waiting', {waitReasons: ['needs_input']}),
      task('Decide me', 'waiting', {waitReasons: ['approval']}),
    ],
    asks: [
      {id: 'a1', kind: 'task_wait', reason: 'review', taskId: 'Review me', title: 'Review me'},
      {id: 'a2', kind: 'task_wait', reason: 'needs_input', taskId: 'Answer me', title: 'Answer me', question: 'Which region?'},
      {id: 'a3', kind: 'approval', action: 'task.cancel', taskId: 'Decide me', title: 'Decide me'},
      // A loose ask has no task on the board; it must still be typed.
      {id: 'a4', kind: 'human', title: 'Rotate the rack key', instructions: 'Only you have the key.'},
      {id: 'a5', kind: 'approval', action: 'task.propose', title: 'Proposal without a task row'},
    ],
  });
  const type = title => cardByTitle(h.doc, title).dataset.type;
  assert.equal(type('Review me'), 'review');
  assert.equal(chipOf(cardByTitle(h.doc, 'Review me')), 'Review');
  assert.equal(type('Answer me'), 'question');
  assert.equal(chipOf(cardByTitle(h.doc, 'Answer me')), 'Question');
  assert.equal(type('Decide me'), 'decision');
  assert.equal(chipOf(cardByTitle(h.doc, 'Decide me')), 'Decision');
  const loose = cardByTitle(h.doc, 'Rotate the rack key');
  assert.equal(loose.dataset.type, 'action');
  assert.equal(chipOf(loose), 'Action');
  const proposalAsk = cardByTitle(h.doc, 'Proposal without a task row');
  assert.equal(proposalAsk.dataset.type, 'proposal');
  assert.equal(chipOf(proposalAsk), 'Proposal');
});

test('preview and credential requests keep their own types', async t => {
  const h = await harness(t, {
    plans: [{id: 'p1', name: 'web', url: 'https://web.piso.local'}],
    mcpPending: [{name: 'github', url: 'https://mcp.example.com/mcp'}],
    tasks: [],
  });
  const preview = cardByTitle(h.doc, 'Open preview web');
  assert.equal(preview.dataset.type, 'preview');
  assert.equal(chipOf(preview), 'Preview');
  // An access grant is not work and not a decision: it hands over a credential,
  // so it keeps its own quieter hue rather than borrowing "Decision".
  const access = cardByTitle(h.doc, 'Authorize MCP server github');
  assert.equal(access.dataset.type, 'access');
  assert.equal(chipOf(access), 'Access');
});
