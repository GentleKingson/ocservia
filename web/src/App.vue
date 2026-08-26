<script setup lang="ts">
import {
  Activity,
  Boxes,
  LayoutDashboard,
  ListChecks,
  Settings,
} from "@lucide/vue";
import type { Workspace } from "@ocservia/api-client";
import { onBeforeUnmount, onMounted, ref } from "vue";
import { useRouter } from "vue-router";

import {
  consumeLoginReturnPath,
  getWorkspace,
  listAuthorizedWorkspaces,
  selectWorkspace,
} from "./api/client";
import { useReadinessStore } from "./shared/readiness";

const readiness = useReadinessStore();
const router = useRouter();
const workspaces = ref<Workspace[]>([]);
const selectedWorkspaceId = ref("");
let refreshTimer: ReturnType<typeof setInterval> | undefined;

onMounted(async () => {
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
    // The centralized API handler starts OIDC login for unauthenticated users.
  }
});
onBeforeUnmount(() => clearInterval(refreshTimer));

async function changeWorkspace(event: Event): Promise<void> {
  const workspaceId = (event.target as HTMLSelectElement).value;
  selectedWorkspaceId.value = (await selectWorkspace(workspaceId)).id;
}

const links = [
  { to: "/", label: "overview", icon: LayoutDashboard },
  { to: "/nodes", label: "nodes", icon: Boxes },
  { to: "/operations", label: "operations", icon: ListChecks },
];
</script>

<template>
  <div class="app-shell">
    <aside class="sidebar">
      <div class="brand">
        <Activity :size="22" stroke-width="2.4" /><span>{{ $t("brand") }}</span>
      </div>
      <div class="workspace-switcher">
        <label for="workspace-select">{{ $t("workspace") }}</label>
        <select
          id="workspace-select"
          v-model="selectedWorkspaceId"
          :aria-label="$t('workspace')"
          :disabled="workspaces.length < 2"
          @change="changeWorkspace"
        >
          <option
            v-for="workspace in workspaces"
            :key="workspace.id"
            :value="workspace.id"
          >
            {{ workspace.name }}
          </option>
        </select>
      </div>
      <nav :aria-label="$t('navigation')">
        <RouterLink
          v-for="link in links"
          :key="link.to"
          :to="link.to"
          :aria-label="$t(link.label)"
          :title="$t(link.label)"
        >
          <component :is="link.icon" :size="18" /><span>{{
            $t(link.label)
          }}</span>
        </RouterLink>
      </nav>
      <RouterLink
        class="settings-link"
        to="/settings"
        :aria-label="$t('settings')"
        :title="$t('settings')"
        ><Settings :size="18" /><span>{{ $t("settings") }}</span></RouterLink
      >
    </aside>
    <section class="content-shell">
      <header class="topbar">
        <span>{{ $t("platform") }}</span>
        <div class="status" :class="{ unavailable: !readiness.isReady }">
          <i></i>{{ $t(readiness.isReady ? "ready" : "unavailable") }}
        </div>
      </header>
      <RouterView />
    </section>
  </div>
</template>
