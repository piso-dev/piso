// PI_CLI_PATH=/path/to/pi-coding-agent/dist/cli.js node --test worker/piso-models.integration.mjs
// Named without .test. on purpose: `bun test` cannot skip a node:test file
// without PI_CLI_PATH, and this suite must not fail the local runner.
// Uses the real CLI startup pipeline, but only a local model catalog and fake credential.
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import test from "node:test";

const cli = process.env.PI_CLI_PATH;
const extension = process.env.PISO_TEST_EXTENSION || fileURLToPath(new URL("./piso-models.ts", import.meta.url));

for (const { mode, provider, defaultModel, cache, extraEnv, menu, active, extraProviders } of [
	{ mode: "rpc", provider: "piso" },
	{ mode: "rpc", provider: "piso", defaultModel: "unbiased/pareto-26.10-preview" },
	{ mode: "print", provider: "piso", defaultModel: "unbiased/pareto-26.10-preview" },
	// A dashboard stack names its provider "piso-<id>"; the saved default
	// must resolve there too, not only in the legacy namespace.
	{ mode: "rpc", provider: "piso-two", defaultModel: "unbiased/pareto-26.10-preview" },
	// The Model providers tab is the whole menu. The cache says this provider is
	// approved for one model and another provider for none, while a built-in
	// family's key sits in the environment: the real CLI must offer exactly the
	// approved id, in the tab's order, and none of the built-in catalog.
	{ mode: "rpc", provider: "piso", defaultModel: "unbiased/pareto-26.10-preview",
		cache: { provider: "piso", model: "unbiased/pareto-26.10-preview",
			allowedModels: ["unbiased/pareto-26.10-preview"],
			providers: { piso: ["unbiased/pareto-26.10-preview"] } },
		extraEnv: { OPENROUTER_API_KEY: "piso_openrouter", OPENAI_API_KEY: "piso_openai" },
		menu: ["piso/unbiased/pareto-26.10-preview"] },
	// A model the tab no longer allows must not stay active just because the
	// built-in catalog still has it: the gateway would 403 every request.
	{ mode: "rpc", provider: "piso", defaultModel: "first-launch-model",
		cache: { provider: "piso", model: "first-launch-model", allowedModels: ["unbiased/pareto-26.10-preview"],
			providers: { piso: ["unbiased/pareto-26.10-preview"] } },
		menu: ["piso/unbiased/pareto-26.10-preview"], active: "unbiased/pareto-26.10-preview" },
	// The reported 403 in full: the tab approves a model on a second provider
	// (piso, which carries its own default model) while this worker is pinned to
	// another, and the menu offered it. Every request through it then failed as
	// model-provider-mismatch, because the pin sends this worker's traffic to the
	// pinned provider's host. Only the pinned provider may appear.
	{ mode: "rpc", provider: "piso-ai-ml-api", defaultModel: "deepseek/deepseek-v4.1-flash",
		extraProviders: {
			piso: { apiKey: "piso_piso_test", baseUrl: "https://openrouter.example.invalid/v1",
				api: "openai-completions", models: [{ id: "deepseek/deepseek-v4.1-flash" }] },
			"piso-ai-ml-api": { apiKey: "piso_piso_test", baseUrl: "https://aimlapi.example.invalid/v1",
				api: "openai-completions",
				models: [{ id: "deepseek/deepseek-v4.1-flash" }, { id: "unbiased/pareto-26.10" }] },
		},
		cache: { provider: "piso-ai-ml-api", model: "deepseek/deepseek-v4.1-flash",
			allowedModels: ["unbiased/pareto-26.10", "deepseek/deepseek-v4.1-flash"],
			providers: { piso: ["deepseek/deepseek-v4.1-flash"],
				"piso-ai-ml-api": ["unbiased/pareto-26.10", "deepseek/deepseek-v4.1-flash"],
				"piso-tinfoil": [] } },
		menu: ["piso-ai-ml-api/unbiased/pareto-26.10", "piso-ai-ml-api/deepseek/deepseek-v4.1-flash"] },
]) {
test(`real Pi CLI ${mode} first launch: ${menu ? `${provider} offers only the Model providers tab's models` : defaultModel ? `respects the saved ${provider} default rather than the first catalog entry` : "selects a model with no default"}`, {
	// Needs a real Pi CLI; set PI_CLI_PATH to its dist/cli.js to run.
	skip: !cli, timeout: 30_000,
}, async (t) => {
	const dir = await mkdtemp(resolve(".piso-model-cli-test-"));
	let child;
	let server;
	t.after(async () => {
		if (child && child.exitCode === null && child.signalCode === null) {
			await new Promise((done) => { child.once("exit", done); child.kill("SIGKILL"); });
		}
		if (server) {
			server.closeAllConnections();
			await new Promise((done) => server.close(done));
		}
		await rm(dir, { recursive: true, force: true });
	});
	const agentDir = join(dir, "agent");
	await mkdir(agentDir);
	const cwd = mode === "print" ? join(agentDir, "monitor-work") : dir;
	await mkdir(cwd, { recursive: true });
	let requests = 0;
	let requestedModel;
	server = createServer(async (req, res) => {
		assert.equal(req.headers.authorization, "Bearer piso_piso_test");
		if (req.url === "/v1/chat/completions") {
			let body = "";
			for await (const chunk of req) body += chunk;
			requestedModel = JSON.parse(body).model;
			res.writeHead(200, { "content-type": "text/event-stream" });
			res.end('data: {"id":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}\n\ndata: {"id":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n');
			return;
		}
		assert.equal(req.url, "/v1/models");
		requests++;
		res.writeHead(200, { "content-type": "application/json" });
		res.end(JSON.stringify({ data: [{ id: "first-launch-model" }, { id: "unbiased/pareto-26.10-preview" }] }));
	});
	await new Promise((done) => server.listen(0, "127.0.0.1", done));
	const profile = JSON.stringify({ providers: {
		[provider]: { apiKey: "piso_piso_test", baseUrl: `http://127.0.0.1:${server.address().port}/v1` },
		...extraProviders,
	} });
	await writeFile(join(agentDir, "models.json"), profile);
	await writeFile(join(agentDir, "settings.json"), JSON.stringify(defaultModel
		? { defaultProvider: provider, defaultModel } : {}));
	const debugArgs = process.env.PISO_TEST_PRELOAD ? ["--import", process.env.PISO_TEST_PRELOAD] : [];
	if (cache) await writeFile(join(agentDir, "piso-model.json"), JSON.stringify(cache));
	child = spawn(process.execPath, [...debugArgs, resolve(cli), ...(mode === "print" ? ["-p", "Say ok"] : ["--mode", "rpc", "--no-session"]),
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "-e", resolve(extension)], {
		cwd,
		env: { PATH: process.env.PATH, HOME: dir, PI_AGENT_DIR: agentDir, PI_CODING_AGENT_DIR: agentDir, ...extraEnv },
		stdio: ["pipe", "pipe", "pipe"],
	});
	let stderr = "";
	child.stderr.on("data", (data) => { stderr += data; });
	if (mode === "print") {
		let stdout = "";
		child.stdout.on("data", (data) => { stdout += data; });
		child.stdin.end();
		const code = await new Promise((done, reject) => { child.on("error", reject); child.on("close", done); });
		assert.equal(code, 0, stderr);
		assert.match(stdout, /\bok\b/, "monitor-style pi -p must print the model response");
		assert.equal(requestedModel, defaultModel, "monitor-style pi -p must send the saved default model");
		// Read the actual session Pi persisted, using the dashboard context
		// watcher's shell code, after the monitor process has already exited.
		const watcher = await readFile(new URL("./context-watch.sh", import.meta.url), "utf8");
		const helpers = watcher.slice(watcher.indexOf("# Match Pi"), watcher.indexOf("# notify POSTs"));
		const reported = spawnSync("bash", ["-c", 'set -u\nslug=monitor\n' + helpers + '\nmodel_ctx ""'], {
			env: { ...process.env, PI_CODING_AGENT_DIR: agentDir, PISO_ROLE: "monitor" }, encoding: "utf8",
		});
		assert.equal(reported.status, 0, reported.stderr);
		assert.equal(reported.stdout, `piso/${defaultModel}`, "dashboard must report the monitor's actual model");
	} else {
	const { state, models } = await new Promise((resolveState, reject) => {
		child.on("error", reject);
		child.on("exit", (code) => reject(new Error(`Pi exited ${code}: ${stderr}`)));
		let gotState;
		let gotModels;
		const maybeDone = () => {
			if (gotState !== undefined && gotModels !== undefined) resolveState({ state: gotState, models: gotModels });
		};
		const lines = createInterface({ input: child.stdout });
		lines.on("line", (line) => {
			let message;
			try { message = JSON.parse(line); } catch { return; }
			if (message.type !== "response") return;
			if (!message.success) {
				reject(new Error(JSON.stringify(message)));
				return;
			}
			if (message.command === "get_state") gotState = message.data;
			else if (message.command === "get_available_models") gotModels = message.data.models;
			maybeDone();
		});
		child.stdin.write(JSON.stringify({ type: "get_state", id: "startup" }) + "\n");
		// What /model shows, from the same snapshot the picker renders.
		child.stdin.write(JSON.stringify({ type: "get_available_models", id: "menu" }) + "\n");
	});
	assert.equal(state.model?.provider, provider, JSON.stringify({ model: state.model, requests, stderr }));
	assert.equal(state.model?.id, active ?? defaultModel ?? "first-launch-model", JSON.stringify({ model: state.model, stderr }));
	if (menu) {
		const offered = models.map((model) => `${model.provider}/${model.id}`).sort();
		assert.deepEqual(offered, [...menu].sort(), JSON.stringify({ offered, stderr }));
	}
	}
	// A narrowed menu may legitimately never ask the provider for its catalog;
	// every other case depends on startup discovery having happened.
	if (!menu) assert.ok(requests >= 1);
	assert.equal(await readFile(join(agentDir, "models.json"), "utf8"), profile);
	const settings = JSON.parse(await readFile(join(agentDir, "settings.json"), "utf8"));
	assert.equal(settings.defaultProvider, defaultModel ? provider : undefined);
	assert.equal(settings.defaultModel, defaultModel);
});
}

// A real CLI regression for the loopback Tinfoil contract. No enclave/real key needed.
for (const scenario of ["discovered", "manual", "unresolved"]) {
 test(`Tinfoil ${scenario}: local discovery and no previous-provider traffic`, {
  skip: !cli, timeout: 30_000,
 }, async (t) => {
  const dir = await mkdtemp(resolve(".piso-tinfoil-cli-test-"));
  const agentDir = join(dir, "agent"); await mkdir(agentDir);
  let child; let localCalls = 0; let oldCalls = 0; let catalogCalls = 0;
  const old = createServer((_req,res) => { oldCalls++; res.writeHead(500); res.end(); });
  const local = createServer(async (req,res) => {
   assert.equal(req.headers.authorization, "Bearer piso_fake_tinfoil");
   if (req.url === "/v1/models") {
    catalogCalls++;
    res.writeHead(scenario === "discovered" ? 200 : 503, {"content-type":"application/json"});
    res.end(JSON.stringify({data:[{id:"enclave-model"}]})); return;
   }
   assert.equal(req.url,"/v1/chat/completions");
   let body=""; for await (const chunk of req) body+=chunk;
   assert.equal(JSON.parse(body).model, scenario === "manual" ? "manual-model" : "enclave-model");
   localCalls++;
   res.writeHead(200,{"content-type":"text/event-stream"});
   res.end('data: {"id":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}\n\ndata: {"id":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n');
  });
  t.after(async () => {
   if (child && child.exitCode === null && child.signalCode === null) {
    await new Promise(done => { child.once("exit",done); child.kill("SIGKILL"); });
   }
   for (const server of [local,old]) if (server.listening) {
    server.closeAllConnections(); await new Promise(done => server.close(done));
   }
   await rm(dir,{recursive:true,force:true});
  });
  await new Promise(done=>old.listen(0,"127.0.0.1",done));
  // Explicit worker-discovery policy permits an ephemeral loopback fake.
  await new Promise((done,reject)=>{local.once("error",reject);local.listen(0,"127.0.0.1",done);});
  const base=`http://127.0.0.1:${local.address().port}/v1`;
  await writeFile(join(agentDir,"models.json"),JSON.stringify({providers:{
   piso:{api:"openai-completions",apiKey:"piso_old",baseUrl:`http://127.0.0.1:${old.address().port}/v1`,models:[{id:"old-model"}]},
   "piso-tinfoil":{api:"openai-completions",apiKey:"piso_fake_tinfoil",baseUrl:base,models:[]},
  }}));
  await writeFile(join(agentDir,"settings.json"),JSON.stringify({defaultProvider:"piso",defaultModel:"old-model"}));
  await writeFile(join(agentDir,"piso-model.json"),JSON.stringify({
   provider:"piso-tinfoil",model:scenario === "manual" ? "manual-model" : "",
   providers:{piso:[]},modelPolicies:{
    piso:{mode:"unavailable",baseUrl:`http://127.0.0.1:${old.address().port}/v1`},
    "piso-tinfoil":{mode:"worker-discovered",baseUrl:base},
   },
  }));
  child=spawn(process.execPath,[resolve(cli),"--mode","rpc","--no-session","--no-extensions","--no-skills","--no-prompt-templates","--no-themes","-e",resolve(extension)],{
   cwd:dir,env:{PATH:process.env.PATH,HOME:dir,PI_AGENT_DIR:agentDir,PI_CODING_AGENT_DIR:agentDir},stdio:["pipe","pipe","pipe"],
  });
  let stderr=""; child.stderr.on("data",chunk=>stderr+=chunk);
  const lines=createInterface({input:child.stdout});
  let resolveMenu; const menu=new Promise(done=>resolveMenu=done);
  let resolveEnd; const end=new Promise(done=>resolveEnd=done);
  const messages=[];
  lines.on("line",line=>{
   let m;try {m=JSON.parse(line);} catch{return;}
   messages.push(m);
   if(m.type==="response" && m.command==="get_available_models") resolveMenu(m.data?.models??[]);
   if(m.type==="agent_end" || (m.type==="response" && m.command==="prompt" && !m.success)) resolveEnd();
  });
  child.stdin.write(JSON.stringify({type:"get_available_models",id:"menu"})+"\n");
  const offered=await menu;
  assert.equal(offered.some(m=>m.provider==="piso"),false,stderr);
  if(scenario!=="unresolved") assert.ok(offered.some(m=>m.provider==="piso-tinfoil" && m.id===(scenario==="manual"?"manual-model":"enclave-model")),stderr);
  child.stdin.write(JSON.stringify({type:"prompt",message:"Say ok",id:"prompt"})+"\n");
  await end;
  assert.ok(catalogCalls>0,stderr);
  assert.equal(oldCalls,0,JSON.stringify({messages,stderr}));
  assert.equal(localCalls,scenario==="unresolved"?0:1,JSON.stringify({messages,stderr}));
 });
}
