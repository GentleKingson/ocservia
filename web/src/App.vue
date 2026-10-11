<script setup lang="ts">
import { ResponseError, type Workspace } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";

import { Button } from "@/components/ui/button";
import AppHeader from "./components/layout/AppHeader.vue";
import AppSidebar from "./components/layout/AppSidebar.vue";
import { consumeLoginReturnPath } from "./shared/session";
import {
  getWorkspace,
  listAuthorizedWorkspaces,
  selectWorkspace,
} from "./api/workspace";
import { useReadinessStore } from "./shared/readiness";
import { useFleetStore } from "./shared/fleet";
import { reloadableNavigation } from "./shared/routes";

import { createForegroundRefresh } from "./shared/foreground-refresh";

const readiness = useReadinessStore();
const router = useRouter();
const isLogin = computed(() => router.currentRoute.value.name === "login");
const workspaces = ref<Workspace[]>([]);
const selectedWorkspaceId = ref("");
// The shell stays hidden until the session is confirmed, so signed-out
// visits go to the login page without rendering the console first.
const authenticated = ref(false);
const foregroundRefresh = createForegroundRefresh(
  () => readiness.refresh(),
  () => (readiness.isReady ? 60_000 : 15_000),
);
let stopLoginWatch: (() => void) | undefined;

onMounted(async () => {
  await router.isReady();
  stopLoginWatch = watch(
    isLogin,
    async (login) => {
      foregroundRefresh.stop();
      if (login) {
        useFleetStore().disconnect();
        return;
      }
      foregroundRefresh.start();
      try {
        workspaces.value = await listAuthorizedWorkspaces();
        authenticated.value = true;
        selectedWorkspaceId.value = (await getWorkspace()).id;
        const returnTo = consumeLoginReturnPath();
        if (returnTo && returnTo !== router.currentRoute.value.fullPath) {
          await router.replace(returnTo);
        }
      } catch (cause) {
        // The centralized API handler opens the unified login page on 401;
        // other failures still show the shell so pages report their errors.
        if (!(cause instanceof ResponseError && cause.response.status === 401))
          authenticated.value = true;
      }
    },
    { immediate: true },
  );
});
onBeforeUnmount(() => {
  stopLoginWatch?.();
  foregroundRefresh.stop();
});

async function changeWorkspace(workspaceId: string): Promise<void> {
  selectedWorkspaceId.value = (await selectWorkspace(workspaceId)).id;
}

// An explicit choice: loading from the server discards unsaved input.
function reloadNavigation(): void {
  if (reloadableNavigation.value)
    window.location.assign(reloadableNavigation.value);
}

const mainContent = ref<HTMLElement>();
function focusMainContent(): void {
  mainContent.value?.focus();
}
</script>

<template>
  <div
    v-if="reloadableNavigation"
    role="alert"
    class="bg-background fixed inset-x-4 bottom-4 z-50 flex flex-wrap items-center gap-2 rounded-lg border p-3 text-sm shadow-md md:left-auto md:max-w-md"
    data-testid="navigation-reload"
  >
    <p class="m-0 min-w-0 flex-1">{{ $t("pageLoadFailed") }}</p>
    <Button type="button" size="sm" @click="reloadNavigation">{{
      $t("reloadPage")
    }}</Button>
    <Button
      type="button"
      size="sm"
      variant="ghost"
      @click="reloadableNavigation = undefined"
      >{{ $t("dismiss") }}</Button
    >
  </div>
  <RouterView v-if="isLogin" />
  <div
    v-else-if="authenticated"
    class="bg-sidebar min-h-screen md:grid md:grid-cols-[16rem_minmax(0,1fr)]"
  >
    <a
      href="#main-content"
      class="bg-primary text-primary-foreground focus-visible:outline-ring sr-only z-50 rounded-md px-4 py-2 text-sm font-medium focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus-visible:outline-2 focus-visible:outline-offset-2"
      @click.prevent="focusMainContent"
      >{{ $t("skipToContent") }}</a
    >
    <aside class="sticky top-0 hidden h-screen md:block">
      <AppSidebar />
    </aside>
    <div
      class="bg-background min-h-screen min-w-0 md:m-2 md:ml-0 md:min-h-[calc(100vh-1rem)] md:rounded-xl md:shadow-sm"
    >
      <AppHeader
        v-model:workspace-id="selectedWorkspaceId"
        :workspaces="workspaces"
        :readiness="readiness.state"
        @change-workspace="changeWorkspace"
        @navigated="focusMainContent"
      />
      <div
        id="main-content"
        ref="mainContent"
        tabindex="-1"
        class="px-4 py-4 focus:outline-none md:px-6 md:py-6"
      >
        <RouterView />
      </div>
    </div>
  </div>
</template>
