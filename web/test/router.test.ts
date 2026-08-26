import {
  createMemoryHistory,
  createRouter,
  type RouteRecordRaw,
} from "vue-router";
import { describe, expect, it } from "vitest";

import { routeRecords } from "../src/shared/routes";

describe("web information architecture routes", () => {
  it("resolves the node detail route on a direct navigation", async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: routeRecords,
    });

    await router.push("/nodes/019fc0a4-6d92-765c-a8a1-4af556614cc3");
    await router.isReady();

    expect(router.currentRoute.value.name).toBe("node-detail");
    expect(router.currentRoute.value.params.nodeId).toBe(
      "019fc0a4-6d92-765c-a8a1-4af556614cc3",
    );
  });

  it("uses real pages for operations and settings", () => {
    const pageNames = new Set(
      routeRecords
        .filter(
          (route): route is RouteRecordRaw & { name: string } =>
            typeof route.name === "string",
        )
        .map((route) => route.name),
    );

    expect(pageNames).toEqual(
      new Set([
        "overview",
        "nodes",
        "node-detail",
        "operations",
        "audit",
        "settings",
      ]),
    );
    expect(
      routeRecords.find((route) => route.name === "operations")?.component,
    ).toBeDefined();
    expect(
      routeRecords.find((route) => route.name === "settings")?.component,
    ).toBeDefined();
  });
});
