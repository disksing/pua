// @ts-nocheck -- pure provider model migrated unchanged and covered by tests.
// Pure model for the simplified provider settings: exactly four built-in
// provider enable/disable switches. Provider add/delete and transport-level
// editing intentionally do not exist in this layer; the executable command
// can be overridden for installations launched outside a shell environment.
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

// buildProviderSwitches derives the four switch descriptors from the daemon
// config and the availability probes. A built-in provider missing from the
// config is shown as disabled and is created with canonical defaults by the
// daemon when enabled. Availability is only reported for enabled providers
// (the daemon does not probe disabled ones) and never hides the Enabled
// state: a provider can be enabled while its CLI is unavailable.
export function buildProviderSwitches(config, probes = []) {
  const configured = new Map((config?.agentProviders || []).map((provider) => [provider.id, provider]));
  const probeById = new Map((probes || []).map((probe) => [probe.providerId, probe]));
  return BUILTIN_PROVIDERS.map((builtin) => {
    const provider = configured.get(builtin.id);
    const enabled = Boolean(provider?.enabled);
    const probe = probeById.get(builtin.id);
    let availability = "";
    let availabilityTone = "";
    let availabilityDetail = "";
    if (enabled) {
      if (probe?.available) {
        availability = "CLI available";
        availabilityTone = "ok";
        availabilityDetail = probe.command || "";
      } else if (probe) {
        availability = "CLI unavailable";
        availabilityTone = "warn";
        availabilityDetail = probe.error || "Command not found";
      } else {
        availability = "CLI not probed";
        availabilityTone = "muted";
      }
    }
    return {
      id: builtin.id,
      name: builtin.name,
      description: builtin.description,
      present: Boolean(provider),
      enabled,
      command: provider?.command || "",
      status: enabled ? "Enabled" : "Disabled",
      availability,
      availabilityTone,
      availabilityDetail,
      ariaLabel: `${enabled ? "Disable" : "Enable"} ${builtin.name}`,
    };
  });
}

// applyProviderToggle returns a new config object with the provider returned
// by the daemon merged in: replaced by id when present (preserving every
// field the daemon kept, including command) or appended when the daemon just
// created it. Agents and all other providers are carried over untouched.
export function applyProviderToggle(config, provider) {
  const providers = [...(config?.agentProviders || [])];
  const index = providers.findIndex((item) => item.id === provider.id);
  if (index >= 0) providers[index] = { ...providers[index], ...provider };
  else providers.push({ ...provider });
  return { ...(config || {}), agentProviders: providers };
}

// requestProviderToggle asks the daemon to flip one built-in provider through
// the minimal toggle endpoint. On success it resolves with the persisted
// provider; on failure it rejects with the daemon's error and no local state
// has changed, so the caller keeps showing the true persisted state.
export async function requestProviderToggle(apiFn, id, enabled) {
  if (!isBuiltinProvider(id)) {
    throw new Error(`Unknown built-in provider "${id}"`);
  }
  const body = await apiFn(`/v1/config/providers/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: JSON.stringify({ enabled: Boolean(enabled) }),
  });
  if (!body?.provider || body.provider.id !== id) {
    throw new Error("The daemon did not return the updated provider");
  }
  return body.provider;
}
