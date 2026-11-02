import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import pisoModels, { loadProfiles, mergeCatalog, normalizeModel, wireLiveApply } from "./piso-models";

const originalAgentDir = process.env.PI_AGENT_DIR;
const originalFetch = globalThis.fetch;
const originalOffline = process.env.PI_OFFLINE;
const originalWarn = console.warn;
const originalTimeout = AbortSignal.timeout;
const originalGatewayUrl = process.env.GATEWAY_URL;
const originalWorkerName = process.env.PISO_WORKER_NAME;
const tempDirs: string[] = [];

afterEach(async () => {
	globalThis.fetch = originalFetch;
	console.warn = originalWarn;
	AbortSignal.timeout = originalTimeout;
	if (originalOffline === undefined) delete process.env.PI_OFFLINE;
	else process.env.PI_OFFLINE = originalOffline;
	if (originalAgentDir === undefined) delete process.env.PI_AGENT_DIR;
	else process.env.PI_AGENT_DIR = originalAgentDir;
	if (originalGatewayUrl === undefined) delete process.env.GATEWAY_URL;
	else process.env.GATEWAY_URL = originalGatewayUrl;
	if (originalWorkerName === undefined) delete process.env.PISO_WORKER_NAME;
	else process.env.PISO_WORKER_NAME = originalWorkerName;
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
		// Both are registered — the tab's stack is what gets published — but only
		// the pinned one carries models: a request to the other would be refused
		// on its host, so its catalog is registered empty rather than offered.
		expect(registrations[0].config.models.map((m: any) => m.id)).toEqual(["m1"]);
		expect(registrations[1].config.models).toEqual([]);
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

describe("Piso worker model overrides", () => {
	// stackDir deliberately leaves GATEWAY_URL unset: every other suite runs
	// without a gateway, and the override fetch must stay inert there.
	async function stackDir(providers: unknown, settings: unknown): Promise<string> {
		const dir = await mkdtemp(join(process.cwd(), ".piso-model-test-"));
		tempDirs.push(dir);
		process.env.PI_AGENT_DIR = dir;
		process.env.PI_OFFLINE = "1";
		delete process.env.GATEWAY_URL;
		delete process.env.PISO_WORKER_NAME;
		await writeFile(join(dir, "models.json"), JSON.stringify({ providers }));
		await writeFile(join(dir, "settings.json"), JSON.stringify(settings));
		return dir;
	}

	function liveApply(providers: Record<string, { id: string }[]>) {
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
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: (provider: string, id: string) => {
					const hit = (providers[provider] ?? []).some((m) => m.id === id);
					return hit ? { id, provider } : undefined;
				},
				getModelsOfType: (_type: string, provider: string) => providers[provider] ?? [],
			},
		};
		return { apply: wireLiveApply(pi as any), ctx: ctx as any, setModels };
	}

	const stack = {
		piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] },
		"piso-two": { apiKey: "piso_two", baseUrl: "https://two/v1", models: [{ id: "m2" }] },
	};
	const catalogs = { piso: [{ id: "m1" }], "piso-two": [{ id: "m2" }] };

	test("a gateway answer pins the worker and outranks the account default", async () => {
		const dir = await stackDir(stack, { defaultProvider: "piso", defaultModel: "m1" });
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		let requested = "";
		globalThis.fetch = (async (input: any) => {
			requested = String(input);
			return Response.json({ worker: "piso-worker-demo", slug: "demo", provider: "piso-two", model: "m2", source: "override" });
		}) as typeof fetch;

		const { apply, ctx, setModels } = liveApply(catalogs);
		await apply(ctx, true);

		expect(requested).toContain("/api/v1/worker/model?worker=piso-worker-demo");
		expect(setModels.at(-1)).toEqual({ id: "m2", provider: "piso-two" });
		// The cached answer is what a restart (or an unreachable gateway) reads.
		expect(JSON.parse(await readFile(join(dir, "piso-model.json"), "utf8"))).toEqual({
			provider: "piso-two",
			model: "m2",
			allowedModels: [],
		});
	});

	test("the approved list filters /model and is read back from the cache", async () => {
		await stackDir(stack, { defaultProvider: "piso", defaultModel: "m1" });
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		globalThis.fetch = (async () =>
			Response.json({
				provider: "piso",
				model: "m1",
				source: "override",
				allowedModels: ["m1"],
			})) as typeof fetch;

		const registrations: Array<{ name: string; config: any }> = [];
		const pi = {
			registerProvider(name: string, config: unknown) {
				registrations.push({ name, config });
			},
			unregisterProvider() {},
			setModel() {
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		await pisoModels(pi as any);

		// The answer narrows the provider it pins, and the pin itself keeps every
		// other provider out of the menu — see "Piso /model mirrors the Model
		// providers tab" for the rest.
		const piso = registrations.find((r) => r.name === "piso")?.config;
		expect(piso.models.map((m: any) => m.id)).toEqual(["m1"]);
		expect(registrations.find((r) => r.name === "piso-two")?.config.models).toEqual([]);
	});

	test("an approved list that shrinks swaps a running session off the removed model", async () => {
		const dir = await stackDir(stack, { defaultProvider: "piso", defaultModel: "m2" });
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		// A session that started while m2 was approved, against a gateway that
		// has since narrowed the list to m1.
		await writeFile(
			join(dir, "piso-model.json"),
			JSON.stringify({ provider: "piso", model: "m2", allowedModels: ["m1"] }),
		);
		const warnings: string[] = [];
		console.warn = (message: string) => warnings.push(String(message));
		globalThis.fetch = (async () => {
			throw new Error("connection refused");
		}) as typeof fetch;

		const { apply, ctx, setModels } = liveApply({
			piso: [{ id: "m1" }, { id: "m2" }],
		});
		await apply(ctx, true);

		expect(setModels.at(-1)).toEqual({ id: "m1", provider: "piso" });
		expect(warnings.join("\n")).toContain("no longer approved");
	});

	test("the account default applies when the gateway reports no override", async () => {
		await stackDir(stack, { defaultProvider: "piso", defaultModel: "m1" });
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		globalThis.fetch = (async () =>
			Response.json({ provider: "piso", model: "m1", source: "account" })) as typeof fetch;

		const { apply, ctx, setModels } = liveApply(catalogs);
		await apply(ctx, true);
		expect(setModels.at(-1)).toEqual({ id: "m1", provider: "piso" });
	});

	test("a removed override does not resurrect from the cache", async () => {
		const dir = await stackDir(stack, { defaultProvider: "piso", defaultModel: "m1" });
		await writeFile(join(dir, "piso-model.json"), JSON.stringify({ provider: "piso-two", model: "m2" }));
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		globalThis.fetch = (async () =>
			Response.json({ provider: "", model: "", source: "account" })) as typeof fetch;

		const { apply, ctx, setModels } = liveApply(catalogs);
		await apply(ctx, true);
		expect(setModels.at(-1)).toEqual({ id: "m1", provider: "piso" });
		expect(JSON.parse(await readFile(join(dir, "piso-model.json"), "utf8")).provider).toBe("");
	});

	test("an unreachable gateway keeps the cached override", async () => {
		const dir = await stackDir(stack, { defaultProvider: "piso", defaultModel: "m1" });
		await writeFile(join(dir, "piso-model.json"), JSON.stringify({ provider: "piso-two", model: "m2" }));
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		globalThis.fetch = (async () => {
			throw new Error("connection refused");
		}) as typeof fetch;

		const { apply, ctx, setModels } = liveApply(catalogs);
		await apply(ctx, true);
		expect(setModels.at(-1)).toEqual({ id: "m2", provider: "piso-two" });
		expect(JSON.parse(await readFile(join(dir, "piso-model.json"), "utf8")).provider).toBe("piso-two");
	});

	test("no gateway configured means no override request at all", async () => {
		await stackDir(stack, { defaultProvider: "piso", defaultModel: "m1" });
		let calls = 0;
		globalThis.fetch = (async () => {
			calls++;
			return Response.json({});
		}) as typeof fetch;

		const { apply, ctx, setModels } = liveApply(catalogs);
		await apply(ctx, true);
		expect(calls).toBe(0);
		expect(setModels.at(-1)).toEqual({ id: "m1", provider: "piso" });
	});
});

describe("Piso /model mirrors the Model providers tab", () => {
	async function tabDir(providers: unknown, cache?: unknown): Promise<string> {
		const dir = await mkdtemp(join(process.cwd(), ".piso-model-test-"));
		tempDirs.push(dir);
		process.env.PI_AGENT_DIR = dir;
		process.env.PI_OFFLINE = "1";
		delete process.env.GATEWAY_URL;
		delete process.env.PISO_WORKER_NAME;
		await writeFile(join(dir, "models.json"), JSON.stringify({ providers }));
		await writeFile(
			join(dir, "settings.json"),
			JSON.stringify({ defaultProvider: "piso", defaultModel: "m1" }),
		);
		// The answer the gateway gave last, written the way refreshOverrideCache
		// writes it: the default export reads the cache, not the network.
		if (cache) await writeFile(join(dir, "piso-model.json"), JSON.stringify(cache));
		return dir;
	}

	async function register(): Promise<Array<{ name: string; config: any }>> {
		const registrations: Array<{ name: string; config: any }> = [];
		await pisoModels({
			registerProvider(name: string, config: unknown) {
				registrations.push({ name, config });
			},
			unregisterProvider() {},
			on() {
				return () => {};
			},
		} as any);
		return registrations;
	}

	const stack = {
		piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }, { id: "m2" }] },
		"piso-two": { apiKey: "piso_two", baseUrl: "https://two/v1", models: [{ id: "m3" }] },
	};

	test("every provider's menu is its own allowlist, in the tab's order", async () => {
		await tabDir(stack, {
			provider: "piso",
			model: "m1",
			allowedModels: ["m2", "m1"],
			providers: { piso: ["m2", "m1"], "piso-two": [] },
		});
		const menus = Object.fromEntries(
			(await register()).map((r) => [r.name, r.config.models.map((m: any) => m.id)]),
		);
		// piso follows the tab rather than the catalog's order, and piso-two is
		// configured with nothing, so it offers nothing instead of its whole
		// catalog.
		expect(menus).toEqual({ piso: ["m2", "m1"], "piso-two": [] });
	});

	test("an approved model the catalog has not returned is still offered", async () => {
		await tabDir(stack, {
			provider: "piso",
			model: "m9",
			allowedModels: ["m1", "m9"],
			providers: { piso: ["m1", "m9"] },
		});
		const [piso] = await register();
		// The tab is the authority: hiding an approved id because discovery has
		// not seen it yet would make the tab a lie.
		expect(piso.config.models.map((m: any) => m.id)).toEqual(["m1", "m9"]);
		expect(piso.config.models[1]).toMatchObject({ id: "m9", name: "m9", contextWindow: 128_000 });
	});

	test("a provider configured with nothing registers no models and never fetches", async () => {
		await tabDir(stack, { provider: "piso", model: "", allowedModels: [], providers: { piso: [] } });
		delete process.env.PI_OFFLINE;
		let calls = 0;
		globalThis.fetch = (() => {
			calls++;
			throw new Error("unexpected network request");
		}) as typeof fetch;
		let persisted = false;
		const [piso] = await register();
		expect(piso.config.models).toEqual([]);
		const refreshed = await piso.config.refreshModels({
			allowNetwork: true,
			credential: { type: "api_key", key: "piso_one" },
			signal: new AbortController().signal,
			publish: async () => {
				persisted = true;
				return true;
			},
		});
		expect(refreshed).toEqual([]);
		expect(calls).toBe(0);
		expect(persisted).toBe(false);
	});

	test("the tab's answer survives the cache round trip", async () => {
		const dir = await tabDir(stack);
		const answers = [
			{ provider: "piso", model: "m1", allowedModels: ["m1"], providers: { piso: ["m1"], "piso-two": [] } },
			{ provider: "piso", model: "m1", allowedModels: ["m1"], providers: { piso: ["m1", "m2"] } },
		];
		process.env.GATEWAY_URL = "http://gateway:8083";
		process.env.PISO_WORKER_NAME = "piso-worker-demo";
		let next = 0;
		globalThis.fetch = (async () =>
			Response.json(answers[Math.min(next++, answers.length - 1)])) as typeof fetch;
		const pi = {
			registerProvider() {},
			unregisterProvider() {},
			setModel() {
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: () => undefined,
				getModelsOfType: () => [],
			},
		};
		const apply = wireLiveApply(pi as any);
		await apply(ctx as any, true);
		expect(JSON.parse(await readFile(join(dir, "piso-model.json"), "utf8")).providers).toEqual({
			piso: ["m1"],
			"piso-two": [],
		});
		// A later answer that approves one more model is not swallowed by the
		// "unchanged" check.
		await apply(ctx as any, false);
		expect(JSON.parse(await readFile(join(dir, "piso-model.json"), "utf8")).providers).toEqual({
			piso: ["m1", "m2"],
		});
	});

	test("a model approved on a provider this worker is not pinned to is not offered", async () => {
		// The reported bug: piso (openrouter) carries its own default model, so
		// the tab's answer for it is one approved id, and the menu offered it. Every
		// request through it then failed with 403 model-provider-mismatch, because
		// the pin sends this worker's traffic to piso-two and the proxy refuses any
		// other host. The pin is a per-worker boundary, so a list from the gateway
		// cannot re-enable the provider.
		await tabDir(
			{
				piso: { apiKey: "piso_one", baseUrl: "https://openrouter.example/v1", models: [{ id: "m1" }] },
				"piso-two": { apiKey: "piso_two", baseUrl: "https://aiml.example/v1", models: [{ id: "m2" }] },
			},
			{
				provider: "piso-two",
				model: "m2",
				allowedModels: ["m2"],
				providers: { piso: ["m1"], "piso-two": ["m2"] },
			},
		);
		const menus = Object.fromEntries(
			(await register()).map((r) => [r.name, r.config.models.map((m: any) => m.id)]),
		);
		expect(menus).toEqual({ piso: [], "piso-two": ["m2"] });
	});

	test("a local provider is denied while the pin is elsewhere", async () => {
		// A tinfoil provider is served by a proxy inside this worker, which NO_PROXY
		// keeps off the gateway, so the pin never judges it. The extension is the
		// only thing that can, and a worker the dashboard says is running on one
		// provider must not generate on another just because it is local.
		await tabDir(
			{
				piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] },
				"piso-tinfoil": { apiKey: "piso_tinfoil", baseUrl: "http://127.0.0.1:3301/v1", models: [{ id: "local-1" }] },
			},
			{
				provider: "piso",
				model: "m1",
				allowedModels: ["m1"],
				providers: { piso: ["m1"], "piso-tinfoil": ["local-1"] },
			},
		);
		const menus = Object.fromEntries(
			(await register()).map((r) => [r.name, r.config.models.map((m: any) => m.id)]),
		);
		expect(menus).toEqual({ piso: ["m1"], "piso-tinfoil": [] });
	});

	test("a local provider cannot generate while the pin is elsewhere", async () => {
		// The gateway never sees a loopback request, so the stream guard is the
		// only thing left: a tinfoil model must fail the same way any other
		// unpinned provider does, instead of reaching the enclave.
		await tabDir(
			{
				piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] },
				"piso-tinfoil": { apiKey: "piso_tinfoil", baseUrl: "http://127.0.0.1:3301/v1", models: [{ id: "local-1" }] },
			},
			{
				provider: "piso",
				model: "m1",
				allowedModels: ["m1"],
				providers: { piso: ["m1"], "piso-tinfoil": ["local-1"] },
			},
		);
		const configs = new Map((await register()).map((r) => [r.name, r.config]));
		const model = { provider: "piso-tinfoil", id: "local-1", baseUrl: "http://127.0.0.1:3301/v1" };
		expect(() => configs.get("piso-tinfoil").streamSimple(model, { messages: [] })).toThrow(
			/previous provider cannot generate/,
		);
	});

	test("an unknown pin narrows nothing", async () => {
		// Before the first successful poll there is no answer yet; blanking the
		// menu on a cold start would be worse than the catalog it always had. No
		// saved default either, so nothing names a provider to pin to.
		const dir = await tabDir({
			piso: { apiKey: "piso_one", baseUrl: "https://one/v1", models: [{ id: "m1" }] },
			"piso-two": { apiKey: "piso_two", baseUrl: "https://two/v1", models: [{ id: "m2" }] },
		});
		await writeFile(join(dir, "settings.json"), JSON.stringify({}));
		const menus = Object.fromEntries(
			(await register()).map((r) => [r.name, r.config.models.map((m: any) => m.id)]),
		);
		expect(menus).toEqual({ piso: ["m1"], "piso-two": ["m2"] });
	});

	test("a saved model the tab does not allow is switched away from, not kept", async () => {
		await tabDir({ piso: stack.piso }, {
			provider: "piso",
			model: "gone",
			allowedModels: ["m1"],
			providers: { piso: ["m2", "m1"] },
		});
		const setModels: any[] = [];
		const warnings: string[] = [];
		console.warn = (message: string) => warnings.push(String(message));
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
		// The registry still lists the model the tab dropped, so "keep the current
		// model" would leave the session on a model /model does not offer and the
		// gateway answers 403 for.
		const registry: Record<string, string[]> = { piso: ["gone", "m1", "m2"] };
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: (provider: string, id: string) =>
					(registry[provider] ?? []).includes(id) ? { id, provider } : undefined,
				getModelsOfType: (_type: string, provider: string) =>
					(registry[provider] ?? []).map((id) => ({ id, provider })),
			},
		};
		await wireLiveApply(pi as any)(ctx as any, true);
		// m2 leads the tab, so it wins over both the dropped pin and catalog order.
		expect(setModels.at(-1)).toEqual({ id: "m2", provider: "piso" });
		expect(warnings.join("\n")).toContain("no longer approved");
	});

	test("a provider the tab does not own contributes no chat models", async () => {
		const dir = await tabDir({ piso: stack.piso });
		const registrations: Array<{ name: string; config: any }> = [];
		const unregistered: string[] = [];
		const pi = {
			registerProvider(name: string, config: any) {
				registrations.push({ name, config });
			},
			unregisterProvider(id: string) {
				unregistered.push(id);
			},
			setModel() {
				return Promise.resolve(true);
			},
			on() {
				return () => {};
			},
		};
		const chat = [
			{ id: "m1", provider: "piso" },
			{ id: "gpt-4o", provider: "openai" },
		];
		const image = [{ id: "gpt-image-1", provider: "openai", type: "image" }];
		const ctx = {
			isIdle: () => true,
			modelRegistry: {
				find: (provider: string, id: string) =>
					chat.find((m) => m.provider === provider && m.id === id),
				getModelsOfType: (type: string, provider?: string) =>
					(type === "chat" ? chat : type === "image" ? image : []).filter(
						(m) => !provider || m.provider === provider,
					),
			},
		};
		const apply = wireLiveApply(pi as any);
		await apply(ctx as any, true);
		await apply(ctx as any, false);

		// Registered once, not on every poll, and with only its non-chat models:
		// the tab never spoke about image models, and codemode still needs them.
		const openai = registrations.filter((r) => r.name === "openai");
		expect(openai).toHaveLength(1);
		expect(openai[0].config.models.map((m: any) => m.id)).toEqual(["gpt-image-1"]);

		// Providers disappear from the tab: pi's own catalog comes back instead of
		// staying invisible for the rest of the session.
		await writeFile(join(dir, "models.json"), JSON.stringify({ providers: {} }));
		await apply(ctx as any, false);
		expect(unregistered).toContain("openai");
	});
});


describe("worker-discovered local policy", () => {
 test("reports discovery, retries without a config change, and blocks the previous stream", async () => {
  const dir = await profile([{id:"old"}]);
  const base = "http://127.0.0.1:3301/v1";
  await writeFile(join(dir,"models.json"),JSON.stringify({providers:{
   piso:{apiKey:"piso_old",baseUrl:"https://remote/v1",models:[{id:"old"}]},
   "piso-tinfoil":{apiKey:"piso_fake",baseUrl:base,models:[]},
  }}));
  process.env.GATEWAY_URL="http://gateway:8083";
  process.env.PISO_WORKER_NAME="piso-worker-demo";
  let ready=false; let discoveries=0; const reports:any[]=[];
  const now=Date.now; let clock=now(); Date.now=()=>clock;
  try {
   globalThis.fetch=(async(input:any,init:any)=>{
    const url=String(input);
    if(url.endsWith("/model/catalog")) { reports.push(JSON.parse(init.body)); return Response.json({ok:true}); }
    if(url.includes("/worker/model?")) return Response.json({provider:"piso-tinfoil",model:"",providers:{piso:[]},modelPolicies:{
     piso:{mode:"unavailable",baseUrl:"https://remote/v1"},
     "piso-tinfoil":{mode:"worker-discovered",discovery:"worker",baseUrl:base},
    }});
    expect(url).toBe(base+"/models"); discoveries++;
    return ready?Response.json({data:[{id:"enclave"}]}):new Response("",{status:503});
   }) as typeof fetch;
   const configs=new Map<string,any>(); const selected:any[]=[];
   const pi={registerProvider:(id:string,c:any)=>configs.set(id,c),unregisterProvider(){},on(){},setModel:async(m:any)=>{selected.push(m);return true;}};
   const ctx:any={isIdle:()=>true,modelRegistry:{
    getModelsOfType:(_t:string,id:string)=>configs.get(id)?.models.map((m:any)=>({...m,provider:id}))??[],
    find:(id:string,model:string)=>configs.get(id)?.models.find((m:any)=>m.id===model),
   }};
   const apply=wireLiveApply(pi as any);
   await apply(ctx,true);
   expect(discoveries).toBe(1);
   expect(selected).toHaveLength(0);
   expect(()=>configs.get("piso").streamSimple({provider:"piso",id:"old",baseUrl:"https://remote/v1"},{messages:[]})).toThrow(/previous provider cannot generate/);
   await apply(ctx,false); expect(discoveries).toBe(1);
   ready=true; clock+=15001;
   await apply(ctx,false);
   expect(discoveries).toBe(2);
   expect(selected.at(-1)?.id).toBe("enclave");
   expect(configs.get("piso-tinfoil").models.map((m:any)=>m.id)).toEqual(["enclave"]);
   expect(reports.some(r=>r.failed)).toBe(true);
   expect(reports.some(r=>!r.failed && r.models[0]==="enclave")).toBe(true);
  } finally { Date.now=now; }
 });
});
