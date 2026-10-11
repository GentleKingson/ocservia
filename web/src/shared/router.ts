import { createRouter, createWebHistory } from "vue-router";

import { loadFailedNavigation, routeRecords } from "./routes";

export const router = createRouter({
  history: createWebHistory(),
  routes: routeRecords,
});

router.onError(loadFailedNavigation);
