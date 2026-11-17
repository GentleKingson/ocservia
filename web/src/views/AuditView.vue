<script setup lang="ts">
import { ChevronRight, RefreshCw } from "@lucide/vue";
import {
  ResponseError,
  type AuditEventPage,
  type AuditVerification,
} from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { listAuditEvents, verifyAuditChain } from "../api/events";
import { getWorkspace, workspaceContext, type WorkspaceContext } from "../api/workspace";
import { createReadRequest } from "../shared/read-request";
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
const verifying = ref(false);
const verification = ref<AuditVerification>();
const verifyError = ref("");
const reads = createReadRequest();
const verificationReads = createReadRequest();

async function refresh(): Promise<void> {
  const request = reads.begin();
  let context: WorkspaceContext | undefined;
  loading.value = true;
  error.value = "";
  try {
    await getWorkspace();
    if (!request.current()) return;
    context = workspaceContext();
    const page = await listAuditEvents(request.signal);
    if (!request.current(context)) return;
    items.value = page.items;
    initialized.value = true;
  } catch (cause) {
    if (!request.current(context)) return;
    error.value = t(
      cause instanceof ResponseError && cause.response.status === 403
        ? "auditForbidden"
        : "auditUnavailable",
    );
  } finally {
    if (request.finish()) loading.value = false;
  }
}

async function verifyChain(): Promise<void> {
  // Verification is a read expressed as POST. Cancellation never retries it.
  const request = verificationReads.begin();
  let context: WorkspaceContext | undefined;
  verifying.value = true;
  verifyError.value = "";
  verification.value = undefined;
  try {
    await getWorkspace();
    if (!request.current()) return;
    context = workspaceContext();
    const result = await verifyAuditChain(request.signal);
    if (!request.current(context)) return;
    verification.value = result;
  } catch (cause) {
    if (!request.current(context)) return;
    verifyError.value = t(
      cause instanceof ResponseError && cause.response.status === 403
        ? "auditForbidden"
        : "auditVerifyUnavailable",
    );
  } finally {
    if (request.finish()) verifying.value = false;
  }
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
  const seen = new Set<string>();
  return items.value
    .map((item, index) => {
      // Server ids are primary keys, so expansion follows the event when newer
      // records shift positions. Historical records may omit fields: a
      // missing or repeated id keeps the row visible under a positional key.
      const id = typeof item.id === "string" ? item.id : "";
      const key = id && !seen.has(id) ? `id:${id}` : `row:${String(index)}`;
      seen.add(id);
      return { item, key };
    })
    .filter(
      ({ item }) =>
        (!resultFilter.value || item.result === resultFilter.value) &&
        (!query ||
          [
            item.action,
            item.actorId,
            item.resourceType,
            item.resourceId,
            item.requestId,
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
  const result: unknown = item.result;
  return typeof result === "string" && resultOptions.includes(result)
    ? t(`auditResult_${result}`)
    : text(result);
}
const detailFields = [
  ["auditActorType", "actorType"],
  ["auditTrace", "traceId"],
  ["auditCommand", "commandId"],
  ["reason", "reason"],
  ["auditErrorType", "errorType"],
  ["auditEventHash", "eventHash"],
  ["auditPreviousHash", "previousEventHash"],
] as const;
onMounted(() => {
  void refresh();
});
onBeforeUnmount(() => {
  reads.cancel();
  verificationReads.cancel();
});
</script>

<template>
  <main>
    <PageHeader :eyebrow="$t('workspace')" :title="$t('audit')">
      <template #actions>
        <Button
          type="button"
          variant="outline"
          size="sm"
          :disabled="verifying || loading"
          @click="verifyChain"
        >
          {{ $t(verifying ? "auditVerifying" : "auditVerify") }}
        </Button>
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
      v-if="verifyError"
      class="border-destructive/40 text-destructive mb-4 flex flex-wrap items-center gap-3 rounded-md border px-4 py-3 text-sm"
      role="alert"
    >
      {{ verifyError }}
    </div>
    <div
      v-else-if="verification"
      data-testid="audit-verification"
      class="border-border mb-4 flex flex-wrap items-center gap-3 rounded-md border px-4 py-3 text-sm"
      role="status"
    >
      <StatusBadge
        :tone="
          verification.valid && verification.checkpointValid
            ? 'success'
            : 'danger'
        "
        :label="
          $t(
            verification.valid && verification.checkpointValid
              ? 'auditVerifyValid'
              : 'auditVerifyFailed',
          )
        "
      />
      <span class="text-muted-foreground">
        {{ $t("auditVerifySummary", { count: verification.events }) }}
      </span>
    </div>
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
                    formatTimestamp(text(item.occurredAt))
                  }}</TableCell>
                  <TableCell>{{ text(item.action) }}</TableCell>
                  <TableCell class="whitespace-normal">
                    <code class="text-xs break-all">{{
                      text(item.actorId)
                    }}</code>
                  </TableCell>
                  <TableCell class="whitespace-normal">
                    {{ text(item.resourceType) }}<br /><code
                      class="text-xs break-all"
                      >{{ text(item.resourceId) }}</code
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
                      text(item.requestId)
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
                      <template v-if="typeof item.nodeId === 'string'">
                        <dt class="text-muted-foreground">
                          {{ $t("auditNode") }}
                        </dt>
                        <dd class="m-0 mb-2 break-all sm:mb-0">
                          <RouterLink
                            :to="{
                              name: 'node-detail',
                              params: { nodeId: item.nodeId },
                            }"
                            class="text-primary font-mono underline-offset-4 hover:underline"
                            >{{ item.nodeId }}</RouterLink
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
