import type { ResourceScope } from "../runtime/resource-scope";

const USER_SELECTIONS_KEY = "pua.web.users.v2";
const LEGACY_USER_SETTINGS_KEY = "pua.web.user.v1";
const USER_SELECTIONS_VERSION = 2;
export const USER_NAME_MAX_LENGTH = 80;
const USER_NAME_PATTERN = /^[A-Za-z0-9_-]+$/;

export function sanitizeUserNameInput(value: unknown): string {
	return String(value || "").replace(/[^A-Za-z0-9_-]/g, "").slice(0, USER_NAME_MAX_LENGTH);
}

export function validateUserName(value: unknown): string {
	const name = String(value || "");
	if (!name) throw new Error("User name is required.");
	if (name.length > USER_NAME_MAX_LENGTH) throw new Error(`User name must be at most ${USER_NAME_MAX_LENGTH} characters.`);
	if (!USER_NAME_PATTERN.test(name)) throw new Error("User name may contain only letters, numbers, underscores, and hyphens.");
	return name;
}

interface StoredSelections {
	version: number;
	selections: Record<string, string>;
}

function decodeSelections(raw: string | null): Record<string, string> {
	if (!raw) return {};
	try {
		const stored = JSON.parse(raw) as Partial<StoredSelections> | null;
		if (!stored || stored.version !== USER_SELECTIONS_VERSION || !stored.selections || typeof stored.selections !== "object") return {};
		return Object.fromEntries(Object.entries(stored.selections).filter(([instanceId, name]) => {
			if (!instanceId) return false;
			try { validateUserName(name); return true; } catch { return false; }
		}));
	} catch {
		return {};
	}
}

export function decodeLegacyUserName(raw: string | null): string {
	if (!raw) return "";
	try {
		const stored = JSON.parse(raw) as { version?: unknown; name?: unknown } | null;
		if (!stored || stored.version !== 1) return "";
		return validateUserName(stored.name);
	} catch {
		return "";
	}
}

export function createUserSettingsController(scope: ResourceScope, onChange: () => void) {
	let selections = read();

	function read(): Record<string, string> {
		try { return decodeSelections(window.localStorage.getItem(USER_SELECTIONS_KEY)); }
		catch { return {}; }
	}

	function persist(next: Record<string, string>): void {
		try {
			window.localStorage.setItem(USER_SELECTIONS_KEY, JSON.stringify({ version: USER_SELECTIONS_VERSION, selections: next }));
		} catch { /* Keep the selection for this page when browser storage is unavailable. */ }
		selections = next;
	}

	scope.listen(window, "storage", (event) => {
		if (event.key !== USER_SELECTIONS_KEY) return;
		selections = decodeSelections(event.newValue);
		onChange();
	});

	return {
		selected: (instanceId: string) => selections[instanceId] || "",
		legacyCandidate: () => {
			try { return decodeLegacyUserName(window.localStorage.getItem(LEGACY_USER_SETTINGS_KEY)); }
			catch { return ""; }
		},
		save: (instanceId: string, value: unknown) => {
			if (!instanceId) throw new Error("Workspace identity is unavailable.");
			const name = validateUserName(value);
			persist({ ...selections, [instanceId]: name });
			return name;
		},
		clear: (instanceId: string) => {
			if (!instanceId || !selections[instanceId]) return;
			const next = { ...selections };
			delete next[instanceId];
			persist(next);
		},
		validate: validateUserName,
	};
}
