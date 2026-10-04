<script setup lang="ts">
import { ChevronRight, RefreshCw } from "@lucide/vue";
import { ResponseError, type AuditEventPage } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { listAuditEvents } from "../api/events";
import {
  getWorkspace,
  workspaceChangedEvent,
  workspaceContext,
} from "../api/workspace";
import { formatTimestamp } from "../shared/timestamp";
import DataState from "@/components/common/DataState.vue";
import PageHeader from "@/components/common/PageHeader.vue";
import SectionCard from "@/components/common/SectionCard.vue";
import StatusBadge from "@/components/common/StatusBadge.vue";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect } from "@/components/ui/native-select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { auditResultTone } from "@/features/operations/state-tone";

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
  expanded.value = [];
  void refresh();
}
function text(value: unknown): string {
  return typeof value === "string" && value ? value : "—";
}

type AuditItem = AuditEventPage["items"][number];
const resultOptions = ["intent", "succeeded", "failed"];
const search = ref("");
const resultFilter = ref("");
const expanded = ref<string[]>([]);
// Filters narrow only the bounded recent slice already loaded; they never
// page or query the server.
const rows = computed(() => {
  const query = search.value.trim().toLowerCase();
  return items.value
    .map((item, index) => ({ item, key: text(item.id) + index }))
    .filter(
      ({ item }) =>
        (!resultFilter.value || item.result === resultFilter.value) &&
        (!query ||
          [
            item.action,
            item.actor_id,
            item.resource_type,
            item.resource_id,
            item.request_id,
            item.approval_id,
          ].some(
            (value) =>
              typeof value === "string" && value.toLowerCase().includes(query),
          )),
    );
});
function toggle(key: string): void {
  expanded.value = expanded.value.includes(key)
    ? expanded.value.filter((value) => value !== key)
    : [...expanded.value, key];
}
function resultLabel(item: AuditItem): string {
  return resultOptions.includes(item.result)
    ? t(`auditResult_${item.result}`)
    : text(item.result);
}
const detailFields = [
  ["auditActorType", "actor_type"],
  ["auditTrace", "trace_id"],
  ["auditCommand", "command_id"],
  ["reason", "reason"],
  ["auditErrorType", "error_type"],
  ["auditEventHash", "event_hash"],
  ["auditPreviousHash", "previous_event_hash"],
] as const;
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
  <main class="overview">
    <PageHeader :eyebrow="$t('workspace')" :title="$t('audit')">
      <template #actions>
        <Button
          type="button"
          variant="outline"
          size="icon"
          :disabled="loading"
          :title="$t('refresh')"
          :aria-label="$t('refresh')"
          @click="refresh"
        >
          <RefreshCw aria-hidden="true" />
        </Button>
      </template>
    </PageHeader>
    <div
      v-if="error"
      class="border-destructive/40 text-destructive mb-4 flex flex-wrap items-center gap-3 rounded-md border px-4 py-3 text-sm"
      role="alert"
    >
      <span>{{ error }}</span>
      <Button
        type="button"
        variant="outline"
        size="sm"
        :disabled="loading"
        @click="refresh"
      >
        {{ $t("loginRetry") }}
      </Button>
    </div>
    <DataState
      v-if="loading && items.length === 0"
      kind="loading"
      :message="$t('loading')"
    />
    <SectionCard v-else-if="initialized || items.length" :title="$t('audit')">
      <p class="text-muted-foreground m-0 text-sm">
        {{ $t("auditRecentBound") }}
      </p>
      <p
        v-if="items.length === 0"
        v-show="!error"
        class="text-muted-foreground m-0 text-sm"
      >
        {{ $t("auditEmpty") }}
      </p>
      <template v-else>
        <div class="flex flex-wrap items-end gap-3">
          <div class="grid min-w-0 flex-1 basis-60 gap-2">
            <Label for="audit-search">{{ $t("auditSearch") }}</Label>
            <Input
              id="audit-search"
              v-model="search"
              type="search"
              aria-describedby="audit-filter-scope"
            />
          </div>
          <div class="grid gap-2">
            <Label for="audit-result">{{ $t("auditResult") }}</Label>
            <NativeSelect
              id="audit-result"
              v-model="resultFilter"
              aria-describedby="audit-filter-scope"
            >
              <option value="">{{ $t("auditResultAll") }}</option>
              <option
                v-for="value in resultOptions"
                :key="value"
                :value="value"
              >
                {{ $t(`auditResult_${value}`) }}
              </option>
            </NativeSelect>
          </div>
        </div>
        <p id="audit-filter-scope" class="text-muted-foreground m-0 text-xs">
          {{ $t("auditFilterScope") }}
        </p>
        <p
          v-if="rows.length === 0"
          class="text-muted-foreground m-0 text-sm"
          role="status"
        >
          {{ $t("auditNoMatches") }}
        </p>
        <div v-else class="@container min-w-0">
          <Table class="min-w-[52rem]" :aria-label="$t('auditRecentBound')">
            <TableHeader>
              <TableRow class="hover:bg-transparent">
                <TableHead class="w-10"
                  ><span class="sr-only">{{
                    $t("auditDetails")
                  }}</span></TableHead
                >
                <TableHead class="text-muted-foreground">{{
                  $t("auditTime")
                }}</TableHead>
                <TableHead class="text-muted-foreground">{{
                  $t("auditAction")
                }}</TableHead>
                <TableHead class="text-muted-foreground">{{
                  $t("auditActor")
                }}</TableHead>
                <TableHead class="text-muted-foreground">{{
                  $t("auditResource")
                }}</TableHead>
                <TableHead class="text-muted-foreground">{{
                  $t("auditResult")
                }}</TableHead>
                <TableHead class="text-muted-foreground">{{
                  $t("auditRequest")
                }}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              <template v-for="{ item, key } in rows" :key="key">
                <TableRow>
                  <TableCell>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      :aria-expanded="expanded.includes(key)"
                      :aria-controls="`audit-detail-${key}`"
                      :aria-label="`${$t('auditDetails')}: ${text(item.action)}`"
                      @click="toggle(key)"
                    >
                      <ChevronRight
                        aria-hidden="true"
                        class="transition-transform"
                        :class="expanded.includes(key) ? 'rotate-90' : ''"
                      />
                    </Button>
                  </TableCell>
                  <TableCell class="text-muted-foreground">{{
                    formatTimestamp(text(item.occurred_at))
                  }}</TableCell>
                  <TableCell>{{ text(item.action) }}</TableCell>
                  <TableCell class="whitespace-normal">
                    <code class="text-xs break-all">{{
                      text(item.actor_id)
                    }}</code>
                  </TableCell>
                  <TableCell class="whitespace-normal">
                    {{ text(item.resource_type) }}<br /><code
                      class="text-xs break-all"
                      >{{ text(item.resource_id) }}</code
                    >
                  </TableCell>
                  <TableCell>
                    <StatusBadge
                      :tone="auditResultTone(text(item.result))"
                      :label="resultLabel(item)"
                    />
                  </TableCell>
                  <TableCell class="whitespace-normal">
                    <code class="text-xs break-all">{{
                      text(item.request_id)
                    }}</code>
                  </TableCell>
                </TableRow>
                <TableRow
                  v-if="expanded.includes(key)"
                  :id="`audit-detail-${key}`"
                  class="bg-muted/40 hover:bg-muted/40"
                >
                  <TableCell colspan="7" class="whitespace-normal">
                    <!-- Stays within the visible width while the wide table scrolls. -->
                    <dl
                      class="sticky left-2 m-0 grid w-[calc(100cqw-1rem)] gap-x-6 gap-y-1 text-sm sm:grid-cols-[max-content_1fr] sm:gap-y-2"
                    >
                      <template v-if="typeof item.node_id === 'string'">
                        <dt class="text-muted-foreground">
                          {{ $t("auditNode") }}
                        </dt>
                        <dd class="m-0 mb-2 break-all sm:mb-0">
                          <RouterLink
                            :to="{
                              name: 'node-detail',
                              params: { nodeId: item.node_id },
                            }"
                            class="text-primary font-mono underline-offset-4 hover:underline"
                            >{{ item.node_id }}</RouterLink
                          >
                        </dd>
                      </template>
                      <template v-if="typeof item.approval_id === 'string'">
                        <dt class="text-muted-foreground">
                          {{ $t("approvalId") }}
                        </dt>
                        <dd class="m-0 mb-2 break-all sm:mb-0">
                          <RouterLink
                            :to="{
                              name: 'approvals',
                              params: { approvalId: item.approval_id },
                            }"
                            class="text-primary font-mono underline-offset-4 hover:underline"
                            >{{ item.approval_id }}</RouterLink
                          >
                        </dd>
                      </template>
                      <template
                        v-for="[label, field] in detailFields"
                        :key="field"
                      >
                        <dt class="text-muted-foreground">{{ $t(label) }}</dt>
                        <dd class="m-0 mb-2 break-all sm:mb-0">
                          <code>{{ text(item[field]) }}</code>
                        </dd>
                      </template>
                    </dl>
                  </TableCell>
                </TableRow>
              </template>
            </TableBody>
          </Table>
        </div>
      </template>
    </SectionCard>
  </main>
</template>
