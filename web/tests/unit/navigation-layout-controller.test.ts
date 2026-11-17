import { beforeEach, describe, expect, it, vi } from "vitest";

import { createPaneLayoutController, normalizePaneSizes } from "../../src/controllers/pane-layout-controller";
import { createRouteController, puaRoutePath, parsePUARoute } from "../../src/controllers/route-controller";
import { MemoryStorage } from "../fixtures/memory-storage";

describe("route and pane layout controllers", () => {
	let storage: MemoryStorage;
	beforeEach(() => {
		storage = new MemoryStorage();
		Object.defineProperty(window, "matchMedia", {
			configurable: true,
			value: vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }))
		});
	});

	it("round-trips encoded Workspace and Resource routes", () => {
		const path = puaRoutePath("workspace one", "project1.task/2");
		expect(path).toBe("/w/workspace%20one/r/project1.task%2F2");
		expect(parsePUARoute(path)).toEqual({ workspaceId: "workspace one", resourceId: "project1.task/2" });
		expect(parsePUARoute("/not-pua")).toEqual({});
	});

	it("increments route revisions and retains replace semantics", () => {
		const changed = vi.fn();
		const routes = createRouteController(changed);
		routes.project("alpha", "task1", { replace: true });
		routes.project("alpha", "task2");
		expect(routes.projection()).toEqual({ path: "/w/alpha/r/task2", revision: 2, replace: false });
		expect(changed).toHaveBeenCalledTimes(2);
	});

	it("normalizes legacy details width and persists committed pane sizes", () => {
		expect(normalizePaneSizes({ detailsWidth: 500, sidebarWidth: 100 }, 1_200)).toEqual({
			sidebarWidth: 220,
			chatWidth: 692,
			chatHeight: 320
		});
		const layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		layout.previewPane("chatWidth", 510);
		layout.commitPane("chatWidth");
		expect(JSON.parse(storage.getItem("pua.web.paneSizes") || "{}").chatWidth).toBe(510);
	});

	it("ignores and drops the former Activity height while preserving other pane sizes", () => {
		storage.setItem("pua.web.paneSizes", JSON.stringify({ sidebarWidth: 300, chatWidth: 480, sidebarAttentionHeight: 260 }));
		const layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(layout.snapshot().paneSizes).toEqual({ sidebarWidth: 300, chatWidth: 480, chatHeight: 320 });
		layout.previewPane("chatHeight", 280);
		layout.commitPane("chatHeight");
		expect(JSON.parse(storage.getItem("pua.web.paneSizes") || "{}")).toEqual({ sidebarWidth: 300, chatWidth: 480, chatHeight: 280 });
	});

	function mockMatchMedia(matches: Record<string, boolean>): void {
		Object.defineProperty(window, "matchMedia", {
			configurable: true,
			value: vi.fn((query: string) => ({ matches: Boolean(matches[query]), addEventListener: vi.fn(), removeEventListener: vi.fn() }))
		});
	}

	it("resolves the effective layout from the preference and viewport, and persists the choice", () => {
		delete document.body.dataset.layout;
		mockMatchMedia({});
		let layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(document.body.dataset.layout).toBe("three");
		expect(layout.snapshot().layout).toEqual({ preference: "auto", effective: "three" });

		mockMatchMedia({ "(max-width: 1440px)": true });
		layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(document.body.dataset.layout).toBe("two");

		layout.setLayoutPreference("split");
		expect(document.body.dataset.layout).toBe("split");
		expect(layout.snapshot().layout).toEqual({ preference: "split", effective: "split" });
		expect(storage.getItem("pua.web.layoutPreference")).toBe("split");

		mockMatchMedia({});
		layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(document.body.dataset.layout).toBe("split");

		mockMatchMedia({ "(max-width: 980px)": true, "(max-width: 1440px)": true });
		layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(document.body.dataset.layout).toBe("single");
	});

	it("applies, clamps, and persists per-column font scales", () => {
		mockMatchMedia({});
		const changed = vi.fn();
		let layout = createPaneLayoutController(changed, storage);
		layout.initialize();
		expect(layout.snapshot().fontScales).toEqual({ sidebar: 1, details: 1, chat: 1 });
		expect(document.documentElement.style.getPropertyValue("--sidebar-font-scale")).toBe("1");

		layout.setFontScale("sidebar", 1.25);
		layout.setFontScale("details", 0.5);
		layout.setFontScale("chat", 2);
		expect(layout.snapshot().fontScales).toEqual({ sidebar: 1.25, details: 0.8, chat: 1.4 });
		expect(document.documentElement.style.getPropertyValue("--sidebar-font-scale")).toBe("1.25");
		expect(document.documentElement.style.getPropertyValue("--details-font-scale")).toBe("0.8");
		expect(document.documentElement.style.getPropertyValue("--chat-font-scale")).toBe("1.4");
		expect(JSON.parse(storage.getItem("pua.web.fontScales") || "{}")).toEqual({ sidebar: 1.25, details: 0.8, chat: 1.4 });
		expect(changed).toHaveBeenCalledTimes(3);

		layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(layout.snapshot().fontScales).toEqual({ sidebar: 1.25, details: 0.8, chat: 1.4 });

		layout.resetFontScales();
		expect(layout.snapshot().fontScales).toEqual({ sidebar: 1, details: 1, chat: 1 });
		expect(storage.getItem("pua.web.fontScales")).toBeNull();
	});

	it("ignores malformed stored font scales", () => {
		storage.setItem("pua.web.fontScales", JSON.stringify({ sidebar: "wide", details: 3, chat: 1.1 }));
		const layout = createPaneLayoutController(() => undefined, storage);
		layout.initialize();
		expect(layout.snapshot().fontScales).toEqual({ sidebar: 1, details: 1.4, chat: 1.1 });
	});
});
