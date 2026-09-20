import { defineStore } from "pinia";
import { computed, ref } from "vue";

import { getReadiness } from "../api/platform";

export const useReadinessStore = defineStore("readiness", () => {
  const state = ref<"loading" | "ready" | "unavailable">("loading");
  const isReady = computed(() => state.value === "ready");

  async function refresh(): Promise<void> {
    try {
      await getReadiness();
      state.value = "ready";
    } catch {
      state.value = "unavailable";
    }
  }

  return { state, isReady, refresh };
});
