import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import routstrModels, { mergeCatalog, normalizeModel } from "./routstr-models";

const originalAgentDir = process.env.PI_AGENT_DIR;
const originalFetch = globalThis.fetch;
const tempDirs: string[] = [];

afterEach(async () => {
	globalThis.fetch = originalFetch;
	if (originalAgentDir === undefined) delete process.env.PI_AGENT_DIR;
	else process.env.PI_AGENT_DIR = originalAgentDir;
	await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("Routstr model discovery", () => {
	test("normalizes common Routstr metadata fields", () => {
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
		const agentDir = await mkdtemp(join(tmpdir(), "piso-routstr-"));
		tempDirs.push(agentDir);
		await mkdir(agentDir, { recursive: true });
		await writeFile(
			join(agentDir, "models.json"),
			JSON.stringify({
				providers: {
					routstr: {
						api: "openai-completions",
						apiKey: "piso_routstr_test",
						baseUrl: "https://routstr.example/v1/",
						models: [{ id: "fallback", contextWindow: 99_000 }],
					},
				},
			}),
		);
		process.env.PI_AGENT_DIR = agentDir;

		let registration: any;
		await routstrModels({
			registerProvider(name: string, config: unknown) {
				registration = { name, config };
			},
		} as any);
		expect(registration.name).toBe("routstr");

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
			credential: { type: "api_key", key: "piso_routstr_test" },
			signal: new AbortController().signal,
			publish: async (publication: any) => {
				persisted = publication.persist;
				return true;
			},
		});

		expect(request?.url).toBe("https://routstr.example/v1/models");
		expect(request?.headers.get("authorization")).toBe("Bearer piso_routstr_test");
		expect(refreshed[0]).toMatchObject({ id: "fresh", contextWindow: 250_000 });
		expect(persisted).toMatchObject({ etag: '"catalog-v1"' });
		expect(persisted.models[0]).toMatchObject({
			id: "fresh",
			provider: "routstr",
			baseUrl: "https://routstr.example/v1",
		});
	});
});
