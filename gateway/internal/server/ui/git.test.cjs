const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const {JSDOM} = require('jsdom');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a,b) => script.slice(script.indexOf(a), script.indexOf(b));
function harness(t) {
 const dom = new JSDOM('<div id="content"></div><span id="resultCount"></span>', {url:'http://piso.local/#/access/git'});
 t.after(()=>dom.window.close());
 const calls=[];
 dom.window.scrollTo=()=>{};
 const sandbox={window:dom.window,document:dom.window.document,URL, $:s=>dom.window.document.querySelector(s), matches:()=>true,list:()=>[],
  api:async(path,method,body)=>{calls.push({path,method,body});return {}},changed:async()=>{},notify:()=>{},count:()=>{},
  renderContent:()=>vm.runInContext('renderGit("")',ctx)};
 const ctx=vm.createContext(sandbox);
 vm.runInContext([slice('const esc =','const endpoints'),slice('function button(','function facts('),slice('function empty(','function record('),slice('function replaceContent(','function count('),slice('const gitCapabilities =','      const intros = {')].join('\n'),ctx);
 const run=s=>vm.runInContext(s,ctx);
 const field=(name,value)=>{
  const el=dom.window.document.querySelector(`[data-git-field="${name}"]`);
  assert.ok(el,`field ${name} rendered`);
  if(el.type==='checkbox') el.checked=value; else el.value=value;
  el.dispatchEvent(new dom.window.Event('input',{bubbles:true}));
  el.dispatchEvent(new dom.window.Event('change',{bubbles:true}));
 };
 return {dom,run,field,calls,sandbox};
}
test('Git wizard traverses all four panels, preserves draft on refresh, and saves',async t=>{
 const h=harness(t); h.run('gitStart()');
 h.field('name','My Gitea');h.field('token','test-token');h.field('envKey','TEA_TOKEN');h.field('workers','demo');
 const token=h.dom.window.document.querySelector('[data-git-field="token"]');
 h.run('renderGit("")');assert.equal(h.dom.window.document.querySelector('[data-git-field="token"]'),token);
 h.run('gitGo(1)');h.field('provider','gitea');
 assert.equal(h.dom.window.document.querySelector('#git-step-title').textContent,'Provider and type');
 h.run('gitGo(2)');h.field('baseURL','https://git.example.test');
 h.run('gitGo(3)');assert.match(h.dom.window.document.body.textContent,/Confirm the credential destinations/);
 h.field('confirmHost',true);h.run('gitGo(3)');
 assert.match(h.dom.window.document.body.textContent,/Operator-provided guidance; not verified or enforced by Piso/);
 const hint=h.dom.window.document.querySelector('[data-git-hint="merge"]');hint.value='denied';
 hint.dispatchEvent(new h.dom.window.Event('input',{bubbles:true}));
 h.run('renderGit("")');assert.equal(h.dom.window.document.querySelector('[data-git-hint="merge"]').value,'denied');
 await h.run('gitSave()');
 assert.equal(h.calls.length,1);assert.equal(h.calls[0].path,'git');
 assert.equal(h.calls[0].body.config.provider,'gitea');assert.equal(h.calls[0].body.config.hints.merge,'denied');
 assert.equal(h.calls[0].body.token,'test-token');assert.equal(h.run('gitDraft'),null);
});
test('Git wizard repaints errors and busy state, retains token after failed save',async t=>{
 const h=harness(t);h.run('gitStart();gitGo(1)');assert.match(h.dom.window.document.body.textContent,/Enter a name and token/);
 h.field('name','GitHub');h.field('token','test-token');h.field('envKey','GH_TOKEN');h.run('gitGo(1);gitGo(2)');h.field('confirmHost',true);h.run('gitGo(3)');
 let reject;h.sandbox.api=()=>new Promise((_,r)=>{reject=r});
 const saving=h.run('gitSave()');
 assert.equal(h.dom.window.document.querySelector('.git-card').getAttribute('aria-busy'),'true');
 assert.ok(h.dom.window.document.querySelector('fieldset').disabled);
 reject(new Error('Save failed'));await saving;
 assert.match(h.dom.window.document.body.textContent,/Save failed/);h.run('gitGo(0)');
 assert.equal(h.dom.window.document.querySelector('[data-git-field="token"]').value,'test-token');
});
test('dashboard scripts parse and expose token Git without legacy App/sign-in UI',()=>{
 const scripts=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
 for(const [,source] of scripts) assert.doesNotThrow(()=>new vm.Script(source));
 assert.doesNotMatch(html,/github-app|github-user|githubMappings|githubPending|renderGithub|github-connect|github-import/);
 assert.match(html,/parts\[1\] === "github"[\s\S]*?#\/access\/git/);
 assert.match(html,/Supplied tokens for GitHub and Gitea/);
});
test('changing Git destination clears confirmation',t=>{
 const h=harness(t);h.run('gitStart()');h.field('name','GitHub');h.field('token','test-token');h.field('envKey','GH_TOKEN');h.run('gitGo(2)');
 h.field('confirmHost',true);h.field('baseURL','https://enterprise.example.test');
 assert.equal(h.run('gitDraft.confirmHost'),false);
});
