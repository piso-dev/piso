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
  mountDialog:(title,html)=>{sandbox.dialog={title,html}},
  api:async(path,method,body)=>{calls.push({path,method,body});return {}},changed:async()=>{},notify:()=>{},count:()=>{},
  renderContent:()=>vm.runInContext('renderGit("")',ctx)};
 const ctx=vm.createContext(sandbox);
 vm.runInContext([slice('const esc =','const endpoints'),slice('function badge(','function facts('),slice('function empty(','function record('),slice('function replaceContent(','function count('),slice('const gitCapabilities =','      const intros = {')].join('\n'),ctx);
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
const connection = {id:'git-1',name:'Engineering <Git>',envKey:'GH_TOKEN',workers:['alpha','beta'],config:{provider:'github',baseURL:'https://github.com',hints:{read:'allowed',merge:'denied'}}};
test('Git rows separate metadata and actions and open capability hints in a modal',t=>{
 const h=harness(t);h.sandbox.list=()=>[connection];h.run('renderGit("")');
 const doc=h.dom.window.document;
 assert.equal(doc.querySelector('.git-record-head h3').textContent,connection.name);
 assert.deepEqual([...doc.querySelectorAll('.git-record-meta dt')].map(el=>el.textContent),['Instance URL','Environment key','Worker scope']);
 assert.equal(doc.querySelectorAll('.git-record-workers span').length,2);
 assert.equal(doc.querySelector('.git-record details'),null);
 assert.equal(doc.querySelector('[data-action="git-capabilities"]').getAttribute('aria-haspopup'),'dialog');
 assert.match(script,/if \(name === "git-capabilities"\) return gitShowCapabilities\(id\)/);
 h.run('gitShowCapabilities("git-1")');
 assert.equal(h.sandbox.dialog.title,'Capability hints');
 const body=new JSDOM(h.sandbox.dialog.html);t.after(()=>body.window.close());
 assert.equal(body.window.document.querySelectorAll('.git-capabilities-list > div').length,10);
 assert.equal(body.window.document.querySelector('[data-hint="allowed"]').textContent,'Allowed');
 assert.equal(body.window.document.querySelector('[data-hint="denied"]').textContent,'Not allowed');
 assert.equal(body.window.document.querySelectorAll('[data-hint="unknown"]').length,8);
 assert.match(body.window.document.body.textContent,/not verified or enforced by Piso/);
 assert.equal(body.window.document.querySelector('[data-action="close"]').textContent,'Done');
 assert.equal(doc.querySelector('git'),null);
});
test('Git hints default to unknown and empty worker scope means all workers',t=>{
 const h=harness(t);h.sandbox.list=()=>[{...connection,workers:[],config:{provider:'gitea',baseURL:'https://git.example.test'}}];
 h.run('renderGit("");gitShowCapabilities("git-1")');
 assert.match(h.dom.window.document.body.textContent,/Gitea/);
 assert.equal(h.dom.window.document.querySelector('.git-record-workers').textContent,'All workers');
 assert.equal((h.sandbox.dialog.html.match(/data-hint="unknown"/g)||[]).length,10);
 h.sandbox.dialog=null;h.run('gitShowCapabilities("missing")');assert.equal(h.sandbox.dialog,null);
});
test('Git discovery shows loading, failures, empty and partial repository results',t=>{
 const h=harness(t);h.sandbox.list=()=>[connection];const doc=h.dom.window.document;
 h.run('gitProbes.set("git-1",{loading:true});renderGit("")');
 assert.equal(doc.querySelector('[data-action="git-probe"]').disabled,true);
 assert.equal(doc.querySelector('.git-probe-result').getAttribute('aria-busy'),'true');
 h.run('gitProbes.set("git-1",{error:"Access <denied>"});renderGit("")');
 assert.equal(doc.querySelector('[role="alert"]').textContent,'Access <denied>');
 assert.equal(doc.querySelector('denied'),null);
 h.run('gitProbes.set("git-1",{repositories:[],complete:true});renderGit("")');
 assert.match(doc.body.textContent,/No repositories returned/);
 assert.equal(doc.querySelector('.git-repositories'),null);
 h.run('gitProbes.set("git-1",{repositories:[{name:"org/<repo>",cloneURL:"https://github.com/org/repo.git"}],complete:false,errors:[{message:"Next page failed"}]});renderGit("")');
 assert.match(doc.body.textContent,/Listing incomplete. Write access is not verified/);
 assert.equal(doc.querySelector('.git-repositories li strong').textContent,'org/<repo>');
 assert.equal(doc.querySelector('[role="alert"]').textContent,'Next page failed');
});
test('changing Git destination clears confirmation',t=>{
 const h=harness(t);h.run('gitStart()');h.field('name','GitHub');h.field('token','test-token');h.field('envKey','GH_TOKEN');h.run('gitGo(2)');
 h.field('confirmHost',true);h.field('baseURL','https://enterprise.example.test');
 assert.equal(h.run('gitDraft.confirmHost'),false);
});
