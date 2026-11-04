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

test('a host request shows the command to run, not a paragraph about it', async t => {
  const h = await harness(t, {
    tasks: [task('Paused on the host', 'waiting', {waitReasons: ['human_action']})],
    asks: [
      // A human action carries its detail in ``instructions``, so a board that
      // only reads ``question``/``detail`` would show this card with no body at
      // all — and the one thing a person needs is the command.
      {
        id: 'h1', kind: 'human', category: 'host', taskId: 'Paused on the host',
        title: 'Host: docker build', instructions: 'docker build -t piso .',
        verification: '/workspace/build.log',
      },
      // A host ask with no task is what a worker files mid-investigation.
      {
        id: 'h2', kind: 'human', category: 'host', title: 'Host: pair the rack key',
        instructions: 'piso-pair --token abc', verification: '/workspace/pair.txt',
      },
      // Non-host human asks keep the prose treatment: their text is a question
      // to a person, not a command to paste.
      {id: 'h3', kind: 'human', category: 'dns', title: 'Prove the domain', instructions: 'Add the TXT record.'},
    ],
  });
  const linked = cardByTitle(h.doc, 'Paused on the host');
  assert.equal(linked.dataset.type, 'action');
  assert.equal(linked.querySelector('.card-command code').textContent, 'docker build -t piso .');
  assert.match(linked.querySelector('.card-result').textContent, /\/workspace\/build\.log/);
  const loose = cardByTitle(h.doc, 'Host: pair the rack key');
  assert.equal(loose.querySelector('.card-command code').textContent, 'piso-pair --token abc');
  assert.match(loose.querySelector('.card-result').textContent, /\/workspace\/pair\.txt/);
  // The verbs are the ones a human ask already has: a host request adds no new
  // way to say no.
  const verbs = [...loose.querySelectorAll('.card-actions button')].map(b => b.dataset.action);
  assert.deepEqual(verbs, ['ask-completed', 'ask-deferred', 'ask-rejected']);
  const prose = cardByTitle(h.doc, 'Prove the domain');
  assert.equal(prose.querySelector('.card-command'), null);
  assert.match(prose.querySelector('.card-note').textContent, /Add the TXT record\./);
});

test('scripts copy exact contents or filename without completing the ask', async t => {
  const contents = '#!/usr/bin/env bash\n' + 'printf "<safe>\\n"\n'.repeat(250) + '# end\n';
  const h = await harness(t, {asks: [
    {id: 'inline', kind: 'human', category: 'host', title: 'Inline script', explanation: 'Needs Docker. Save as action.sh and run bash action.sh.', script: {kind: 'inline', filename: 'action.sh', language: 'bash', contents}},
    {id: 'file', kind: 'human', category: 'host', title: 'Script file', explanation: 'Run bash .piso/host-actions/action.sh from the project.', script: {kind: 'file', path: '.piso/host-actions/action.sh', language: 'bash'}},
  ]});
  let copied;
  Object.defineProperty(h.w.navigator, 'clipboard', {value: {writeText: async value => {copied = value;}}});
  const inline = cardByTitle(h.doc, 'Inline script');
  assert.equal(inline.querySelector('.card-command code').textContent, contents);
  // The cap lives in the parsed stylesheet, not in a text measurement: the
  // command rule itself must cap at 300px, scroll internally, and carry no
  // line-clamp that would truncate it.
  const commandRule = [...h.doc.styleSheets].flatMap((sheet) => [...sheet.cssRules])
    .find((rule) => rule.selectorText === '.card-command');
  assert.ok(commandRule, '.card-command rule must exist in the stylesheet');
  assert.equal(commandRule.style.getPropertyValue('max-height'), '300px');
  assert.equal(commandRule.style.getPropertyValue('overflow'), 'auto');
  assert.equal(commandRule.style.getPropertyValue('-webkit-line-clamp'), '');
  assert.equal(commandRule.style.getPropertyValue('line-clamp'), '');
  assert.equal(h.w.getComputedStyle(inline.querySelector('.host-panel')).maxHeight, '300px');
  assert.equal(h.w.getComputedStyle(inline.querySelector('.card-command')).overflow, 'auto');
  assert.equal(h.w.getComputedStyle(inline.querySelector('.host-request-body')).maxHeight, '420px');
  inline.querySelector('[data-action="host-copy"]').click();
  await tick();
  assert.equal(copied, contents);
  assert.equal(inline.querySelector('[role="status"]').textContent, 'Copied.');
  assert.ok(inline.querySelector('[data-action="ask-completed"]'));
  const file = cardByTitle(h.doc, 'Script file');
  file.querySelector('[data-action="host-copy"]').click();
  await tick();
  assert.equal(copied, '.piso/host-actions/action.sh');
  assert.match(file.querySelector('.host-explanation').textContent, /Run bash/);
});

test('polls keep host and script boxes mounted while updating changed content', async t => {
  const asks = [
    {id: 'inline', kind: 'human', category: 'host', title: 'Inline script', explanation: 'Run on the host.', script: {kind: 'inline', filename: 'action.sh', language: 'bash', contents: 'echo first\n'.repeat(100)}},
    {id: 'file', kind: 'human', category: 'host', title: 'Script file', script: {kind: 'file', path: '.piso/action.sh', language: 'bash'}},
  ];
  const h = await harness(t, {asks});
  const inline = cardByTitle(h.doc, 'Inline script');
  const body = inline.querySelector('.host-request-body');
  const code = inline.querySelector('.card-command');
  const file = cardByTitle(h.doc, 'Script file').querySelector('.card-command');
  body.scrollTop = 80;
  code.scrollTop = 120;
  code.scrollLeft = 50;
  file.scrollLeft = 30;
  code.focus();
  const range = h.doc.createRange();
  range.selectNodeContents(code.querySelector('code'));
  h.w.getSelection().addRange(range);
  const selected = h.w.getSelection().toString();
  for (let i = 0; i < 3; i++) {
    await h.w.typeTest.loadTasks();
    assert.equal(cardByTitle(h.doc, 'Inline script'), inline);
    assert.equal(inline.querySelector('.host-request-body'), body);
    assert.equal(inline.querySelector('.card-command'), code);
    assert.equal(cardByTitle(h.doc, 'Script file').querySelector('.card-command'), file);
    assert.equal(body.scrollTop, 80);
    assert.equal(code.scrollTop, 120);
    assert.equal(code.scrollLeft, 50);
    assert.equal(file.scrollLeft, 30);
    assert.equal(h.doc.activeElement, code);
    assert.equal(h.w.getSelection().toString(), selected);
  }
  asks[0].explanation = 'Updated instructions.';
  asks[0].script.contents = 'echo changed';
  asks.reverse();
  await h.w.typeTest.loadTasks();
  assert.equal(inline.querySelector('.card-command'), code);
  assert.equal(code.textContent, 'echo changed');
  assert.equal(inline.querySelector('.host-explanation').textContent, 'Updated instructions.');
  asks.splice(asks.findIndex(a => a.id === 'inline'), 1);
  await h.w.typeTest.loadTasks();
  assert.equal(cardByTitle(h.doc, 'Inline script'), undefined);
  assert.equal(cardByTitle(h.doc, 'Script file').querySelector('.card-command'), file);
});

test('HTTP clipboard fallback restores focus and failures stay inline', async t => {
  const h = await harness(t, {asks: [{id: 'fallback', kind: 'human', category: 'host', title: 'Fallback', instructions: 'echo hello\necho world'}]});
  const card = cardByTitle(h.doc, 'Fallback');
  const button = card.querySelector('[data-action="host-copy"]');
  let copied;
  h.doc.execCommand = command => {assert.equal(command, 'copy'); copied = h.doc.querySelector('textarea').value; return true;};
  button.focus(); button.click(); await tick();
  assert.equal(copied, 'echo hello\necho world');
  assert.equal(h.doc.activeElement, button);
  assert.equal(h.doc.querySelector('textarea'), null);
  h.doc.execCommand = () => false;
  button.click(); await tick();
  assert.match(card.querySelector('[role="status"]').textContent, /Could not copy/);
  assert.equal(h.doc.querySelector('textarea'), null);
});

test('a host command is escaped and keeps its newlines', async t => {
  const h = await harness(t, {
    tasks: [],
    asks: [{
      id: 'h4', kind: 'human', category: 'host', title: 'Host: two steps',
      instructions: 'git fetch origin\n<script>alert(1)</script>',
      verification: '/workspace/fetch.log',
    }],
  });
  const card = cardByTitle(h.doc, 'Host: two steps');
  const code = card.querySelector('.card-command code');
  // A command is untrusted text from a worker: it must land as text, never as
  // markup, and the newline that separates two steps must survive.
  assert.equal(code.querySelector('script'), null);
  assert.equal(code.textContent, 'git fetch origin\n<script>alert(1)</script>');
});
