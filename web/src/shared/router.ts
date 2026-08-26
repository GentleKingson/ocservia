import { createRouter, createWebHistory } from "vue-router";

import { routeRecords } from "./routes";

export const router = createRouter({
  history: createWebHistory(),
  routes: routeRecords,
});
