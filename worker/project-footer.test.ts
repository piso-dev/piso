// NODE_PATH=$(npm root -g):$(npm root -g)/@earendil-works/pi-coding-agent/node_modules bun test worker/project-footer.test.ts
import { afterEach, expect, test } from "bun:test";
import { FooterComponent, initTheme, type ExtensionAPI, type Theme } from "@earendil-works/pi-coding-agent";
import { visibleWidth } from "@earendil-works/pi-tui";
import projectFooter, { decorateFooter } from "./project-footer";

initTheme("dark");
const cleanups: (() => void)[] = [];
afterEach(() => { for (const cleanup of cleanups.splice(0).reverse()) cleanup(); });
const plain = (value: string) => value.replace(/\x1b\[[0-9;]*m/g, "");
const theme = {
	fg: (color: string, text: string) => `\x1b[${color === "accent" ? 36 : 2}m${text}\x1b[0m`,
	bold: (text: string) => `\x1b[1m${text}\x1b[22m`,
} as Theme;

function fixture() {
	let branch: string | null = "complex/technocore";
	let name: string | undefined;
	const model = { id: "pareto", provider: "piso", contextWindow: 128000, reasoning: true };
	const session = {
		state: { model, thinkingLevel: "high" }, model,
		routedModel: { model: { ...model, id: "routed" }, thinkingLevel: "low" },
		sessionManager: {
			getCwd: () => "/workspace", getSessionName: () => name,
			getEntryCount: () => 1, getSessionId: () => "session", getLeafId: () => "leaf",
			getEntries: () => [{ type: "message", message: { role: "assistant", usage: {
				input: 24000, output: 1300, cacheRead: 49000, cacheWrite: 100,
				cost: { input: 0.1, output: 0.2, cacheRead: 0.01, cacheWrite: 0.01, total: 0.32 },
			} } }],
		},
		getContextUsage: () => ({ contextWindow: 128000, percent: 11 }),
		modelRuntime: { isUsingSubscription: () => true },
	};
	const footerData = {
		getGitBranch: () => branch, getAvailableProviderCount: () => 2,
		getExtensionStatuses: () => new Map([["other", "Other extension status"]]),
	};
	const footer = new FooterComponent(session as any, footerData as any);
	return { footer, setBranch: (value: string | null) => { branch = value; }, setName: (value?: string) => { name = value; } };
}

test("only the identity line changes; real Pi stats, routing and statuses are retained", () => {
	const { footer } = fixture();
	const before = footer.render(180);
	cleanups.push(decorateFooter("piso", () => theme));
	const after = footer.render(180);
	expect(plain(after[0])).toBe("piso · /workspace · complex/technocore");
	expect(after[0]).toContain("\x1b[1m\x1b[36mpiso");
	expect(after.slice(1)).toEqual(before.slice(1));
	for (const part of ["↑24k", "↓1.3k", "R49k", "CH", "(sub)", "(auto)", "pareto", "routed", "Other extension status"]) {
		expect(plain(after.join("\n"))).toContain(part);
	}
	footer.setAutoCompactEnabled(false);
	expect(plain(footer.render(180)[1])).not.toContain("(auto)");
});

test("branch, session name and current theme are read on every render", () => {
	const f = fixture();
	let currentTheme = theme;
	cleanups.push(decorateFooter("piso", () => currentTheme));
	f.setBranch(null);
	f.setName("Debugging");
	expect(plain(f.footer.render(180)[0])).toBe("piso · /workspace • Debugging");
	f.setBranch("next");
	currentTheme = { fg: (_: string, text: string) => text, bold: (text: string) => `[${text}]` } as Theme;
	expect(f.footer.render(180)[0]).toBe("[piso] · /workspace · next • Debugging");
});

test("Unicode, narrow terminals, and control characters remain single-line and bounded", () => {
	const { footer, setName } = fixture();
	setName("test\nname\tmore");
	cleanups.push(decorateFooter("项目🚀\nslug", () => theme));
	for (const width of [1, 2, 4, 10, 30, 80]) {
		const line = footer.render(width)[0];
		expect(visibleWidth(line)).toBeLessThanOrEqual(width);
		expect(line).not.toMatch(/[\n\r\t]/);
	}
});

test("extension lifecycle is inert outside TUI and restores renderer across reloads", () => {
	const savedSlug = process.env.PISO_WORKER_SLUG;
	cleanups.push(() => { if (savedSlug === undefined) delete process.env.PISO_WORKER_SLUG; else process.env.PISO_WORKER_SLUG = savedSlug; });
	const original = FooterComponent.prototype.render;
	const handlers = new Map<string, Function>();
	projectFooter({ on: (name: string, fn: Function) => handlers.set(name, fn) } as unknown as ExtensionAPI);
	const start = (mode: string, hasUI = true) => handlers.get("session_start")!({}, { mode, hasUI, ui: { theme } });
	const stop = () => handlers.get("session_shutdown")!();
	cleanups.push(stop);
	delete process.env.PISO_WORKER_SLUG;
	start("tui");
	expect(FooterComponent.prototype.render).toBe(original);
	process.env.PISO_WORKER_SLUG = "piso";
	for (const mode of ["rpc", "json", "print"]) start(mode);
	start("tui", false);
	expect(FooterComponent.prototype.render).toBe(original);
	start("tui");
	start("tui"); // session switch must not stack wrappers
	expect(plain(fixture().footer.render(180)[0])).toBe("piso · /workspace · complex/technocore");
	stop(); stop();
	expect(FooterComponent.prototype.render).toBe(original);
	start("tui"); stop();
	expect(FooterComponent.prototype.render).toBe(original);
});
