const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const {JSDOM} = require('jsdom');
const script = fs.readFileSync(`${__dirname}/index.html`, 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a,b) => script.slice(script.indexOf(a),script.indexOf(b));
function harness(t) {
 const dom = new JSDOM('<div id="content"></div><span id="resultCount"></span><div id="toast"></div>',{url:'http://piso.local'});
 t.after(()=>dom.window.close());
 const calls=[];
 const sandbox={document:dom.window.document,window:dom.window,location:dom.window.location,URL,count:()=>{},sub:'connections',query:'',loaded:new Set(['connections']),
  data:{connections:{connections:[{id:'ai-boost',name:'AI Boost',status:'ok',detail:'Connected · read and suggest only'}]},aiBoost:{configured:true}},
  $:s=>dom.window.document.querySelector(s), matches:()=>true,list:()=>[],hideProject:false,ingressPort:80,
  api:async(path,method,body)=>{calls.push({path,method,body});return {url:'https://piso.example/oauth/ai-boost/start?ticket=one'}},
  changed:async()=>{},refresh:async()=>{},action:async()=>{},errorDialog:e=>{throw e},safeURL:s=>s,
  confirmAction:(...args)=>{sandbox.confirmation=args},formDialog:(...args)=>{sandbox.form=args},
 };
 const ctx=new Proxy(sandbox,{has:()=>true,get:(o,k)=>k in o?o[k]:globalThis[k]});vm.createContext(ctx);
 const helpers=[
  slice('const esc =','const endpoints'),slice('function badge(','function empty('),slice('function empty(','function record('),
  slice('function record(','function routeURL('),slice('function button(','function facts('),slice('function facts(','function notify('),
  slice('function replaceContent(','function count('),slice('function count(','function mountDialog('),
  slice('const intros = {','function renderSettings()'),slice('function renderSettings()','async function changed('),
  slice('function boostForm()','async function githubUserForm()'),slice('function field(','function formDialog(')
 ].join('\n');
 vm.runInContext(helpers,ctx);
 return {dom,sandbox,ctx,calls,run:s=>vm.runInContext(s,ctx)};
}
test('Connections renders AI Boost status, configure, reconnect and disconnect controls',t=>{
 const h=harness(t);h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/Connected · read and suggest only/);
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-authorize"]'));
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-disconnect"]'));
 h.sandbox.data.aiBoost.configured=false;h.run('renderSettings()');
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-config"]'));
 h.sandbox.data.aiBoost.configured=true;h.sandbox.data.connections.connections[0].status='needs-auth';h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/Reconnect AI Boost/);
});
test('sign-in opens synchronously and obtains a host POST ticket before navigating',async t=>{
 const h=harness(t);const tab={opener:{},location:{},close(){}};h.sandbox.window.open=()=>tab;
 h.run('boostStart()');assert.equal(tab.opener,null);
 await new Promise(resolve=>setImmediate(resolve));
 assert.equal(h.calls[0].path,'ai-boost/authorize');assert.equal(h.calls[0].method,'POST');
 assert.match(tab.location.href,/\/oauth\/ai-boost\/start\?ticket=/);
});
test('disconnect clearly states local-only and requires confirmation',async t=>{
 const h=harness(t);h.run('boostDisconnect()');
 assert.match(h.sandbox.confirmation[1],/grant is not revoked/);assert.equal(h.calls.length,0);
 await h.sandbox.confirmation[3]();assert.equal(h.calls[0].path,'ai-boost/disconnect');assert.equal(h.calls[0].method,'POST');
});
