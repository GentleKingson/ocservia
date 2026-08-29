import type { RouteRecordRaw } from "vue-router";

import NodeDetailView from "../views/NodeDetailView.vue";
import NodesView from "../views/NodesView.vue";
import OperationsView from "../views/OperationsView.vue";
import OverviewView from "../views/OverviewView.vue";
import RolloutDetailView from "../views/RolloutDetailView.vue";
import SettingsView from "../views/SettingsView.vue";

const AuditPlaceholderView = {
  template:
    '<main class="empty-view"><h1>{{ $t(String($route.name)) }}</h1></main>',
};

// The development simulator stays reachable only on development runtimes
// (vite dev server or a build with a development auth token); production
// navigation never registers the route.
export const developmentRuntime =
  import.meta.env.DEV || Boolean(import.meta.env.VITE_DEV_AUTH_TOKEN);

export const routeRecords: RouteRecordRaw[] = [
  { path: "/", name: "overview", component: OverviewView },
  { path: "/nodes", name: "nodes", component: NodesView },
  {
    path: "/nodes/:nodeId",
    name: "node-detail",
    component: NodeDetailView,
  },
  { path: "/operations", name: "operations", component: OperationsView },
  {
    path: "/rollouts/:rolloutId",
    name: "rollout-detail",
    component: RolloutDetailView,
  },
  { path: "/audit", name: "audit", component: AuditPlaceholderView },
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
