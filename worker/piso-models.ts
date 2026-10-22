import { readFile, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import type { Api, Model, ModelsStoreEntry, RefreshModelsContext } from "@earendil-works/pi-ai";
import type { ExtensionAPI, ExtensionContext, ProviderConfig, ProviderModelConfig } from "@earendil-works/pi-coding-agent";

const PROVIDER_ID = "piso";
const LEGACY_PROVIDER_ID = "routstr";
const MANAGED_PREFIX = "piso-";
const DEFAULT_CONTEXT_WINDOW = 128_000;
const DEFAULT_MAX_TOKENS = 16_384;
const STARTUP_DISCOVERY_TIMEOUT_MS = 10_000;
// The shared pi profile is a read-only mount the gateway rewrites. Poll it so
// an already-running session picks up a provider or default change without a
// restart. 15s keeps a switch feeling live without busy-looping.
const PROFILE_POLL_MS = 15_000;

type JsonObject = Record<string, unknown>;

// ProviderProfile is one dashboard-managed endpoint from models.json.
export type ProviderProfile = {
	id: string;
	name?: string;
	api?: Api;
	apiKey: string;
	baseUrl: string;
	models: JsonObject[];
};

type ActiveDefault = {
	provider: string;
	model: string;
};

// The gateway's per-worker answer lives in the agent dir rather than in the
// shared profile: settings.json is a read-only mount, and a worker that starts
// while the gateway is unreachable must still know which provider it was pinned
// to. The file holds no secret — a provider id and a model id.
const OVERRIDE_CACHE_FILE = "piso-model.json";
const OVERRIDE_FETCH_TIMEOUT_MS = 3_000;

function isObject(value: unknown): value is JsonObject {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function managedProviderId(id: string): boolean {
	return id === PROVIDER_ID || id === LEGACY_PROVIDER_ID || id.startsWith(MANAGED_PREFIX);
}

// normalizeProviderId maps the legacy namespace onto the current one so a
// migrated profile does not register a second provider.
function normalizeProviderId(id: string): string {
	return id === LEGACY_PROVIDER_ID ? PROVIDER_ID : id;
}

function positiveNumber(...values: unknown[]): number | undefined {
	for (const value of values) {
		if (typeof value === "number" && Number.isFinite(value) && value > 0) return value;
	}
	return undefined;
}

function stringArray(value: unknown): string[] | undefined {
	if (!Array.isArray(value) || !value.every((entry) => typeof entry === "string")) return undefined;
	return value;
}

function cost(value: unknown): ProviderModelConfig["cost"] | undefined {
	if (!isObject(value)) return undefined;
	const input = positiveNumber(value.input) ?? (value.input === 0 ? 0 : undefined);
	const output = positiveNumber(value.output) ?? (value.output === 0 ? 0 : undefined);
	const cacheRead = positiveNumber(value.cacheRead, value.cache_read) ??
		(value.cacheRead === 0 || value.cache_read === 0 ? 0 : undefined);
	const cacheWrite = positiveNumber(value.cacheWrite, value.cache_write) ??
		(value.cacheWrite === 0 || value.cache_write === 0 ? 0 : undefined);
	if (input === undefined || output === undefined) return undefined;
	return { input, output, cacheRead: cacheRead ?? 0, cacheWrite: cacheWrite ?? 0 };
}

function normalizeInput(value: unknown, fallback: ProviderModelConfig["input"]): ProviderModelConfig["input"] {
	const modalities = stringArray(value);
	if (!modalities) return fallback;
	const input: ProviderModelConfig["input"] = ["text"];
	if (modalities.some((entry) => entry === "image" || entry === "vision")) input.push("image");
	return input;
}

export function normalizeModel(
	remote: JsonObject,
	fallback?: Partial<ProviderModelConfig>,
): ProviderModelConfig | undefined {
	if (typeof remote.id !== "string" || remote.id.trim() === "") return undefined;
	const id = remote.id.trim();
	const remoteName = typeof remote.name === "string" && remote.name.trim() ? remote.name.trim() : undefined;
	const remoteReasoning = typeof remote.reasoning === "boolean" ? remote.reasoning : undefined;
	const remoteInput = remote.input ?? remote.input_modalities ?? remote.modalities;

	return {
		id,
		name: remoteName ?? fallback?.name ?? id,
		reasoning: remoteReasoning ?? fallback?.reasoning ?? false,
		input: normalizeInput(remoteInput, fallback?.input ?? ["text"]),
		cost: cost(remote.cost) ?? fallback?.cost ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow:
			positiveNumber(remote.contextWindow, remote.context_window, remote.context_length) ??
			fallback?.contextWindow ??
			DEFAULT_CONTEXT_WINDOW,
		maxTokens:
			positiveNumber(remote.maxTokens, remote.max_tokens, remote.max_output_tokens) ??
			fallback?.maxTokens ??
			DEFAULT_MAX_TOKENS,
		...(fallback?.thinkingLevelMap ? { thinkingLevelMap: fallback.thinkingLevelMap } : {}),
		...(fallback?.compat ? { compat: fallback.compat } : {}),
	};
}

function configModel(value: JsonObject): ProviderModelConfig | undefined {
	return normalizeModel(value, {
		name: typeof value.name === "string" ? value.name : undefined,
		reasoning: typeof value.reasoning === "boolean" ? value.reasoning : undefined,
		input: normalizeInput(value.input, ["text"]),
		cost: cost(value.cost),
		contextWindow: positiveNumber(value.contextWindow),
		maxTokens: positiveNumber(value.maxTokens),
		thinkingLevelMap: isObject(value.thinkingLevelMap)
			? (value.thinkingLevelMap as ProviderModelConfig["thinkingLevelMap"])
			: undefined,
		compat: isObject(value.compat) ? (value.compat as ProviderModelConfig["compat"]) : undefined,
	});
}

function storedModel(value: Readonly<Model<Api>>): ProviderModelConfig {
	return {
		id: value.id,
		name: value.name,
		reasoning: value.reasoning,
		input: [...value.input],
		cost: value.cost,
		contextWindow: value.contextWindow,
		maxTokens: value.maxTokens,
		...(value.thinkingLevelMap ? { thinkingLevelMap: value.thinkingLevelMap } : {}),
		...(value.compat ? { compat: value.compat } : {}),
	};
}

export function mergeCatalog(
	remote: JsonObject[],
	fallback: ProviderModelConfig[],
	stored: ProviderModelConfig[] = [],
): ProviderModelConfig[] {
	const metadata = new Map(fallback.map((model) => [model.id, model]));
	for (const model of stored) metadata.set(model.id, model);

	const models: ProviderModelConfig[] = [];
	const seen = new Set<string>();
	for (const entry of remote) {
		const id = typeof entry.id === "string" ? entry.id.trim() : "";
		if (!id || seen.has(id)) continue;
		const model = normalizeModel(entry, metadata.get(id));
		if (!model) continue;
		seen.add(id);
		models.push(model);
	}
	return models;
}

function persistedModels(profile: ProviderProfile, models: ProviderModelConfig[]): Model<Api>[] {
	const api = profile.api ?? "openai-completions";
	return models.map((model) => ({
		...model,
		api,
		provider: profile.id,
		baseUrl: profile.baseUrl,
	}));
}

function catalogEntries(payload: unknown): JsonObject[] {
	if (!isObject(payload) || !Array.isArray(payload.data)) {
		throw new Error("Piso returned an invalid /models response");
	}
	return payload.data.filter(isObject);
}

function modelsUrl(baseUrl: string): string {
	return `${baseUrl.replace(/\/+$/, "")}/models`;
}

function agentDir(): string {
	return process.env.PI_AGENT_DIR?.trim() || join(homedir(), ".pi", "agent");
}

async function readJson(path: string): Promise<unknown> {
	try {
		return JSON.parse(await readFile(path, "utf8"));
	} catch {
		return undefined;
	}
}

// loadProfiles reads every dashboard-managed provider from the shared
// models.json. The file is secret-free: apiKey is always a piso_ placeholder.
export async function loadProfiles(dir = agentDir()): Promise<ProviderProfile[]> {
	const payload = await readJson(join(dir, "models.json"));
	if (!isObject(payload) || !isObject(payload.providers)) return [];
	const providers = payload.providers;
	const byId = new Map<string, ProviderProfile>();
	for (const [rawId, raw] of Object.entries(providers)) {
		if (!managedProviderId(rawId) || !isObject(raw)) continue;
		if (typeof raw.baseUrl !== "string" || !raw.baseUrl.trim()) continue;
		if (typeof raw.apiKey !== "string" || !raw.apiKey.trim()) continue;
		const id = normalizeProviderId(rawId);
		byId.set(id, {
			id,
			name: typeof raw.name === "string" && raw.name.trim() ? raw.name.trim() : undefined,
			api: typeof raw.api === "string" ? (raw.api as Api) : undefined,
			apiKey: raw.apiKey,
			baseUrl: raw.baseUrl.replace(/\/+$/, ""),
			models: Array.isArray(raw.models) ? raw.models.filter(isObject) : [],
		});
	}
	return [...byId.values()];
}

// ModelOverride is the gateway's answer to "which provider should this worker
// start on". An empty provider means the worker follows the account default.
type ModelOverride = { provider: string; model: string };

function overrideCachePath(dir: string): string {
	return join(dir, OVERRIDE_CACHE_FILE);
}

function gatewayBase(): string {
	return (process.env.GATEWAY_URL ?? "").trim().replace(/\/+$/, "");
}

function parseOverride(payload: unknown): ModelOverride | undefined {
	if (!isObject(payload)) return undefined;
	const raw = typeof payload.provider === "string" ? payload.provider.trim() : "";
	return {
		provider: raw ? normalizeProviderId(raw) : "",
		model: typeof payload.model === "string" ? payload.model.trim() : "",
	};
}

// refreshOverrideCache asks the gateway which provider this worker is pinned to
// and stores the answer next to the agent profile. Failures are silent and
// non-retrying: the previous answer stays in the cache, so an unreachable
// gateway means a worker keeps starting on the provider it last used instead of
// losing its override. The file is only rewritten when the answer changes.
async function refreshOverrideCache(dir = agentDir()): Promise<void> {
	const base = gatewayBase();
	const worker = process.env.PISO_WORKER_NAME?.trim();
	if (!base || !worker) return;
	let next: ModelOverride | undefined;
	try {
		const response = await fetch(
			`${base}/api/v1/worker/model?worker=${encodeURIComponent(worker)}`,
			{
				headers: { accept: "application/json" },
				redirect: "error",
				signal: AbortSignal.timeout(OVERRIDE_FETCH_TIMEOUT_MS),
			},
		);
		if (!response.ok) return;
		next = parseOverride(await response.json());
	} catch {
		return;
	}
	if (!next) return;
	const current = parseOverride(await readJson(overrideCachePath(dir)));
	if (current && current.provider === next.provider && current.model === next.model) return;
	try {
		await writeFile(
			overrideCachePath(dir),
			`${JSON.stringify({ provider: next.provider, model: next.model })}\n`,
		);
	} catch {
		// A read-only or full agent dir must not break the session.
	}
}

async function loadActiveDefault(dir = agentDir()): Promise<ActiveDefault> {
	// A worker override outranks the account default. The cache is written by
	// refreshOverrideCache, and an empty provider in it means "the gateway says
	// follow the account default", so a removed override cannot resurrect from
	// a stale file a restart later.
	const override = parseOverride(await readJson(overrideCachePath(dir)));
	if (override && override.provider) {
		return { provider: override.provider, model: override.model };
	}
	const payload = await readJson(join(dir, "settings.json"));
	if (!isObject(payload)) return { provider: "", model: "" };
	const provider = typeof payload.defaultProvider === "string" ? payload.defaultProvider.trim() : "";
	return {
		// The dashboard may still write the legacy namespace (or an older
		// write may not have migrated yet). loadProfiles normalizes it, so an
		// unnormalized id here would find neither the provider nor its catalog.
		provider: provider ? normalizeProviderId(provider) : "",
		model: typeof payload.defaultModel === "string" ? payload.defaultModel.trim() : "",
	};
}

// warned keeps one line per distinct problem. The shared profile is polled
// every 15s, so an unresolved default must not repeat on every tick.
const warned = new Set<string>();

function warnOnce(key: string, message: string): void {
	if (warned.has(key)) return;
	warned.add(key);
	console.warn(message);
}

// unresolvedStreak counts consecutive polls in which the dashboard default
// resolved to no model. An empty catalog is normal while providers register at
// start-up; a catalog that lists models without the configured one, or a long
// empty streak, means the saved default is stale.
const unresolvedStreak = new Map<string, number>();
const UNRESOLVED_POLLS = 8; // ~2 minutes at PROFILE_POLL_MS

// Read Pi's catalog snapshot without modifying the shared, read-only profile.
// Ignore catalogs from an old dashboard endpoint.
async function loadStartupModels(profile: ProviderProfile): Promise<ProviderModelConfig[]> {
	try {
		const payload = JSON.parse(await readFile(join(agentDir(), "models-store.json"), "utf8"));
		const models = payload?.[profile.id]?.models;
		if (!Array.isArray(models)) return [];
		return models.flatMap((model) => {
			if (!isObject(model) || model.provider !== profile.id || model.baseUrl !== profile.baseUrl) return [];
			const normalized = configModel(model);
			return normalized ? [normalized] : [];
		});
	} catch {
		return [];
	}
}

// buildProviderConfig prepares one provider: cached catalog, then the
// models.json fallback, then a bounded cold-start discovery. A failed
// discovery leaves the provider registered with an empty catalog so the
// session still starts and the default can resolve later.
async function buildProviderConfig(profile: ProviderProfile): Promise<ProviderConfig> {
	const fallback = (profile.models ?? []).flatMap((model) => {
		const normalized = configModel(model);
		return normalized ? [normalized] : [];
	});

	let startupModels = await loadStartupModels(profile);
	if (startupModels.length === 0) startupModels = fallback;
	// Interactive Pi refreshes in the background *after* initial model selection.
	// Await discovery only on a cold start, using the dashboard's literal scoped
	// placeholder. Do not interpret arbitrary credential commands here; normal
	// refresh below continues to use Pi's credential resolution.
	if (startupModels.length === 0 && !process.env.PI_OFFLINE) {
		try {
			if (!profile.apiKey.startsWith("piso_")) throw new Error("unsupported bootstrap credential");
			const response = await fetch(modelsUrl(profile.baseUrl), {
				headers: { accept: "application/json", authorization: `Bearer ${profile.apiKey}` },
				redirect: "error",
				signal: AbortSignal.timeout(STARTUP_DISCOVERY_TIMEOUT_MS),
			});
			if (!response.ok) throw new Error("discovery failed");
			startupModels = mergeCatalog(catalogEntries(await response.json()), fallback);
			if (startupModels.length === 0) throw new Error("empty catalog");
		} catch {
			// Never print the response, URL, credentials, or raw network errors.
			console.warn("Piso: initial model discovery failed (10s limit). Check the dashboard API connection, then use /model or retry attach.");
		}
	}

	const config: ProviderConfig = {
		name: profile.name ?? "Piso",
		models: startupModels,
		baseUrl: profile.baseUrl,
		api: profile.api ?? "openai-completions",
		async refreshModels(context: RefreshModelsContext): Promise<ProviderModelConfig[]> {
			const cached = (context.stored?.models ?? [])
				.filter((model) => model.provider === profile.id && model.baseUrl === profile.baseUrl)
				.map(storedModel);
			const current = cached.length > 0 ? cached : startupModels;
			if (!context.allowNetwork) return current;
			if (context.credential?.type !== "api_key" || !context.credential.key) return current;

			const headers = new Headers({
				accept: "application/json",
				authorization: `Bearer ${context.credential.key}`,
			});
			if (context.stored?.etag) headers.set("if-none-match", context.stored.etag);

			const response = await fetch(modelsUrl(profile.baseUrl), {
				headers,
				redirect: "error",
				signal: context.signal,
			});
			if (response.status === 304 && context.stored) {
				await context.publish({ persist: { ...context.stored, checkedAt: Date.now() } });
				return current;
			}
			if (!response.ok) throw new Error(`Piso model discovery failed: HTTP ${response.status}`);

			const refreshed = mergeCatalog(catalogEntries(await response.json()), fallback, cached);
			if (refreshed.length === 0) throw new Error("Piso model discovery returned no models");
			const entry: ModelsStoreEntry = {
				models: persistedModels(profile, refreshed),
				checkedAt: Date.now(),
				etag: response.headers.get("etag") ?? undefined,
			};
			await context.publish({ persist: entry });
			startupModels = refreshed;
			return refreshed;
		},
	};
	return config;
}

export default async function pisoModels(pi: ExtensionAPI): Promise<void> {
	const profiles = await loadProfiles();
	if (profiles.length === 0) return;
	for (const profile of profiles) {
		pi.registerProvider(profile.id, await buildProviderConfig(profile));
	}
	if (typeof pi.on === "function") wireLiveApply(pi, profiles);
}

// A snapshot signature covers everything the shared profile can change about
// model selection. Comparing it avoids re-applying on every poll tick.
function snapshotKey(profiles: ProviderProfile[], active: ActiveDefault): string {
	return JSON.stringify([
		profiles.map((p) => [p.id, p.baseUrl, p.api ?? "", p.models.map((m) => m.id)]),
		active.provider,
		active.model,
	]);
}

function providerConfigKey(profile: ProviderProfile): string {
	return JSON.stringify([profile.baseUrl, profile.api ?? "", profile.models.map((m) => m.id)]);
}

type LiveState = {
	signature: string;
	configs: Map<string, string>;
	applied: string;
	timer?: ReturnType<typeof setInterval>;
	pending: boolean;
};

// wireLiveApply re-reads the shared profile and applies changes to a running
// session at an idle boundary. No restart or manual /reload is needed. It
// returns the apply function so tests can drive it without waiting on timers.
// initialProfiles are the providers already registered during extension load,
// so the first pass does not redo their discovery.
export function wireLiveApply(
	pi: ExtensionAPI,
	initialProfiles: ProviderProfile[] = [],
): (ctx: ExtensionContext, force: boolean) => Promise<void> {
	const state: LiveState = { signature: "", configs: new Map(), applied: "", pending: false };
	for (const profile of initialProfiles) state.configs.set(profile.id, providerConfigKey(profile));

	const apply = async (ctx: ExtensionContext, force: boolean): Promise<void> => {
		// Ask the gateway for this worker's provider pin before reading the
		// shared profile: the pin decides which of the shared providers wins.
		await refreshOverrideCache();
		const profiles = await loadProfiles();
		const active = await loadActiveDefault();
		const signature = snapshotKey(profiles, active);

		// Reconcile providers even while busy: registration is safe and does
		// not disturb the in-flight request.
		const seen = new Set<string>();
		for (const profile of profiles) {
			seen.add(profile.id);
			const key = providerConfigKey(profile);
			if (state.configs.get(profile.id) === key) continue;
			try {
				pi.registerProvider(profile.id, await buildProviderConfig(profile));
				state.configs.set(profile.id, key);
			} catch {
				// A registration failure must not take down the session.
			}
		}
		for (const id of [...state.configs.keys()]) {
			if (seen.has(id)) continue;
			try {
				pi.unregisterProvider(id);
			} catch {
				// Ignore: the provider may already be gone.
			}
			state.configs.delete(id);
		}

		if (signature === state.signature && !force) return;

		if (!ctx.isIdle()) {
			// Defer: agent_end re-applies. Do not record the signature yet, or
			// the change would be dropped.
			state.pending = true;
			return;
		}
		if (!active.provider) {
			state.signature = signature;
			return;
		}
		const modelId = active.model || firstModelId(ctx, active.provider);
		// A dashboard-chosen model that this session's catalog does not have
		// must not degrade into "whichever model the catalog listed first":
		// that is the arbitrary-model symptom. The first-model fallback stays
		// only for the documented case where the dashboard saved no model
		// (its catalog fetch failed).
		const catalog = ctx.modelRegistry.getModelsOfType("chat", active.provider);
		const model = modelId
			? ctx.modelRegistry.find(active.provider, modelId) ?? (active.model ? undefined : catalog[0])
			: undefined;
		if (!model) {
			if (active.model) {
				const key = `unresolved:${active.provider}/${active.model}`;
				const streak = (unresolvedStreak.get(key) ?? 0) + 1;
				unresolvedStreak.set(key, streak);
				if (catalog.length > 0 || streak >= UNRESOLVED_POLLS) {
					warnOnce(
						key,
						`Piso: default model ${active.provider}/${active.model} is not in this session's catalog ` +
							`(${catalog.length} chat models registered for ${active.provider}); keeping the current model. ` +
							"Re-save the provider in the dashboard, or clear its default model to accept the first catalog entry.",
					);
				}
			} else if (catalog.length === 0) {
				// The selected provider has no models at all: a worker override
				// pointing at a provider the dashboard never wrote into the
				// shared profile lands here. Retry silently while the catalog
				// fills in, then say it once instead of looping forever.
				const key = `empty:${active.provider}`;
				const streak = (unresolvedStreak.get(key) ?? 0) + 1;
				unresolvedStreak.set(key, streak);
				if (streak >= UNRESOLVED_POLLS) {
					warnOnce(
						key,
						`Piso: provider ${active.provider} has no models registered in this session; keeping the current model. ` +
							"Re-save it in the dashboard (or clear the worker override) to apply.",
					);
				}
			}
			// The catalog may still be filling in. Retry on the next poll
			// instead of recording the change as applied.
			return;
		}
		if (active.model) unresolvedStreak.delete(`unresolved:${active.provider}/${active.model}`);
		const applied = `${active.provider}\u0000${model.id}`;
		if (applied !== state.applied) {
			const ok = await pi.setModel(model);
			if (!ok) return;
			state.applied = applied;
		}
		state.signature = signature;
	};

	const schedule = (ctx: ExtensionContext): void => {
		if (state.timer) clearInterval(state.timer);
		state.timer = setInterval(() => {
			void apply(ctx, false);
		}, PROFILE_POLL_MS);
	};

	pi.on("session_start", async (_event, ctx) => {
		await apply(ctx, true);
		schedule(ctx);
	});
	pi.on("agent_end", async (_event, ctx) => {
		if (state.pending) {
			state.pending = false;
			await apply(ctx, true);
		}
	});
	pi.on("session_shutdown", () => {
		if (state.timer) clearInterval(state.timer);
		state.timer = undefined;
	});
	return apply;
}

// firstModelId picks a deterministic default from the registered catalog when
// the dashboard could not name one (its catalog fetch failed).
function firstModelId(ctx: ExtensionContext, provider: string): string | undefined {
	for (const model of ctx.modelRegistry.getModelsOfType("chat", provider)) {
		if (model.id) return model.id;
	}
	return undefined;
}
