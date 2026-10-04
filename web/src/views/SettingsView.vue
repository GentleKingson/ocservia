<script setup lang="ts">
import { CheckCircle2, CircleAlert } from "@lucide/vue";
import type { BuildInfo, Workspace } from "@ocservia/api-client";
import { onBeforeUnmount, onMounted, ref } from "vue";

import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import { getWorkspace, workspaceChangedEvent } from "../api/workspace";
import { getVersion } from "../api/platform";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const workspace = ref<Workspace>();
const buildInfo = ref<BuildInfo>();
const loading = ref(true);
const unavailable = ref(false);
const buildUnavailable = ref(false);
let loadSequence = 0;

async function loadWorkspace(): Promise<void> {
  const sequence = ++loadSequence;
  loading.value = true;
  unavailable.value = false;
  buildUnavailable.value = false;
  buildInfo.value = undefined;
  try {
    const [workspaceResult, buildResult] = await Promise.all([
      getWorkspace(),
      getVersion().then(
        (value) => ({ value, unavailable: false }),
        () => ({ value: undefined, unavailable: true }),
      ),
    ]);
    if (sequence !== loadSequence) return;
    workspace.value = workspaceResult;
    buildInfo.value = buildResult.value;
    buildUnavailable.value = buildResult.unavailable;
  } catch {
    if (sequence !== loadSequence) return;
    workspace.value = undefined;
    unavailable.value = true;
  } finally {
    if (sequence === loadSequence) loading.value = false;
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
  loadSequence++;
  window.removeEventListener(workspaceChangedEvent, refreshForWorkspace);
});
</script>

<template>
  <main>
    <PageHeader :eyebrow="$t('platform')" :title="$t('settings')" />
    <DataState v-if="loading" kind="loading" :message="$t('loading')" />
    <DataState
      v-else-if="unavailable"
      kind="error"
      :message="$t('noWorkspace')"
    />
    <div v-else class="grid gap-4 md:grid-cols-2">
      <section
        class="bg-card border-border rounded-lg border p-5"
        aria-labelledby="settings-workspace"
      >
        <header class="border-border border-b pb-3">
          <p class="text-muted-foreground m-0 mb-1 text-xs uppercase">
            {{ $t("workspace") }}
          </p>
          <h2 id="settings-workspace" class="m-0 text-base font-semibold">
            {{ $t("workspaceInformation") }}
          </h2>
        </header>
        <dl class="m-0 mt-2 text-sm">
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("workspaceName") }}</dt>
            <dd class="m-0 text-right wrap-anywhere">
              {{ workspace?.name ?? $t("notAvailable") }}
            </dd>
          </div>
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("workspaceSlug") }}</dt>
            <dd class="m-0 text-right wrap-anywhere">
              {{ workspace?.slug ?? $t("notAvailable") }}
            </dd>
          </div>
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("workspaceId") }}</dt>
            <dd class="m-0 text-right wrap-anywhere">
              <code class="font-mono text-xs">{{
                workspace?.id ?? $t("notAvailable")
              }}</code>
            </dd>
          </div>
        </dl>
      </section>
      <section
        class="bg-card border-border rounded-lg border p-5"
        aria-labelledby="settings-platform"
      >
        <header class="border-border border-b pb-3">
          <p class="text-muted-foreground m-0 mb-1 text-xs uppercase">
            {{ $t("platform") }}
          </p>
          <h2 id="settings-platform" class="m-0 text-base font-semibold">
            {{ $t("platformContext") }}
          </h2>
        </header>
        <dl class="m-0 mt-2 text-sm">
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("readiness") }}</dt>
            <dd
              class="m-0 inline-flex items-center gap-1.5 text-right"
              :class="readiness.isReady ? 'text-success' : 'text-destructive'"
            >
              <CheckCircle2
                v-if="readiness.isReady"
                :size="16"
                aria-hidden="true"
              />
              <CircleAlert v-else :size="16" aria-hidden="true" />
              {{ $t(readiness.isReady ? "ready" : "unavailable") }}
            </dd>
          </div>
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">
              {{ $t("recommendedAgentVersion") }}
            </dt>
            <dd class="m-0 text-right wrap-anywhere">
              {{
                buildUnavailable
                  ? $t("recommendationUnavailable")
                  : buildInfo?.recommendedAgentVersion ||
                    $t("recommendationNotConfigured")
              }}
            </dd>
          </div>
        </dl>
      </section>
    </div>
  </main>
</template>
