import { createRouter, createWebHistory } from "vue-router";

import OverviewView from "../views/OverviewView.vue";
import NodesView from "../views/NodesView.vue";

const PlaceholderView = {
  template:
    '<main class="empty-view"><h1>{{ $t(String($route.name)) }}</h1></main>',
};

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/", name: "overview", component: OverviewView },
    { path: "/nodes", name: "nodes", component: NodesView },
    { path: "/operations", name: "operations", component: PlaceholderView },
    { path: "/audit", name: "audit", component: PlaceholderView },
    { path: "/settings", name: "settings", component: PlaceholderView },
  ],
});
