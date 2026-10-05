import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import { join } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { attachActivity } from "./activity-state";

const cleanups: (() => void)[] = [];
afterEach(() => { for (const cleanup of cleanups.splice(0)) cleanup(); });

function harness(dir?: string) {
	if (!dir) {
		dir = mkdtempSync(join(process.cwd(), ".activity-test-"));
		const root = dir;
		cleanups.unshift(() => rmSync(root, { recursive: true, force: true }));
	}
	const handlers = new Map<string, Function>();
	const pi = { on: (name: string, handler: Function) => handlers.set(name, handler) };
	let idle = true;
	const ctx = { isIdle: () => idle } as ExtensionContext;
	attachActivity(pi as unknown as ExtensionAPI, dir, 10);
	const emit = (name: string) => handlers.get(name)!({}, ctx);
	cleanups.unshift(() => emit("session_shutdown"));
	const rows = () => readdirSync(dir!).filter(f => f.endsWith(".json"))
		.map(f => JSON.parse(readFileSync(join(dir!, f), "utf8")));
	return { dir, emit, rows, setIdle: (value: boolean) => { idle = value; } };
}

const tick = () => new Promise(resolve => setTimeout(resolve, 35));

test("no resources until session start; start, settled, abort, and shutdown lifecycle", () => {
	const h = harness();
	expect(h.rows()).toEqual([]);
	h.emit("session_start");
	expect(h.rows()[0]).toMatchObject({ version: 1, pid: process.pid, state: "idle" });
	h.setIdle(false);
	h.emit("agent_start");
	expect(h.rows()[0].state).toBe("busy");
	// End is not settled: automatic recovery may still be running.
	h.emit("agent_end");
	expect(h.rows()[0].state).toBe("busy");
	h.setIdle(true);
	h.emit("agent_settled");
	expect(h.rows()[0].state).toBe("idle");
	h.setIdle(false);
	h.emit("agent_start");
	h.setIdle(true); // cancellation/error has returned the runtime to idle
	h.emit("agent_end");
	expect(h.rows()[0].state).toBe("idle");
	h.emit("session_shutdown");
	h.emit("session_shutdown");
	expect(h.rows()).toEqual([]);
});

test("quiet long-running turns renew; runtime idle reconciles a missed end", async () => {
	const h = harness();
	h.emit("session_start");
	h.setIdle(false);
	h.emit("agent_start");
	const file = join(h.dir, readdirSync(h.dir)[0]);
	const first = h.rows()[0].updated - 100;
	writeFileSync(file, JSON.stringify({ ...h.rows()[0], updated: first }));
	await tick();
	expect(h.rows()[0].updated).toBeGreaterThan(first);
	expect(h.rows()[0].state).toBe("busy");
	h.setIdle(true);
	await tick();
	expect(h.rows()[0].state).toBe("idle");
});

test("concurrent instances and reload cleanup do not remove another lease", async () => {
	const a = harness();
	const b = harness(a.dir);
	a.emit("session_start");
	b.setIdle(false);
	b.emit("session_start");
	expect(a.rows()).toHaveLength(2);
	a.emit("session_shutdown");
	expect(b.rows()).toHaveLength(1);
	expect(b.rows()[0].state).toBe("busy");
	b.emit("session_shutdown");
	await tick();
	expect(b.rows()).toEqual([]); // no timer resurrects the old runtime
	b.setIdle(true);
	b.emit("session_start");
	expect(b.rows()).toHaveLength(1);
	expect(b.rows()[0].state).toBe("idle");
});

test("unwritable state path is non-fatal", () => {
	const h = harness();
	// A regular file cannot serve as the state directory.
	const file = join(h.dir, "blocker");
	writeFileSync(file, "x");
	const blocked = harness(file);
	expect(() => blocked.emit("session_start")).not.toThrow();
	expect(() => blocked.emit("agent_start")).not.toThrow();
});

test("Python watcher sees idle despite a persistent MCP-style child", () => {
	const child = spawn("sleep", ["60"]);
	cleanups.unshift(() => child.kill());
	const h = harness();
	const status = () => {
		const result = spawnSync("python3", [join(import.meta.dir, "activity-state.py")], {
			env: { ...process.env, PISO_ACTIVITY_DIR: h.dir }, encoding: "utf8",
		});
		expect(result.status).toBe(0);
		return result.stdout.trim();
	};
	h.emit("session_start");
	expect(status()).toBe("idle");
	h.setIdle(false);
	h.emit("agent_start");
	expect(status()).toBe("busy");
	h.setIdle(true);
	h.emit("agent_settled");
	expect(status()).toBe("idle");
	expect(child.exitCode).toBeNull();
	h.emit("session_shutdown");
	expect(status()).toBe("down");
});
