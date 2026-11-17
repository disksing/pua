const PANE_SIZE_KEY = "pua.web.paneSizes";
const LAYOUT_PREFERENCE_KEY = "pua.web.layoutPreference";
const FONT_SCALE_KEY = "pua.web.fontScales";
const PANE_HANDLE_WIDTH = 8;
const SIDEBAR_MIN_WIDTH = 220;
const DETAILS_MIN_WIDTH = 360;
const CHAT_MIN_WIDTH = 320;
// Stacked two-column mode shrinks each pane down to its 56px header band.
const CHAT_MIN_HEIGHT = 56;
const PANE_MAX_SIZE = 10_000;

export interface PaneSizes {
	sidebarWidth: number;
	chatWidth: number;
	chatHeight: number;
}

export interface MobilePaneState {
	sidebarOpen: boolean;
}

export type LayoutPreference = "auto" | "three" | "two" | "split";
export type EffectiveLayout = "three" | "two" | "split" | "single";

export interface LayoutState {
	preference: LayoutPreference;
	effective: EffectiveLayout;
}

export type FontScaleColumn = "sidebar" | "details" | "chat";

export type FontScales = Record<FontScaleColumn, number>;

export type PaneName = keyof PaneSizes;

const PANE_DEFAULTS: PaneSizes = Object.freeze({
	sidebarWidth: 280,
	chatWidth: 420,
	chatHeight: 320
});

const PANE_CSS_VARIABLES: Record<PaneName, string> = Object.freeze({
	sidebarWidth: "--sidebar-width",
	chatWidth: "--chat-width",
	chatHeight: "--chat-height"
});

const PANE_MIN_SIZES: Record<PaneName, number> = Object.freeze({
	sidebarWidth: SIDEBAR_MIN_WIDTH,
	chatWidth: CHAT_MIN_WIDTH,
	chatHeight: CHAT_MIN_HEIGHT
});

function clamp(value: number, min: number, max: number): number {
	return Math.min(max, Math.max(min, value));
}

function finiteSize(value: unknown): value is number {
	return typeof value === "number" && Number.isFinite(value);
}

const LAYOUT_PREFERENCES: LayoutPreference[] = ["auto", "three", "two", "split"];

function normalizeLayoutPreference(value: unknown): LayoutPreference {
	return LAYOUT_PREFERENCES.includes(value as LayoutPreference) ? value as LayoutPreference : "auto";
}

const FONT_SCALE_MIN = 0.8;
const FONT_SCALE_MAX = 1.4;
const FONT_SCALE_DEFAULT = 1;

const FONT_SCALE_COLUMNS: FontScaleColumn[] = ["sidebar", "details", "chat"];

const FONT_SCALE_CSS_VARIABLES: Record<FontScaleColumn, string> = Object.freeze({
	sidebar: "--sidebar-font-scale",
	details: "--details-font-scale",
	chat: "--chat-font-scale"
});

function normalizeFontScale(value: unknown): number {
	if (!finiteSize(value)) return FONT_SCALE_DEFAULT;
	return Math.round(clamp(value, FONT_SCALE_MIN, FONT_SCALE_MAX) * 100) / 100;
}

export function normalizeFontScales(raw: unknown): FontScales {
	const source = raw && typeof raw === "object" ? raw as Partial<FontScales> : {};
	return {
		sidebar: normalizeFontScale(source.sidebar),
		details: normalizeFontScale(source.details),
		chat: normalizeFontScale(source.chat)
	};
}

export function normalizePaneSizes(raw: unknown, availableWorkspaceWidth = 0): PaneSizes {
	const source = raw && typeof raw === "object" ? raw as Partial<PaneSizes> & { detailsWidth?: unknown } : {};
	const sizes = { ...PANE_DEFAULTS };
	if (finiteSize(source.sidebarWidth)) sizes.sidebarWidth = clamp(source.sidebarWidth, SIDEBAR_MIN_WIDTH, PANE_MAX_SIZE);
	if (finiteSize(source.chatWidth)) sizes.chatWidth = clamp(source.chatWidth, CHAT_MIN_WIDTH, PANE_MAX_SIZE);
	else if (finiteSize(source.detailsWidth) && availableWorkspaceWidth >= 688) {
		const detailsWidth = clamp(source.detailsWidth, DETAILS_MIN_WIDTH, availableWorkspaceWidth - PANE_HANDLE_WIDTH - CHAT_MIN_WIDTH);
		sizes.chatWidth = clamp(availableWorkspaceWidth - PANE_HANDLE_WIDTH - detailsWidth, CHAT_MIN_WIDTH, PANE_MAX_SIZE);
	}
	if (finiteSize(source.chatHeight)) sizes.chatHeight = clamp(source.chatHeight, CHAT_MIN_HEIGHT, PANE_MAX_SIZE);
	return sizes;
}

export function createPaneLayoutController(onChange: () => void, storage: Storage | null = window.localStorage) {
	let paneSizes = { ...PANE_DEFAULTS };
	let mobile: MobilePaneState = { sidebarOpen: false };
	let layoutPreference: LayoutPreference = "auto";
	let fontScales: FontScales = normalizeFontScales(null);
	const mobileQuery = window.matchMedia("(max-width: 980px)");
	const twoColumnQuery = window.matchMedia("(max-width: 1440px)");

	function readStoredPaneSizes(): Record<string, unknown> {
		if (!storage) return {};
		try {
			const saved = JSON.parse(storage.getItem(PANE_SIZE_KEY) || "{}");
			return saved && typeof saved === "object" && !Array.isArray(saved) ? saved : {};
		} catch (_) {
			return {};
		}
	}

	function readStoredFontScales(): Record<string, unknown> {
		if (!storage) return {};
		try {
			const saved = JSON.parse(storage.getItem(FONT_SCALE_KEY) || "{}");
			return saved && typeof saved === "object" && !Array.isArray(saved) ? saved : {};
		} catch (_) {
			return {};
		}
	}

	function applyFontScale(column: FontScaleColumn): void {
		document.documentElement.style.setProperty(FONT_SCALE_CSS_VARIABLES[column], String(fontScales[column]));
	}

	function applyFontScales(): void {
		for (const column of FONT_SCALE_COLUMNS) applyFontScale(column);
	}

	function workspacePanelWidth(): number {
		return document.querySelector(".workspace-panel")?.getBoundingClientRect().width || 0;
	}

	function setCSSPixels(name: string, value: number): void {
		document.documentElement.style.setProperty(name, `${Math.round(value)}px`);
	}

	function setPaneSize(name: string, value: number): void {
		if (!Object.hasOwn(PANE_CSS_VARIABLES, name) || !Number.isFinite(value)) return;
		const paneName = name as PaneName;
		const next = Math.round(clamp(value, PANE_MIN_SIZES[paneName], PANE_MAX_SIZE));
		paneSizes[paneName] = next;
		setCSSPixels(PANE_CSS_VARIABLES[paneName], next);
	}

	function applyPaneSizes(): void {
		for (const name of Object.keys(PANE_CSS_VARIABLES) as PaneName[]) setPaneSize(name, paneSizes[name]);
	}

	function saveAllPaneSizes(): void {
		storage?.setItem(PANE_SIZE_KEY, JSON.stringify(paneSizes));
	}

	function initialize(): void {
		const raw = readStoredPaneSizes();
		paneSizes = normalizePaneSizes(raw, 0);
		applyPaneSizes();
		let migrated = false;
		if (finiteSize(raw.detailsWidth) && !finiteSize(raw.chatWidth) && !mobileQuery.matches) {
			paneSizes = normalizePaneSizes(raw, workspacePanelWidth());
			applyPaneSizes();
			migrated = true;
		}
		if (migrated) saveAllPaneSizes();
		try {
			layoutPreference = normalizeLayoutPreference(storage?.getItem(LAYOUT_PREFERENCE_KEY));
		} catch (_) {
			layoutPreference = "auto";
		}
		applyLayout();
		fontScales = normalizeFontScales(readStoredFontScales());
		applyFontScales();
		const onLayoutMediaChange = () => {
			applyLayout();
			onChange();
		};
		mobileQuery.addEventListener?.("change", onLayoutMediaChange);
		twoColumnQuery.addEventListener?.("change", onLayoutMediaChange);
	}

	function commitPane(name: string): void {
		if (!Object.hasOwn(PANE_CSS_VARIABLES, name) || !storage) return;
		const paneName = name as PaneName;
		const saved = normalizePaneSizes(readStoredPaneSizes());
		saved[paneName] = paneSizes[paneName];
		storage.setItem(PANE_SIZE_KEY, JSON.stringify(saved));
	}

	function syncViewport(): void {
		if (mobileQuery.matches) return;
		const raw = readStoredPaneSizes();
		if (!finiteSize(raw.detailsWidth) || finiteSize(raw.chatWidth)) return;
		paneSizes = normalizePaneSizes(raw, workspacePanelWidth());
		applyPaneSizes();
		saveAllPaneSizes();
	}

	function effectiveLayout(): EffectiveLayout {
		if (mobileQuery.matches) return "single";
		if (layoutPreference !== "auto") return layoutPreference;
		return twoColumnQuery.matches ? "two" : "three";
	}

	function applyLayout(): void {
		document.body.dataset.layout = effectiveLayout();
	}

	function setLayoutPreference(preference: string): void {
		layoutPreference = normalizeLayoutPreference(preference);
		try {
			storage?.setItem(LAYOUT_PREFERENCE_KEY, layoutPreference);
		} catch (_) {}
		applyLayout();
		onChange();
	}

	function setFontScale(column: FontScaleColumn, value: number): void {
		if (!Object.hasOwn(FONT_SCALE_CSS_VARIABLES, column)) return;
		fontScales[column] = normalizeFontScale(value);
		applyFontScale(column);
		try {
			storage?.setItem(FONT_SCALE_KEY, JSON.stringify(fontScales));
		} catch (_) {}
		onChange();
	}

	function resetFontScales(): void {
		fontScales = normalizeFontScales(null);
		applyFontScales();
		try {
			storage?.removeItem(FONT_SCALE_KEY);
		} catch (_) {}
		onChange();
	}

	function setMobileSidebar(open: boolean): void {
		mobile.sidebarOpen = Boolean(open);
		document.body.classList.toggle("mobile-sidebar-open", mobile.sidebarOpen);
		onChange();
	}

	return {
		initialize,
		previewPane: setPaneSize,
		commitPane,
		syncViewport,
		setLayoutPreference,
		setFontScale,
		resetFontScales,
		setMobileSidebar,
		snapshot: () => ({ paneSizes: { ...paneSizes }, mobile: { ...mobile }, layout: { preference: layoutPreference, effective: effectiveLayout() }, fontScales: { ...fontScales } })
	};
}
