<script setup lang="ts">
import { RefreshCw } from "@lucide/vue";
import type { AgentRollout, Operation } from "@/api/models";
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from "vue";
import { useI18n } from "vue-i18n";

import { getOperation, listOperations } from "../api/operations";
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
import { createReadRequest } from "../shared/read-request";
import { formatTimestamp } from "../shared/timestamp";

const operations = shallowRef<Operation[]>([]);
const { t } = useI18n();
const rollouts = shallowRef<AgentRollout[]>([]);
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
const operationReads = createReadRequest();
const detailReads = createReadRequest();
const rolloutReads = createReadRequest();

async function loadOperations(reset = true): Promise<void> {
  const request = operationReads.begin();
  if (reset) {
    operations.value = [];
    nextCursor.value = undefined;
    hasMore.value = false;
  }
  loading.value = true;
  error.value = "";
  try {
    const page = await listOperations(
      reset ? undefined : nextCursor.value,
      request.signal,
    );
    if (!request.current()) return;
    operations.value = reset
      ? page.items
      : [...operations.value, ...page.items];
    hasMore.value = page.page.has_more && Boolean(page.page.next_cursor);
    nextCursor.value = page.page.next_cursor;
    unavailable.value = false;
  } catch (cause) {
    if (!request.current()) return;
    unavailable.value = true;
    error.value =
      (cause instanceof Error && cause.message) || t("operationsUnavailable");
  } finally {
    if (request.finish()) {
      loading.value = false;
    }
  }
}

async function inspectOperation(operationId: string): Promise<void> {
  const request = detailReads.begin();
  detailLoading.value = true;
  detailState.value = { error: "" };
  try {
    const operation = await getOperation(operationId, request.signal);
    if (!request.current()) return;
    detailState.value = { selected: operation, error: "" };
  } catch {
    if (!request.current()) return;
    detailState.value = { error: t("operationDetailsUnavailable") };
  } finally {
    if (request.finish()) {
      detailLoading.value = false;
    }
  }
}

async function loadRollouts(): Promise<void> {
  const request = rolloutReads.begin();
  try {
    const page = await listAgentRollouts(10, request.signal);
    if (!request.current()) return;
    rollouts.value = page.rollouts;
    rolloutsUnavailable.value = false;
  } catch {
    if (!request.current()) return;
    rollouts.value = [];
    rolloutsUnavailable.value = true;
  } finally {
    request.finish();
  }
}

function refresh(): void {
  void loadOperations();
  void loadRollouts();
}

onMounted(() => {
  refresh();
});
onBeforeUnmount(() => {
  operationReads.cancel();
  detailReads.cancel();
  detailLoading.value = false;
  rolloutReads.cancel();
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
                >{{ item.target_version }}</RouterLink
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
              formatTimestamp(item.created_at)
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
                  v-if="operation.agent_upgrade_target_version"
                  class="text-xs"
                  >{{ operation.agent_upgrade_target_version }}</code
                >
              </span>
            </TableCell>
            <TableCell>
              <RouterLink
                v-if="operation.node_id"
                :to="{
                  name: 'node-detail',
                  params: { nodeId: operation.node_id },
                }"
                class="text-primary font-mono text-xs underline-offset-4 hover:underline"
                :title="operation.node_id"
                >{{ operation.node_id.slice(0, 8) }}</RouterLink
              ><span v-else class="text-muted-foreground">{{
                $t("notAvailable")
              }}</span>
            </TableCell>
            <TableCell class="text-muted-foreground">{{
              formatTimestamp(operation.created_at)
            }}</TableCell>
            <TableCell class="text-muted-foreground">{{
              formatTimestamp(operation.updated_at)
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
        <template v-if="selectedOperation.agent_upgrade_target_version">
          <dt class="text-muted-foreground">
            {{ $t("targetVersion") }}
          </dt>
          <dd class="m-0 break-all">
            <code>{{ selectedOperation.agent_upgrade_target_version }}</code>
          </dd>
        </template>
        <template v-if="selectedOperation.config_apply_failure_code">
          <dt class="text-muted-foreground">
            {{ $t("failureCode") }}
          </dt>
          <dd class="text-destructive m-0 break-all">
            <code>{{ selectedOperation.config_apply_failure_code }}</code>
          </dd>
        </template>
        <dt class="text-muted-foreground">
          {{ $t("operationNode") }}
        </dt>
        <dd class="m-0 break-all">
          <RouterLink
            v-if="selectedOperation.node_id"
            :to="{
              name: 'node-detail',
              params: { nodeId: selectedOperation.node_id },
            }"
            class="text-primary font-mono underline-offset-4 hover:underline"
            >{{ selectedOperation.node_id }}</RouterLink
          >
          <span v-else>{{ $t("notAvailable") }}</span>
        </dd>
        <dt class="text-muted-foreground">{{ $t("created") }}</dt>
        <dd class="m-0">
          {{ formatTimestamp(selectedOperation.created_at) }}
        </dd>
        <dt class="text-muted-foreground">{{ $t("updated") }}</dt>
        <dd class="m-0">
          {{ formatTimestamp(selectedOperation.updated_at) }}
        </dd>
      </dl>
    </SectionCard>
  </main>
</template>
