<script setup lang="ts">
import { RefreshCw, Server } from "@lucide/vue";
import type { Operation } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";

import {
  getOperation,
  getWorkspace,
  listOperations,
  workspaceChangedEvent,
  workspaceContext,
} from "../api/client";
import {
  completeOperationDetail,
  failOperationDetail,
  startOperationDetail,
  type OperationDetailState,
} from "../shared/operation-detail";

const operations = ref<Operation[]>([]);
const { t } = useI18n();
const detailState = ref<OperationDetailState<Operation>>(
  startOperationDetail(),
);
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
let requestSequence = 0;
let detailSequence = 0;

function operationLabel(state: string): string {
  return "operation_" + state;
}

function dateLabel(value: Date): string {
  return value.toLocaleString();
}

function cancelRequests(): void {
  requestController?.abort();
  detailController?.abort();
  requestController = undefined;
  detailController = undefined;
  requestSequence += 1;
  detailSequence += 1;
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
  detailState.value = startOperationDetail();
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
    detailState.value = completeOperationDetail(operation);
  } catch {
    if (controller.signal.aborted || sequence !== detailSequence) return;
    detailState.value = failOperationDetail(t("operationDetailsUnavailable"));
  } finally {
    if (detailController === controller) {
      detailController = undefined;
      detailLoading.value = false;
    }
  }
}

function refreshForWorkspace(): void {
  detailState.value = startOperationDetail();
  void loadOperations();
}

onMounted(() => {
  window.addEventListener(workspaceChangedEvent, refreshForWorkspace);
  void loadOperations();
});
onBeforeUnmount(() => {
  window.removeEventListener(workspaceChangedEvent, refreshForWorkspace);
  cancelRequests();
});
</script>

<template>
  <main class="overview operations-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("operations") }}</h1>
      </div>
      <button
        type="button"
        class="icon-command page-command"
        :disabled="loading"
        :title="$t('refresh')"
        :aria-label="$t('refresh')"
        @click="loadOperations()"
      >
        <RefreshCw :size="16" />
      </button>
    </div>
    <p v-if="error" class="operation-error page-error" role="alert">
      {{ error }}
    </p>
    <div v-if="loading && operations.length === 0" class="detail-state">
      <Server :size="24" /><span>{{ $t("loading") }}</span>
    </div>
    <div
      v-else-if="unavailable && operations.length === 0"
      class="detail-state"
    >
      <Server :size="24" /><span>{{ $t("systemsUnavailable") }}</span>
    </div>
    <section v-else class="operations-panel">
      <div v-if="operations.length === 0" class="empty-state">
        <Server :size="24" /><span>{{ $t("noOperations") }}</span>
      </div>
      <div v-else class="operations-table-wrap">
        <table class="operations-table">
          <thead>
            <tr>
              <th>{{ $t("operationId") }}</th>
              <th>{{ $t("state") }}</th>
              <th>{{ $t("operationNode") }}</th>
              <th>{{ $t("created") }}</th>
              <th>{{ $t("updated") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="operation in operations" :key="operation.id">
              <td>
                <button
                  type="button"
                  class="table-link"
                  @click="inspectOperation(operation.id)"
                >
                  {{ operation.id }}
                </button>
              </td>
              <td>
                <strong :class="operation.state">{{
                  $t(operationLabel(operation.state))
                }}</strong>
              </td>
              <td>
                <code v-if="operation.nodeId">{{
                  operation.nodeId.slice(0, 8)
                }}</code
                ><span v-else>{{ $t("notAvailable") }}</span>
              </td>
              <td>
                <time :datetime="operation.createdAt.toISOString()">{{
                  dateLabel(operation.createdAt)
                }}</time>
              </td>
              <td>
                <time :datetime="operation.updatedAt.toISOString()">{{
                  dateLabel(operation.updatedAt)
                }}</time>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <button
        v-if="hasMore"
        type="button"
        class="load-more"
        :disabled="loading"
        @click="loadOperations(false)"
      >
        {{ $t("loadMore") }}
      </button>
    </section>
    <p v-if="detailError" class="operation-error page-error" role="alert">
      {{ detailError }}
    </p>
    <section v-if="selectedOperation" class="operation-detail">
      <header>
        <h2>{{ $t("operationDetails") }}</h2>
        <button
          type="button"
          class="icon-command"
          :disabled="detailLoading"
          :title="$t('refresh')"
          :aria-label="$t('refresh')"
          @click="inspectOperation(selectedOperation.id)"
        >
          <RefreshCw :size="15" />
        </button>
      </header>
      <dl>
        <div>
          <dt>{{ $t("operationId") }}</dt>
          <dd>
            <code>{{ selectedOperation.id }}</code>
          </dd>
        </div>
        <div>
          <dt>{{ $t("state") }}</dt>
          <dd>{{ $t(operationLabel(selectedOperation.state)) }}</dd>
        </div>
        <div>
          <dt>{{ $t("operationNode") }}</dt>
          <dd>{{ selectedOperation.nodeId ?? $t("notAvailable") }}</dd>
        </div>
        <div>
          <dt>{{ $t("created") }}</dt>
          <dd>{{ dateLabel(selectedOperation.createdAt) }}</dd>
        </div>
        <div>
          <dt>{{ $t("updated") }}</dt>
          <dd>{{ dateLabel(selectedOperation.updatedAt) }}</dd>
        </div>
      </dl>
    </section>
  </main>
</template>
