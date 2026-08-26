import type { RouteRecordRaw } from "vue-router";

import NodeDetailView from "../views/NodeDetailView.vue";
import NodesView from "../views/NodesView.vue";
import OperationsView from "../views/OperationsView.vue";
import OverviewView from "../views/OverviewView.vue";
import SettingsView from "../views/SettingsView.vue";

const AuditPlaceholderView = {
  template:
    '<main class="empty-view"><h1>{{ $t(String($route.name)) }}</h1></main>',
};

export const routeRecords: RouteRecordRaw[] = [
  { path: "/", name: "overview", component: OverviewView },
  { path: "/nodes", name: "nodes", component: NodesView },
  {
    path: "/nodes/:nodeId",
    name: "node-detail",
    component: NodeDetailView,
  },
  { path: "/operations", name: "operations", component: OperationsView },
  { path: "/audit", name: "audit", component: AuditPlaceholderView },
  { path: "/settings", name: "settings", component: SettingsView },
];
