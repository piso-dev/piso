const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { JSDOM } = require('jsdom');
const script = fs.readFileSync(`${__dirname}/index.html`, 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a, b) => script.slice(script.indexOf(a), script.indexOf(b));

function harness(t) {
 const dom = new JSDOM('<dialog id="dialog"><h2 id="dialogTitle"></h2><div id="dialogBody"></div></dialog><button id="closeDialog"></button><div id="toast"></div>', {url:'http://piso.local'});
 t.after(() => dom.window.close());
 const calls = [], opened = [];
 const context = { document:dom.window.document, window:dom.window, URL, FormData:dom.window.FormData,
  $: s => dom.window.document.querySelector(s), data:{githubUser:{configured:true, connected:true, callbackUrl:'http://127.0.0.1:8080/api/v1/github-user/callback'}},
  dialogBusy:false, notify:()=>{}, closeDialog:()=>{}, changed:async()=>{},
  mountDialog: (title, body) => { dom.window.document.querySelector('#dialogBody').innerHTML = body; },
  openExternal: url => opened.push(url), safeURL: url => url || '',
  api: async (path, method='GET', body) => { calls.push({path, method, body}); return {login:'octocat', orgs:[{login:'acme', id:1}]}; },
  esc: v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
  // refresh() delegates to fetchKey and reads the pairing gate; give this
  // slice the same pieces the page provides.
  coordinatorKeys: new Set(),
  coordinatorState: () => ({tc:{}, known:true, status:'active', ready:true}),
  taskUI: {rows:[], detail:null, cursor:'', previous:[], next:null, error:''},
  view: 'projects',
 };
 vm.createContext(context);
 vm.runInContext(slice('function button(', 'function facts(') + slice('function field(', 'async function refresh(') + slice('async function githubUserForm(', 'function providerForm(') + slice('async function githubConnect(', 'async function githubInstall('), context);
 return {context, document:dom.window.document, calls, opened, window:dom.window};
}

test('status refresh preserves session on failure, recovers, and honors confirmed sign-out', async t => {
 const h = harness(t);
 Object.assign(h.context, {
  endpoints:{githubUser:'github-user'}, pending:new Map(), loaded:new Set(), failures:new Map(),
  renderGlobal:()=>{}, renderContent:()=>{}, lastRefresh:0,
 });
 vm.runInContext(slice('function fetchKey(', 'async function refresh(') + slice('async function refresh(', 'async function health('), h.context);
 const session = h.context.data.githubUser;
 h.context.api = async () => { throw new Error('GitHub verification unavailable'); };
 await vm.runInContext('refresh()', h.context);
 assert.equal(h.context.data.githubUser, session);
 assert.equal(h.context.data.githubUser.connected, true);
 assert.equal(h.context.failures.get('githubUser'), 'GitHub verification unavailable');
 assert.equal(h.context.pending.size, 0);
 h.context.api = async () => ({...session, connected:true});
 await vm.runInContext('refresh()', h.context);
 assert.equal(h.context.data.githubUser.connected, true);
 assert.equal(h.context.failures.has('githubUser'), false);
 h.context.api = async () => ({...session, connected:false});
 await vm.runInContext('refresh()', h.context);
 assert.equal(h.context.data.githubUser.connected, false);
 h.context.api = async () => { throw new Error('Network unavailable'); };
 await vm.runInContext('refresh()', h.context);
 assert.equal(h.context.data.githubUser.connected, false);
});

test('sign-in action navigates via GET on callback origin, not a JSON POST', t => {
 const h = harness(t);
 vm.runInContext('githubUserStart()', h.context);
 assert.deepEqual(h.opened, ['http://127.0.0.1:8080/api/v1/github-user/authorize']);
 assert.equal(h.calls.length, 0);
 assert.match(script, /name === "github-user-authorize"\) return githubUserStart\(\)/);
});

test('OAuth setup shows exact callback and submits required gateway-only configuration', async t => {
 const h = harness(t);
 await vm.runInContext('githubUserForm()', h.context);
 assert.match(h.document.body.textContent, /http:\/\/127.0.0.1:8080\/api\/v1\/github-user\/callback/);
 const link = h.document.querySelector('#actionForm a');
 assert.equal(link.href, 'https://github.com/settings/applications/new');
 assert.equal(link.target, '_blank');
 assert.match(link.rel, /noopener/);
 const clientId = h.document.querySelector('[name="clientId"]');
 assert.ok(link.compareDocumentPosition(clientId) & h.window.Node.DOCUMENT_POSITION_FOLLOWING);
 assert.equal(clientId.type, 'text');
 assert.equal(clientId.autocomplete, 'off');
 assert.equal(clientId.getAttribute('spellcheck'), 'false');
 assert.equal(clientId.getAttribute('data-lpignore'), 'true');
 assert.ok(clientId.hasAttribute('data-1p-ignore'));
 assert.equal(h.document.querySelector('form').getAttribute('autocomplete'), 'off');
 assert.ok(h.document.querySelector('.github-callback #github-callback-value'));
 const secret = h.document.querySelector('[name="clientSecret"]');
 assert.equal(secret.type, 'password'); assert.equal(secret.required, true);
 h.document.querySelector('[name="clientId"]').value = 'client'; secret.value = 'secret';
 const form = h.document.querySelector('form');
 await form.onsubmit({preventDefault(){}, currentTarget:form});
 assert.equal(h.calls[0].path, 'github-user/config'); assert.equal(h.calls[0].method, 'PUT');
 assert.deepEqual(JSON.parse(JSON.stringify(h.calls[0].body)), {clientId:'client', clientSecret:'secret'});
});

test('callback copy uses Clipboard API and announces success', async t => {
 const h = harness(t);
 let copied;
 Object.defineProperty(h.window.navigator, 'clipboard', {value:{writeText:async value => { copied = value; }}});
 await vm.runInContext('githubUserForm()', h.context);
 const button = h.document.querySelector('#github-callback-copy');
 assert.equal(button.type, 'button');
 await button.onclick();
 assert.equal(copied, h.context.data.githubUser.callbackUrl);
 assert.equal(h.document.querySelector('#github-callback-status').textContent, 'Copied.');
 assert.equal(h.calls.length, 0);
});

test('callback copy works on HTTP and reports failures without claiming success', async t => {
 const h = harness(t);
 await vm.runInContext('githubUserForm()', h.context);
 h.document.execCommand = command => {
  assert.equal(command, 'copy');
  assert.equal(h.document.querySelector('textarea').value, h.context.data.githubUser.callbackUrl);
  return true;
 };
 await h.document.querySelector('#github-callback-copy').onclick();
 assert.equal(h.document.querySelector('#github-callback-status').textContent, 'Copied.');
 assert.equal(h.document.querySelector('textarea'), null);
 h.document.execCommand = () => false;
 await h.document.querySelector('#github-callback-copy').onclick();
 assert.match(h.document.querySelector('#github-callback-status').textContent, /copy it manually/);
 assert.equal(h.document.querySelector('textarea'), null);
});

test('missing callback disables copy rather than copying an example', async t => {
 const h = harness(t);
 delete h.context.data.githubUser.callbackUrl;
 await vm.runInContext('githubUserForm()', h.context);
 assert.equal(h.document.querySelector('#github-callback-copy').disabled, true);
});

test('create app picker uses discovered owners and posts selected org with routing group', async t => {
 const h = harness(t);
 let submitted;
 h.window.open = () => ({name:'', close(){}});
 h.window.HTMLFormElement.prototype.submit = function() { submitted = {action:this.action, manifest:this.querySelector('[name="manifest"]').value}; };
 const api = h.context.api;
 h.context.api = async (path, method, body) => {
  if (path === 'github-app/manifest') { h.calls.push({path, method, body}); return {action:'https://github.com/organizations/acme/settings/apps/new?state=nonce', manifest:{public:false}}; }
  return api(path, method, body);
 };
 await vm.runInContext('githubConnect("draft", "group")', h.context);
 const select = h.document.querySelector('[name="account"]');
 assert.deepEqual([...select.options].map(o=>o.value), ['', 'octocat', 'acme']);
 assert.equal(select.required, true);
 select.value = 'acme';
 const form = h.document.querySelector('#actionForm');
 await form.onsubmit({preventDefault(){}, currentTarget:form});
 const call = h.calls.find(c=>c.path==='github-app/manifest');
 assert.equal(call.method, 'POST');
 assert.deepEqual(JSON.parse(JSON.stringify(call.body)), {id:'draft', secretId:'group', account:'acme'});
 assert.match(submitted.action, /organizations\/acme\/settings\/apps\/new/);
 assert.equal(JSON.parse(submitted.manifest).public, false);
});

test('organization discovery failure does not open registration or a fake picker', async t => {
 const h = harness(t);
 h.context.api = async () => { throw new Error('Sign in again'); };
 await assert.rejects(vm.runInContext('githubConnect()', h.context), /Sign in again/);
 assert.equal(h.document.querySelector('select'), null);
 assert.equal(h.opened.length, 0);
});
