import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

// This is advisory telemetry, not an authorization signal. No prompt, tool
// arguments, session content, or credentials are written. Each pi instance
// owns a separate lease so concurrent sessions and /reload cannot clobber it.
export function attachActivity(pi: ExtensionAPI, dir: string, intervalMs = 10_000): void {
	let timer: ReturnType<typeof setInterval> | undefined;
	let file: string | undefined;
	let identity: { pid: number; startTicks: string; bootId: string };
	let context: ExtensionContext | undefined;

	function publish(busy: boolean): void {
		if (!file) return;
		const temp = `${file}.tmp`;
		try {
			writeFileSync(temp, JSON.stringify({
				version: 1, ...identity, state: busy ? "busy" : "idle",
				// Same monotonic clock/precision as the Python reader (Bun's
				// os.uptime rounds to seconds and can appear in the future).
				updated: Number(readFileSync("/proc/uptime", "utf8").split(" ")[0]),
			}), { mode: 0o600 });
			renameSync(temp, file);
		} catch {
			// A failed renewal expires naturally. Telemetry must not break a turn.
			try { rmSync(temp, { force: true }); } catch { /* best effort */ }
		}
	}

	function stop(): void {
		if (timer) clearInterval(timer);
		timer = undefined;
		if (file) {
			try { rmSync(file, { force: true }); } catch { /* expires naturally */ }
		}
		file = undefined;
		context = undefined;
	}

	pi.on("session_start", (_event, ctx) => {
		stop();
		try {
			const stat = readFileSync(`/proc/${process.pid}/stat`, "utf8");
			identity = {
				pid: process.pid,
				startTicks: stat.slice(stat.lastIndexOf(")") + 2).split(/\s+/)[19],
				bootId: readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim(),
			};
			mkdirSync(dir, { recursive: true, mode: 0o700 });
			file = join(dir, `${process.pid}-${randomUUID()}.json`);
			context = ctx;
			publish(!ctx.isIdle());
			// Renew during long model calls/tools even when the transcript is quiet.
			// Reconcile with runtime state so a missed end event cannot latch busy.
			timer = setInterval(() => publish(!context!.isIdle()), intervalMs);
			timer.unref();
		} catch { stop(); }
	});
	pi.on("agent_start", (_event, ctx) => { context = ctx; publish(true); });
	// agent_end may precede automatic retries/compaction/queued continuations.
	pi.on("agent_end", (_event, ctx) => { context = ctx; publish(!ctx.isIdle()); });
	pi.on("agent_settled", (_event, ctx) => { context = ctx; publish(false); });
	pi.on("session_shutdown", stop);
}

export default function activityState(pi: ExtensionAPI): void {
	if (!process.env.PISO_WORKER_NAME || process.env.PISO_ROLE === "monitor") return;
	attachActivity(pi, process.env.PISO_ACTIVITY_DIR || "/root/.pi/agent/activity-state");
}
