import { createRouter, createWebHistory } from "vue-router";

import { installNavigationRecovery, routeRecords } from "./routes";

export const router = createRouter({
  history: createWebHistory(),
  routes: routeRecords,
});

installNavigationRecovery(router);
