// @ts-nocheck -- pure config model migrated unchanged and covered by tests.
// Pure data helpers with no framework dependency, shared by the settings UI and tests.
// The config model mirrors internal/config/config.go.

export const PROVIDER_TYPES = [
  { value: "codex", label: "Codex" },
  { value: "opencode", label: "OpenCode" },
  { value: "kimi", label: "Kimi Code" },
  { value: "pi", label: "Pi Coding Agent" },
];

export const SANDBOX_OPTIONS = [
  { value: "read-only", label: "Read only" },
  { value: "workspace-write", label: "Workspace write" },
  { value: "danger-full-access", label: "Danger full access" },
];

export const APPROVAL_OPTIONS = [
  { value: "untrusted", label: "Ask when untrusted" },
  { value: "on-failure", label: "Ask on failure" },
  { value: "on-request", label: "Ask on request" },
  { value: "never", label: "Never ask" },
];

export const MODE_OPTIONS = [
  { value: "", label: "Default" },
  { value: "build", label: "Build" },
  { value: "plan", label: "Plan" },
];

// Reasoning effort values are advertised per model by the Codex app-server
// (model/list); this list covers the values current models advertise. The
// daemon re-validates against the selected model when a session starts.
export const REASONING_EFFORT_OPTIONS = [
  { value: "", label: "Provider default" },
  { value: "low", label: "Low" },
  { value: "medium", label: "Medium" },
  { value: "high", label: "High" },
  { value: "xhigh", label: "Extra high" },
  { value: "max", label: "Max" },
  { value: "ultra", label: "Ultra" },
];

// normalizeAgentName mirrors config.NormalizeAgentName on the daemon: trim
// and lower-case. Names are unique under this normalization.
export function normalizeAgentName(name) {
  return String(name ?? "").trim().toLowerCase();
}

export const AGENT_NAME_MAX_LENGTH = 80;

export const DEFAULT_ONWATCH = {
  enabled: false,
  serverUrl: "http://127.0.0.1:9211",
  authMode: "trusted_proxy",
  username: "admin",
  password: "",
  refreshIntervalSeconds: 60,
};

// The model option is not a free-text field: the settings UI loads the
// provider's live model list and renders a dropdown (see ModelSelect).
const MODEL_FIELD = { key: "model", kind: "model", label: "Model" };

// providerOptionSchema returns the form description of the Agent options a
// provider type supports.
export function providerOptionSchema(type) {
  switch (type) {
    case "codex":
      return [
        MODEL_FIELD,
        { key: "sandbox", kind: "enum", label: "Sandbox", options: SANDBOX_OPTIONS, fallback: "workspace-write" },
        { key: "approval", kind: "enum", label: "Approval policy", options: APPROVAL_OPTIONS, fallback: "on-request" },
        { key: "reasoning_effort", kind: "enum", label: "Reasoning effort", options: REASONING_EFFORT_OPTIONS, fallback: "" },
      ];
    case "kimi":
    case "opencode":
      return [
        MODEL_FIELD,
        { key: "mode", kind: "enum", label: "Mode", options: MODE_OPTIONS, fallback: "" },
      ];
    case "pi":
      return [MODEL_FIELD];
    default:
      return [MODEL_FIELD];
  }
}

function cleanOptions(options) {
  const result = {};
  for (const [key, value] of Object.entries(options || {})) {
    const trimmed = String(value ?? "").trim();
    if (trimmed) result[key] = trimmed;
  }
  return result;
}

// cleanEnvironment normalizes an agent environment map: keys are trimmed and
// empty keys are dropped, while values are kept verbatim so an explicitly
// empty value ("FOO=\"\"") survives a settings round-trip.
function cleanEnvironment(environment) {
  const result = {};
  for (const [rawKey, rawValue] of Object.entries(environment || {})) {
    const key = String(rawKey ?? "").trim();
    if (!key) continue;
    result[key] = String(rawValue ?? "");
  }
  return result;
}

// normalizeConfig deep-copies config from any source and normalizes it into a
// fixed shape: missing arrays become empty, values become strings, blank
// option keys and empty optional fields are dropped.
export function normalizeConfig(config = {}) {
  const providers = (Array.isArray(config.agentProviders) ? config.agentProviders : []).map((provider) => {
    const result = {
      id: String(provider?.id ?? ""),
      name: String(provider?.name ?? ""),
      type: String(provider?.type ?? ""),
      enabled: Boolean(provider?.enabled),
    };
    const command = String(provider?.command ?? "").trim();
    if (command) result.command = command;
    return result;
  });
  const agents = (Array.isArray(config.agents) ? config.agents : []).map((agent) => {
    const result = {
      name: String(agent?.name ?? ""),
      providerId: String(agent?.providerId ?? ""),
    };
    const options = cleanOptions(agent?.options);
    if (Object.keys(options).length) result.options = options;
    const environment = cleanEnvironment(agent?.environment);
    if (Object.keys(environment).length) result.environment = environment;
    return result;
  });

  const rawOnWatch = config.onWatch || {};
  const onWatch = {
    enabled: Boolean(rawOnWatch.enabled),
    serverUrl: String(rawOnWatch.serverUrl ?? DEFAULT_ONWATCH.serverUrl).trim(),
    authMode: String(rawOnWatch.authMode ?? DEFAULT_ONWATCH.authMode),
    username: String(rawOnWatch.username ?? DEFAULT_ONWATCH.username).trim(),
    password: String(rawOnWatch.password ?? ""),
    refreshIntervalSeconds: Number(rawOnWatch.refreshIntervalSeconds) || DEFAULT_ONWATCH.refreshIntervalSeconds,
  };
  return {
    version: Number(config.version) || 1,
    agentProviders: providers,
    agents,
    onWatch,
  };
}

// createDraft builds an editing draft (deep copy + normalization) from the
// server-side config.
export function createDraft(config) {
  return normalizeConfig(config);
}

// isDirty compares a draft against the snapshot taken when it was opened. Both
// sides are normalized before the deep comparison so equivalent differences do
// not report false positives.
export function isDirty(draft, snapshot) {
  return JSON.stringify(normalizeConfig(draft)) !== JSON.stringify(normalizeConfig(snapshot));
}

// canSaveDraft is the settings save-bar gate: a dirty draft is only
// submittable when validation has no blocking errors and a save is not already
// in flight.
export function canSaveDraft(dirty, errors = [], saving = false) {
  return Boolean(dirty && !saving && errors.length === 0);
}

// buildPayload produces the config object for PUT /v1/config from a draft,
// keeping the version and all supported fields and dropping empty option keys.
export function buildPayload(draft) {
  return normalizeConfig(draft);
}

// normalizeAgentOptions drops fields that do not apply after a provider
// switch: only keys from the new schema are kept, invalid enum values fall
// back to the default, and a non-empty model is preserved.
export function normalizeAgentOptions(providerType, oldOptions = {}) {
  const result = {};
  for (const field of providerOptionSchema(providerType)) {
    const raw = String(oldOptions?.[field.key] ?? "").trim();
    if (field.kind === "text" || field.kind === "model") {
      if (raw) result[field.key] = raw;
      continue;
    }
    const allowed = field.options.map((option) => option.value);
    const value = allowed.includes(raw) ? raw : field.fallback;
    if (value) result[field.key] = value;
  }
  return result;
}

function providerMap(draft) {
  return new Map((draft.agentProviders || []).map((provider) => [provider.id, provider]));
}

// validateDraft performs full client-side validation and returns structured
// errors: { section, index, field, message },
// section ∈ providers/agents.
export function validateDraft(draft) {
  const errors = [];
  const push = (section, index, field, message) => errors.push({ section, index, field, message });

  try {
    const parsed = new URL(draft.onWatch.serverUrl);
    if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password) throw new Error("invalid URL");
  } catch {
    push("general", 0, "serverUrl", "Enter an absolute HTTP or HTTPS URL without credentials");
  }
  if (!["trusted_proxy", "basic", "none"].includes(draft.onWatch.authMode)) {
    push("general", 0, "authMode", "Select a supported authentication mode");
  }
  if (draft.onWatch.authMode === "basic" && !draft.onWatch.username.trim()) {
    push("general", 0, "username", "Username is required for Basic Auth");
  }
  if (![30, 60, 300].includes(draft.onWatch.refreshIntervalSeconds)) {
    push("general", 0, "refreshIntervalSeconds", "Select a supported refresh interval");
  }
  const providers = draft.agentProviders || [];
  const providerIds = new Set();
  providers.forEach((provider, index) => {
    if (!provider.id.trim()) push("providers", index, "id", "Provider ID is required");
    else if (providerIds.has(provider.id)) push("providers", index, "id", `Provider ID "${provider.id}" is already used`);
    providerIds.add(provider.id);
    if (!provider.type.trim()) push("providers", index, "type", "Provider type is required");
    else if (!PROVIDER_TYPES.some((type) => type.value === provider.type)) {
      push("providers", index, "type", `Unsupported provider type "${provider.type}"`);
    }
  });

  const providerById = providerMap({ agentProviders: providers });
  const agents = draft.agents || [];
  const agentNames = new Map();
  agents.forEach((agent, index) => {
    const trimmed = String(agent.name ?? "").trim();
    if (!trimmed) push("agents", index, "name", "Agent name is required");
    else if (Array.from(trimmed).length > AGENT_NAME_MAX_LENGTH) {
      push("agents", index, "name", `Agent name must be ${AGENT_NAME_MAX_LENGTH} characters or fewer`);
    } else {
      const key = normalizeAgentName(trimmed);
      if (agentNames.has(key)) {
        push("agents", index, "name", `Agent name "${trimmed}" is already used by agent ${agentNames.get(key) + 1}`);
      } else {
        agentNames.set(key, index);
      }
    }
    if (!agent.providerId.trim()) push("agents", index, "providerId", "Select a provider");
    else if (!providerById.has(agent.providerId)) {
      push("agents", index, "providerId", `Referenced provider "${agent.providerId}" does not exist`);
    }
    for (const [rawKey, rawValue] of Object.entries(agent.environment || {})) {
      const key = String(rawKey ?? "").trim();
      if (!key) push("agents", index, "environment", "Environment variable name cannot be empty");
      else if (/[=\0]/.test(key)) push("agents", index, "environment", `Environment variable name "${key}" contains invalid characters`);
      if (/\0/.test(String(rawValue ?? ""))) push("agents", index, "environment", `Environment variable "${key || rawKey}" contains NUL`);
    }
  });

  return errors;
}

// uniqueAgentName appends " 2" / " 3" … until the name is free under the
// normalized (case-insensitive, trimmed) uniqueness rule.
export function uniqueAgentName(base, existing) {
  const taken = new Set((existing || []).map((name) => normalizeAgentName(name)));
  const cleanBase = String(base ?? "").trim() || "Agent";
  if (!taken.has(normalizeAgentName(cleanBase))) return cleanBase;
  for (let index = 2; ; index += 1) {
    const candidate = `${cleanBase} ${index}`;
    if (!taken.has(normalizeAgentName(candidate))) return candidate;
  }
}

// summarizeOptions builds the key=value pairs shown in an agent summary pill.
export function summarizeOptions(options = {}) {
  return Object.entries(options)
    .filter(([, value]) => String(value ?? "").trim())
    .map(([key, value]) => `${key}=${value}`);
}

// reorderAgents returns a new agents array with the element at fromIndex moved
// to toIndex. It is the pure counterpart of the drag-and-drop (and keyboard)
// reordering in the Agents settings panel; the source array is never mutated.
export function reorderAgents(agents, fromIndex, toIndex) {
  const next = agents.slice();
  const [moved] = next.splice(fromIndex, 1);
  next.splice(toIndex, 0, moved);
  return next;
}
