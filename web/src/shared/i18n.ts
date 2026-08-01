import { createI18n } from "vue-i18n";

export const i18n = createI18n({
  legacy: false,
  locale: "en",
  fallbackLocale: "en",
  messages: {
    en: {
      brand: "ocservia",
      overview: "Overview",
      nodes: "Nodes",
      operations: "Operations",
      audit: "Audit",
      settings: "Settings",
      workspace: "Workspace",
      platform: "Platform",
      ready: "Ready",
      controlPlane: "Control plane",
      activeNodes: "Active nodes",
      pendingOperations: "Pending operations",
      recentActivity: "Recent activity",
      noActivity: "No activity yet",
      allSystems: "All systems operational",
      navigation: "Primary navigation",
    },
  },
});
