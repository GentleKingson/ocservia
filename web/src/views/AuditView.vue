<script setup lang="ts">
import { ResponseError, type AuditEventPage } from "@ocservia/api-client";
import { onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { listAuditEvents } from "../api/events";
import {
  getWorkspace,
  workspaceChangedEvent,
  workspaceContext,
} from "../api/workspace";
import { formatTimestamp } from "../shared/timestamp";

const { t } = useI18n();
const items = ref<AuditEventPage["items"]>([]);
const loading = ref(false);
const initialized = ref(false);
const error = ref("");
let controller: AbortController | undefined;
let sequence = 0;

async function refresh(): Promise<void> {
  controller?.abort();
  controller = new AbortController();
  const signal = controller.signal;
  const ticket = ++sequence;
  loading.value = true;
  error.value = "";
  try {
    await getWorkspace();
    if (ticket !== sequence) return;
    const context = workspaceContext();
    const page = await listAuditEvents(signal);
    const current = workspaceContext();
    if (
      signal.aborted ||
      ticket !== sequence ||
      context.id !== current.id ||
      context.generation !== current.generation
    )
      return;
    items.value = page.items;
    initialized.value = true;
  } catch (cause) {
    if (signal.aborted || ticket !== sequence) return;
    error.value = t(
      cause instanceof ResponseError && cause.response.status === 403
        ? "auditForbidden"
        : "auditUnavailable",
    );
  } finally {
    if (ticket === sequence) loading.value = false;
  }
}

function workspaceChanged(): void {
  items.value = [];
  initialized.value = false;
  void refresh();
}
function text(value: unknown): string {
  return typeof value === "string" && value ? value : "—";
}
onMounted(() => {
  window.addEventListener(workspaceChangedEvent, workspaceChanged);
  void refresh();
});
onBeforeUnmount(() => {
  sequence += 1;
  controller?.abort();
  window.removeEventListener(workspaceChangedEvent, workspaceChanged);
});
</script>

<template>
  <main class="overview audit-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("audit") }}</h1>
      </div>
      <button type="button" :disabled="loading" @click="refresh">
        {{ $t("refresh") }}
      </button>
    </div>
    <p>{{ $t("auditRecentBound") }}</p>
    <p v-if="error" role="alert">
      {{ error }}
      <button type="button" :disabled="loading" @click="refresh">
        {{ $t("loginRetry") }}
      </button>
    </p>
    <p v-if="loading" role="status">{{ $t("loading") }}</p>
    <p
      v-else-if="initialized && !error && items.length === 0"
      class="empty-state"
    >
      {{ $t("auditEmpty") }}
    </p>
    <div v-if="items.length" class="audit-table-wrap">
      <table class="node-table audit-table">
        <caption class="sr-only">
          {{
            $t("auditRecentBound")
          }}
        </caption>
        <thead>
          <tr>
            <th>{{ $t("auditTime") }}</th>
            <th>{{ $t("auditAction") }}</th>
            <th>{{ $t("auditActor") }}</th>
            <th>{{ $t("auditResource") }}</th>
            <th>{{ $t("auditResult") }}</th>
            <th>{{ $t("auditRequest") }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(item, index) in items" :key="text(item.id) + index">
            <td>{{ formatTimestamp(text(item.occurred_at)) }}</td>
            <td>{{ text(item.action) }}</td>
            <td>{{ text(item.actor_id) }}</td>
            <td>
              {{ text(item.resource_type) }}<br /><code>{{
                text(item.resource_id)
              }}</code>
            </td>
            <td>{{ text(item.result) }}</td>
            <td>
              <code>{{ text(item.request_id) }}</code>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </main>
</template>
