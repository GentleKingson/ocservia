<script setup lang="ts">
import { ArrowLeft } from "@lucide/vue";
import {
  ResponseError,
  type AgentRollout,
  type AgentRolloutNode,
} from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { useRoute } from "vue-router";

import { getAgentRollout, resumeAgentRollout } from "../api/agents";
import {
  getWorkspace,
  workspaceChangedEvent,
  workspaceContext,
  type WorkspaceContext,
} from "../api/workspace";
import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import { Button } from "@/components/ui/button";
import {
  rolloutNodeTone,
  rolloutTone,
} from "../features/operations/state-tone";

import { createForegroundRefresh } from "../shared/foreground-refresh";

const route = useRoute();
const { t } = useI18n();

const rollout = ref<AgentRollout | undefined>(undefined);
const loading = ref(true);
const unavailable = ref(false);
const notFound = ref(false);
const resuming = ref(false);
const resumeError = ref("");
// Set after an unconfirmed resume until a fresh read shows the server state.
const resumeBlocked = ref(false);
let controller: AbortController | undefined;
let sequence = 0;
let loadedContext: WorkspaceContext | undefined;

const activeStates = new Set(["queued", "running", "paused"]);
const foregroundRefresh = createForegroundRefresh(async () => {
  if (rollout.value === undefined || activeStates.has(rollout.value.state)) {
    await refresh();
  }
}, 2000);

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

// Every read and resume belongs to one route rollout in one workspace
// generation; a newer ticket makes all older results inert.
function invalidate(): void {
  sequence += 1;
  controller?.abort();
  controller = undefined;
}

function current(context: WorkspaceContext, ticket: number): boolean {
  const workspace = workspaceContext();
  return (
    sequence === ticket &&
    workspace.id === context.id &&
    workspace.generation === context.generation
  );
}

// A different rollout or workspace never shows the previous resource or its
// resume action, and the terminal-state cache starts empty.
function reset(): void {
  invalidate();
  rollout.value = undefined;
  loadedContext = undefined;
  loading.value = true;
  unavailable.value = false;
  notFound.value = false;
  resuming.value = false;
  resumeError.value = "";
  resumeBlocked.value = false;
}

// Reads directly so the new resource never waits behind an older read.
function reload(): void {
  reset();
  void refresh();
}

async function refresh(): Promise<void> {
  // A read started before the resume response could restore the old state.
  if (resuming.value) return;
  const rolloutId = String(route.params.rolloutId ?? "");
  if (!rolloutId) return;
  invalidate();
  const ticket = sequence;
  const request = new AbortController();
  controller = request;
  try {
    await getWorkspace();
    if (ticket !== sequence) return;
    const context = workspaceContext();
    const value = await getAgentRollout(rolloutId, request.signal);
    if (!current(context, ticket)) return;
    if (value.id !== rolloutId || value.workspaceId !== context.id)
      throw new Error("rollout outside the requested workspace");
    loadedContext = context;
    rollout.value = value;
    unavailable.value = false;
    notFound.value = false;
    resumeBlocked.value = false;
  } catch (cause) {
    if (ticket !== sequence || request.signal.aborted) return;
    notFound.value =
      cause instanceof ResponseError && cause.response.status === 404;
    unavailable.value = !notFound.value;
  } finally {
    if (ticket === sequence) {
      controller = undefined;
      loading.value = false;
    }
  }
}

async function resume(): Promise<void> {
  const value = rollout.value;
  const context = loadedContext;
  if (!value || !context || resuming.value || resumeBlocked.value) return;
  invalidate();
  const ticket = sequence;
  if (!current(context, ticket)) return;
  resuming.value = true;
  resumeError.value = "";
  try {
    const result = await resumeAgentRollout(value.id);
    if (!current(context, ticket)) return;
    if (result.id !== value.id || result.workspaceId !== context.id)
      throw new Error(t("rolloutResumeUnconfirmed"));
    rollout.value = result;
  } catch (cause) {
    if (!current(context, ticket)) return;
    // A lost POST response is not permission to send the resume again; only
    // a fresh read of the server state re-enables the action.
    resumeBlocked.value = true;
    resumeError.value =
      cause instanceof ResponseError
        ? cause.message
        : t("rolloutResumeUnconfirmed");
    resuming.value = false;
    foregroundRefresh.request();
  } finally {
    // The next scheduled read follows a confirmed resume.
    if (ticket === sequence) resuming.value = false;
  }
}

watch(() => route.params.rolloutId, reload);
onMounted(() => {
  window.addEventListener(workspaceChangedEvent, reload);
  foregroundRefresh.start();
});
onBeforeUnmount(() => {
  window.removeEventListener(workspaceChangedEvent, reload);
  foregroundRefresh.stop();
  invalidate();
});
</script>

<template>
  <main>
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
        <Button
          type="button"
          :disabled="resuming || resumeBlocked"
          @click="resume"
        >
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
