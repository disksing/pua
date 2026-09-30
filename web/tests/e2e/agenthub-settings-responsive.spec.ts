import { expect, test } from "@playwright/test";

// The isolated E2E server hosts AgentHub's static UI. All AgentHub API calls
// are intercepted so this layout test never reads or writes server settings.
test("AgentHub settings actions remain reachable on narrow screens", async ({ page }) => {
  await page.route("**/agenthub/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const data = path.endsWith("/config") ? {
      config: {
        version: 1,
        agentProviders: [{ id: "codex", name: "Codex", type: "codex" }],
        agents: [{ name: "Codex", providerId: "codex" }],
      },
    } : path.endsWith("/agents") ? {
      agents: [{ name: "Codex", providerId: "codex" }],
      probes: [{ providerId: "codex", available: true, command: "/bin/codex" }],
    } : path.endsWith("/sessions") ? {
      sessions: [], page: { limit: 50, hasMore: false },
    } : path.endsWith("/quota") ? { quota: { providers: [] } } : {};
    await route.fulfill({ json: data });
  });

  await page.goto("/agenthub/");
  await page.locator("button.topbar-link").click();
  await page.locator(".settings-nav button").filter({ hasText: "Agents" }).click();
  await page.getByRole("button", { name: "Add agent" }).click();

  for (const width of [320, 375, 440, 560, 980]) {
    await page.setViewportSize({ width, height: 800 });
    for (const section of ["Agents", "General", "Activity"]) {
      await page.locator(".settings-nav button").filter({ hasText: section }).click();
      const rects = await page.locator(".settings-savebar").evaluate((footer) => {
        const cancel = footer.querySelector(".secondary-button")!;
        const save = footer.querySelector(".primary-button")!;
        return {
          viewport: window.innerWidth,
          footerRight: footer.getBoundingClientRect().right,
          cancelRight: cancel.getBoundingClientRect().right,
          saveRight: save.getBoundingClientRect().right,
        };
      });
      expect(rects.footerRight).toBeLessThanOrEqual(rects.viewport);
      expect(rects.cancelRight).toBeLessThanOrEqual(rects.footerRight);
      expect(rects.saveRight).toBeLessThanOrEqual(rects.footerRight);
      await expect(page.getByRole("button", { name: "Save all" })).toBeVisible();
    }
  }
});
