import assert from "node:assert/strict";
import test from "node:test";
import {
  BUILTIN_PROVIDERS,
  applyProviderToggle,
  buildProviderSwitches,
  isBuiltinProvider,
  requestProviderToggle,
} from "../src/settings/providerSwitches.js";

function sampleConfig() {
  return {
    version: 1,
    agentProviders: [
      { id: "codex", name: "Codex app-server", type: "codex", enabled: true },
      { id: "kimi", name: "Kimi Code", type: "kimi", enabled: false, command: "/opt/kimi/bin/kimi" },
      { id: "pi", name: "Pi Coding Agent", type: "pi", enabled: true },
    ],
    agents: [
      { id: "main", name: "Main", providerId: "codex" },
      { id: "backup", name: "Backup", providerId: "kimi" },
    ],
  };
}

test("exactly four built-in providers in product order", () => {
  assert.deepEqual(
    BUILTIN_PROVIDERS.map((provider) => provider.id),
    ["codex", "kimi", "pi", "opencode"],
  );
  assert.deepEqual(
    BUILTIN_PROVIDERS.map((provider) => provider.name),
    ["Codex", "Kimi", "Pi", "OpenCode"],
  );
  const pi = BUILTIN_PROVIDERS.find((provider) => provider.id === "pi");
  assert.equal(pi.description, "Pi Coding Agent runtime supporting multiple providers over JSON-RPC.");
  assert.doesNotMatch(pi.description, /grok/i);
  for (const provider of BUILTIN_PROVIDERS) {
    assert.ok(provider.description.length > 0, `${provider.id} needs a one-line description`);
  }
  assert.ok(isBuiltinProvider("pi"));
  assert.ok(!isBuiltinProvider("ghost"));
});

test("buildProviderSwitches always returns the four switches with states", () => {
  const switches = buildProviderSwitches(sampleConfig());
  assert.equal(switches.length, 4);
  const byId = new Map(switches.map((item) => [item.id, item]));

  const codex = byId.get("codex");
  assert.equal(codex.enabled, true);
  assert.equal(codex.status, "Enabled");
  assert.equal(codex.ariaLabel, "Disable Codex");
  assert.equal(codex.present, true);
  assert.equal(codex.command, "");

  const kimi = byId.get("kimi");
  assert.equal(kimi.enabled, false);
  assert.equal(kimi.status, "Disabled");
  assert.equal(kimi.ariaLabel, "Enable Kimi");
  // Disabled providers are not probed and show no availability claim.
  assert.equal(kimi.availability, "");

  // A built-in provider missing from the config shows as disabled and is
  // marked as not yet configured; the daemon creates defaults on enable.
  const opencode = byId.get("opencode");
  assert.equal(opencode.enabled, false);
  assert.equal(opencode.present, false);
  assert.equal(opencode.command, "");

  // Tolerates an empty config entirely.
  const empty = buildProviderSwitches(undefined);
  assert.equal(empty.length, 4);
  assert.ok(empty.every((item) => !item.enabled && !item.present));
});

test("buildProviderSwitches distinguishes enabled from CLI availability", () => {
  const probes = [
    { providerId: "codex", available: true, command: "/usr/local/bin/codex" },
    { providerId: "pi", available: false, error: "pi executable not found" },
  ];
  const byId = new Map(buildProviderSwitches(sampleConfig(), probes).map((item) => [item.id, item]));
  assert.equal(byId.get("codex").availability, "CLI available");
  assert.equal(byId.get("codex").availabilityTone, "ok");
  assert.equal(byId.get("codex").availabilityDetail, "/usr/local/bin/codex");
  // Enabled but the CLI probe failed: still enabled, clearly unavailable.
  assert.equal(byId.get("pi").enabled, true);
  assert.equal(byId.get("pi").availability, "CLI unavailable");
  assert.equal(byId.get("pi").availabilityTone, "warn");
  assert.equal(byId.get("pi").availabilityDetail, "pi executable not found");
});

test("switch descriptors expose the configured executable path", () => {
  const allowed = new Set([
    "id", "name", "description", "present", "enabled", "status",
    "command", "availability", "availabilityTone", "availabilityDetail", "ariaLabel",
  ]);
  for (const item of buildProviderSwitches(sampleConfig())) {
    for (const key of Object.keys(item)) {
      assert.ok(allowed.has(key), `unexpected advanced field "${key}" in switch descriptor`);
    }
  }
  const kimi = buildProviderSwitches(sampleConfig()).find((item) => item.id === "kimi");
  assert.equal(kimi.command, "/opt/kimi/bin/kimi");
});

test("applyProviderToggle preserves the underlying configuration", () => {
  const config = sampleConfig();
  const next = applyProviderToggle(config, { id: "kimi", enabled: true });
  const kimi = next.agentProviders.find((provider) => provider.id === "kimi");
  assert.equal(kimi.enabled, true);
  assert.equal(kimi.command, "/opt/kimi/bin/kimi");
  assert.equal(kimi.name, "Kimi Code");
  // Other providers and agents are carried over untouched.
  assert.equal(next.agentProviders.length, 3);
  assert.deepEqual(next.agents, config.agents);
  // The input is not mutated.
  assert.equal(config.agentProviders[1].enabled, false);
});

test("applyProviderToggle appends a provider created by the daemon", () => {
  const next = applyProviderToggle(sampleConfig(), { id: "opencode", name: "OpenCode", type: "opencode", enabled: true });
  assert.equal(next.agentProviders.length, 4);
  assert.equal(next.agentProviders[3].id, "opencode");
  assert.equal(next.agentProviders[3].enabled, true);
});

test("requestProviderToggle calls the minimal endpoint and returns the provider", async () => {
  const calls = [];
  const apiFn = async (path, options) => {
    calls.push({ path, options });
    return { provider: { id: "kimi", enabled: true, command: "/opt/kimi/bin/kimi" } };
  };
  const provider = await requestProviderToggle(apiFn, "kimi", true);
  assert.equal(provider.id, "kimi");
  assert.equal(provider.command, "/opt/kimi/bin/kimi");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/v1/config/providers/kimi");
  assert.equal(calls[0].options.method, "PUT");
  assert.deepEqual(JSON.parse(calls[0].options.body), { enabled: true });
});

test("requestProviderToggle surfaces failures without changing state", async () => {
  const apiFn = async () => {
    throw new Error("daemon unavailable");
  };
  await assert.rejects(() => requestProviderToggle(apiFn, "kimi", true), /daemon unavailable/);
  // Unknown providers never reach the daemon.
  let called = false;
  await assert.rejects(
    () => requestProviderToggle(async () => { called = true; }, "ghost", true),
    /Unknown built-in provider/,
  );
  assert.equal(called, false);
  // A malformed daemon response is an error, not a silent success.
  await assert.rejects(
    () => requestProviderToggle(async () => ({}), "kimi", true),
    /did not return the updated provider/,
  );
});
