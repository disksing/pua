import { describe, expect, it } from "vitest";

import { createSettingsController, configWithAgentHubCatalog, normalizeWorkspaceConfig } from "../../src/controllers/settings-controller";
import type { AgentHubData, SettingsControllerDependencies } from "../../src/controllers/settings-controller";
import type { SettingsModel } from "../../src/components/models";
import { createSettingsDraft } from "../../src/components/settings-draft";
import type { PUASettingsConfig } from "../../src/controllers/settings-controller";

describe("SettingsController", () => {
	function settingsDependencies(activeWorkspaceId: string, base: PUASettingsConfig, publish: (model: SettingsModel) => void): SettingsControllerDependencies {
		return {
			config: () => base,
			setConfig: () => undefined,
			activeWorkspaceId: () => activeWorkspaceId,
			setActiveWorkspaceId: () => undefined,
			selectWorkspaceResource: () => undefined,
			request: async <T>(path: string): Promise<T> => {
				if (path === "/api/workspaces") return base as T;
				if (path === "/api/settings/agenthub") return {} as T;
				if (path === "/api/settings/system") return {
					pua: { address: "127.0.0.1", port: "4936", configPath: "/tmp/pua/serve.json", workspaces: [], buildBranch: "master", buildCommit: "pua-commit" },
					agentHub: { mode: "embedded", address: "127.0.0.1", port: "4936", endpoint: "http://127.0.0.1:4936/agenthub", connected: true, compatible: true, version: "hub-commit", paths: { config: "", sessions: "", archive: "", logs: "" }, error: "" },
				} as T;
				throw new Error(`Unexpected request: ${path}`);
			},
			publish,
			agentOptions: () => [],
			workspaceIcons: [],
			appearance: () => ({ layout: "auto" as const, fontScales: { sidebar: 1, details: 1, chat: 1 }, theme: "default", themeOptions: [{ id: "default", label: "Default", description: "The standard PUA appearance" }] }),
			setLayoutPreference: () => undefined,
			setFontScale: () => undefined,
			resetFontScales: () => undefined,
			setThemePreference: () => undefined,
			notificationPreferences: () => ({ browser: false, sound: false, permission: "default" as const, permissionError: "", soundError: "" }),
			setBrowserNotifications: () => undefined,
			setCompletionSound: () => undefined,
			flushDraft: () => undefined,
			resetAgentState: () => undefined,
			reloadWorkspaceContext: async () => undefined,
			clearWorkspaceContext: () => undefined,
			renderWorkspace: () => undefined,
			renderAgentViews: () => undefined,
			toast: () => undefined,
		};
	}

	it("marks the routed Workspace active even when the persisted fallback points elsewhere", async () => {
		const published: SettingsModel[] = [];
		const config: PUASettingsConfig = {
			activeId: "workspace-c",
			workspaces: [
				{ id: "workspace-a", name: "Workspace A", path: "/tmp/a" },
				{ id: "workspace-c", name: "Workspace C", path: "/tmp/c" },
			],
			agents: [],
			agentProfiles: [],
		};
		const controller = createSettingsController(settingsDependencies("workspace-a", config, (model) => published.push(model)));

		await controller.open();

		expect(published.at(-1)?.activeWorkspaceId).toBe("workspace-a");
		expect(published.at(-1)?.initialTab).toBe("system");
		expect(published.at(-1)?.system?.pua.configPath).toBe("/tmp/pua/serve.json");
	});

	it("keeps the routed Workspace marker aligned when route and persisted fallback match", async () => {
		const published: SettingsModel[] = [];
		const config: PUASettingsConfig = {
			activeId: "workspace-a",
			workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a" }],
			agents: [],
			agentProfiles: [],
		};
		const controller = createSettingsController(settingsDependencies("workspace-a", config, (model) => published.push(model)));

		await controller.open();

		expect(published.at(-1)?.activeWorkspaceId).toBe("workspace-a");
	});

	it("sends the selected generated-content language only when creating a Workspace", async () => {
		const published: SettingsModel[] = [];
		const requests: unknown[] = [];
		const config: PUASettingsConfig = { activeId: "", workspaces: [], agents: [], agentProfiles: [], suggestedUserName: "ServerUser" };
		const dependencies = settingsDependencies("", config, (model) => published.push(model));
		const baseRequest = dependencies.request;
		dependencies.request = async <T>(path: string, init?: RequestInit): Promise<T> => {
			if (path === "/api/workspaces" && init?.method === "POST") {
				requests.push(JSON.parse(String(init.body)));
				return { id: "workspace-new", name: "new", path: "/tmp/new" } as T;
			}
			return baseRequest<T>(path);
		};
		const controller = createSettingsController(dependencies);
		await controller.open();
		const blankDraft = createSettingsDraft(published.at(-1)!);
		await published.at(-1)!.onAddWorkspace(blankDraft);
		expect(requests).toHaveLength(0);

		const draft = createSettingsDraft(published.at(-1)!);
		draft.workspacePath = "/tmp/new";
		draft.createWorkspace = true;
		draft.workspaceLanguage = "zh-CN";
		await published.at(-1)!.onAddWorkspace(draft);

		expect(requests).toEqual([{ path: "/tmp/new", create: true, language: "zh-CN", initialUserName: "ServerUser" }]);
	});

	it("joins AgentHub availability and profiles into the PUA configuration without mutating the base", () => {
		const base = {
			activeId: "alpha",
			workspaces: [],
			agents: [],
			agentProfiles: []
		};
		const merged = configWithAgentHubCatalog(base, {
			catalog: {
				providers: [{ id: "openai", name: "OpenAI" }],
				agents: [
					{ name: "Codex", providerId: "openai", available: true },
					{ name: "Offline", providerId: "openai", available: false, unavailableReason: "offline" }
				]
			},
			config: { agentProfiles: [{ key: "default", description: "", agentName: "Codex" }] }
		});

		expect(merged.agents).toEqual([
			{ id: "Codex", name: "Codex", providerId: "openai", available: true },
			{ id: "Offline", name: "Offline", providerId: "openai", available: false, unavailableReason: "offline" }
		]);
		expect(merged.agentHubProviders).toEqual([{ id: "openai", name: "OpenAI" }]);
		expect(merged.agentProfiles).toEqual([{ key: "default", description: "", agentName: "Codex" }]);
		expect(base.agents).toEqual([]);
	});

	it("normalizes null arrays from persisted or older server responses", () => {
		const malformed = {
			activeId: "",
			workspaces: null,
			agents: null,
			agentProfiles: null,
		} as unknown as PUASettingsConfig;

		expect(normalizeWorkspaceConfig(malformed)).toMatchObject({
			workspaces: [],
			agents: [],
			agentProfiles: [],
		});
		expect(configWithAgentHubCatalog(malformed, {})).toMatchObject({
			workspaces: [],
			agents: [],
			agentProfiles: [],
		});
	});

	it("saves workspace names through the workspace endpoint and refreshes the published model", async () => {
		const published: SettingsModel[] = [];
		const requests: Array<{ path: string; body: unknown }> = [];
		const config: PUASettingsConfig = {
			activeId: "workspace-a",
			workspaces: [{ id: "workspace-a", name: "a", path: "/tmp/a" }],
			agents: [],
			agentProfiles: [],
		};
		let current = config;
		const dependencies = settingsDependencies("workspace-a", config, (model) => published.push(model));
		dependencies.config = () => current;
		dependencies.setConfig = (next) => { current = next; };
		const baseRequest = dependencies.request;
		dependencies.request = async <T>(path: string, init?: RequestInit): Promise<T> => {
			if (init?.method === "PUT" && path === "/api/workspaces/workspace-a") {
				requests.push({ path, body: JSON.parse(String(init.body)) });
				return { ...current.workspaces[0], name: "Named Workspace" } as T;
			}
			return baseRequest<T>(path);
		};
		let workspaceRenders = 0;
		dependencies.renderWorkspace = () => { workspaceRenders++; };
		const toasts: string[] = [];
		dependencies.toast = (message) => toasts.push(message);
		const controller = createSettingsController(dependencies);

		await controller.open();
		const model = published.at(-1)!;
		await model.onSaveWorkspaceName("workspace-a", "Named Workspace", createSettingsDraft(model));

		expect(requests).toEqual([{ path: "/api/workspaces/workspace-a", body: { name: "Named Workspace" } }]);
		expect(current.workspaces[0]?.name).toBe("Named Workspace");
		expect(published.at(-1)?.workspaces[0]?.name).toBe("Named Workspace");
		expect(workspaceRenders).toBeGreaterThan(0);
		expect(toasts).toContain("Workspace name saved.");
	});

	it("saves AgentHub providers and agents through the AgentHub-backed settings endpoint", async () => {
		const published: SettingsModel[] = [];
		const base: PUASettingsConfig = {
			activeId: "workspace-a",
			workspaces: [{ id: "workspace-a", name: "a", path: "/tmp/a" }],
			agents: [],
			agentProfiles: [],
		};
		let hub: AgentHubData = {
			mode: "external",
			configuredEndpoint: "http://127.0.0.1:4646/agenthub",
			connected: true,
			compatible: true,
			status: { apiVersion: "1", version: "test", capabilities: [] },
			catalog: { providers: [{ id: "codex", name: "Codex", type: "codex" }], agents: [], probes: [] },
			config: { agentProfiles: [] },
			agentConfig: {
				providers: [{ id: "codex", name: "Codex", type: "codex" }],
				agents: [{ name: "Default", providerId: "codex" }],
			},
		};
		const requests: Array<{ path: string; body?: unknown }> = [];
		const dependencies = settingsDependencies("workspace-a", base, (model) => published.push(model));
		dependencies.request = async <T>(path: string, init?: RequestInit): Promise<T> => {
			if (path === "/api/workspaces") return base as T;
			if (path === "/api/settings/agenthub" && init?.method === "PUT") {
				const body = JSON.parse(String(init.body));
				requests.push({ path, body });
				hub = { ...hub, configuredEndpoint: body.endpoint, config: { agentProfiles: body.agentProfiles }, agentConfig: { providers: body.agentProviders, agents: body.agents } };
				return hub as T;
			}
			if (path === "/api/settings/agenthub/providers/codex" && init?.method === "PUT") {
				const body = JSON.parse(String(init.body));
				requests.push({ path, body });
				const currentProvider = hub.agentConfig?.providers?.[0];
				if (!currentProvider) throw new Error("missing test provider");
				if (body.command === "/bad/path") throw new Error("codex executable \"/bad/path\" not found");
				const provider = { ...currentProvider, command: body.command || undefined };
				hub = { ...hub, agentConfig: { ...hub.agentConfig!, providers: [provider] } };
				return { provider } as T;
			}
			if (path === "/api/settings/agenthub") return hub as T;
			if (path === "/api/settings/system") return {
				pua: { address: "", port: "", configPath: "", workspaces: [], buildBranch: "", buildCommit: "" },
				agentHub: { mode: "", address: "", port: "", endpoint: "", connected: false, compatible: false, version: "", paths: { config: "", sessions: "", archive: "", logs: "" }, error: "" },
			} as T;
			throw new Error(`Unexpected request: ${path}`);
		};
		const controller = createSettingsController(dependencies);
		await controller.open("agenthub");

		const draft = createSettingsDraft(published.at(-1)!);
		draft.agentProviders[0]!.command = "/opt/homebrew/bin/codex";
		draft.agentConfigs[0]!.name = "Worker";
		draft.dirty = true;
		await published.at(-1)!.onSaveAgentHub(draft);
		const savedBody = requests.find((request) => request.path === "/api/settings/agenthub")?.body as Record<string, unknown>;
		expect(savedBody.agentProviders).toEqual([{ id: "codex", name: "Codex", type: "codex", command: "/opt/homebrew/bin/codex" }]);
		expect(savedBody.agents).toEqual([{ name: "Worker", providerId: "codex" }]);

		await published.at(-1)!.onSetProviderCommand("codex", "/usr/local/bin/codex");
		expect(requests.at(-1)).toEqual({ path: "/api/settings/agenthub/providers/codex", body: { command: "/usr/local/bin/codex" } });
		expect(published.at(-1)?.agentHub.agentConfig?.providers[0]?.command).toBe("/usr/local/bin/codex");
	});

	it("externalSync refreshes an open modal with the latest server settings", async () => {
		const published: SettingsModel[] = [];
		let current: PUASettingsConfig = {
			activeId: "workspace-a",
			workspaces: [{ id: "workspace-a", name: "a", path: "/tmp/a" }],
			agents: [],
			agentProfiles: [{ key: "default", description: "", agentName: "old-agent" }],
		};
		const dependencies = settingsDependencies("workspace-a", current, (model) => published.push(model));
		let settingsReads = 0;
		const baseRequest = dependencies.request;
		dependencies.request = async <T>(path: string, init?: RequestInit): Promise<T> => {
			if (path === "/api/workspaces" && !init) return current as T;
			if (path === "/api/settings/agenthub") {
				settingsReads++;
				return { config: { agentProfiles: current.agentProfiles } } as T;
			}
			return baseRequest<T>(path, init);
		};
		const controller = createSettingsController(dependencies);

		await controller.open();
		expect(published.at(-1)?.profiles[0]?.agentName).toBe("old-agent");

		// Another tab saved new profile routes; the next external sync must pick
		// them up without reopening the modal.
		current = { ...current, agentProfiles: [{ key: "default", description: "", agentName: "new-agent" }] };
		const readsBefore = settingsReads;
		await controller.externalSync();

		expect(settingsReads).toBe(readsBefore + 1);
		expect(published.at(-1)?.profiles[0]?.agentName).toBe("new-agent");
	});

	it("externalSync keeps an unsaved agent draft untouched", async () => {
		const published: SettingsModel[] = [];
		const config: PUASettingsConfig = {
			activeId: "workspace-a",
			workspaces: [{ id: "workspace-a", name: "a", path: "/tmp/a" }],
			agents: [],
			agentProfiles: [{ key: "default", description: "", agentName: "old-agent" }],
		};
		const dependencies = settingsDependencies("workspace-a", config, (model) => published.push(model));
		let settingsReads = 0;
		const baseRequest = dependencies.request;
		dependencies.request = async <T>(path: string, init?: RequestInit): Promise<T> => {
			if (path === "/api/settings/agenthub") {
				settingsReads++;
				return { config: { agentProfiles: config.agentProfiles } } as T;
			}
			if (init?.method === "PUT" && path === "/api/workspaces/workspace-a") return { ...config.workspaces[0], icon: "research-lab" } as T;
			return baseRequest<T>(path, init);
		};
		const controller = createSettingsController(dependencies);

		await controller.open();
		// Editing the agent settings marks the modal dirty through the draft that
		// accompanies every settings action.
		const dirtyDraft = { ...createSettingsDraft(published.at(-1)!), dirty: true };
		await published.at(-1)!.onWorkspaceIcon("workspace-a", "research-lab", dirtyDraft);

		const readsBefore = settingsReads;
		await controller.externalSync();

		expect(settingsReads).toBe(readsBefore);
	});

	it("externalSync is a no-op while the modal is closed", async () => {
		const config: PUASettingsConfig = {
			activeId: "workspace-a",
			workspaces: [{ id: "workspace-a", name: "a", path: "/tmp/a" }],
			agents: [],
			agentProfiles: [],
		};
		const dependencies = settingsDependencies("workspace-a", config, () => undefined);
		let requests = 0;
		const baseRequest = dependencies.request;
		dependencies.request = async <T>(path: string, init?: RequestInit): Promise<T> => {
			requests++;
			return baseRequest<T>(path, init);
		};
		const controller = createSettingsController(dependencies);

		await controller.externalSync();

		expect(requests).toBe(0);
	});
});
