import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  effectiveSelection,
  filterByNamespace,
  namespaceOptions,
  parseNamespace,
  resetNamespaceViewForTests,
  scopedKey,
  setStoredNamespace,
} from "../namespaceView";

const confined = (namespaces: string[]) => ({ namespaces, kind: "api_key", subject: "k", role: null });
const clusterWide = { namespaces: ["*"], kind: "api_key", subject: "k", role: "admin" as const };
const anonymous = { namespaces: [] as string[], kind: null, subject: null, role: null };

describe("namespaceOptions", () => {
  it("offers legacy jobs with no namespace as default, so the switcher shows", () => {
    expect(
      namespaceOptions({ principal: clusterWide, jobNamespaces: ["", undefined, "  ", "marketing"] }),
    ).toEqual(["default", "marketing"]);
  });

  it("offers a confined principal exactly its own namespaces, sorted with default first", () => {
    expect(namespaceOptions({ principal: confined(["marketing", "default", "finance"]) })).toEqual([
      "default",
      "finance",
      "marketing",
    ]);
  });

  it("ignores declared namespaces and job namespaces for a confined principal", () => {
    expect(
      namespaceOptions({ principal: confined(["marketing"]), declared: ["finance"], jobNamespaces: ["finance"] }),
    ).toEqual(["marketing"]);
  });

  it("unions declared namespaces and the namespaces of readable jobs for a cluster-wide principal", () => {
    expect(
      namespaceOptions({ principal: clusterWide, declared: ["default", "finance"], jobNamespaces: ["marketing", undefined, "finance"] }),
    ).toEqual(["default", "finance", "marketing"]);
  });

  it("derives options from the jobs alone when authentication is disabled", () => {
    expect(namespaceOptions({ principal: anonymous, jobNamespaces: ["default", "marketing"] })).toEqual([
      "default",
      "marketing",
    ]);
  });

  it("drops names that are not DNS labels", () => {
    expect(namespaceOptions({ principal: clusterWide, jobNamespaces: ["Bad_NS", "ok"] })).toEqual(["ok"]);
  });
});

describe("effectiveSelection", () => {
  it("applies the stored value only when the principal can see it", () => {
    expect(effectiveSelection("marketing", ["default", "marketing"], true)).toBe("marketing");
    expect(effectiveSelection("finance", ["default", "marketing"], true)).toBeNull();
    expect(effectiveSelection(null, ["default"], true)).toBeNull();
  });

  it("uses the stored value provisionally while the options load", () => {
    expect(effectiveSelection("finance", [], false)).toBe("finance");
  });
});

describe("filterByNamespace", () => {
  const items = [{ namespace: "marketing" }, { namespace: "finance" }, { namespace: "" }, {}];
  it("keeps everything with no selection", () => {
    expect(filterByNamespace(items, null)).toHaveLength(4);
  });
  it("narrows by namespace, with a missing namespace meaning default", () => {
    expect(filterByNamespace(items, "marketing")).toEqual([{ namespace: "marketing" }]);
    expect(filterByNamespace(items, "default")).toHaveLength(2);
  });
});

describe("scopedKey", () => {
  it("is the plain key without a selection and a distinct key with one", () => {
    expect(scopedKey("jobs", null)).toEqual(["jobs"]);
    expect(scopedKey("jobs", "marketing")).toEqual(["jobs", "namespace", "marketing"]);
  });
});

describe("parseNamespace", () => {
  it("accepts DNS labels only", () => {
    expect(parseNamespace("marketing")).toBe("marketing");
    expect(parseNamespace("*")).toBeNull();
    expect(parseNamespace("a b")).toBeNull();
    expect(parseNamespace(7)).toBeNull();
  });
});

describe("the stored selection", () => {
  beforeEach(() => {
    resetNamespaceViewForTests();
    window.localStorage.clear();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    resetNamespaceViewForTests();
  });

  it("persists to localStorage under a non-credential key", () => {
    setStoredNamespace("marketing");
    expect(window.localStorage.getItem("caesium.namespaceView")).toBe("marketing");
    setStoredNamespace(null);
    expect(window.localStorage.getItem("caesium.namespaceView")).toBeNull();
  });

  it("keeps working in memory when storage throws", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("storage disabled");
    });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new Error("storage disabled");
    });
    expect(() => setStoredNamespace("finance")).not.toThrow();
    expect(() => setStoredNamespace(null)).not.toThrow();
  });
});
