<script setup lang="ts">
import { CheckCircle2, CircleAlert, Settings2 } from "@lucide/vue";
import type { Workspace } from "@ocservia/api-client";
import { onBeforeUnmount, onMounted, ref } from "vue";

import { getWorkspace, workspaceChangedEvent } from "../api/client";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const workspace = ref<Workspace>();
const loading = ref(true);
const unavailable = ref(false);

async function loadWorkspace(): Promise<void> {
  loading.value = true;
  unavailable.value = false;
  try {
    workspace.value = await getWorkspace();
  } catch {
    workspace.value = undefined;
    unavailable.value = true;
  } finally {
    loading.value = false;
  }
}

function refreshForWorkspace(): void {
  void loadWorkspace();
}

onMounted(() => {
  window.addEventListener(workspaceChangedEvent, refreshForWorkspace);
  void loadWorkspace();
});
onBeforeUnmount(() => {
  window.removeEventListener(workspaceChangedEvent, refreshForWorkspace);
});
</script>

<template>
  <main class="overview settings-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("platform") }}</p>
        <h1>{{ $t("settings") }}</h1>
      </div>
    </div>
    <div v-if="loading" class="detail-state" role="status">
      <Settings2 :size="24" /><span>{{ $t("loading") }}</span>
    </div>
    <div v-else-if="unavailable" class="detail-state" role="alert">
      <CircleAlert :size="24" /><span>{{ $t("noWorkspace") }}</span>
    </div>
    <section v-else class="settings-layout">
      <section class="settings-section">
        <header>
          <div>
            <span>{{ $t("workspace") }}</span>
            <h2>{{ $t("workspaceInformation") }}</h2>
          </div>
        </header>
        <dl>
          <div>
            <dt>{{ $t("workspaceName") }}</dt>
            <dd>{{ workspace?.name ?? $t("notAvailable") }}</dd>
          </div>
          <div>
            <dt>{{ $t("workspaceSlug") }}</dt>
            <dd>{{ workspace?.slug ?? $t("notAvailable") }}</dd>
          </div>
          <div>
            <dt>{{ $t("workspaceId") }}</dt>
            <dd>
              <code>{{ workspace?.id ?? $t("notAvailable") }}</code>
            </dd>
          </div>
        </dl>
      </section>
      <section class="settings-section">
        <header>
          <div>
            <span>{{ $t("platform") }}</span>
            <h2>{{ $t("platformContext") }}</h2>
          </div>
        </header>
        <dl>
          <div>
            <dt>{{ $t("readiness") }}</dt>
            <dd class="settings-status">
              <CheckCircle2 v-if="readiness.isReady" :size="16" />
              <CircleAlert v-else :size="16" />
              {{ $t(readiness.isReady ? "ready" : "unavailable") }}
            </dd>
          </div>
        </dl>
      </section>
    </section>
  </main>
</template>
