<script setup lang="ts">
import type { Workspace } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";

import AppHeader from "./components/layout/AppHeader.vue";
import AppSidebar from "./components/layout/AppSidebar.vue";
import { consumeLoginReturnPath } from "./shared/session";
import {
  getWorkspace,
  listAuthorizedWorkspaces,
  selectWorkspace,
} from "./api/workspace";
import { useReadinessStore } from "./shared/readiness";

const readiness = useReadinessStore();
const router = useRouter();
const isLogin = computed(() => router.currentRoute.value.name === "login");
const workspaces = ref<Workspace[]>([]);
const selectedWorkspaceId = ref("");
let refreshTimer: ReturnType<typeof setInterval> | undefined;
let stopLoginWatch: (() => void) | undefined;

onMounted(async () => {
  await router.isReady();
  stopLoginWatch = watch(
    isLogin,
    async (login) => {
      clearInterval(refreshTimer);
      if (login) return;
      void readiness.refresh();
      refreshTimer = setInterval(() => void readiness.refresh(), 15_000);
      try {
        workspaces.value = await listAuthorizedWorkspaces();
        selectedWorkspaceId.value = (await getWorkspace()).id;
        const returnTo = consumeLoginReturnPath();
        if (returnTo && returnTo !== router.currentRoute.value.fullPath) {
          await router.replace(returnTo);
        }
      } catch {
        // The centralized API handler opens the unified login page.
      }
    },
    { immediate: true },
  );
});
onBeforeUnmount(() => {
  stopLoginWatch?.();
  clearInterval(refreshTimer);
});

async function changeWorkspace(workspaceId: string): Promise<void> {
  selectedWorkspaceId.value = (await selectWorkspace(workspaceId)).id;
}

const mainContent = ref<HTMLElement>();
function focusMainContent(): void {
  mainContent.value?.focus();
}
</script>

<template>
  <RouterView v-if="isLogin" />
  <div v-else class="min-h-screen md:grid md:grid-cols-[15rem_minmax(0,1fr)]">
    <a
      href="#main-content"
      class="bg-primary text-primary-foreground focus-visible:outline-ring sr-only z-50 rounded-md px-4 py-2 text-sm font-medium focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus-visible:outline-2 focus-visible:outline-offset-2"
      @click.prevent="focusMainContent"
      >{{ $t("skipToContent") }}</a
    >
    <aside
      class="bg-card border-border sticky top-0 hidden h-screen border-r md:block"
    >
      <AppSidebar />
    </aside>
    <div class="min-w-0">
      <AppHeader
        v-model:workspace-id="selectedWorkspaceId"
        :workspaces="workspaces"
        :ready="readiness.isReady"
        @change-workspace="changeWorkspace"
        @navigated="focusMainContent"
      />
      <div
        id="main-content"
        ref="mainContent"
        tabindex="-1"
        class="focus:outline-none"
      >
        <RouterView />
      </div>
    </div>
  </div>
</template>
