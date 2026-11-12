import { describe, expect, it } from "vitest";

import { normalizeAgentOptions, uniqueAgentName, validateAgentHubAgents } from "../../src/components/agenthub-config";

describe("AgentHub config editing", () => {
  const providers = [{ id: "codex", name: "Codex", type: "codex" }];

  it("keeps only options supported by the selected provider", () => {
    expect(normalizeAgentOptions("codex", { model: " gpt-test ", sandbox: "danger-full-access", mode: "plan" })).toEqual({
      model: "gpt-test",
      sandbox: "danger-full-access",
    });
    expect(normalizeAgentOptions("pi", { model: "pi-model", approval: "never" })).toEqual({ model: "pi-model" });
  });

  it("generates a case-insensitive unique agent name", () => {
    expect(uniqueAgentName("Agent", ["agent", "Agent 2"])).toBe("Agent 3");
  });

  it("mirrors AgentHub validation for names, providers, and environment variables", () => {
    const errors = validateAgentHubAgents([
      { name: " Worker ", providerId: "codex", environment: { "": "bad", "A=B": "bad", NUL: "\0" } },
      { name: "worker", providerId: "missing" },
    ], providers);
    expect(errors.map((error) => `${error.index}:${error.field}`)).toEqual([
      "0:environment",
      "0:environment",
      "0:environment",
      "1:name",
      "1:providerId",
    ]);
  });
});
