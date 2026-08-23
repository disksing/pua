import { RequestCoordinator, StaleResponseError } from "../api/client";
import type { SchedulerMutationCallbacks, SchedulerSaveInput } from "../models/detail";
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
	scheduler?: {
		resolveResourceTitle(resourceId: string): string | null;
		reloadTree(): Promise<void>;
		reloadDetail(): Promise<void>;
		publish(): void;
		toast(message: string): void;
	};
}

export function createResourceDetailController(dependencies: ResourceDetailDependencies) {
	const schedulerRequests = new RequestCoordinator();

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

	function schedulerActions(): SchedulerMutationCallbacks {
		const identity = dependencies.context();
		return {
			validateTarget: (target) => validateSchedulerTarget(identity, target),
			save: (input) => saveSchedule(identity, input),
			setPaused: (scheduleId, paused) => mutateSchedule(
				identity,
				`schedule:${scheduleId}`,
				`/api/workspaces/${encodeURIComponent(identity.workspaceId)}/scheduler/${encodeURIComponent(scheduleId)}/${paused ? "pause" : "resume"}`,
				{ method: "POST" },
				paused ? "Schedule paused." : "Schedule resumed."
			),
			remove: (scheduleId) => mutateSchedule(
				identity,
				`schedule:${scheduleId}`,
				`/api/workspaces/${encodeURIComponent(identity.workspaceId)}/scheduler/${encodeURIComponent(scheduleId)}`,
				{ method: "DELETE" },
				"Schedule removed."
			),
		};
	}

	function validateSchedulerTarget(identity: ResourceDetailContext, value: string): string {
		const resourceId = value.trim();
		if (!resourceId) return "Target resource is required.";
		if (!schedulerIdentityIsCurrent(identity)) return "Target must be an open resource in the current Workspace.";
		if (resourceId === "workspace" || resourceId === "scheduler") return "";
		return dependencies.scheduler?.resolveResourceTitle(resourceId)
			? ""
			: "Target must be an open resource in the current Workspace.";
	}

	async function saveSchedule(identity: ResourceDetailContext, input: SchedulerSaveInput): Promise<boolean> {
		if (!input.description.trim() || !input.condition.trim() || validateSchedulerTarget(identity, input.target)) return false;
		const scheduleId = input.scheduleId || "";
		return mutateSchedule(
			identity,
			"save",
			`/api/workspaces/${encodeURIComponent(identity.workspaceId)}/scheduler${scheduleId ? `/${encodeURIComponent(scheduleId)}` : ""}`,
			{
				method: scheduleId ? "PUT" : "POST",
				body: JSON.stringify({ description: input.description, condition: input.condition, target: input.target }),
			},
			scheduleId ? "Schedule update request sent." : "Schedule request sent."
		);
	}

	async function mutateSchedule(identity: ResourceDetailContext, key: string, path: string, init: RequestInit, successMessage: string): Promise<boolean> {
		const scheduler = dependencies.scheduler;
		if (!scheduler || !schedulerIdentityIsCurrent(identity)) return false;
		const ticket = schedulerRequests.begin(`scheduler:${key}`);
		const requestIsCurrent = (): boolean => {
			try {
				schedulerRequests.assertCurrent(ticket);
				return schedulerIdentityIsCurrent(identity);
			} catch (_) {
				return false;
			}
		};
		const assertRequestIsCurrent = (): void => {
			schedulerRequests.assertCurrent(ticket);
			if (!schedulerIdentityIsCurrent(identity)) throw new StaleResponseError(ticket.scope);
		};
		try {
			await dependencies.request(path, { ...init, signal: ticket.controller.signal });
			assertRequestIsCurrent();
			await scheduler.reloadTree();
			assertRequestIsCurrent();
			await scheduler.reloadDetail();
			assertRequestIsCurrent();
			scheduler.publish();
			assertRequestIsCurrent();
			scheduler.toast(successMessage);
			return true;
		} catch (reason) {
			if (reason instanceof StaleResponseError || !requestIsCurrent()) return false;
			scheduler.toast(reason instanceof Error ? reason.message : String(reason));
			return false;
		} finally {
			schedulerRequests.finish(ticket);
		}
	}

	function schedulerIdentityIsCurrent(identity: ResourceDetailContext): boolean {
		const current = dependencies.context();
		return identity.selectedId === "scheduler"
			&& current.selectedId === identity.selectedId
			&& dependencies.isCurrentWorkspace(identity.workspaceId, identity.navigationVersion);
	}

	function invalidateSchedulerOperations(): void {
		schedulerRequests.dispose();
	}

	return { reset, snapshot, apply, fetch, load, schedulerActions, invalidateSchedulerOperations };
}
