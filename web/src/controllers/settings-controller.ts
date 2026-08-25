import type { AgentHubConfigAgent, AgentHubConfigProvider, AgentOption, AppearanceSettings, NotificationPreferences, SettingsDraft, SettingsModel, SystemInfo, WorkspaceOption } from "../components/models";
import { confirmDialog } from "./confirm-dialog-controller";
import type { AgentConfig, AgentProfile, WorkspaceConfig } from "../models/workspace";

export type SettingsAgent = AgentConfig;
export type SettingsProfile = AgentProfile;

export type SettingsWorkspace = WorkspaceOption;

export type PUASettingsConfig = WorkspaceConfig;

export interface AgentHubData {
	mode?: string;
	configuredEndpoint?: string;
	connected?: boolean;
	compatible?: boolean;
	error?: string;
	status?: { apiVersion?: string; version?: string; capabilities?: string[] };
	catalog?: {
		providers?: Array<{ id: string; name?: string; type?: string; command?: string }>;
		agents?: Array<{ name: string; providerId?: string; options?: Record<string, string>; environment?: Record<string, string>; available?: boolean; unavailableReason?: string }>;
		probes?: Array<{ providerId: string; type?: string; command?: string; available?: boolean }>;
	};
	config?: {
		agentProfiles?: SettingsProfile[];
	};
	agentConfig?: {
		providers?: AgentHubConfigProvider[];
		agents?: AgentHubConfigAgent[];
	};
}

interface SettingsData {
	workspaces?: SettingsWorkspace[];
	activeId?: string;
	agents?: SettingsAgent[];
	agentProfiles?: SettingsProfile[];
	agentHub?: AgentHubData;
	system?: SystemInfo;
	suggestedUserName?: string;
}

export interface SettingsControllerDependencies {
	config(): PUASettingsConfig;
	setConfig(config: PUASettingsConfig): void;
	activeWorkspaceId(): string;
	setActiveWorkspaceId(id: string): void;
	selectWorkspaceResource(): void;
	request<T>(path: string, init?: RequestInit): Promise<T>;
	publish(model: SettingsModel): void;
	agentOptions(): AgentOption[];
	workspaceIcons: SettingsModel["workspaceIcons"];
	appearance(): AppearanceSettings;
	setLayoutPreference(preference: AppearanceSettings["layout"]): void;
	setFontScale(column: keyof AppearanceSettings["fontScales"], value: number): void;
	resetFontScales(): void;
	setThemePreference(theme: string): void;
	notificationPreferences(): NotificationPreferences;
	setBrowserNotifications(enabled: boolean): void;
	setCompletionSound(enabled: boolean): void;
	flushDraft(): void;
	resetAgentState(): void;
	reloadWorkspaceContext(initialUserName?: string): Promise<void>;
	clearWorkspaceContext(): void;
	renderWorkspace(): void;
	renderAgentViews(): void;
	toast(message: string): void;
}

export function normalizeWorkspaceConfig(base: PUASettingsConfig): PUASettingsConfig {
	return {
		...base,
		workspaces: base.workspaces || [],
		agents: base.agents || [],
		agentProfiles: base.agentProfiles || []
	};
}

export function configWithAgentHubCatalog(base: PUASettingsConfig, agentHub: AgentHubData): PUASettingsConfig {
	const normalized = normalizeWorkspaceConfig(base);
	const catalog = agentHub?.catalog || {};
	// AgentHub is the source of truth for Agent definitions. PUA's workspace
	// settings endpoint only contains workspaces and profile routes, so there
	// is no local agent list to merge against.
	const agents = (catalog.agents || []).map((agent) => ({
		...agent,
		id: agent.name
	}));
	return {
		...normalized,
		agents,
		agentHubProviders: catalog.providers || [],
		agentProfiles: agentHub.config?.agentProfiles || []
	};
}

export function createSettingsController(dependencies: SettingsControllerDependencies) {
	let identity = 0;
	const state: {
		open: boolean;
		identity: number;
		dataVersion: number;
		tab: SettingsDraft["tab"];
		data: SettingsData | null;
		agentDirty: boolean;
		workspacePath: string;
		createWorkspace: boolean;
		workspaceLanguage: SettingsDraft["workspaceLanguage"];
		initialUserName: string;
		workspaceIconSavingId: string;
	} = {
		open: false,
		identity: 0,
		dataVersion: 0,
		tab: "system",
		data: null,
		agentDirty: false,
		workspacePath: "",
		createWorkspace: false,
		workspaceLanguage: "en",
		initialUserName: "",
		workspaceIconSavingId: ""
	};

	function render(): void {
		const config = dependencies.config();
		const data: SettingsData = state.data || {
			workspaces: config.workspaces,
			activeId: dependencies.activeWorkspaceId(),
			agents: config.agents,
			agentProfiles: config.agentProfiles
		};
		const hub = data.agentHub || {};
		const status = hub.status || {};
		const catalog = hub.catalog || {};
		dependencies.publish({
			open: state.open,
			identity: `${state.identity}`,
			dataVersion: state.dataVersion,
			initialTab: state.tab,
			workspaces: data.workspaces || [],
			// The settings marker describes the Workspace currently being browsed.
			// `data.activeId` is the persisted fallback used when there is no route;
			// it can differ from the route while the user is viewing another Workspace.
			activeWorkspaceId: dependencies.activeWorkspaceId(),
			workspaceIcons: dependencies.workspaceIcons,
			workspaceIconSavingId: state.workspaceIconSavingId,
			suggestedUserName: String(data.suggestedUserName || config.suggestedUserName || ""),
			system: data.system || null,
			appearance: dependencies.appearance(),
			agentHub: {
				mode: hub.mode || "embedded",
				configuredEndpoint: hub.configuredEndpoint || "http://127.0.0.1:4646/agenthub",
				connected: Boolean(hub.connected),
				compatible: Boolean(hub.compatible),
				error: hub.error || "",
				apiVersion: status.apiVersion || "",
				version: status.version || "",
				capabilities: status.capabilities || [],
				providers: catalog.providers || [],
				agents: catalog.agents || [],
				probes: catalog.probes || [],
				agentConfig: {
					providers: hub.agentConfig?.providers || [],
					agents: hub.agentConfig?.agents || []
				}
			},
			profiles: (data.agentProfiles || []).map((profile) => ({ ...profile })),
			agents: dependencies.agentOptions(),
			notifications: dependencies.notificationPreferences(),
			onClose: close,
			onAddWorkspace: async (draft) => { syncDraft(draft); await addWorkspace(); },
			onRemoveWorkspace: async (id, draft) => { syncDraft(draft); await removeWorkspace(id); },
			onWorkspaceIcon: async (id, icon, draft) => { syncDraft(draft); await updateWorkspaceIcon(id, icon); },
			onSaveWorkspaceName: async (id, name, draft) => { syncDraft(draft); await updateWorkspaceName(id, name); },
			onLayoutPreference: (preference) => {
				dependencies.setLayoutPreference(preference);
				render();
			},
			onFontScale: (column, value) => {
				dependencies.setFontScale(column, value);
				render();
			},
			onResetFontScales: () => {
				dependencies.resetFontScales();
				render();
			},
			onThemePreference: (theme) => {
				dependencies.setThemePreference(theme);
				render();
			},
			onSaveAgentHub: async (draft) => { syncDraft(draft); await saveAgentSettings(); },
			onSetProviderCommand: setProviderCommand,
			onBrowserNotifications: dependencies.setBrowserNotifications,
			onCompletionSound: dependencies.setCompletionSound,
			onToast: dependencies.toast
		});
	}

	async function open(tab: SettingsDraft["tab"] = "system"): Promise<void> {
		state.open = true;
		state.identity = ++identity;
		state.tab = tab;
		state.agentDirty = false;
		state.workspaceIconSavingId = "";
		render();
		await refresh();
		render();
	}

	async function close(dirty = state.agentDirty): Promise<void> {
		if (state.open && dirty && !(await confirmDialog({ title: "Discard changes", message: "Discard unsaved agent settings changes?", confirmLabel: "Discard", danger: true }))) return;
		state.open = false;
		state.identity = ++identity;
		state.agentDirty = false;
		render();
	}

	async function refresh(): Promise<void> {
		const [base, agentHub, system] = await Promise.all([
			dependencies.request<PUASettingsConfig>("/api/workspaces"),
			dependencies.request<AgentHubData>("/api/settings/agenthub"),
			dependencies.request<SystemInfo>("/api/settings/system")
		]);
		state.data = { ...base, agentHub, system };
		state.dataVersion++;
	}

	function syncDraft(draft: SettingsDraft): void {
		if (!draft || !state.open) return;
		state.tab = draft.tab || state.tab;
		state.workspacePath = String(draft.workspacePath || "");
		state.createWorkspace = Boolean(draft.createWorkspace);
		state.workspaceLanguage = draft.workspaceLanguage === "zh-CN" ? "zh-CN" : "en";
		state.initialUserName = String(draft.initialUserName || "");
		state.agentDirty = Boolean(draft.dirty);
		state.data = {
			...state.data,
			agentHub: {
				...state.data?.agentHub,
				configuredEndpoint: String(draft.endpoint || ""),
				agentConfig: {
					providers: (draft.agentProviders || []).map((provider) => ({ ...provider })),
					agents: (draft.agentConfigs || []).map((agent) => ({
						...agent,
						options: agent.options ? { ...agent.options } : undefined,
						environment: agent.environment ? { ...agent.environment } : undefined
					}))
				}
			},
			agentProfiles: (draft.profiles || []).map((profile) => ({ ...profile }))
		};
	}

	function snapshotAgentDraft() {
		const agentConfig = state.data?.agentHub?.agentConfig || { providers: [], agents: [] };
		return {
			agentHub: {
				...state.data?.agentHub,
				agentConfig: {
					providers: (agentConfig.providers || []).map((provider) => ({ ...provider })),
					agents: (agentConfig.agents || []).map((agent) => ({
						...agent,
						options: agent.options ? { ...agent.options } : undefined,
						environment: agent.environment ? { ...agent.environment } : undefined
					}))
				}
			},
			agentProfiles: (state.data?.agentProfiles || []).map((profile) => ({ ...profile }))
		};
	}

	async function refreshPreservingAgentDraft(): Promise<void> {
		const draft = state.agentDirty ? snapshotAgentDraft() : null;
		await refresh();
		if (draft) state.data = { ...state.data, ...draft };
	}

	// externalSync refreshes the open settings modal after the serve settings
	// changed elsewhere (another browser tab, another client, or the CLI). An
	// unsaved agent draft wins: the modal keeps the user's edits and picks up
	// the server state on the next open.
	async function externalSync(): Promise<void> {
		if (!state.open || state.agentDirty) return;
		await refresh();
		render();
	}

	async function addWorkspace(): Promise<void> {
		const path = state.workspacePath.trim();
		if (!path) return;
		const created = state.createWorkspace;
		const workspace = await dependencies.request<SettingsWorkspace>("/api/workspaces", {
			method: "POST", body: JSON.stringify({
				path,
				create: created,
				...(created ? { language: state.workspaceLanguage, initialUserName: state.initialUserName } : {})
			})
		});
		dependencies.flushDraft();
		state.workspacePath = "";
		state.createWorkspace = false;
		state.workspaceLanguage = "en";
		dependencies.setConfig(normalizeWorkspaceConfig(await dependencies.request("/api/workspaces")));
		dependencies.setActiveWorkspaceId(workspace.id);
		dependencies.resetAgentState();
		dependencies.renderWorkspace();
		await dependencies.reloadWorkspaceContext(created ? state.initialUserName : undefined);
		await refreshPreservingAgentDraft();
		render();
		dependencies.toast(created ? "Workspace created." : "Workspace added.");
	}

	async function removeWorkspace(id: string): Promise<void> {
		if (!id) return;
		dependencies.flushDraft();
		await dependencies.request(`/api/workspaces/${encodeURIComponent(id)}`, { method: "DELETE" });
		const config = normalizeWorkspaceConfig(await dependencies.request<PUASettingsConfig>("/api/workspaces"));
		dependencies.setConfig(config);
		if (dependencies.activeWorkspaceId() === id) {
			const nextId = config.activeId || config.workspaces[0]?.id || "";
			dependencies.setActiveWorkspaceId(nextId);
			dependencies.selectWorkspaceResource();
			dependencies.resetAgentState();
			if (nextId) await dependencies.reloadWorkspaceContext();
			else dependencies.clearWorkspaceContext();
		} else dependencies.renderWorkspace();
		await refreshPreservingAgentDraft();
		render();
		dependencies.toast("Workspace removed from PUA.");
	}

	async function updateWorkspaceIcon(id: string, iconId: string): Promise<void> {
		if (!id || state.workspaceIconSavingId) return;
		state.workspaceIconSavingId = id;
		render();
		try {
			const workspace = await dependencies.request<SettingsWorkspace>(`/api/workspaces/${encodeURIComponent(id)}`, {
				method: "PUT", body: JSON.stringify({ icon: iconId || "" })
			});
			const replace = (items: SettingsWorkspace[] = []) => items.map((item) => item.id === workspace.id ? workspace : item);
			dependencies.setConfig({ ...dependencies.config(), workspaces: replace(dependencies.config().workspaces) });
			state.data = { ...state.data, workspaces: replace(state.data?.workspaces) };
			dependencies.renderWorkspace();
			dependencies.toast(iconId ? "Workspace icon saved." : "Workspace icon reset to the PUA default.");
		} finally {
			state.workspaceIconSavingId = "";
			render();
		}
	}

	async function updateWorkspaceName(id: string, name: string): Promise<void> {
		if (!id) return;
		const workspace = await dependencies.request<SettingsWorkspace>(`/api/workspaces/${encodeURIComponent(id)}`, {
			method: "PUT", body: JSON.stringify({ name })
		});
		const replace = (items: SettingsWorkspace[] = []) => items.map((item) => item.id === workspace.id ? workspace : item);
		dependencies.setConfig({ ...dependencies.config(), workspaces: replace(dependencies.config().workspaces) });
		state.data = { ...state.data, workspaces: replace(state.data?.workspaces) };
		dependencies.renderWorkspace();
		render();
		dependencies.toast("Workspace name saved.");
	}

	async function saveAgentSettings(): Promise<void> {
		await dependencies.request("/api/settings/agenthub", {
			method: "PUT",
			body: JSON.stringify({
				endpoint: state.data?.agentHub?.configuredEndpoint || "http://127.0.0.1:4646/agenthub",
				agentProfiles: (state.data?.agentProfiles || []).map((profile) => ({ ...profile })),
				agentProviders: (state.data?.agentHub?.agentConfig?.providers || []).map((provider) => ({ ...provider })),
				agents: (state.data?.agentHub?.agentConfig?.agents || []).map((agent) => ({
					...agent,
					options: agent.options ? { ...agent.options } : undefined,
					environment: agent.environment ? { ...agent.environment } : undefined
				}))
			})
		});
		await refresh();
		dependencies.setConfig(configWithAgentHubCatalog(await dependencies.request("/api/workspaces"), state.data?.agentHub || {}));
		state.agentDirty = false;
		dependencies.renderAgentViews();
		render();
		dependencies.toast("AgentHub settings saved.");
	}

	async function setProviderCommand(providerID: string, command: string): Promise<AgentHubConfigProvider> {
		const response = await dependencies.request<{ provider: AgentHubConfigProvider }>(`/api/settings/agenthub/providers/${encodeURIComponent(providerID)}`, {
			method: "PUT",
			body: JSON.stringify({ command })
		});
		const current = state.data?.agentHub?.agentConfig || { providers: [], agents: [] };
		const providers = (current.providers || []).map((provider) => provider.id === response.provider.id ? { ...response.provider } : { ...provider });
		if (!providers.some((provider) => provider.id === response.provider.id)) providers.push({ ...response.provider });
		state.data = {
			...state.data,
			agentHub: {
				...state.data?.agentHub,
				agentConfig: { providers, agents: (current.agents || []).map((agent) => ({ ...agent })) }
			}
		};
		await refreshPreservingAgentDraft();
		dependencies.setConfig(configWithAgentHubCatalog(await dependencies.request("/api/workspaces"), state.data?.agentHub || {}));
		dependencies.renderAgentViews();
		render();
		return response.provider;
	}

	return {
		open,
		close,
		render,
		refresh,
		externalSync,
		isOpenTab: (tab: SettingsDraft["tab"]) => state.open && state.tab === tab,
		providers: () => state.data?.agentHub?.catalog?.providers || [],
		profiles: () => state.data?.agentProfiles || [],
		withAgentHubCatalog: configWithAgentHubCatalog
	};
}
