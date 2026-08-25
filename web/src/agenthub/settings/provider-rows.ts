// @ts-nocheck -- pure provider model migrated unchanged and covered by tests.
// Pure model for the simplified provider settings: exactly four built-in
// provider rows whose availability is detected from the executable, not from
// a user-controlled switch. Provider add/delete intentionally does not exist
// in this layer; the executable command can be overridden for installations
// launched outside a shell environment.
// Unit tested with node --test.

// BUILTIN_PROVIDERS lists the four user-recognizable built-in integrations in
// display order. The ids match the canonical provider ids of the daemon
// config; Pi can connect to multiple model providers through its runtime.
export const BUILTIN_PROVIDERS = [
  { id: "codex", name: "Codex", description: "OpenAI Codex app-server integration." },
  { id: "kimi", name: "Kimi", description: "Kimi Code CLI over the ACP protocol." },
  { id: "pi", name: "Pi", description: "Pi Coding Agent runtime supporting multiple providers over JSON-RPC." },
  { id: "opencode", name: "OpenCode", description: "OpenCode CLI over the ACP protocol." },
];

export function isBuiltinProvider(id) {
  return BUILTIN_PROVIDERS.some((provider) => provider.id === id);
}

// buildProviderRows derives the four provider row descriptors from the daemon
// config and the availability probes. A provider is available whenever its
// executable resolves: the daemon probes the configured command or falls back
// to PATH and common install directories. `command` is the user-configured
// override (empty when the provider relies on auto-detection), `path` the
// resolved executable reported by the probe, and `error` the probe's
// resolution failure. A built-in provider missing from the config (only
// possible through a hand-edited config file) renders as unavailable with an
// empty command so the row offers the path editor directly.
export function buildProviderRows(config, probes = []) {
  const configured = new Map((config?.agentProviders || []).map((provider) => [provider.id, provider]));
  const probeById = new Map((probes || []).map((probe) => [probe.providerId, probe]));
  return BUILTIN_PROVIDERS.map((builtin) => {
    const provider = configured.get(builtin.id);
    const probe = probeById.get(builtin.id);
    const available = probe ? probe.available !== false : Boolean(provider);
    return {
      id: builtin.id,
      name: builtin.name,
      description: builtin.description,
      present: Boolean(provider),
      command: provider?.command || "",
      available,
      path: probe?.command || "",
      error: probe && !available ? probe.error || "Command not found" : "",
      status: available ? "Enabled" : "Unavailable",
      tone: available ? "ok" : "danger",
    };
  });
}

// applyProviderUpdate returns a new config object with the provider returned
// by the daemon merged in: replaced by id when present (preserving every
// field the daemon kept) or appended when the daemon just created it. Agents
// and all other providers are carried over untouched.
export function applyProviderUpdate(config, provider) {
  const providers = [...(config?.agentProviders || [])];
  const index = providers.findIndex((item) => item.id === provider.id);
  if (index >= 0) providers[index] = { ...providers[index], ...provider };
  else providers.push({ ...provider });
  return { ...(config || {}), agentProviders: providers };
}

// requestProviderCommand asks the daemon to replace one built-in provider's
// executable path through the minimal command endpoint. The daemon validates
// the path and rejects it when it does not resolve to an executable; an empty
// command clears the override and restores automatic detection. On success
// the promise resolves with the persisted provider; on failure it rejects
// with the daemon's error and no local state has changed, so the caller keeps
// showing the true persisted state.
export async function requestProviderCommand(apiFn, id, command) {
  if (!isBuiltinProvider(id)) {
    throw new Error(`Unknown built-in provider "${id}"`);
  }
  const body = await apiFn(`/v1/config/providers/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: JSON.stringify({ command: String(command ?? "") }),
  });
  if (!body?.provider || body.provider.id !== id) {
    throw new Error("The daemon did not return the updated provider");
  }
  return body.provider;
}
