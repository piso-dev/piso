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

for (const { mode, provider, defaultModel } of [
	{ mode: "rpc", provider: "piso" },
	{ mode: "rpc", provider: "piso", defaultModel: "unbiased/pareto-26.10-preview" },
	{ mode: "print", provider: "piso", defaultModel: "unbiased/pareto-26.10-preview" },
	// A dashboard stack names its provider "piso-<id>"; the saved default
	// must resolve there too, not only in the legacy namespace.
	{ mode: "rpc", provider: "piso-two", defaultModel: "unbiased/pareto-26.10-preview" },
]) {
test(`real Pi CLI ${mode} first launch: ${defaultModel ? `respects the saved ${provider} default rather than the first catalog entry` : "selects a model with no default"}`, {
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
	const profile = JSON.stringify({ providers: { [provider]: {
		apiKey: "piso_piso_test", baseUrl: `http://127.0.0.1:${server.address().port}/v1`,
	} } });
	await writeFile(join(agentDir, "models.json"), profile);
	await writeFile(join(agentDir, "settings.json"), JSON.stringify(defaultModel
		? { defaultProvider: provider, defaultModel } : {}));
	const debugArgs = process.env.PISO_TEST_PRELOAD ? ["--import", process.env.PISO_TEST_PRELOAD] : [];
	child = spawn(process.execPath, [...debugArgs, resolve(cli), ...(mode === "print" ? ["-p", "Say ok"] : ["--mode", "rpc", "--no-session"]),
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "-e", resolve(extension)], {
		cwd,
		env: { PATH: process.env.PATH, HOME: dir, PI_AGENT_DIR: agentDir, PI_CODING_AGENT_DIR: agentDir },
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
	const state = await new Promise((resolveState, reject) => {
		child.on("error", reject);
		child.on("exit", (code) => reject(new Error(`Pi exited ${code}: ${stderr}`)));
		const lines = createInterface({ input: child.stdout });
		lines.on("line", (line) => {
			let message;
			try { message = JSON.parse(line); } catch { return; }
			if (message.type === "response" && message.command === "get_state") {
				if (!message.success) reject(new Error(JSON.stringify(message)));
				else resolveState(message.data);
			}
		});
		child.stdin.write(JSON.stringify({ type: "get_state", id: "startup" }) + "\n");
	});
	assert.equal(state.model?.provider, provider, JSON.stringify({ model: state.model, requests, stderr }));
	assert.equal(state.model?.id, defaultModel ?? "first-launch-model");
	}
	assert.ok(requests >= 1);
	assert.equal(await readFile(join(agentDir, "models.json"), "utf8"), profile);
	const settings = JSON.parse(await readFile(join(agentDir, "settings.json"), "utf8"));
	assert.equal(settings.defaultProvider, defaultModel ? provider : undefined);
	assert.equal(settings.defaultModel, defaultModel);
});
}
