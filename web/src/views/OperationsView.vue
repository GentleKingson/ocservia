<script setup lang="ts">
import { RefreshCw } from "@lucide/vue";
import type { AgentRollout, Operation } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";

import { getOperation, listOperations } from "../api/operations";
import {
  getWorkspace,
  workspaceChangedEvent,
  workspaceContext,
} from "../api/workspace";
import { listAgentRollouts } from "../api/agents";
import { operationStatusKey } from "../shared/operation-status";
import { operationTone, rolloutTone } from "../features/operations/state-tone";
import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatTimestamp } from "../shared/timestamp";

const operations = ref<Operation[]>([]);
const { t } = useI18n();
const rollouts = ref<AgentRollout[]>([]);
const rolloutsUnavailable = ref(false);
const detailState = ref<{ selected?: Operation; error: string }>({ error: "" });
const selectedOperation = computed(() => detailState.value.selected);
const detailError = computed(() => detailState.value.error);
const loading = ref(false);
const detailLoading = ref(false);
const unavailable = ref(false);
const error = ref("");
const hasMore = ref(false);
const nextCursor = ref<string>();
let requestController: AbortController | undefined;
let detailController: AbortController | undefined;
let rolloutController: AbortController | undefined;
let requestSequence = 0;
let detailSequence = 0;
let rolloutSequence = 0;

function invalidateDetail(): void {
  detailController?.abort();
  detailController = undefined;
  detailSequence += 1;
  detailLoading.value = false;
}

function invalidateRollouts(): void {
  rolloutController?.abort();
  rolloutController = undefined;
  rolloutSequence += 1;
}

function cancelRequests(): void {
  requestController?.abort();
  requestController = undefined;
  requestSequence += 1;
  invalidateDetail();
  invalidateRollouts();
}

async function loadOperations(reset = true): Promise<void> {
  requestController?.abort();
  const controller = new AbortController();
  requestController = controller;
  const sequence = ++requestSequence;
  if (reset) {
    operations.value = [];
    nextCursor.value = undefined;
    hasMore.value = false;
  }
  loading.value = true;
  error.value = "";
  try {
    await getWorkspace();
    const context = workspaceContext();
    const page = await listOperations(
      reset ? undefined : nextCursor.value,
      controller.signal,
    );
    const current = workspaceContext();
    if (
      controller.signal.aborted ||
      sequence !== requestSequence ||
      current.id !== context.id ||
      current.generation !== context.generation
    )
      return;
    operations.value = reset
      ? page.items
      : [...operations.value, ...page.items];
    hasMore.value = page.page.hasMore && Boolean(page.page.nextCursor);
    nextCursor.value = page.page.nextCursor;
    unavailable.value = false;
  } catch (cause) {
    if (controller.signal.aborted || sequence !== requestSequence) return;
    unavailable.value = true;
    error.value =
      cause instanceof Error ? cause.message : t("operationsUnavailable");
  } finally {
    if (requestController === controller) {
      requestController = undefined;
      loading.value = false;
    }
  }
}

async function inspectOperation(operationId: string): Promise<void> {
  detailController?.abort();
  const controller = new AbortController();
  detailController = controller;
  const sequence = ++detailSequence;
  detailLoading.value = true;
  detailState.value = { error: "" };
  try {
    await getWorkspace();
    const context = workspaceContext();
    const operation = await getOperation(operationId, controller.signal);
    const current = workspaceContext();
    if (
      controller.signal.aborted ||
      sequence !== detailSequence ||
      current.id !== context.id ||
      current.generation !== context.generation
    )
      return;
    detailState.value = { selected: operation, error: "" };
  } catch {
    if (controller.signal.aborted || sequence !== detailSequence) return;
    detailState.value = { error: t("operationDetailsUnavailable") };
  } finally {
    if (detailController === controller) {
      detailController = undefined;
      detailLoading.value = false;
    }
  }
}

async function loadRollouts(): Promise<void> {
  invalidateRollouts();
  const controller = new AbortController();
  rolloutController = controller;
  const sequence = rolloutSequence;
  try {
    await getWorkspace();
    if (sequence !== rolloutSequence) return;
    const context = workspaceContext();
    const page = await listAgentRollouts(10, controller.signal);
    const current = workspaceContext();
    if (
      sequence !== rolloutSequence ||
      current.id !== context.id ||
      current.generation !== context.generation
    )
      return;
    rollouts.value = page.rollouts;
    rolloutsUnavailable.value = false;
  } catch {
    if (controller.signal.aborted || sequence !== rolloutSequence) return;
    rollouts.value = [];
    rolloutsUnavailable.value = true;
  } finally {
    if (rolloutController === controller) rolloutController = undefined;
  }
}

function refresh(): void {
  void loadOperations();
  void loadRollouts();
}

// Rows and details from the previous workspace disappear before new reads.
function refreshForWorkspace(): void {
  invalidateDetail();
  detailState.value = { error: "" };
  rollouts.value = [];
  rolloutsUnavailable.value = false;
  refresh();
}

onMounted(() => {
  window.addEventListener(workspaceChangedEvent, refreshForWorkspace);
  refresh();
});
onBeforeUnmount(() => {
  window.removeEventListener(workspaceChangedEvent, refreshForWorkspace);
  cancelRequests();
});
</script>

<template>
  <main>
    <PageHeader :eyebrow="$t('workspace')" :title="$t('operations')">
      <template #actions>
        <Button
          type="button"
          variant="outline"
          size="icon"
          :disabled="loading"
          :title="$t('refresh')"
          :aria-label="$t('refresh')"
          @click="refresh()"
        >
          <RefreshCw aria-hidden="true" />
        </Button>
      </template>
    </PageHeader>
    <p v-if="error" class="text-destructive m-0 mb-4 text-sm" role="alert">
      {{ error }}
    </p>
    <SectionCard
      v-if="rollouts.length || !rolloutsUnavailable"
      :title="$t('rollouts')"
    >
      <Table v-if="rollouts.length" class="min-w-[36rem]">
        <TableHeader>
          <TableRow class="hover:bg-transparent">
            <TableHead class="text-muted-foreground">{{
              $t("targetVersion")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("state")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("reason")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("created")
            }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow v-for="item in rollouts" :key="item.id">
            <TableCell>
              <RouterLink
                :to="{ name: 'rollout-detail', params: { rolloutId: item.id } }"
                class="text-primary font-mono underline-offset-4 hover:underline"
                >{{ item.targetVersion }}</RouterLink
              >
            </TableCell>
            <TableCell>
              <StatusBadge
                :tone="rolloutTone(item.state)"
                :label="$t(`rolloutState_${item.state}`)"
              />
            </TableCell>
            <TableCell class="max-w-64 truncate" :title="item.reason">{{
              item.reason
            }}</TableCell>
            <TableCell class="text-muted-foreground">{{
              formatTimestamp(item.createdAt)
            }}</TableCell>
          </TableRow>
        </TableBody>
      </Table>
      <p v-else class="text-muted-foreground m-0 text-sm">
        {{ $t("noRollouts") }}
      </p>
    </SectionCard>
    <DataState
      v-if="loading && operations.length === 0"
      kind="loading"
      :message="$t('loading')"
    />
    <DataState
      v-else-if="unavailable && operations.length === 0"
      kind="error"
      :message="$t('systemsUnavailable')"
    />
    <SectionCard v-else :title="$t('operations')">
      <p
        v-if="operations.length === 0"
        class="text-muted-foreground m-0 text-sm"
      >
        {{ $t("noOperations") }}
      </p>
      <Table v-else class="min-w-[44rem]">
        <TableHeader>
          <TableRow class="hover:bg-transparent">
            <TableHead class="text-muted-foreground">{{
              $t("operationId")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("state")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("operationNode")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("created")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("updated")
            }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow
            v-for="operation in operations"
            :key="operation.id"
            :data-state="
              selectedOperation?.id === operation.id ? 'selected' : undefined
            "
          >
            <TableCell>
              <Button
                type="button"
                variant="link"
                class="h-auto p-0 font-mono text-xs"
                :aria-pressed="selectedOperation?.id === operation.id"
                @click="inspectOperation(operation.id)"
              >
                {{ operation.id }}
              </Button>
            </TableCell>
            <TableCell>
              <span class="flex flex-wrap items-center gap-2">
                <StatusBadge
                  :tone="operationTone(operation)"
                  :label="$t(operationStatusKey(operation))"
                />
                <code
                  v-if="operation.agentUpgradeTargetVersion"
                  class="text-xs"
                  >{{ operation.agentUpgradeTargetVersion }}</code
                >
              </span>
            </TableCell>
            <TableCell>
              <RouterLink
                v-if="operation.nodeId"
                :to="{
                  name: 'node-detail',
                  params: { nodeId: operation.nodeId },
                }"
                class="text-primary font-mono text-xs underline-offset-4 hover:underline"
                :title="operation.nodeId"
                >{{ operation.nodeId.slice(0, 8) }}</RouterLink
              ><span v-else class="text-muted-foreground">{{
                $t("notAvailable")
              }}</span>
            </TableCell>
            <TableCell class="text-muted-foreground">{{
              formatTimestamp(operation.createdAt)
            }}</TableCell>
            <TableCell class="text-muted-foreground">{{
              formatTimestamp(operation.updatedAt)
            }}</TableCell>
          </TableRow>
        </TableBody>
      </Table>
      <Button
        v-if="hasMore"
        type="button"
        variant="outline"
        class="justify-self-center"
        :disabled="loading"
        @click="loadOperations(false)"
      >
        {{ $t("loadMore") }}
      </Button>
    </SectionCard>
    <p
      v-if="detailError"
      class="text-destructive m-0 mb-4 text-sm"
      role="alert"
    >
      {{ detailError }}
    </p>
    <SectionCard
      v-if="selectedOperation"
      :title="$t('operationDetails')"
      data-testid="operation-detail"
    >
      <template #actions>
        <Button
          type="button"
          variant="outline"
          size="icon-sm"
          :disabled="detailLoading"
          :title="$t('refresh')"
          :aria-label="$t('refresh')"
          @click="inspectOperation(selectedOperation.id)"
        >
          <RefreshCw aria-hidden="true" />
        </Button>
      </template>
      <dl
        class="m-0 grid gap-x-6 gap-y-1 text-sm sm:grid-cols-[max-content_1fr] sm:gap-y-3"
      >
        <dt class="text-muted-foreground">
          {{ $t("operationId") }}
        </dt>
        <dd class="m-0 break-all">
          <code>{{ selectedOperation.id }}</code>
        </dd>
        <dt class="text-muted-foreground">{{ $t("state") }}</dt>
        <dd class="m-0">
          <StatusBadge
            :tone="operationTone(selectedOperation)"
            :label="$t(operationStatusKey(selectedOperation))"
          />
        </dd>
        <template v-if="selectedOperation.agentUpgradeTargetVersion">
          <dt class="text-muted-foreground">
            {{ $t("targetVersion") }}
          </dt>
          <dd class="m-0">
            <code>{{ selectedOperation.agentUpgradeTargetVersion }}</code>
          </dd>
        </template>
        <template v-if="selectedOperation.configApplyFailureCode">
          <dt class="text-muted-foreground">
            {{ $t("failureCode") }}
          </dt>
          <dd class="text-destructive m-0 break-all">
            <code>{{ selectedOperation.configApplyFailureCode }}</code>
          </dd>
        </template>
        <dt class="text-muted-foreground">
          {{ $t("operationNode") }}
        </dt>
        <dd class="m-0 break-all">
          <RouterLink
            v-if="selectedOperation.nodeId"
            :to="{
              name: 'node-detail',
              params: { nodeId: selectedOperation.nodeId },
            }"
            class="text-primary font-mono underline-offset-4 hover:underline"
            >{{ selectedOperation.nodeId }}</RouterLink
          >
          <span v-else>{{ $t("notAvailable") }}</span>
        </dd>
        <dt class="text-muted-foreground">{{ $t("created") }}</dt>
        <dd class="m-0">
          {{ formatTimestamp(selectedOperation.createdAt) }}
        </dd>
        <dt class="text-muted-foreground">{{ $t("updated") }}</dt>
        <dd class="m-0">
          {{ formatTimestamp(selectedOperation.updatedAt) }}
        </dd>
      </dl>
    </SectionCard>
  </main>
</template>
