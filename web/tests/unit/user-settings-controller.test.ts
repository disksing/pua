import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { createUserSettingsController, decodeLegacyUserName, sanitizeUserNameInput, validateUserName } from "../../src/controllers/user-settings-controller";
import { ResourceScope } from "../../src/runtime/resource-scope";

const storedValues = new Map<string, string>();
const localStorageMock = {
  getItem: (key: string) => storedValues.get(key) ?? null,
  setItem: (key: string, value: string) => { storedValues.set(key, value); },
  removeItem: (key: string) => { storedValues.delete(key); },
  clear: () => storedValues.clear(),
  key: (index: number) => [...storedValues.keys()][index] ?? null,
  get length() { return storedValues.size; },
} satisfies Storage;

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { configurable: true, value: localStorageMock });
});

afterEach(() => storedValues.clear());

describe("user settings validation", () => {
  it("keeps only filesystem-safe identifier characters in the input", () => {
    expect(sanitizeUserNameInput("Alice 张/.._2-test")).toBe("Alice_2-test");
  });

  it("keeps validation strict without inventing a default user", () => {
    expect(() => validateUserName("")).toThrow("required");
    expect(() => validateUserName("two words")).toThrow("only letters");
    expect(() => validateUserName("name.dot")).toThrow("only letters");
    expect(validateUserName("User_2-test")).toBe("User_2-test");
  });

  it("persists independent selections by Workspace instance", () => {
    const scope = new ResourceScope();
    const controller = createUserSettingsController(scope, () => undefined);

    expect(controller.save("instance-a", "Alice")).toBe("Alice");
    expect(controller.save("instance-b", "Bob")).toBe("Bob");
    expect(controller.selected("instance-a")).toBe("Alice");
    expect(controller.selected("instance-b")).toBe("Bob");
    expect(JSON.parse(window.localStorage.getItem("pua.web.users.v2")!)).toMatchObject({ version: 2, selections: { "instance-a": "Alice", "instance-b": "Bob" } });

    scope.dispose();
  });

  it("only offers a valid legacy selection as a migration candidate", () => {
    expect(decodeLegacyUserName(JSON.stringify({ version: 1, name: "Old User" }))).toBe("");
    expect(decodeLegacyUserName(JSON.stringify({ version: 1, name: "Alice" }))).toBe("Alice");
  });
});
