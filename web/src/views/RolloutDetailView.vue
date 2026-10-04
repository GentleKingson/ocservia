<script setup lang="ts">
import { ArrowLeft } from "@lucide/vue";
import type { AgentRollout, AgentRolloutNode } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRoute } from "vue-router";

import { getAgentRollout, resumeAgentRollout } from "../api/agents";
import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import { Button } from "@/components/ui/button";
import {
  rolloutNodeTone,
  rolloutTone,
} from "../features/operations/state-tone";

const route = useRoute();

const rollout = ref<AgentRollout | undefined>(undefined);
const loading = ref(true);
const unavailable = ref(false);
const notFound = ref(false);
const resuming = ref(false);
const resumeError = ref("");

const activeStates = new Set(["queued", "running", "paused"]);
let pollTimer: ReturnType<typeof setInterval> | undefined;

const exclusions = computed(() =>
  (rollout.value?.excluded ?? []).map((value: unknown) => {
    if (
      value !== null &&
      typeof value === "object" &&
      "node_id" in value &&
      typeof value.node_id === "string" &&
      "reason" in value &&
      typeof value.reason === "string"
    ) {
      return { nodeId: value.node_id, reason: value.reason, raw: "" };
    }
    return { nodeId: "", reason: "", raw: JSON.stringify(value) };
  }),
);

// Failed and unknown stay separate counts so a batch never reads as
// successful while any node has no confirmed outcome.
function nodeCounts(nodes: AgentRolloutNode[]) {
  const count = (...states: string[]) =>
    nodes.filter((node) => states.includes(node.state)).length;
  return {
    total: nodes.length,
    succeeded: count("succeeded"),
    failed: count("failed", "rolled_back"),
    unknown: count("unknown"),
  };
}

const totals = computed(() => nodeCounts(rollout.value?.nodes ?? []));

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
      ...nodeCounts(nodes),
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
  <main class="overview">
    <RouterLink
      :to="{ name: 'operations' }"
      class="text-primary mb-4 inline-flex items-center gap-1.5 text-sm underline-offset-4 hover:underline"
    >
      <ArrowLeft class="size-4" aria-hidden="true" />{{
        $t("backToOperations")
      }}
    </RouterLink>
    <PageHeader :eyebrow="$t('rollouts')" :title="$t('rollingUpgrade')">
      <template v-if="rollout" #actions>
        <StatusBadge
          data-testid="rollout-state"
          :tone="rolloutTone(rollout.state)"
          :label="$t(`rolloutState_${rollout.state}`)"
        />
      </template>
    </PageHeader>

    <DataState v-if="loading" kind="loading" :message="$t('loading')" />
    <DataState
      v-else-if="notFound"
      kind="error"
      :message="$t('rolloutNotFound')"
    />
    <DataState
      v-else-if="unavailable || !rollout"
      kind="error"
      :message="$t('rolloutUnavailable')"
    />
    <template v-else>
      <section
        class="bg-card border-border mb-6 rounded-lg border p-4 md:p-5"
        :aria-label="$t('rollingUpgrade')"
      >
        <dl
          class="m-0 grid grid-cols-2 gap-4 text-sm sm:grid-cols-3 lg:grid-cols-6"
        >
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">
              {{ $t("targetVersion") }}
            </dt>
            <dd class="m-0 break-all">
              <code>{{ rollout.targetVersion }}</code>
            </dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">{{ $t("batchSize") }}</dt>
            <dd class="m-0">{{ rollout.batchSize }}</dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">{{ $t("canary") }}</dt>
            <dd class="m-0">{{ $t("canaryOneNode") }}</dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">{{ $t("remaining") }}</dt>
            <dd class="m-0">{{ remaining }}</dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">{{ $t("reason") }}</dt>
            <dd class="m-0 break-words">{{ rollout.reason }}</dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">
              {{ $t("approvalId") }}
            </dt>
            <dd class="m-0">
              <code :title="rollout.approvalId">{{
                rollout.approvalId.slice(0, 8)
              }}</code>
            </dd>
          </div>
        </dl>
        <p
          class="m-0 mt-4 flex flex-wrap gap-2 text-sm"
          data-testid="rollout-totals"
        >
          <StatusBadge
            tone="neutral"
            :label="`${totals.succeeded}/${totals.total} ${$t('operation_succeeded')}`"
          />
          <StatusBadge
            v-if="totals.failed"
            tone="danger"
            :label="`${totals.failed} ${$t('failed')}`"
          />
          <StatusBadge
            v-if="totals.unknown"
            tone="danger"
            :label="`${totals.unknown} ${$t('unknown')}`"
          />
        </p>
      </section>

      <div
        v-if="rollout.state === 'paused'"
        class="mb-6 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-300 bg-amber-50 p-4 text-amber-900"
        role="alert"
      >
        <div class="grid min-w-0 gap-1 text-sm">
          <strong>{{ $t("rolloutState_paused") }}</strong>
          <span>{{ $t("pausedNotice") }}</span>
          <small v-if="rollout.pauseCode"
            >{{ $t("pauseCode") }}: <code>{{ rollout.pauseCode }}</code></small
          >
        </div>
        <Button type="button" :disabled="resuming" @click="resume">
          {{ $t("resumeRollout") }}
        </Button>
      </div>
      <p
        v-if="resumeError"
        class="text-destructive m-0 mb-4 text-sm"
        role="alert"
      >
        {{ resumeError }}
      </p>

      <SectionCard
        v-for="batch in batches"
        :key="batch.batch"
        :title="
          batch.canary ? $t('canaryBatch') : $t('batchN', { n: batch.batch })
        "
      >
        <template #actions>
          <StatusBadge
            tone="neutral"
            :label="`${batch.succeeded}/${batch.total} ${$t('operation_succeeded')}`"
          />
          <StatusBadge
            v-if="batch.failed"
            tone="danger"
            :label="`${batch.failed} ${$t('failed')}`"
          />
          <StatusBadge
            v-if="batch.unknown"
            tone="danger"
            :label="`${batch.unknown} ${$t('unknown')}`"
          />
        </template>
        <ul class="m-0 grid list-none gap-0 p-0">
          <li
            v-for="node in batch.nodes"
            :key="node.nodeId"
            class="border-border flex flex-wrap items-center gap-x-3 gap-y-1 border-b py-2 text-sm last:border-b-0"
          >
            <RouterLink
              :to="{ name: 'node-detail', params: { nodeId: node.nodeId } }"
              class="text-primary font-mono underline-offset-4 hover:underline"
              :title="node.nodeId"
              >{{ node.nodeId.slice(0, 8) }}</RouterLink
            >
            <StatusBadge
              :tone="rolloutNodeTone(node.state)"
              :label="$t(`rolloutNode_${node.state}`)"
            />
            <code
              v-if="node.operationId"
              class="text-muted-foreground text-xs"
              :title="node.operationId"
              >{{ node.operationId.slice(0, 8) }}</code
            >
            <code
              v-if="node.failureCode"
              class="text-destructive text-xs break-all"
              >{{ node.failureCode }}</code
            >
          </li>
        </ul>
      </SectionCard>

      <SectionCard
        v-if="rollout.excluded && rollout.excluded.length"
        :title="$t('excludedNodes')"
      >
        <ul class="m-0 grid list-none gap-2 p-0 text-sm">
          <li
            v-for="(exclusion, index) in exclusions"
            :key="index"
            class="flex flex-wrap items-center gap-3"
          >
            <template v-if="exclusion.nodeId">
              <code :title="exclusion.nodeId">{{
                exclusion.nodeId.slice(0, 8)
              }}</code>
              <span class="text-muted-foreground">{{
                $t(`exclusion_${exclusion.reason}`)
              }}</span>
            </template>
            <code v-else class="break-all">{{ exclusion.raw }}</code>
          </li>
        </ul>
      </SectionCard>
    </template>
  </main>
</template>
