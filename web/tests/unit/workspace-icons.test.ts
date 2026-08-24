import { describe, expect, it } from "vitest";

import { DEFAULT_WORKSPACE_ICON, WORKSPACE_ICONS, workspaceIconOption } from "../../src/models/workspace-icons";

describe("workspace icons", () => {
  it("puts the PUA set first and uses the yellow umbrella as the default", () => {
    expect(WORKSPACE_ICONS.slice(0, 5).map((icon) => icon.id)).toEqual([
      "", "pua-red", "pua-green", "pua-blue", "pua-purple",
    ]);
    expect(DEFAULT_WORKSPACE_ICON).toMatchObject({
      id: "",
      label: "PUA default",
      src: "/workspace-icons/pua-yellow.png",
      type: "image/png",
    });
    expect(WORKSPACE_ICONS[5].id).toBe("home-base");
  });

  it("falls back to yellow for legacy empty and unknown workspace icons", () => {
    expect(workspaceIconOption({ icon: "" })).toBe(DEFAULT_WORKSPACE_ICON);
    expect(workspaceIconOption({})).toBe(DEFAULT_WORKSPACE_ICON);
    expect(workspaceIconOption({ icon: "missing" })).toBe(DEFAULT_WORKSPACE_ICON);
    expect(workspaceIconOption({ icon: "pua-purple" }).src).toBe("/workspace-icons/pua-purple.png");
  });
});
