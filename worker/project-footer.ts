import { isAbsolute, relative, resolve, sep } from "node:path";
import { FooterComponent, type ExtensionAPI, type Theme } from "@earendil-works/pi-coding-agent";
import { truncateToWidth } from "@earendil-works/pi-tui";

// Pi's public setFooter() replaces the whole footer, with no way to delegate
// stats rendering. Decorate the exported built-in component instead: Pi keeps
// ownership of usage, routing, compaction, subscriptions and extension statuses.
// This small internal adapter is covered against the real Pi FooterComponent
// in project-footer.test.ts; recheck it when updating the worker's Pi version.
type FooterIdentity = {
	session: { sessionManager: { getCwd(): string; getSessionName(): string | undefined } };
	footerData: { getGitBranch(): string | null };
};

function singleLine(value: string): string {
	return value.replace(/[\x00-\x1f\x7f-\x9f]/g, " ").replace(/\s+/g, " ").trim();
}

function displayPath(cwd: string): string {
	const home = process.env.HOME || process.env.USERPROFILE;
	if (!home) return cwd;
	const rel = relative(resolve(home), resolve(cwd));
	return rel === "" ? "~" : rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel) ? `~${sep}${rel}` : cwd;
}

export function decorateFooter(slug: string, getTheme: () => Theme): () => void {
	const prototype = FooterComponent.prototype;
	const original = prototype.render;
	function render(this: FooterComponent, width: number): string[] {
		const lines = original.call(this, width);
		// Fail open if a Pi update changes its private fields; never break the TUI.
		const identity = this as unknown as Partial<FooterIdentity>;
		const manager = identity.session?.sessionManager;
		if (!lines.length || typeof manager?.getCwd !== "function" ||
			typeof manager.getSessionName !== "function" ||
			typeof identity.footerData?.getGitBranch !== "function") return lines;

		const theme = getTheme(); // Resolve on every render so theme changes work.
		const branch = identity.footerData.getGitBranch();
		const name = manager.getSessionName();
		const details = [displayPath(manager.getCwd()), branch].filter(Boolean).map(value => singleLine(value!));
		const suffix = ` · ${details.join(" · ")}${name ? ` • ${singleLine(name)}` : ""}`;
		lines[0] = truncateToWidth(
			theme.bold(theme.fg("accent", singleLine(slug))) + theme.fg("dim", suffix),
			width, theme.fg("dim", "…"),
		);
		return lines;
	}
	prototype.render = render;
	return () => {
		// Do not clobber a renderer installed by somebody else after ours.
		if (prototype.render === render) prototype.render = original;
	};
}

export default function projectFooter(pi: ExtensionAPI): void {
	let restore: (() => void) | undefined;
	pi.on("session_start", (_event, ctx) => {
		restore?.();
		restore = undefined;
		const slug = process.env.PISO_WORKER_SLUG?.trim();
		// Older Pi versions expose hasUI but not mode. RPC/print do not render
		// FooterComponent; skip them explicitly on versions that expose mode.
		if (!slug || !ctx.hasUI || (ctx.mode !== undefined && ctx.mode !== "tui")) return;
		restore = decorateFooter(slug, () => ctx.ui.theme);
	});
	pi.on("session_shutdown", () => {
		restore?.();
		restore = undefined;
	});
}
