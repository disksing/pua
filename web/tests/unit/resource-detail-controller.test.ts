import { describe, expect, it, vi } from "vitest";

import { createResourceDetailController, type ResourceDetailContext, type ResourceDetailDependencies, type ResourceDetailRecord } from "../../src/controllers/resource-detail-controller";

describe("ResourceDetailController", () => {
	it("loads a resource detail without History query parameters", async () => {
		const details: Record<string, ResourceDetailRecord> = {};
		const context: ResourceDetailContext = { workspaceId: "alpha", navigationVersion: 1, selectedId: "task1", detailRequestVersion: 0 };
		const request = vi.fn(async <T>(_path: string, _init?: RequestInit) => ({ id: "task1", type: "task", artifacts: [] } as T));
		const controller = createResourceDetailController({
			details,
			context: () => context,
			nextDetailRequestVersion: () => ++context.detailRequestVersion,
			isCurrentWorkspace: () => true,
			request: request as unknown as <T>(path: string, init?: RequestInit) => Promise<T>,
		});

		await controller.load("task1");

		expect(request).toHaveBeenCalledWith("/api/workspaces/alpha/resources/task1");
		expect(details.task1).toMatchObject({ id: "task1" });
	});

	it("rejects a late Resource result after selection changes", async () => {
		const details: Record<string, ResourceDetailRecord> = {};
		let context: ResourceDetailContext = { workspaceId: "alpha", navigationVersion: 1, selectedId: "task1", detailRequestVersion: 0 };
		let resolve!: (detail: ResourceDetailRecord) => void;
		const request = vi.fn(() => new Promise<ResourceDetailRecord>((done) => { resolve = done; }));
		const controller = createResourceDetailController({
			details,
			context: () => context,
			nextDetailRequestVersion: () => ++context.detailRequestVersion,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			request: async <T>() => await request() as T,
		});

		const pending = controller.load("task1");
		context = { ...context, selectedId: "task2", navigationVersion: 2 };
		resolve({ id: "task1", artifacts: [] });

		expect(await pending).toBeNull();
		expect(details).toEqual({});
	});

	it.each(["success", "error"] as const)("ignores a stale Scheduler %s without clearing the current operation", async (settlement) => {
		const scheduleId = "schedule-0123456789abcdef01234567";
		let context: ResourceDetailContext = { workspaceId: "workspace-old", navigationVersion: 1, selectedId: "scheduler", detailRequestVersion: 0 };
		const pending: Array<{
			path: string;
			init?: RequestInit;
			resolve(value: unknown): void;
			reject(reason: unknown): void;
		}> = [];
		const request = vi.fn((path: string, init?: RequestInit) => new Promise<unknown>((resolve, reject) => {
			pending.push({ path, init, resolve, reject });
		}));
		const reloadTree = vi.fn(async () => undefined);
		const reloadDetail = vi.fn(async () => undefined);
		const publish = vi.fn();
		const toast = vi.fn();
		const controller = createResourceDetailController({
			details: {},
			context: () => context,
			nextDetailRequestVersion: () => ++context.detailRequestVersion,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			request: request as ResourceDetailDependencies["request"],
			scheduler: {
				resolveResourceTitle: () => null,
				reloadTree,
				reloadDetail,
				publish,
				toast,
			},
		});

		const oldOperation = controller.schedulerActions().setPaused(scheduleId, true);
		expect(pending).toHaveLength(1);
		expect(pending[0].path).toBe(`/api/workspaces/workspace-old/scheduler/${scheduleId}/pause`);
		expect(pending[0].init?.signal?.aborted).toBe(false);

		context = { workspaceId: "workspace-current", navigationVersion: 2, selectedId: "scheduler", detailRequestVersion: 0 };
		controller.invalidateSchedulerOperations();
		expect(pending[0].init?.signal?.aborted).toBe(true);
		const currentOperation = controller.schedulerActions().setPaused(scheduleId, true);
		expect(pending).toHaveLength(2);
		expect(pending[1].path).toBe(`/api/workspaces/workspace-current/scheduler/${scheduleId}/pause`);

		if (settlement === "success") pending[0].resolve({});
		else pending[0].reject(new Error("old Workspace failure"));
		await expect(oldOperation).resolves.toBe(false);
		expect(reloadTree).not.toHaveBeenCalled();
		expect(reloadDetail).not.toHaveBeenCalled();
		expect(publish).not.toHaveBeenCalled();
		expect(toast).not.toHaveBeenCalled();
		expect(pending[1].init?.signal?.aborted).toBe(false);

		pending[1].resolve({});
		await expect(currentOperation).resolves.toBe(true);
		expect(reloadTree).toHaveBeenCalledOnce();
		expect(reloadDetail).toHaveBeenCalledOnce();
		expect(publish).toHaveBeenCalledOnce();
		expect(toast).toHaveBeenCalledWith("Schedule paused.");
	});

	it("keeps Scheduler mutation endpoints, payloads, and messages behind its callbacks", async () => {
		const context: ResourceDetailContext = { workspaceId: "workspace current", navigationVersion: 3, selectedId: "scheduler", detailRequestVersion: 0 };
		const request = vi.fn(async (_path: string, _init?: RequestInit) => ({}));
		const toast = vi.fn();
		const controller = createResourceDetailController({
			details: {},
			context: () => context,
			nextDetailRequestVersion: () => ++context.detailRequestVersion,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			request: request as ResourceDetailDependencies["request"],
			scheduler: {
				resolveResourceTitle: (resourceId) => resourceId === "project1.task1" ? "Target Task" : null,
				reloadTree: async () => undefined,
				reloadDetail: async () => undefined,
				publish: () => undefined,
				toast,
			},
		});
		const actions = controller.schedulerActions();
		const input = { description: "Review release", condition: "when green", target: "project1.task1" };

		await expect(actions.save(input)).resolves.toBe(true);
		await expect(actions.save({ ...input, scheduleId: "schedule/one" })).resolves.toBe(true);
		await expect(actions.setPaused("schedule/one", false)).resolves.toBe(true);
		await expect(actions.remove("schedule/one")).resolves.toBe(true);

		expect(request.mock.calls.map(([path, init]) => ({ path, method: init?.method, body: init?.body }))).toEqual([
			{ path: "/api/workspaces/workspace%20current/scheduler", method: "POST", body: JSON.stringify(input) },
			{ path: "/api/workspaces/workspace%20current/scheduler/schedule%2Fone", method: "PUT", body: JSON.stringify(input) },
			{ path: "/api/workspaces/workspace%20current/scheduler/schedule%2Fone/resume", method: "POST", body: undefined },
			{ path: "/api/workspaces/workspace%20current/scheduler/schedule%2Fone", method: "DELETE", body: undefined },
		]);
		expect(toast.mock.calls.map(([message]) => message)).toEqual([
			"Schedule request sent.",
			"Schedule update request sent.",
			"Schedule resumed.",
			"Schedule removed.",
		]);
	});
});
