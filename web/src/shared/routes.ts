import {
  START_LOCATION,
  type RouteLocationNormalized,
  type RouteRecordRaw,
} from "vue-router";

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

// A redeploy replaces the hashed page chunks (the gateway answers a missing
// asset with index.html), so a tab opened before it cannot load a page it has
// not visited yet. Load that one navigation from the server instead of
// aborting it silently. The initial navigation already came from the server,
// so it is never retried and a missing chunk cannot reload in a loop.
export function loadFailedNavigation(
  _error: unknown,
  to: RouteLocationNormalized,
  from: RouteLocationNormalized,
): void {
  if (from === START_LOCATION) return;
  window.location.assign(to.fullPath);
}
