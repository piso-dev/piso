import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import pisoModels, { loadProfiles, mergeCatalog, normalizeModel, wireLiveApply } from "./piso-models";

const originalAgentDir = process.env.PI_AGENT_DIR;
const originalFetch = globalThis.fetch;
const originalOffline = process.env.PI_OFFLINE;
const originalWarn = console.warn;
const originalTimeout = AbortSignal.timeout;
const tempDirs: string[] = [];

afterEach(async () => {
	globalThis.fetch = originalFetch;
	console.warn = originalWarn;
	AbortSignal.timeout = originalTimeout;
	if (originalOffline === undefined) delete process.env.PI_OFFLINE;
	else process.env.PI_OFFLINE = originalOffline;
	if (originalAgentDir === undefined) delete process.env.PI_AGENT_DIR;
	else process.env.PI_AGENT_DIR = originalAgentDir;
	await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

async function profile(models: unknown[] = []): Promise<string> {
	const dir = await mkdtemp(join(process.cwd(), ".piso-model-test-"));
	tempDirs.push(dir);
	process.env.PI_AGENT_DIR = dir;
	delete process.env.PI_OFFLINE;
	await writeFile(join(dir, "models.json"), JSON.stringify({ providers: { piso: {
		apiKey: "piso_piso_test", baseUrl: "https://piso.example/v1/", models,
	} } }));
	return dir;
}

async function registration(): Promise<any> {
	let config: any;
	await pisoModels({ registerProvider(_name: string, value: unknown) { config = value; } } as any);
	return config;
}

describe("Piso model discovery", () => {
	test("cold start awaits discovery and registers models before initial selection", async () => {
		await profile();
		let release!: () => void;
		const pending = new Promise<void>((resolve) => { release = resolve; });
		let requested!: () => void;
		const started = new Promise<void>((resolve) => { requested = resolve; });
		let request: Request | undefined;
		globalThis.fetch = (async (input, init) => {
			request = new Request(input, init);
			requested();
			await pending;
			return Response.json({ data: [{ id: "first-model" }] });
		}) as typeof fetch;
		let registered = false;
		const startup = registration().then((config) => { registered = true; return config; });
		// Wait for either the request or premature registration to detect the old bug.
		await Promise.race([started, startup]);
		expect(registered).toBe(false);
		release();
		const config = await startup;
		expect(request?.url).toBe("https://piso.example/v1/models");
		expect(request?.headers.get("authorization")).toBe("Bearer piso_piso_test");
		expect(config.models[0].id).toBe("first-model");
		// Pi performs an offline refresh during startup; it must not erase discovery.
		expect(await config.refreshModels({ allowNetwork: false })).toEqual(config.models);
	});
	test("warm cache skips startup network discovery", async () => {
		const dir = await profile();
		await writeFile(join(dir, "models-store.json"), JSON.stringify({ piso: {
			models: [{ id: "cached", provider: "piso", baseUrl: "https://piso.example/v1" }],
		} }));
		globalThis.fetch = (() => { throw new Error("unexpected network request"); }) as typeof fetch;
		expect((await registration()).models[0].id).toBe("cached");
	});

	test("explicit models skip startup discovery", async () => {
		await profile([{ id: "configured" }]);
		globalThis.fetch = (() => { throw new Error("unexpected network request"); }) as typeof fetch;
		expect((await registration()).models[0].id).toBe("configured");
	});

	test("offline cold start does not fetch or warn", async () => {
		await profile();
		process.env.PI_OFFLINE = "1";
		let calls = 0;
		globalThis.fetch = (() => { calls++; throw new Error("unexpected network request"); }) as typeof fetch;
		console.warn = () => { calls++; };
		expect((await registration()).models).toEqual([]);
		expect(calls).toBe(0);
	});

	test("catalog from a previous endpoint does not suppress discovery", async () => {
		const dir = await profile();
		await writeFile(join(dir, "models-store.json"), JSON.stringify({ piso: {
			models: [{ id: "old", provider: "piso", baseUrl: "https://old.example/v1" }],
		} }));
		globalThis.fetch = (async () => Response.json({ data: [{ id: "new" }] })) as typeof fetch;
		expect((await registration()).models[0].id).toBe("new");
	});

	for (const failure of ["http", "invalid", "empty", "network", "timeout"]) {
		test(`cold-start ${failure} failure is bounded and warns without exposing credentials`, async () => {
			await profile();
			const warnings: string[] = [];
			console.warn = (message) => { warnings.push(message); };
			AbortSignal.timeout = ((ms: number) => {
				expect(ms).toBe(10_000);
				const controller = new AbortController();
				setTimeout(() => controller.abort(new Error("timed out")), 5);
				return controller.signal;
			}) as typeof AbortSignal.timeout;
			globalThis.fetch = (async (_input, init) => {
				if (failure === "timeout") {
					await new Promise((_resolve, reject) => {
						init?.signal?.addEventListener("abort", () => reject(init.signal?.reason), { once: true });
					});
				}
				if (failure === "network") throw new Error("piso_piso_test secret response");
				if (failure === "http") return new Response("secret response", { status: 401 });
				if (failure === "invalid") return new Response("secret response");
				return Response.json({ data: [] });
			}) as typeof fetch;
			const config = await registration();
			expect(config.models).toEqual([]);
			expect(warnings).toHaveLength(1);
			expect(warnings[0]).toContain("dashboard API connection");
			expect(warnings[0]).not.toContain("piso_piso_test");
			expect(warnings[0]).not.toContain("secret response");
			// Keep normal refresh available so the session can recover.
			globalThis.fetch = (async () => Response.json({ data: [{ id: "recovered" }] })) as typeof fetch;
			expect((await config.refreshModels({
				allowNetwork: true, credential: { type: "api_key", key: "piso_piso_test" },
				signal: new AbortController().signal, publish: async () => true,
			}))[0].id).toBe("recovered");
		});
	}

	test("normalizes common Piso metadata fields", () => {
		expect(
			normalizeModel({
				id: "vision-model",
				name: "Vision Model",
				context_length: 262_144,
				max_output_tokens: 32_768,
				input_modalities: ["text", "image"],
				reasoning: true,
			}),
		).toMatchObject({
			id: "vision-model",
			name: "Vision Model",
			contextWindow: 262_144,
			maxTokens: 32_768,
			input: ["text", "image"],
			reasoning: true,
		});
	});

	test("uses cached metadata ahead of models.json fallback", () => {
		const fallback = [
			{
				id: "model-a",
				name: "Fallback",
				reasoning: false,
				input: ["text"] as ("text" | "image")[],
				cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
				contextWindow: 100_000,
				maxTokens: 4_096,
			},
		];
		const cached = [{ ...fallback[0], name: "Cached", contextWindow: 200_000 }];
		const models = mergeCatalog([{ id: "model-a" }, { id: "model-b" }], fallback, cached);
		expect(models).toHaveLength(2);
		expect(models[0]).toMatchObject({ name: "Cached", contextWindow: 200_000 });
		expect(models[1]).toMatchObject({
			id: "model-b",
			name: "model-b",
			contextWindow: 128_000,
			maxTokens: 16_384,
		});
	});

	test("remote metadata overrides cache and duplicate ids are ignored", () => {
		const cached = [
			{
				id: "model-a",
				name: "Cached",
				reasoning: false,
				input: ["text"] as ("text" | "image")[],
				cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
				contextWindow: 100_000,
				maxTokens: 4_096,
			},
		];
		const models = mergeCatalog(
			[
				{ id: "model-a", name: "Remote", context_window: 300_000 },
				{ id: "model-a", name: "Duplicate" },
			],
			[],
			cached,
		);
		expect(models).toHaveLength(1);
		expect(models[0]).toMatchObject({ name: "Remote", contextWindow: 300_000, maxTokens: 4_096 });
	});

	test("registers fallback models and persists a successful refresh", async () => {
		const agentDir = await mkdtemp(join(process.cwd(), ".piso-model-test-"));
		tempDirs.push(agentDir);
		await mkdir(agentDir, { recursive: true });
		await writeFile(
			join(agentDir, "models.json"),
			JSON.stringify({
				providers: {
					piso: {
						api: "openai-completions",
						apiKey: "piso_piso_test",
						baseUrl: "https://piso.example/v1/",
						models: [{ id: "fallback", contextWindow: 99_000 }],
					},
				},
			}),
		);
		process.env.PI_AGENT_DIR = agentDir;

		let registration: any;
		await pisoModels({
			registerProvider(name: string, config: unknown) {
				registration = { name, config };
			},
		} as any);
		expect(registration.name).toBe("piso");

		const offline = await registration.config.refreshModels({
			allowNetwork: false,
			signal: new AbortController().signal,
			publish: async () => true,
		});
		expect(offline).toHaveLength(1);
		expect(offline[0]).toMatchObject({ id: "fallback", contextWindow: 99_000 });

		let request: Request | undefined;
		globalThis.fetch = (async (input, init) => {
			request = new Request(input, init);
			return new Response(JSON.stringify({ data: [{ id: "fresh", context_window: 250_000 }] }), {
				status: 200,
				headers: { "content-type": "application/json", etag: '"catalog-v1"' },
			});
		}) as typeof fetch;
		let persisted: any;
		const refreshed = await registration.config.refreshModels({
			allowNetwork: true,
			credential: { type: "api_key", key: "piso_piso_test" },
			signal: new AbortController().signal,
			publish: async (publication: any) => {
				persisted = publication.persist;
				return true;
			},
		});

		expect(request?.url).toBe("https://piso.example/v1/models");
		expect(request?.headers.get("authorization")).toBe("Bearer piso_piso_test");
		expect(refreshed[0]).toMatchObject({ id: "fresh", contextWindow: 250_000 });
		expect(persisted).toMatchObject({ etag: '"catalog-v1"' });
		expect(persisted.models[0]).toMatchObject({
			id: "fresh",
			provider: "piso",
			baseUrl: "https://piso.example/v1",
		});
	});
});

describe("Piso provider stack", () => {
	async function stackDir(providers: unknown, settings: unknown): Promise<string> {
		const dir = await mkdtemp(join(process.cwd(), ".piso-model-test-"));
		tempDirs.push(dir);
		process.env.PI_AGENT_DIR = dir;
		process.env.PI_OFFLINE = "1";
		await writeFile(join(dir, "models.json"), JSON.stringify({ providers }));
		await writeFile(join(dir, "settings.json"), JSON.stringify(settings));
		return dir;
	}

	test("registers every dashboard-managed provider and ignores others", async () => {
		await stackDir(
			{
				piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] },
				"piso-two": { apiKey: "piso_two", baseUrl: "https://two/v1", models: [{ id: "m2" }] },
				other: { apiKey: "keep", baseUrl: "https://other/v1", models: [{ id: "keep" }] },
			},
			{ defaultProvider: "piso", defaultModel: "m1" },
		);
		const registrations: Array<{ name: string; config: any }> = [];
		await pisoModels({
			registerProvider(name: string, config: unknown) {
				registrations.push({ name, config });
			},
		} as any);
		expect(registrations.map((r) => r.name)).toEqual(["piso", "piso-two"]);
		expect(registrations[1].config.models[0].id).toBe("m2");
	});

	test("loadProfiles promotes the legacy routstr namespace", async () => {
		await stackDir(
			{ routstr: { apiKey: "piso_legacy", baseUrl: "https://legacy/v1" } },
			{},
		);
		expect((await loadProfiles()).map((p) => p.id)).toEqual(["piso"]);
	});

	test("live apply re-registers changed providers and switches the active model", async () => {
		const dir = await stackDir(
			{ piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] } },
			{ defaultProvider: "piso", defaultModel: "m1" },
		);
		const setModels: any[] = [];
		const unregistered: string[] = [];
		const pi = {
			registerProvider() {},
			unregisterProvider(id: string) {
				unregistered.push(id);
			},
			setModel(model: any) {
				setModels.push(model);
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: (provider: string, id: string) => ({ id, provider }),
				getModelsOfType: (_type: string, provider: string) =>
					({
						piso: [{ id: "m1", provider: "piso" }],
						"piso-two": [{ id: "m2", provider: "piso-two" }],
					})[provider] ?? [],
			},
		};
		const apply = wireLiveApply(pi as any);
		await apply(ctx as any, true);
		expect(setModels.at(-1)?.provider).toBe("piso");
		expect(setModels.at(-1)?.id).toBe("m1");

		// The dashboard activates a second provider after its catalog failed:
		// no default model was written, so the first registered model resolves.
		await writeFile(
			join(dir, "models.json"),
			JSON.stringify({
				providers: {
					piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] },
					"piso-two": { apiKey: "piso_two", baseUrl: "https://two/v1", models: [{ id: "m2" }] },
				},
			}),
		);
		await writeFile(join(dir, "settings.json"), JSON.stringify({ defaultProvider: "piso-two", defaultModel: "" }));
		await apply(ctx as any, false);
		expect(setModels.at(-1)?.provider).toBe("piso-two");
		expect(setModels.at(-1)?.id).toBe("m2");

		// Deleting a provider unregisters it.
		await writeFile(
			join(dir, "models.json"),
			JSON.stringify({ providers: { piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] } } }),
		);
		await writeFile(join(dir, "settings.json"), JSON.stringify({ defaultProvider: "piso", defaultModel: "m1" }));
		await apply(ctx as any, false);
		expect(unregistered).toContain("piso-two");
	});

	test("live apply defers while busy and retries when the catalog resolves", async () => {
		await stackDir(
			{ piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] } },
			{ defaultProvider: "piso", defaultModel: "m1" },
		);
		const setModels: any[] = [];
		const pi = {
			registerProvider() {},
			unregisterProvider() {},
			setModel(model: any) {
				setModels.push(model);
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		let idle = false;
		let catalog: any[] = [];
		const ctx = {
			isIdle: () => idle,
			modelRegistry: {
				find: (_provider: string, id: string) => catalog.find((m) => m.id === id),
				getModelsOfType: () => catalog,
			},
		};
		const apply = wireLiveApply(pi as any);
		// Busy: the switch waits for an idle boundary.
		await apply(ctx as any, true);
		expect(setModels).toHaveLength(0);
		// Idle, but the provider catalog is not ready yet: keep the change.
		idle = true;
		await apply(ctx as any, false);
		expect(setModels).toHaveLength(0);
		// The catalog resolves; the next poll applies without a new change.
		catalog = [{ id: "m1", provider: "piso" }];
		await apply(ctx as any, false);
		expect(setModels.at(-1)?.id).toBe("m1");
	});

	test("a missing default model is not replaced by the first catalog entry", async () => {
		await stackDir(
			{ "piso-two": { apiKey: "piso_two", baseUrl: "https://two/v1", models: [{ id: "m2" }] } },
			{ defaultProvider: "piso-two", defaultModel: "deleted-model" },
		);
		const setModels: any[] = [];
		const warnings: string[] = [];
		console.warn = (message) => { warnings.push(message); };
		const pi = {
			registerProvider() {},
			unregisterProvider() {},
			setModel(model: any) {
				setModels.push(model);
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		const catalog = [{ id: "m2", provider: "piso-two" }];
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: (_provider: string, id: string) => catalog.find((m) => m.id === id),
				getModelsOfType: () => catalog,
			},
		};
		const apply = wireLiveApply(pi as any);
		await apply(ctx as any, true);
		await apply(ctx as any, false);
		// The old behavior switched to m2 here, which is how a worker ended up
		// on an arbitrary model with no explanation.
		expect(setModels).toHaveLength(0);
		expect(warnings).toHaveLength(1);
		expect(warnings[0]).toContain("piso-two/deleted-model");
		expect(warnings[0]).toContain("keeping the current model");
	});

	test("a legacy routstr default resolves against the piso namespace", async () => {
		await stackDir(
			{ routstr: { apiKey: "piso_legacy", baseUrl: "https://legacy/v1", models: [{ id: "m1" }] } },
			{ defaultProvider: "routstr", defaultModel: "m1" },
		);
		const setModels: any[] = [];
		const seen: string[] = [];
		const pi = {
			registerProvider() {},
			unregisterProvider() {},
			setModel(model: any) {
				setModels.push(model);
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: (provider: string, id: string) => {
					seen.push(provider);
					return provider === "piso" && id === "m1" ? { id, provider } : undefined;
				},
				getModelsOfType: () => [{ id: "m1", provider: "piso" }],
			},
		};
		await wireLiveApply(pi as any)(ctx as any, true);
		expect(seen).toContain("piso");
		expect(seen).not.toContain("routstr");
		expect(setModels.at(-1)).toEqual({ id: "m1", provider: "piso" });
	});
});
