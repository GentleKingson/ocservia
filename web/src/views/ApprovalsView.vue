<script setup lang="ts">
import { Check, RefreshCw, Search } from "@lucide/vue";
import {
  ApprovalToJSON,
  ResponseError,
  type Approval,
} from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { useRoute, useRouter } from "vue-router";
import {
  approveRequest,
  getApproval,
  listPendingApprovals,
} from "../api/approvals";
import {
  getWorkspace,
  workspaceChangedEvent,
  workspaceContext,
  type WorkspaceContext,
} from "../api/workspace";
import { formatTimestamp } from "../shared/timestamp";
import DataState from "@/components/common/DataState.vue";
import FormField from "@/components/common/FormField.vue";
import PageHeader from "@/components/common/PageHeader.vue";
import SectionCard from "@/components/common/SectionCard.vue";
import StatusBadge from "@/components/common/StatusBadge.vue";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { approvalTone } from "@/features/operations/state-tone";

const { t } = useI18n();
const route = useRoute();
const router = useRouter();
const lookupId = ref("");
const approval = ref<Approval>();
const reason = ref("");
const reviewed = ref(false);
const loading = ref(false);
const submitting = ref(false);
const error = ref("");
let controller: AbortController | undefined;
let sequence = 0;
let loadedContext: WorkspaceContext | undefined;
let expiryTimer: ReturnType<typeof setTimeout> | undefined;
const expired = ref(false);

const queue = ref<Approval[]>([]);
const queueLoading = ref(false);
const queueInitialized = ref(false);
const queueError = ref("");
const nextCursor = ref("");
let queueController: AbortController | undefined;
let queueSequence = 0;

function invalidateQueue(): void {
  queueSequence++;
  queueController?.abort();
  queueController = undefined;
  queue.value = [];
  nextCursor.value = "";
  queueError.value = "";
  queueLoading.value = false;
  queueInitialized.value = false;
}

async function loadQueue(cursor = ""): Promise<void> {
  const ticket = ++queueSequence;
  queueController?.abort();
  queueController = new AbortController();
  const signal = queueController.signal;
  queueLoading.value = true;
  queueError.value = "";
  const contextMatches = (context: WorkspaceContext): boolean => {
    const workspace = workspaceContext();
    return (
      ticket === queueSequence &&
      workspace.id === context.id &&
      workspace.generation === context.generation
    );
  };
  let context: WorkspaceContext | undefined;
  try {
    await getWorkspace();
    if (ticket !== queueSequence) return;
    context = workspaceContext();
    const page = await listPendingApprovals(cursor || undefined, signal);
    if (!contextMatches(context)) return;
    if (
      page.items.some(
        (item) => item.workspaceId !== context?.id || item.status !== "pending",
      )
    )
      throw new Error("invalid approval page");
    queue.value = page.items;
    nextCursor.value = page.page.hasMore ? (page.page.nextCursor ?? "") : "";
    queueInitialized.value = true;
  } catch (cause) {
    if (
      ticket !== queueSequence ||
      signal.aborted ||
      (context && !contextMatches(context))
    )
      return;
    // Clear stale actionable rows after a permission or refresh failure.
    queue.value = [];
    nextCursor.value = "";
    queueInitialized.value = false;
    queueError.value = t(
      cause instanceof ResponseError && cause.response.status === 403
        ? "approvalForbidden"
        : "approvalQueueUnavailable",
    );
  } finally {
    if (ticket === queueSequence) queueLoading.value = false;
  }
}

function refresh(): void {
  void loadApproval();
  void loadQueue();
}

function refreshWorkspace(): void {
  invalidateQueue();
  lookupId.value = "";
  refresh();
}

function inspectQueued(value: Approval): void {
  if (submitting.value || queueLoading.value) return;
  void router.push({ name: "approvals", params: { approvalId: value.id } });
}

function current(context: WorkspaceContext, ticket: number): boolean {
  const workspace = workspaceContext();
  return (
    sequence === ticket &&
    workspace.id === context.id &&
    workspace.generation === context.generation
  );
}

const summary = computed(() => {
  const value = ApprovalToJSON(approval.value) as
    | {
        config_plan_summary?: object;
        certificate_summary?: object;
        request_summary?: object;
      }
    | undefined;
  const content =
    value?.config_plan_summary ??
    value?.certificate_summary ??
    value?.request_summary;
  return content && Object.keys(content).length
    ? JSON.stringify(content, null, 2)
    : undefined;
});
// A pending request past its expiry is shown as expired; the server decides.
const displayStatus = computed(() =>
  expired.value && approval.value?.status === "pending"
    ? "expired"
    : approval.value?.status,
);
const canApprove = computed(() =>
  Boolean(
    approval.value?.status === "pending" &&
    /^[0-9a-f]{64}$/.test(approval.value.requestHash ?? "") &&
    summary.value &&
    !expired.value &&
    reviewed.value &&
    reason.value.trim() &&
    !loading.value &&
    !submitting.value,
  ),
);

function invalidate(): void {
  sequence += 1;
  controller?.abort();
  clearTimeout(expiryTimer);
  controller = undefined;
  loadedContext = undefined;
  approval.value = undefined;
  reason.value = "";
  reviewed.value = false;
  loading.value = false;
  error.value = "";
  expired.value = false;
}

function display(value: Approval): void {
  approval.value = value;
  clearTimeout(expiryTimer);
  const remaining = Date.parse(value.expiresAt) - Date.now();
  // Unknown/extended expiry is not a basis for enabling a destructive decision.
  expired.value = !Number.isFinite(remaining) || remaining <= 0;
  if (!expired.value)
    expiryTimer = setTimeout(
      () => {
        expired.value = true;
      },
      Math.min(remaining, 2_147_483_647),
    );
}

async function loadApproval(): Promise<void> {
  invalidate();
  const id =
    typeof route.params.approvalId === "string" ? route.params.approvalId : "";
  lookupId.value = id;
  if (!id) return;
  const ticket = sequence;
  controller = new AbortController();
  const signal = controller.signal;
  loading.value = true;
  try {
    await getWorkspace();
    if (ticket !== sequence) return;
    const context = workspaceContext();
    const value = await getApproval(id, signal);
    if (!current(context, ticket)) return;
    if (value.id !== id || value.workspaceId !== context.id)
      throw new Error(t("approvalWorkspaceMismatch"));
    loadedContext = context;
    display(value);
  } catch (cause) {
    if (ticket !== sequence || signal.aborted) return;
    error.value =
      cause instanceof Error ? cause.message : t("approvalUnavailable");
  } finally {
    if (ticket === sequence) loading.value = false;
  }
}

async function inspect(): Promise<void> {
  if (submitting.value || !lookupId.value.trim()) return;
  await router.push({
    name: "approvals",
    params: { approvalId: lookupId.value.trim() },
  });
}

async function approve(): Promise<void> {
  const value = approval.value;
  const context = loadedContext;
  const ticket = sequence;
  if (
    !canApprove.value ||
    !value?.requestHash ||
    !context ||
    !current(context, ticket)
  )
    return;
  submitting.value = true;
  reviewed.value = false;
  error.value = "";
  try {
    // The decision binds exactly the content on screen, never a silent refetch.
    const result = await approveRequest(value.id, {
      expectedRequestHash: value.requestHash,
      reason: reason.value.trim(),
    });
    if (!current(context, ticket)) return;
    if (
      result.id !== value.id ||
      result.workspaceId !== context.id ||
      result.requestHash !== value.requestHash
    )
      throw new Error(t("approvalUnavailable"));
    display(result);
    void loadQueue();
  } catch (cause) {
    if (!current(context, ticket)) return;
    // A lost POST response is not permission to send the decision again.
    approval.value = undefined;
    loadedContext = undefined;
    error.value = t(
      cause instanceof ResponseError && cause.response.status === 403
        ? "approvalForbidden"
        : "approvalDecisionUnconfirmed",
    );
  } finally {
    submitting.value = false;
  }
}

watch(
  () => route.params.approvalId,
  () => {
    void loadApproval();
  },
);
onMounted(() => {
  window.addEventListener(workspaceChangedEvent, refreshWorkspace);
  refresh();
});
onBeforeUnmount(() => {
  window.removeEventListener(workspaceChangedEvent, refreshWorkspace);
  invalidateQueue();
  invalidate();
});
</script>

<template>
  <main>
    <PageHeader :eyebrow="$t('workspace')" :title="$t('approvals')">
      <template #actions>
        <Button
          type="button"
          variant="outline"
          size="icon"
          :disabled="loading || queueLoading || submitting"
          :title="$t('refresh')"
          :aria-label="$t('refresh')"
          @click="refresh"
        >
          <RefreshCw aria-hidden="true" />
        </Button>
      </template>
    </PageHeader>
    <SectionCard :title="$t('pendingApprovals')">
      <p class="text-muted-foreground m-0 text-sm">
        {{ $t("approvalQueueScope") }}
      </p>
      <p
        v-if="queueLoading"
        class="text-muted-foreground m-0 text-sm"
        role="status"
      >
        {{ $t("loading") }}
      </p>
      <p
        v-else-if="queueError"
        class="text-destructive m-0 text-sm"
        role="alert"
      >
        {{ queueError }}
      </p>
      <p
        v-else-if="queueInitialized && !queue.length"
        class="text-muted-foreground m-0 text-sm"
      >
        {{ $t("noPendingApprovals") }}
      </p>
      <Table v-if="queue.length" class="min-w-[48rem]">
        <TableHeader>
          <TableRow class="hover:bg-transparent">
            <TableHead class="text-muted-foreground">{{
              $t("approvalAction")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("approvalResource")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("approvalRequester")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("state")
            }}</TableHead>
            <TableHead class="text-muted-foreground">{{
              $t("expires")
            }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow
            v-for="item in queue"
            :key="item.id"
            :data-state="approval?.id === item.id ? 'selected' : undefined"
          >
            <TableCell>
              <Button
                type="button"
                variant="link"
                class="h-auto p-0"
                :disabled="submitting || queueLoading"
                @click="inspectQueued(item)"
              >
                {{ item.action }}
              </Button>
            </TableCell>
            <TableCell class="whitespace-normal">
              {{ item.resourceType }}
              <code class="text-xs break-all">{{ item.resourceId }}</code>
            </TableCell>
            <TableCell class="whitespace-normal">
              <code class="text-xs break-all">{{ item.requesterId }}</code>
            </TableCell>
            <TableCell>
              <StatusBadge
                :tone="approvalTone(item.status)"
                :label="$t(`approvalStatus_${item.status}`)"
              />
            </TableCell>
            <TableCell class="text-muted-foreground">{{
              formatTimestamp(item.expiresAt)
            }}</TableCell>
          </TableRow>
        </TableBody>
      </Table>
      <Button
        v-if="nextCursor"
        type="button"
        variant="outline"
        class="justify-self-center"
        :disabled="queueLoading || submitting"
        @click="loadQueue(nextCursor)"
      >
        {{ $t("nextPage") }}
      </Button>
    </SectionCard>
    <form class="mb-6 flex max-w-xl items-end gap-2" @submit.prevent="inspect">
      <FormField id="approval-lookup" :label="$t('approvalId')" class="flex-1">
        <Input
          id="approval-lookup"
          v-model="lookupId"
          required
          class="font-mono"
          :disabled="submitting"
        />
      </FormField>
      <Button
        type="submit"
        variant="outline"
        size="icon"
        :disabled="submitting || !lookupId.trim()"
        :title="$t('inspectApproval')"
        :aria-label="$t('inspectApproval')"
      >
        <Search aria-hidden="true" />
      </Button>
    </form>
    <p v-if="error" class="text-destructive m-0 mb-4 text-sm" role="alert">
      {{ error }}
    </p>
    <DataState v-if="loading" kind="loading" :message="$t('loading')" />
    <SectionCard
      v-if="approval && displayStatus"
      :title="$t('approvalDetails')"
    >
      <div class="flex flex-wrap items-start gap-x-3 gap-y-1">
        <StatusBadge
          data-testid="approval-status"
          :tone="approvalTone(displayStatus)"
          :label="$t(`approvalStatus_${displayStatus}`)"
        />
        <p
          class="text-muted-foreground m-0 min-w-0 flex-1 basis-60 text-sm"
          data-testid="approval-status-help"
        >
          {{ $t(`approvalStatusHelp_${displayStatus}`) }}
        </p>
      </div>
      <dl
        class="m-0 grid gap-x-6 gap-y-1 text-sm sm:grid-cols-[max-content_1fr] sm:gap-y-3"
      >
        <dt class="text-muted-foreground">{{ $t("approvalId") }}</dt>
        <dd class="m-0 mb-2 break-all sm:mb-0">
          <code>{{ approval.id }}</code>
        </dd>
        <dt class="text-muted-foreground">{{ $t("approvalAction") }}</dt>
        <dd class="m-0 mb-2 sm:mb-0">{{ approval.action }}</dd>
        <dt class="text-muted-foreground">{{ $t("approvalResource") }}</dt>
        <dd class="m-0 mb-2 break-all sm:mb-0">
          {{ approval.resourceType }} <code>{{ approval.resourceId }}</code>
        </dd>
        <dt class="text-muted-foreground">{{ $t("approvalRequester") }}</dt>
        <dd class="m-0 mb-2 break-all sm:mb-0">
          <code>{{ approval.requesterId }}</code>
        </dd>
        <dt class="text-muted-foreground">{{ $t("approvalApprover") }}</dt>
        <dd class="m-0 mb-2 break-all sm:mb-0">
          <code>{{ approval.approverId ?? "-" }}</code>
        </dd>
        <dt class="text-muted-foreground">{{ $t("reason") }}</dt>
        <dd class="m-0 mb-2 break-words sm:mb-0">{{ approval.reason }}</dd>
        <dt class="text-muted-foreground">{{ $t("expires") }}</dt>
        <dd class="m-0 mb-2 sm:mb-0">
          {{ formatTimestamp(approval.expiresAt) }}
        </dd>
        <dt class="text-muted-foreground">{{ $t("approvalHash") }}</dt>
        <dd class="m-0 break-all">
          <code data-testid="approval-hash">{{
            approval.requestHash ?? "-"
          }}</code>
        </dd>
      </dl>
      <h3 class="m-0 text-sm font-semibold">{{ $t("approvalContent") }}</h3>
      <pre
        class="bg-muted border-border m-0 overflow-x-auto rounded-md border p-4 text-xs break-words whitespace-pre-wrap"
        data-testid="approval-summary"
        >{{ summary ?? "-" }}</pre>
      <form
        v-if="approval.status === 'pending' && !expired"
        class="grid max-w-2xl gap-4"
        @submit.prevent="approve"
      >
        <FormField id="approval-reason" :label="$t('approvalDecisionReason')">
          <Textarea
            id="approval-reason"
            v-model="reason"
            required
            maxlength="512"
            :disabled="submitting"
          />
        </FormField>
        <label class="flex items-center gap-2 text-sm">
          <input
            v-model="reviewed"
            type="checkbox"
            class="accent-primary size-4"
            :disabled="submitting"
          />{{ $t("approvalReviewed") }}
        </label>
        <Button
          type="submit"
          class="justify-self-start"
          :disabled="!canApprove"
        >
          <Check aria-hidden="true" />{{ $t("approve") }}
        </Button>
      </form>
    </SectionCard>
  </main>
</template>
