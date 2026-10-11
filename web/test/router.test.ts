import {
  createMemoryHistory,
  createRouter,
  type RouteRecordRaw,
} from "vue-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { loadFailedNavigation, routeRecords } from "../src/shared/routes";

describe("web information architecture routes", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
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
        "login",
        "overview",
        "nodes",
        "node-detail",
        "operations",
        "approvals",
        "rollout-detail",
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

  it("loads a page whose chunk is gone from the server, except on the initial navigation", async () => {
    const assign = vi.fn();
    vi.stubGlobal("window", { location: { assign } });
    const missingChunk = () =>
      Promise.reject(
        new TypeError("Failed to fetch dynamically imported module"),
      );
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: "/", component: {} },
        { path: "/missing/:id", component: missingChunk },
      ],
    });
    router.onError(loadFailedNavigation);

    await expect(router.push("/missing/a?tab=x")).rejects.toThrow(TypeError);
    expect(assign).not.toHaveBeenCalled();

    await router.push("/");
    await expect(router.push("/missing/b?tab=y")).rejects.toThrow(TypeError);
    expect(assign).toHaveBeenCalledExactlyOnceWith("/missing/b?tab=y");
    expect(router.currentRoute.value.fullPath).toBe("/");
  });
});
