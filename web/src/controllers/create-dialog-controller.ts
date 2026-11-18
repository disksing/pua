import type { AgentOption } from "../models/common";
import { isValidResourceSlug, type CreateDialogModel, type CreateDraft } from "../models/create";
import type { ResourceAgentBindingModel, ResourceAgentProfileModel } from "../models/detail";
import { errorMessage } from "../runtime/errors";

interface CreateDialogState extends CreateDraft {
	identity: number;
	open: boolean;
	submitting: boolean;
}

export interface CreateDialogDependencies {
	workspaceId(): string;
	request<T>(path: string, init?: RequestInit): Promise<T>;
	publish(model: CreateDialogModel): void;
	toast(message: string): void;
	reloadTree(): Promise<void>;
	selectResource(resourceId: string): Promise<void>;
	onOpen(): void;
	agents(): AgentOption[];
	agentProfiles(): ResourceAgentProfileModel[];
	defaultTaskBinding(projectId: string): ResourceAgentBindingModel;
}

interface CreatedResource {
	id?: unknown;
}

export const DEFAULT_TASK_START_PROMPT = "请阅读 AGENTS.md、spec.md 和 memory.md，了解任务背景后开始工作。";

function createdResourceId(result: CreatedResource): string {
	const id = String(result?.id || "").trim();
	if (!id) throw new Error("The created resource did not return an id.");
	return id;
}

function sameBinding(left: ResourceAgentBindingModel, right: ResourceAgentBindingModel): boolean {
	return left.kind === right.kind && left.name.trim().toLowerCase() === right.name.trim().toLowerCase();
}

function emptyState(identity: number): CreateDialogState {
	return {
		open: false,
		identity,
		type: "project",
		projectId: "",
		startAfterCreate: false,
		startBinding: { kind: "profile", name: "" },
		startPrompt: DEFAULT_TASK_START_PROMPT,
		title: "",
		description: "",
		detail: "",
		slug: "",
		submitting: false
	};
}

export function createCreateDialogController(dependencies: CreateDialogDependencies) {
	let identity = 0;
	let state = emptyState(identity);

	function render(): void {
		const { open, identity, submitting, ...draft } = state;
		dependencies.publish({
			open,
			identity: `${identity}:${draft.type}:${draft.projectId}`,
			workspaceId: dependencies.workspaceId(),
			draft: { ...draft, startBinding: { ...draft.startBinding } },
			submitting,
			agents: dependencies.agents(),
			agentProfiles: dependencies.agentProfiles(),
			defaultTaskBinding: draft.type === "task" ? dependencies.defaultTaskBinding(draft.projectId) : { kind: "profile", name: "" },
			onClose: close,
			onSubmit: submit
		});
	}

	function open(type: "project" | "task", projectId = ""): void {
		state = {
			...emptyState(++identity),
			open: true,
			type,
			projectId
		};
		dependencies.onOpen();
		render();
	}

	function close(): void {
		if (state.submitting) return;
		state = emptyState(++identity);
		render();
	}

	async function startCreatedTask(workspaceId: string, resourceId: string, draft: CreateDraft, fallback: ResourceAgentBindingModel): Promise<void> {
		const selected = draft.startBinding.name ? draft.startBinding : fallback;
		const resourcePath = `/api/workspaces/${workspaceId}/resources/${encodeURIComponent(resourceId)}`;
		if (selected.name && !sameBinding(selected, fallback)) {
			await dependencies.request(`${resourcePath}/agent-binding`, {
				method: "PUT",
				body: JSON.stringify({ kind: selected.kind, name: selected.name })
			});
		}
		const text = draft.startPrompt.trim() || DEFAULT_TASK_START_PROMPT;
		await dependencies.request(`${resourcePath}/messages`, {
			method: "POST",
			body: JSON.stringify({ text, role: "system", sender: { name: "PUA Task Start" } })
		});
	}

	async function submit(next: CreateDraft): Promise<void> {
		if (!state.open || state.submitting) return;
		const title = next.type === "project" ? next.description : next.title;
		if (!title.trim() || (next.type === "project" && !isValidResourceSlug(next.slug))) return;
		state = { ...state, ...next, startBinding: { ...next.startBinding }, submitting: true };
		const draft = state;
		const workspaceId = dependencies.workspaceId();
		const dialogIdentity = state.identity;
		render();
		try {
			let resourceId = "";
			if (draft.type === "project") {
				const created = await dependencies.request<CreatedResource>(`/api/workspaces/${workspaceId}/projects`, {
					method: "POST",
					body: JSON.stringify({ description: draft.description, slug: draft.slug })
				});
				resourceId = createdResourceId(created);
				dependencies.toast("Project created.");
			} else {
				const fallback = dependencies.defaultTaskBinding(draft.projectId);
				const created = await dependencies.request<CreatedResource>(`/api/workspaces/${workspaceId}/tasks`, {
					method: "POST",
					body: JSON.stringify({ project: draft.projectId, title: draft.title, detail: draft.detail, slug: draft.slug })
				});
				resourceId = createdResourceId(created);
				if (draft.startAfterCreate) {
					try {
						await startCreatedTask(workspaceId, resourceId, draft, fallback);
						dependencies.toast("Task created and started.");
					} catch (error) {
						dependencies.toast(`Task created, but auto-start failed: ${errorMessage(error)}`);
					}
				} else {
					dependencies.toast("Task created.");
				}
			}
			if (workspaceId !== dependencies.workspaceId() || state.identity !== dialogIdentity) return;
			state.open = false;
			const completedDialogIdentity = ++identity;
			state.identity = completedDialogIdentity;
			await dependencies.reloadTree();
			if (workspaceId === dependencies.workspaceId() && state.identity === completedDialogIdentity) await dependencies.selectResource(resourceId);
		} catch (error) {
			if (state.identity === dialogIdentity) {
				state.submitting = false;
				render();
				dependencies.toast(errorMessage(error));
			}
		}
	}

	return { open, close, render };
}
