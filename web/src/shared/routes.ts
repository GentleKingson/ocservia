import type { RouteRecordRaw } from "vue-router";

import NodeDetailView from "../views/NodeDetailView.vue";
import NodesView from "../views/NodesView.vue";
import OperationsView from "../views/OperationsView.vue";
import OverviewView from "../views/OverviewView.vue";
import RolloutDetailView from "../views/RolloutDetailView.vue";
import SettingsView from "../views/SettingsView.vue";
import ApprovalsView from "../views/ApprovalsView.vue";

import AuditView from "../views/AuditView.vue";

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
  { path: "/nodes", name: "nodes", component: NodesView },
  {
    path: "/nodes/:nodeId",
    name: "node-detail",
    component: NodeDetailView,
  },
  { path: "/operations", name: "operations", component: OperationsView },
  {
    path: "/approvals/:approvalId?",
    name: "approvals",
    component: ApprovalsView,
  },
  {
    path: "/rollouts/:rolloutId",
    name: "rollout-detail",
    component: RolloutDetailView,
  },
  { path: "/audit", name: "audit", component: AuditView },
  { path: "/settings", name: "settings", component: SettingsView },
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
