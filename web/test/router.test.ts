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

  describe("failed page chunks", () => {
    function failingChunk(reportedByVite: boolean) {
      return () => {
        const error = new TypeError(
          "Failed to fetch dynamically imported module",
        );
        if (reportedByVite) {
          // What Vite's preload helper does before rethrowing.
          const event = new Event("vite:preloadError", { cancelable: true });
          Object.assign(event, { payload: error });
          window.dispatchEvent(event);
        }
        return Promise.reject(error);
      };
    }

    async function setup(reportedByVite = true) {
      const assign = vi.fn();
      vi.stubGlobal(
        "window",
        Object.assign(new EventTarget(), { location: { assign } }),
      );
      const { installNavigationRecovery, reloadableNavigation } =
        await import("../src/shared/routes");
      reloadableNavigation.value = undefined;
      let loads = 0;
      const page = failingChunk(reportedByVite);
      const router = createRouter({
        history: createMemoryHistory(),
        routes: [
          { path: "/", component: {} },
          { path: "/other", component: {} },
          {
            path: "/page/:id",
            component: () => (++loads > 1 ? Promise.resolve({}) : page()),
          },
        ],
      });
      installNavigationRecovery(router);
      return { assign, reloadableNavigation, router };
    }

    it("keeps the current page and offers a reload, never reloading itself", async () => {
      const { assign, reloadableNavigation, router } = await setup();
      await router.push("/");

      await expect(router.push("/page/a?tab=x")).rejects.toThrow(TypeError);
      expect(router.currentRoute.value.fullPath).toBe("/");
      expect(reloadableNavigation.value).toBe("/page/a?tab=x");
      expect(assign).not.toHaveBeenCalled();

      // A later successful load of the page clears the offer.
      await router.push("/page/a?tab=x");
      expect(router.currentRoute.value.fullPath).toBe("/page/a?tab=x");
      expect(reloadableNavigation.value).toBeUndefined();
    });

    it("never reloads on a failed initial navigation", async () => {
      const { assign, reloadableNavigation, router } = await setup();

      await expect(router.push("/page/a")).rejects.toThrow(TypeError);
      expect(assign).not.toHaveBeenCalled();
      expect(reloadableNavigation.value).toBe("/page/a");
    });

    it("leaves other navigation errors to the console", async () => {
      const error = vi.spyOn(console, "error").mockImplementation(() => {});
      const { assign, reloadableNavigation, router } = await setup(false);
      await router.push("/");

      await expect(router.push("/page/a")).rejects.toThrow(TypeError);
      expect(reloadableNavigation.value).toBeUndefined();
      expect(error).toHaveBeenCalledWith(expect.any(TypeError));
      expect(assign).not.toHaveBeenCalled();
      error.mockRestore();
    });

    it("mounts after a failed initial chunk so the offer can render", async () => {
      const { router } = await setup();
      const { mountAfterInitialNavigation } =
        await import("../src/shared/routes");
      const mount = vi.fn();
      const mounted = mountAfterInitialNavigation(router, mount);

      await expect(router.push("/page/a")).rejects.toThrow(TypeError);
      await mounted;
      expect(mount).toHaveBeenCalledOnce();
    });

    it("mounts once after a successful initial navigation", async () => {
      const { router } = await setup();
      const { mountAfterInitialNavigation } =
        await import("../src/shared/routes");
      const mount = vi.fn();
      const mounted = mountAfterInitialNavigation(router, mount);

      await router.push("/");
      await mounted;
      expect(mount).toHaveBeenCalledOnce();
    });

    it("does not mount after a failed initial navigation that is not a chunk", async () => {
      vi.spyOn(console, "error").mockImplementation(() => {});
      const { router } = await setup(false);
      const { mountAfterInitialNavigation } =
        await import("../src/shared/routes");
      const mount = vi.fn();
      const mounted = mountAfterInitialNavigation(router, mount);

      await expect(router.push("/page/a")).rejects.toThrow(TypeError);
      await mounted;
      expect(mount).not.toHaveBeenCalled();
      vi.mocked(console.error).mockRestore();
    });

    it("drops the offer after the user navigates elsewhere", async () => {
      const { reloadableNavigation, router } = await setup();
      await router.push("/");
      await expect(router.push("/page/a")).rejects.toThrow(TypeError);

      await router.push("/other");
      expect(reloadableNavigation.value).toBeUndefined();
    });
  });
});
