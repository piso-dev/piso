// PI_CLI_PATH=/path/to/pi-coding-agent/dist/cli.js node --test worker/routstr-models.integration.test.mjs
// Uses the real CLI startup pipeline, but only a local model catalog and fake credential.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { join, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import test from "node:test";

const cli = process.env.PI_CLI_PATH;
const extension = process.env.PISO_TEST_EXTENSION || fileURLToPath(new URL("./routstr-models.ts", import.meta.url));

test("real Pi CLI selects a model on the first launch with no defaults or cached catalog", {
	skip: !cli && "Set PI_CLI_PATH to the installed Pi dist/cli.js", timeout: 30_000,
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
	let requests = 0;
	server = createServer((req, res) => {
		assert.equal(req.url, "/v1/models");
		assert.equal(req.headers.authorization, "Bearer piso_routstr_test");
		requests++;
		res.writeHead(200, { "content-type": "application/json" });
		res.end(JSON.stringify({ data: [{ id: "first-launch-model" }] }));
	});
	await new Promise((done) => server.listen(0, "127.0.0.1", done));
	const profile = JSON.stringify({ providers: { routstr: {
		apiKey: "piso_routstr_test", baseUrl: `http://127.0.0.1:${server.address().port}/v1`,
	} } });
	await writeFile(join(agentDir, "models.json"), profile);
	await writeFile(join(agentDir, "settings.json"), "{}");
	const debugArgs = process.env.PISO_TEST_PRELOAD ? ["--import", process.env.PISO_TEST_PRELOAD] : [];
	child = spawn(process.execPath, [...debugArgs, resolve(cli), "--mode", "rpc", "--no-session",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "-e", resolve(extension)], {
		cwd: dir,
		env: { PATH: process.env.PATH, HOME: dir, PI_AGENT_DIR: agentDir, PI_CODING_AGENT_DIR: agentDir },
		stdio: ["pipe", "pipe", "pipe"],
	});
	let stderr = "";
	child.stderr.on("data", (data) => { stderr += data; });
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
	assert.equal(state.model?.provider, "routstr", JSON.stringify({ model: state.model, requests, stderr }));
	assert.equal(state.model?.id, "first-launch-model");
	assert.ok(requests >= 1);
	assert.equal(await readFile(join(agentDir, "models.json"), "utf8"), profile);
	const settings = JSON.parse(await readFile(join(agentDir, "settings.json"), "utf8"));
	assert.equal(settings.defaultProvider, undefined);
	assert.equal(settings.defaultModel, undefined);
});
