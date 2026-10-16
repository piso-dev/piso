// Run with jsdom installed: NODE_PATH=<node_modules> node scripts/project-visibility-test.cjs
const { JSDOM, VirtualConsole } = require('jsdom');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const errors = [];
const virtualConsole = new VirtualConsole();
virtualConsole.on('jsdomError', e => errors.push(e.message));
const workers = [
  { name: 'piso-worker-alpha', slug: 'alpha' },
  { name: 'piso-worker-beta', slug: 'beta', projectHidden: true },
];
const activities = ['alpha', 'beta', 'removed'].map(slug => ({
  id: slug, slug, kind: 'waiting', text: 'Needs input', ts: Date.now(),
}));
const calls = [];
const dom = new JSDOM(fs.readFileSync(path.join(__dirname, '../gateway/internal/server/ui/index.html'), 'utf8'), {
  url: 'https://dashboard.test', runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole,
  beforeParse(w) {
    w.AbortSignal = AbortSignal;
    w.EventSource = class { addEventListener() {} };
    w.scrollTo = () => {};
    w.fetch = async (url, opts) => {
      const endpoint = url.replace('/api/v1/', '');
      let body = [];
      if (endpoint.startsWith('projects/')) {
        calls.push([endpoint, opts.method, JSON.parse(opts.body)]);
        const slug = endpoint.split('/')[1];
        workers.find(w => w.slug === slug).projectHidden = JSON.parse(opts.body).hidden;
        body = { ok: true };
      } else if (endpoint === 'workers') body = workers;
      else if (endpoint.startsWith('activities')) body = activities;
      else if (endpoint === 'health') body = {};
      return { ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(body)) };
    };
  },
});
(async () => {
  await new Promise(resolve => setTimeout(resolve, 50));
  const w = dom.window;
  const rows = () => [...w.document.querySelectorAll('.timeline-group')].map(e => e.dataset.slug);
  const filter = value => {
    const select = w.document.querySelector('#projectFilter');
    select.value = value;
    select.dispatchEvent(new w.Event('change'));
  };
  assert.deepEqual(rows(), ['alpha'], 'hidden and unregistered projects must be absent');
  const hide = w.document.querySelector('[data-action="project-hide"]');
  assert.equal(hide.textContent, '×');
  assert.equal(hide.getAttribute('aria-label'), 'Hide project alpha');
  assert.equal(hide.title, 'Hide project alpha');
  hide.focus();
  assert.equal(w.document.activeElement, hide);
  filter('needs');
  assert.deepEqual(rows(), ['alpha'], 'attention filter must exclude hidden projects');
  filter('hidden');
  assert.deepEqual(rows(), ['beta']);
  const restore = w.document.querySelector('[data-action="project-restore"]');
  assert.equal(restore.textContent, 'Restore');
  assert.equal(restore.getAttribute('aria-label'), 'Restore project beta');
  assert(!restore.classList.contains('project-visibility'));
  await w.eval('action("project-restore", "beta")');
  assert.deepEqual(rows(), []);
  filter('all');
  assert.deepEqual(rows(), ['alpha', 'beta']);
  await w.eval('action("project-hide", "alpha")');
  assert.deepEqual(rows(), ['beta']);
  await w.eval('refresh(["workers", "activities"])');
  assert.deepEqual(rows(), ['beta'], 'refresh must preserve visibility');
  filter('hidden');
  assert.deepEqual(rows(), ['alpha']);
  await w.eval('action("project-restore", "alpha")');
  filter('all');
  workers.splice(0, 1);
  await w.eval('refresh(["workers", "activities"])');
  assert.deepEqual(rows(), ['beta'], 'piso down must remove the row despite remaining history');
  workers.push({name: 'piso-worker-alpha', slug: 'alpha'});
  await w.eval('refresh(["workers"])');
  assert.deepEqual(rows(), ['alpha', 'beta'], 'piso up should restore the project');
  activities.length = 0;
  await w.eval('refresh(["activities"])');
  assert.deepEqual(rows(), ['alpha', 'beta'], 'inactive projects remain manageable');
  assert.equal(w.document.querySelectorAll('[data-action="project-hide"]').length, 2);
  assert.equal(calls.length, 3);
  assert(calls.every(([, method]) => method === 'POST'));
  assert.deepEqual(errors, []);
  console.log('PASS: visible/hidden/attention filters, hide/restore, refresh persistence, down/up with retained history');
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(() => dom.window.close());
