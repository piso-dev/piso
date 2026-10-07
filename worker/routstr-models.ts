import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import type { Api, Model, ModelsStoreEntry, RefreshModelsContext } from "@earendil-works/pi-ai";
import type { ExtensionAPI, ProviderModelConfig } from "@earendil-works/pi-coding-agent";

const PROVIDER_ID = "routstr";
const DEFAULT_CONTEXT_WINDOW = 128_000;
const DEFAULT_MAX_TOKENS = 16_384;
const STARTUP_DISCOVERY_TIMEOUT_MS = 10_000;

type JsonObject = Record<string, unknown>;

type RoutstrConfig = {
	api?: Api;
	apiKey: string;
	baseUrl: string;
	models?: JsonObject[];
};

function isObject(value: unknown): value is JsonObject {
	return typeof value === "object" && value !== null && !Array.isArray(value);
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

function persistedModels(models: ProviderModelConfig[], config: RoutstrConfig): Model<Api>[] {
	const api = config.api ?? "openai-completions";
	return models.map((model) => ({
		...model,
		api,
		provider: PROVIDER_ID,
		baseUrl: config.baseUrl,
	}));
}

function catalogEntries(payload: unknown): JsonObject[] {
	if (!isObject(payload) || !Array.isArray(payload.data)) {
		throw new Error("Routstr returned an invalid /models response");
	}
	return payload.data.filter(isObject);
}

function modelsUrl(baseUrl: string): string {
	return `${baseUrl.replace(/\/+$/, "")}/models`;
}

function agentDir(): string {
	return process.env.PI_AGENT_DIR?.trim() || join(homedir(), ".pi", "agent");
}

// Read Pi's catalog snapshot without modifying the shared, read-only profile.
// Ignore catalogs from an old dashboard endpoint.
async function loadStartupModels(config: RoutstrConfig): Promise<ProviderModelConfig[]> {
	try {
		const payload = JSON.parse(await readFile(join(agentDir(), "models-store.json"), "utf8"));
		const models = payload?.[PROVIDER_ID]?.models;
		if (!Array.isArray(models)) return [];
		return models.flatMap((model) => {
			if (!isObject(model) || model.provider !== PROVIDER_ID || model.baseUrl !== config.baseUrl) return [];
			const normalized = configModel(model);
			return normalized ? [normalized] : [];
		});
	} catch {
		return [];
	}
}

async function loadConfig(): Promise<RoutstrConfig | undefined> {
	let payload: unknown;
	try {
		payload = JSON.parse(await readFile(join(agentDir(), "models.json"), "utf8"));
	} catch {
		return undefined;
	}
	if (!isObject(payload) || !isObject(payload.providers) || !isObject(payload.providers[PROVIDER_ID])) return;
	const provider = payload.providers[PROVIDER_ID];
	if (typeof provider.baseUrl !== "string" || !provider.baseUrl.trim()) return;
	if (typeof provider.apiKey !== "string" || !provider.apiKey.trim()) return;
	return {
		api: typeof provider.api === "string" ? (provider.api as Api) : undefined,
		apiKey: provider.apiKey,
		baseUrl: provider.baseUrl.replace(/\/+$/, ""),
		models: Array.isArray(provider.models) ? provider.models.filter(isObject) : [],
	};
}

export default async function routstrModels(pi: ExtensionAPI): Promise<void> {
	const config = await loadConfig();
	if (!config) return;

	const fallback = (config.models ?? []).flatMap((model) => {
		const normalized = configModel(model);
		return normalized ? [normalized] : [];
	});

	let startupModels = await loadStartupModels(config);
	if (startupModels.length === 0) startupModels = fallback;
	// Interactive Pi refreshes in the background *after* initial model selection.
	// Await discovery only on a cold start, using the dashboard's literal scoped
	// placeholder. Do not interpret arbitrary credential commands here; normal
	// refresh below continues to use Pi's credential resolution.
	if (startupModels.length === 0 && !process.env.PI_OFFLINE) {
		try {
			if (!config.apiKey.startsWith("piso_")) throw new Error("unsupported bootstrap credential");
			const response = await fetch(modelsUrl(config.baseUrl), {
				headers: { accept: "application/json", authorization: `Bearer ${config.apiKey}` },
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

	pi.registerProvider(PROVIDER_ID, {
		name: "Routstr",
		models: startupModels,
		baseUrl: config.baseUrl,
		api: config.api ?? "openai-completions",
		async refreshModels(context: RefreshModelsContext): Promise<ProviderModelConfig[]> {
			const cached = (context.stored?.models ?? [])
				.filter((model) => model.provider === PROVIDER_ID && model.baseUrl === config.baseUrl)
				.map(storedModel);
			const current = cached.length > 0 ? cached : startupModels;
			if (!context.allowNetwork) return current;
			if (context.credential?.type !== "api_key" || !context.credential.key) return current;

			const headers = new Headers({
				accept: "application/json",
				authorization: `Bearer ${context.credential.key}`,
			});
			if (context.stored?.etag) headers.set("if-none-match", context.stored.etag);

			const response = await fetch(modelsUrl(config.baseUrl), {
				headers,
				redirect: "error",
				signal: context.signal,
			});
			if (response.status === 304 && context.stored) {
				await context.publish({ persist: { ...context.stored, checkedAt: Date.now() } });
				return current;
			}
			if (!response.ok) throw new Error(`Routstr model discovery failed: HTTP ${response.status}`);

			const refreshed = mergeCatalog(catalogEntries(await response.json()), fallback, cached);
			if (refreshed.length === 0) throw new Error("Routstr model discovery returned no models");
			const entry: ModelsStoreEntry = {
				models: persistedModels(refreshed, config),
				checkedAt: Date.now(),
				etag: response.headers.get("etag") ?? undefined,
			};
			await context.publish({ persist: entry });
			startupModels = refreshed;
			return refreshed;
		},
	});
}
