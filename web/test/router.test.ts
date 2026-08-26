import {
  createMemoryHistory,
  createRouter,
  type RouteRecordRaw,
} from "vue-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { routeRecords } from "../src/shared/routes";

describe("web information architecture routes", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
  });

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
        "development",
      ]),
    );
    expect(
      routeRecords.find((route) => route.name === "operations")?.component,
    ).toBeDefined();
    expect(
      routeRecords.find((route) => route.name === "settings")?.component,
    ).toBeDefined();
  });

  it("registers the development simulator route only on development runtimes", async () => {
    vi.stubEnv("DEV", false);
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "");
    vi.resetModules();
    const { routeRecords: productionRoutes } =
      await import("../src/shared/routes");

    expect(
      productionRoutes.find((route) => route.name === "development"),
    ).toBeUndefined();

    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "local-development-token-32-characters");
    vi.resetModules();
    const { routeRecords: developmentRoutes } =
      await import("../src/shared/routes");

    expect(
      developmentRoutes.find((route) => route.name === "development"),
    ).toMatchObject({ path: "/dev" });
  });
});
