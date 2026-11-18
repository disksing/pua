import { describe, expect, it, vi } from "vitest";

import type { CreateDialogModel, CreateDraft } from "../../src/components/models";
import { createCreateDialogController, DEFAULT_TASK_START_PROMPT } from "../../src/controllers/create-dialog-controller";

interface RequestRecord {
	path: string;
	init?: RequestInit;
}

function harness(responder: (path: string, init?: RequestInit) => unknown | Promise<unknown> = (path) => path.endsWith("/projects") ? { id: "project2" } : { id: "project1.task1" }) {
	let published: CreateDialogModel | undefined;
	const requests: RequestRecord[] = [];
	const reloadTree = vi.fn().mockResolvedValue(undefined);
	const selectResource = vi.fn().mockResolvedValue(undefined);
	const toast = vi.fn();
	const controller = createCreateDialogController({
		workspaceId: () => "workspace-a",
		request: async <T>(path: string, init?: RequestInit): Promise<T> => {
			requests.push({ path, init });
			return await responder(path, init) as T;
		},
		publish: (model) => { published = model; },
		toast,
		reloadTree,
		selectResource,
		onOpen: vi.fn(),
		agents: () => [{ id: "agent-b", label: "Agent B", summary: "" }],
		agentProfiles: () => [{ key: "default", agentName: "agent-a" }, { key: "review", agentName: "agent-b" }],
		defaultTaskBinding: () => ({ kind: "profile", name: "default" }),
	});
	return {
		controller,
		requests,
		reloadTree,
		selectResource,
		toast,
		current: () => {
			if (!published) throw new Error("dialog was not published");
			return published;
		},
	};
}

function body(record: RequestRecord): Record<string, unknown> {
	return JSON.parse(String(record.init?.body || "{}"));
}

describe("CreateDialogController", () => {
	it("maps Project drafts to the Project API without Task-only fields", async () => {
		const test = harness();
		test.controller.open("project");
		const created = { ...test.current().draft, description: "New workspace project", slug: "new-project" };
		await test.current().onSubmit(created);

		expect(test.requests).toHaveLength(1);
		expect(test.requests[0].path).toBe("/api/workspaces/workspace-a/projects");
		expect(body(test.requests[0])).toEqual({ description: "New workspace project", slug: "new-project" });
		expect(test.reloadTree).toHaveBeenCalledOnce();
		expect(test.selectResource).toHaveBeenCalledWith("project2");
	});

	it("does not submit a project when its description is blank", async () => {
		const test = harness();
		test.controller.open("project");
		await test.current().onSubmit({ ...test.current().draft, description: " \n\t" });

		expect(test.requests).toHaveLength(0);
		expect(test.current().submitting).toBe(false);
		expect(test.current().open).toBe(true);
	});

	it("does not submit a project when its slug is invalid", async () => {
		const test = harness();
		test.controller.open("project");
		await test.current().onSubmit({ ...test.current().draft, description: "A focused project", slug: "bad slug" });

		expect(test.requests).toHaveLength(0);
		expect(test.current().submitting).toBe(false);
		expect(test.current().open).toBe(true);
	});

	it("submits an ordinary task with its title and detail", async () => {
		const test = harness();
		test.controller.open("task", "project1");
		await test.current().onSubmit({ ...test.current().draft, title: "Review", detail: "Read the plan", slug: "review" });

		expect(test.requests).toHaveLength(1);
		expect(test.requests[0].path).toBe("/api/workspaces/workspace-a/tasks");
		expect(body(test.requests[0])).toEqual({ project: "project1", title: "Review", detail: "Read the plan", slug: "review" });
		expect(test.selectResource).toHaveBeenCalledWith("project1.task1");
	});

	it("publishes the agent options and resolved default binding for the start step", () => {
		const test = harness();
		test.controller.open("task", "project1");

		expect(test.current().agents.map((agent) => agent.id)).toEqual(["agent-b"]);
		expect(test.current().agentProfiles.map((profile) => profile.key)).toEqual(["default", "review"]);
		expect(test.current().defaultTaskBinding).toEqual({ kind: "profile", name: "default" });
		expect(test.current().draft.startAfterCreate).toBe(false);
		expect(test.current().draft.startPrompt).toBe(DEFAULT_TASK_START_PROMPT);
	});

	it("starts the task with one execution message without opening chat", async () => {
		const test = harness(() => ({ id: "project1.task9" }));
		test.controller.open("task", "project1");
		const next: CreateDraft = {
			...test.current().draft,
			title: "Start me",
			detail: "Body",
			startAfterCreate: true,
			startBinding: { kind: "profile", name: "default" },
			startPrompt: "Please begin.",
		};

		await test.current().onSubmit(next);

		expect(test.requests.map((record) => record.path)).toEqual([
			"/api/workspaces/workspace-a/tasks",
			"/api/workspaces/workspace-a/resources/project1.task9/messages",
		]);
		expect(test.requests[1].init?.method).toBe("POST");
		expect(body(test.requests[1])).toEqual({ text: "Please begin.", role: "system", sender: { name: "PUA Task Start" } });
		expect(test.toast).toHaveBeenCalledWith("Task created and started.");
		expect(test.selectResource).toHaveBeenCalledWith("project1.task9");
	});

	it("switches the binding before the prompt when the start agent differs from the default", async () => {
		const test = harness(() => ({ id: "project1.task9" }));
		test.controller.open("task", "project1");
		const next: CreateDraft = {
			...test.current().draft,
			title: "Start elsewhere",
			startAfterCreate: true,
			startBinding: { kind: "agent", name: "agent-b" },
			startPrompt: "Please begin.",
		};

		await test.current().onSubmit(next);

		expect(test.requests.map((record) => record.path)).toEqual([
			"/api/workspaces/workspace-a/tasks",
			"/api/workspaces/workspace-a/resources/project1.task9/agent-binding",
			"/api/workspaces/workspace-a/resources/project1.task9/messages",
		]);
		expect(test.requests[1].init?.method).toBe("PUT");
		expect(body(test.requests[1])).toEqual({ kind: "agent", name: "agent-b" });
	});

	it("uses the default start prompt when the submitted prompt is blank", async () => {
		const test = harness();
		test.controller.open("task", "project1");
		await test.current().onSubmit({ ...test.current().draft, title: "Start", startAfterCreate: true, startPrompt: "  " });

		expect(body(test.requests[1])).toEqual({ text: DEFAULT_TASK_START_PROMPT, role: "system", sender: { name: "PUA Task Start" } });
	});

	it("still selects the created task when auto-start fails", async () => {
		const test = harness((path) => {
			if (path.endsWith("/messages")) throw new Error("mailbox offline");
			return { id: "project1.task9" };
		});
		test.controller.open("task", "project1");
		const next: CreateDraft = {
			...test.current().draft,
			title: "Start fails",
			startAfterCreate: true,
			startBinding: { kind: "profile", name: "default" },
			startPrompt: "Please begin.",
		};

		await test.current().onSubmit(next);

		expect(test.toast).toHaveBeenCalledWith("Task created, but auto-start failed: mailbox offline");
		expect(test.selectResource).toHaveBeenCalledWith("project1.task9");
	});

	it("deduplicates Task submission while the first request is pending", async () => {
		let resolveRequest: ((value: unknown) => void) | undefined;
		const test = harness(() => new Promise((resolve) => { resolveRequest = resolve; }));
		test.controller.open("task", "project1");
		const next = { ...test.current().draft, title: "Only once", detail: "Pending request" };

		const first = test.current().onSubmit(next);
		const second = test.current().onSubmit(next);
		expect(test.requests).toHaveLength(1);
		resolveRequest?.({ id: "project1.task1" });
		await Promise.all([first, second]);
		expect(test.requests).toHaveLength(1);
	});
});
