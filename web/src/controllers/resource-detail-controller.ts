import type { ResourceRecord } from "../models/workspace";

export type ResourceDetailRecord = ResourceRecord;

export interface ResourceDetailContext {
	workspaceId: string;
	navigationVersion: number;
	selectedId: string;
	detailRequestVersion: number;
}

export interface ResourceDetailDependencies {
	details: Record<string, ResourceDetailRecord>;
	context(): ResourceDetailContext;
	nextDetailRequestVersion(): number;
	isCurrentWorkspace(workspaceId: string, navigationVersion: number): boolean;
	request<T>(path: string, init?: RequestInit): Promise<T>;
}

export function createResourceDetailController(dependencies: ResourceDetailDependencies) {
	function reset(resourceId: string): void {
		if (resourceId) delete dependencies.details[resourceId];
	}

	function snapshot(resourceId: string): { detail: ResourceDetailRecord | null } {
		return { detail: dependencies.details[resourceId] || null };
	}

	function apply(detail: ResourceDetailRecord): ResourceDetailRecord | null {
		if (!detail?.id) return null;
		dependencies.details[detail.id] = detail;
		return detail;
	}

	function fetch(resourceId: string, workspaceId = dependencies.context().workspaceId): Promise<ResourceDetailRecord> {
		return dependencies.request(`/api/workspaces/${workspaceId}/resources/${encodeURIComponent(resourceId)}`);
	}

	async function load(resourceId: string, options: { force?: boolean } = {}): Promise<ResourceDetailRecord | null | undefined> {
		if (!resourceId || resourceId === "workspace" || dependencies.details[resourceId] && !options.force) return;
		if (options.force) reset(resourceId);
		const context = dependencies.context();
		const requestVersion = dependencies.nextDetailRequestVersion();
		const detail = await fetch(resourceId, context.workspaceId);
		const current = dependencies.context();
		if (!dependencies.isCurrentWorkspace(context.workspaceId, context.navigationVersion) || current.selectedId !== resourceId || requestVersion !== current.detailRequestVersion) return null;
		return apply(detail);
	}

	return { reset, snapshot, apply, fetch, load };
}
