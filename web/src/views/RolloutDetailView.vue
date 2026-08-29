<script setup lang="ts">
import { ArrowLeft, Server } from "@lucide/vue";
import type { AgentRollout, AgentRolloutNode } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { getAgentRollout, resumeAgentRollout } from "../api/client";

const route = useRoute();
const router = useRouter();

const rollout = ref<AgentRollout | undefined>(undefined);
const loading = ref(true);
const unavailable = ref(false);
const notFound = ref(false);
const resuming = ref(false);
const resumeError = ref("");

const activeStates = new Set(["queued", "running", "paused"]);
let pollTimer: ReturnType<typeof setInterval> | undefined;

const batches = computed(() => {
  const grouped = new Map<number, AgentRolloutNode[]>();
  for (const node of rollout.value?.nodes ?? []) {
    const batch = grouped.get(node.batch) ?? [];
    batch.push(node);
    grouped.set(node.batch, batch);
  }
  return [...grouped.entries()]
    .sort(([left], [right]) => left - right)
    .map(([batch, nodes]) => ({
      batch,
      canary: batch === 0,
      succeeded: nodes.filter((node) => node.state === "succeeded").length,
      failed: nodes.filter(
        (node) =>
          node.state === "failed" ||
          node.state === "rolled_back" ||
          node.state === "unknown",
      ).length,
      nodes,
    }));
});

const remaining = computed(
  () =>
    (rollout.value?.nodes ?? []).filter(
      (node) => node.state === "pending" || node.state === "running",
    ).length,
);

async function refresh(): Promise<void> {
  const rolloutId = String(route.params.rolloutId ?? "");
  if (!rolloutId) return;
  try {
    rollout.value = await getAgentRollout(rolloutId);
    unavailable.value = false;
    notFound.value = false;
  } catch (cause) {
    const status = (cause as { status?: number }).status;
    if (status === 404) notFound.value = true;
    else unavailable.value = true;
  } finally {
    loading.value = false;
  }
}

async function resume(): Promise<void> {
  if (!rollout.value || resuming.value) return;
  resuming.value = true;
  resumeError.value = "";
  try {
    rollout.value = await resumeAgentRollout(rollout.value.id);
  } catch (cause) {
    resumeError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    resuming.value = false;
  }
}

function openNode(nodeId: string): void {
  void router.push({ name: "node-detail", params: { nodeId } });
}

onMounted(() => {
  void refresh();
  pollTimer = setInterval(() => {
    if (rollout.value === undefined || activeStates.has(rollout.value.state)) {
      void refresh();
    }
  }, 2000);
});

onBeforeUnmount(() => {
  if (pollTimer) clearInterval(pollTimer);
});
</script>

<template>
  <main class="overview rollout-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("rollouts") }}</p>
        <h1>{{ $t("rollingUpgrade") }}</h1>
      </div>
      <span
        v-if="rollout"
        class="health"
        :class="{
          unavailable: rollout.state === 'paused' || rollout.state === 'failed',
        }"
        ><i></i>{{ $t(`rolloutState_${rollout.state}`) }}</span
      >
    </div>
    <button type="button" class="link-button" @click="router.back()">
      <ArrowLeft :size="14" />{{ $t("backToOperations") }}
    </button>

    <div v-if="loading" class="detail-state">
      <Server :size="24" /><span>{{ $t("loading") }}</span>
    </div>
    <div v-else-if="notFound" class="detail-state" role="alert">
      <Server :size="24" /><span>{{ $t("rolloutNotFound") }}</span>
    </div>
    <div v-else-if="unavailable || !rollout" class="detail-state" role="alert">
      <Server :size="24" /><span>{{ $t("rolloutUnavailable") }}</span>
    </div>
    <template v-else>
      <section class="rollout-summary">
        <dl>
          <div>
            <dt>{{ $t("targetVersion") }}</dt>
            <dd>
              <code>{{ rollout.targetVersion }}</code>
            </dd>
          </div>
          <div>
            <dt>{{ $t("batchSize") }}</dt>
            <dd>{{ rollout.batchSize }}</dd>
          </div>
          <div>
            <dt>{{ $t("canary") }}</dt>
            <dd>{{ $t("canaryOneNode") }}</dd>
          </div>
          <div>
            <dt>{{ $t("remaining") }}</dt>
            <dd>{{ remaining }}</dd>
          </div>
          <div>
            <dt>{{ $t("reason") }}</dt>
            <dd>{{ rollout.reason }}</dd>
          </div>
          <div>
            <dt>{{ $t("approvalId") }}</dt>
            <dd>
              <code>{{ rollout.approvalId.slice(0, 8) }}</code>
            </dd>
          </div>
        </dl>
      </section>

      <div
        v-if="rollout.state === 'paused'"
        class="rollout-paused"
        role="alert"
      >
        <div>
          <strong>{{ $t("rolloutState_paused") }}</strong>
          <span>{{ $t("pausedNotice") }}</span>
          <small v-if="rollout.pauseCode"
            >{{ $t("pauseCode") }}: <code>{{ rollout.pauseCode }}</code></small
          >
        </div>
        <button
          type="button"
          class="primary"
          :disabled="resuming"
          @click="resume"
        >
          {{ $t("resumeRollout") }}
        </button>
      </div>
      <p v-if="resumeError" class="page-error" role="alert">
        {{ resumeError }}
      </p>

      <section class="rollout-batches">
        <article v-for="batch in batches" :key="batch.batch">
          <header>
            <h2>
              {{
                batch.canary
                  ? $t("canaryBatch")
                  : $t("batchN", { n: batch.batch })
              }}
            </h2>
            <span
              >{{ batch.succeeded }}/{{ batch.nodes.length }}
              {{ $t("operation_succeeded") }}</span
            >
            <span v-if="batch.failed" class="rollout-batch-failed"
              >{{ batch.failed }} {{ $t("failed") }}</span
            >
          </header>
          <ul>
            <li v-for="node in batch.nodes" :key="node.nodeId">
              <button
                type="button"
                class="link-button"
                @click="openNode(node.nodeId)"
              >
                <code>{{ node.nodeId.slice(0, 8) }}</code>
              </button>
              <span class="state-dot" :class="node.state"></span>
              <span>{{ $t(`rolloutNode_${node.state}`) }}</span>
              <code v-if="node.operationId">{{
                node.operationId.slice(0, 8)
              }}</code>
              <code v-if="node.failureCode" class="rollout-failure-code">{{
                node.failureCode
              }}</code>
            </li>
          </ul>
        </article>
      </section>

      <section
        v-if="rollout.excluded && rollout.excluded.length"
        class="rollout-exclusions"
      >
        <h2>{{ $t("excludedNodes") }}</h2>
        <ul>
          <li v-for="exclusion in rollout.excluded" :key="exclusion.nodeId">
            <code>{{ exclusion.nodeId.slice(0, 8) }}</code>
            <span>{{ $t(`exclusion_${exclusion.reason}`) }}</span>
          </li>
        </ul>
      </section>
    </template>
  </main>
</template>
