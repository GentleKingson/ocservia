import { ref } from "vue";
import type { RouteRecordRaw, Router } from "vue-router";

// The default landing page stays in the entry chunk; other pages load on
// first visit.
import OverviewView from "../views/OverviewView.vue";

// The development simulator stays reachable only on development runtimes
// (the vite dev server); production navigation never registers the route.
// vite.config.ts refuses builds with VITE_DEV_AUTH_TOKEN, so the token term
// only applies where a runtime sets it without DEV (see test/router.test.ts).
export const developmentRuntime =
  import.meta.env.DEV || Boolean(import.meta.env.VITE_DEV_AUTH_TOKEN);

export const routeRecords: RouteRecordRaw[] = [
  {
    path: "/login",
    name: "login",
    component: () => import("../views/LoginView.vue"),
  },
  { path: "/", name: "overview", component: OverviewView },
  {
    path: "/nodes",
    name: "nodes",
    component: () => import("../views/NodesView.vue"),
  },
  {
    path: "/nodes/:nodeId",
    name: "node-detail",
    component: () => import("../views/NodeDetailView.vue"),
  },
  {
    path: "/operations",
    name: "operations",
    component: () => import("../views/OperationsView.vue"),
  },
  {
    path: "/approvals/:approvalId?",
    name: "approvals",
    component: () => import("../views/ApprovalsView.vue"),
  },
  {
    path: "/rollouts/:rolloutId",
    name: "rollout-detail",
    component: () => import("../views/RolloutDetailView.vue"),
  },
  {
    path: "/audit",
    name: "audit",
    component: () => import("../views/AuditView.vue"),
  },
  {
    path: "/settings",
    name: "settings",
    component: () => import("../views/SettingsView.vue"),
  },
  ...(developmentRuntime
    ? [
        {
          path: "/dev",
          name: "development",
          component: () => import("../views/DevelopmentView.vue"),
        },
      ]
    : []),
];

// A page chunk fails to load after a redeploy removed it (the gateway answers
// a missing asset with index.html) or on a network failure. Vite reports each
// failed chunk with vite:preloadError before the router sees the same error,
// so only those errors are handled here. The current page and any unsaved
// input stay; the user chooses whether to load the page from the server.
export const reloadableNavigation = ref<string>();

export function installNavigationRecovery(router: Router): void {
  const chunkErrors = new WeakSet<object>();
  window.addEventListener("vite:preloadError", (event) => {
    const error: unknown = (event as Event & { payload?: unknown }).payload;
    if (typeof error === "object" && error !== null) chunkErrors.add(error);
  });
  router.onError((error: unknown, to) => {
    if (typeof error === "object" && error !== null && chunkErrors.has(error))
      reloadableNavigation.value = to.fullPath;
    // Vue Router logs only while no error handler is registered.
    else console.error(error);
  });
  router.afterEach((_to, _from, failure) => {
    if (!failure) reloadableNavigation.value = undefined;
  });
}

// Mounts after the initial navigation so the shell never renders for /login.
// When that navigation's chunk failed, it mounts anyway so the reload offer
// shows instead of a blank page; App waits on isReady(), which stays pending,
// so neither the shell nor a page renders and nothing is requested.
export function mountAfterInitialNavigation(
  router: Router,
  mount: () => void,
): Promise<void> {
  return router.isReady().then(mount, () => {
    if (reloadableNavigation.value) mount();
  });
}
