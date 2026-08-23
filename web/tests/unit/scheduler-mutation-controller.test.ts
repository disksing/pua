import { describe, expect, it, vi } from "vitest";

import {
	createSchedulerMutationController,
	type SchedulerMutationContext,
	type SchedulerMutationDependencies,
	type SchedulerMutationLease,
} from "../../src/controllers/scheduler-mutation-controller";

interface PendingRequest {
	path: string;
	init?: RequestInit;
	resolve(value: unknown): void;
	reject(reason: unknown): void;
}

function deferredRequests(): { pending: PendingRequest[]; request: SchedulerMutationDependencies["request"] } {
	const pending: PendingRequest[] = [];
	const request = vi.fn((path: string, init?: RequestInit) => new Promise<unknown>((resolve, reject) => {
		pending.push({ path, init, resolve, reject });
	})) as unknown as SchedulerMutationDependencies["request"];
	return { pending, request };
}

async function release(operation: Promise<SchedulerMutationLease | null>): Promise<void> {
	const lease = await operation;
	expect(lease?.isCurrent()).toBe(true);
	lease?.release();
}

describe("SchedulerMutationController", () => {
	it("adapts Scheduler endpoints and payloads behind one mutation interface", async () => {
		const context: SchedulerMutationContext = { workspaceId: "workspace current", navigationVersion: 3, selectedId: "scheduler" };
		const request = vi.fn(async (_path: string, _init?: RequestInit) => ({}));
		const controller = createSchedulerMutationController({
			context: () => context,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			resolveResourceTitle: (resourceId) => resourceId === "project1.task1" ? "Target Task" : null,
			request: request as SchedulerMutationDependencies["request"],
		});
		const operations = controller.operations();
		const input = { description: "Review release", condition: "when green", target: "project1.task1" };

		expect(operations.validateTarget("project1.task1")).toBe("");
		expect(operations.validateTarget("missing-task")).toBe("Target must be an open resource in the current Workspace.");
		expect(operations.validateTarget("scheduler")).toBe("The Scheduler resource cannot be a schedule target.");
		await expect(operations.save({ ...input, target: "scheduler" })).resolves.toBeNull();
		expect(request).not.toHaveBeenCalled();
		await release(operations.save(input));
		await release(operations.save({ ...input, scheduleId: "schedule/one" }));
		await release(operations.setPaused("schedule/one", false));
		await release(operations.remove("schedule/one"));

		expect(request.mock.calls.map(([path, init]) => ({ path, method: init?.method, body: init?.body }))).toEqual([
			{ path: "/api/workspaces/workspace%20current/scheduler", method: "POST", body: JSON.stringify(input) },
			{ path: "/api/workspaces/workspace%20current/scheduler/schedule%2Fone", method: "PUT", body: JSON.stringify(input) },
			{ path: "/api/workspaces/workspace%20current/scheduler/schedule%2Fone/resume", method: "POST", body: undefined },
			{ path: "/api/workspaces/workspace%20current/scheduler/schedule%2Fone", method: "DELETE", body: undefined },
		]);
	});

	it("supersedes duplicate keys without cancelling independent operations", async () => {
		const context: SchedulerMutationContext = { workspaceId: "alpha", navigationVersion: 1, selectedId: "scheduler" };
		const { pending, request } = deferredRequests();
		const controller = createSchedulerMutationController({
			context: () => context,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			resolveResourceTitle: () => "Resource",
			request,
		});
		const operations = controller.operations();
		const input = { description: "Review release", condition: "when green", target: "workspace" };

		const firstSave = operations.save(input);
		const currentSave = operations.save(input);
		expect(pending[0].init?.signal?.aborted).toBe(true);
		expect(pending[1].init?.signal?.aborted).toBe(false);

		const pause = operations.setPaused("schedule-one", true);
		expect(pending[1].init?.signal?.aborted).toBe(false);
		const remove = operations.remove("schedule-one");
		expect(pending[2].init?.signal?.aborted).toBe(true);
		expect(pending[1].init?.signal?.aborted).toBe(false);
		expect(pending[3].init?.signal?.aborted).toBe(false);

		pending[0].resolve({});
		pending[2].resolve({});
		await expect(firstSave).resolves.toBeNull();
		await expect(pause).resolves.toBeNull();
		pending[1].resolve({});
		pending[3].resolve({});
		await release(currentSave);
		await release(remove);
	});

	it.each(["success", "error"] as const)("rejects a stale Workspace switch after a late %s", async (settlement) => {
		const scheduleId = "schedule-0123456789abcdef01234567";
		let context: SchedulerMutationContext = { workspaceId: "workspace-old", navigationVersion: 1, selectedId: "scheduler" };
		const { pending, request } = deferredRequests();
		const controller = createSchedulerMutationController({
			context: () => context,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			resolveResourceTitle: () => null,
			request,
		});

		const oldOperation = controller.operations().setPaused(scheduleId, true);
		expect(pending[0].path).toBe(`/api/workspaces/workspace-old/scheduler/${scheduleId}/pause`);

		context = { workspaceId: "workspace-current", navigationVersion: 2, selectedId: "scheduler" };
		controller.invalidate();
		expect(pending[0].init?.signal?.aborted).toBe(true);
		const currentOperation = controller.operations().setPaused(scheduleId, true);
		expect(pending[1].path).toBe(`/api/workspaces/workspace-current/scheduler/${scheduleId}/pause`);

		if (settlement === "success") pending[0].resolve({});
		else pending[0].reject(new Error("old Workspace failure"));
		await expect(oldOperation).resolves.toBeNull();
		expect(pending[1].init?.signal?.aborted).toBe(false);

		pending[1].resolve({});
		await release(currentOperation);
	});
});
