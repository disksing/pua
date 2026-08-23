import { describe, expect, it } from "vitest";

import { sessionsQuery } from "../../src/agenthub/core/archive";
import { buildCreatePayload } from "../../src/agenthub/core/new-session";
import { activitySessions, SessionToneAllocator, TERMINAL_TONE_HOLD_MS } from "../../src/agenthub/companion/model";
import { activityPlaybackPlan } from "../../src/agenthub/companion/schedule";
import { buildProviderSwitches } from "../../src/agenthub/settings/provider-switches";

describe("AgentHub audit application", () => {
  it("builds an explicit paginated current/archived inventory query", () => {
    expect(sessionsQuery({ limit: 25, state: ["running", "ready"], sourceApp: "pua", cursor: "next page" }))
      .toBe("/v1/sessions?state=running%2Cready&sourceApp=pua&limit=25&cursor=next+page");
    expect(sessionsQuery({ archivedOnly: true, limit: 50 }))
      .toBe("/v1/sessions?archived=true&limit=50");
  });

  it("validates new Sessions against the existing agent catalog", () => {
    const agents = [{ name: "Codex", providerId: "codex", available: true }];
    expect(buildCreatePayload({ cwd: "relative", agentName: "Missing", title: "", agents }).errors).toMatchObject({ cwd: expect.any(String), agent: expect.any(String) });
    expect(buildCreatePayload({ cwd: "/tmp/project", agentName: "Codex", title: "Audit", agents }).payload)
      .toEqual({ cwd: "/tmp/project", agentName: "Codex", title: "Audit" });
  });

  it("keeps provider switches fixed to the four daemon integrations", () => {
    const switches = (buildProviderSwitches as any)({ agentProviders: [{ id: "codex", name: "Codex", type: "codex", enabled: true }] }, [{ providerId: "codex", available: true, command: "/bin/codex" }]);
    expect(switches.map((item: any) => item.id)).toEqual(["codex", "kimi", "pi", "opencode"]);
    expect(switches[0]).toMatchObject({ enabled: true, availability: "CLI available" });
  });

  it("retains a terminal activity row for five minutes but its tone for ten seconds", () => {
    const now = 100_000;
    const sessions = (activitySessions as any)(new Map(), { sessions: [{ sessionId: "s1", turnId: "t1", completed: true, lastEventAt: new Date(now).toISOString() }] }, now);
    const session = sessions.get("s1") as any;
    expect(session.toneReleaseAt).toBe(now + TERMINAL_TONE_HOLD_MS);
    expect(session.expiresAt).toBe(now + 5 * 60 * 1000);
  });

  it("uses the approved four-subdivision activity patterns and stable tone slots", () => {
    expect((activityPlaybackPlan as any)(["a", "b", "c"], 1).map((entry: any) => entry.slot)).toEqual([0, 1, 2]);
    expect((activityPlaybackPlan as any)(["a", "b", "c"], 2).map((entry: any) => entry.slot)).toEqual([0, 1, 3]);
    const allocator = new SessionToneAllocator(3);
    expect([allocator.assign("a"), allocator.assign("b"), allocator.assign("a")]).toEqual([0, 1, 0]);
  });
});
