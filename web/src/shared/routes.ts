import type { RouteRecordRaw } from "vue-router";

import NodeDetailView from "../views/NodeDetailView.vue";
import NodesView from "../views/NodesView.vue";
import OperationsView from "../views/OperationsView.vue";
import OverviewView from "../views/OverviewView.vue";
import SettingsView from "../views/SettingsView.vue";

export const routeRecords: RouteRecordRaw[] = [
  { path: "/", name: "overview", component: OverviewView },
  { path: "/nodes", name: "nodes", component: NodesView },
  {
    path: "/nodes/:nodeId",
    name: "node-detail",
    component: NodeDetailView,
  },
  { path: "/operations", name: "operations", component: OperationsView },
  { path: "/settings", name: "settings", component: SettingsView },
];
