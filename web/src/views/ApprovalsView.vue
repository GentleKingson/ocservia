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
import { approveRequest, getApproval } from "../api/approvals";
import {
  getWorkspace,
  workspaceChangedEvent,
  workspaceContext,
  type WorkspaceContext,
} from "../api/workspace";
import { formatTimestamp } from "../shared/timestamp";

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
  window.addEventListener(workspaceChangedEvent, loadApproval);
  void loadApproval();
});
onBeforeUnmount(() => {
  window.removeEventListener(workspaceChangedEvent, loadApproval);
  invalidate();
});
</script>

<template>
  <main class="overview approvals-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("approvals") }}</h1>
      </div>
      <button
        type="button"
        class="icon-command page-command"
        :disabled="loading || submitting"
        :title="$t('refresh')"
        :aria-label="$t('refresh')"
        @click="loadApproval"
      >
        <RefreshCw :size="16" />
      </button>
    </div>
    <form class="approval-lookup" @submit.prevent="inspect">
      <label for="approval-lookup">{{ $t("approvalId") }}</label>
      <input
        id="approval-lookup"
        v-model="lookupId"
        required
        :disabled="submitting"
      />
      <button
        type="submit"
        class="icon-command"
        :disabled="submitting || !lookupId.trim()"
        :title="$t('inspectApproval')"
        :aria-label="$t('inspectApproval')"
      >
        <Search :size="18" />
      </button>
    </form>
    <p v-if="error" class="operation-error" role="alert">{{ error }}</p>
    <p v-if="loading" role="status">{{ $t("loading") }}</p>
    <section
      v-if="approval"
      class="approval-details"
      :aria-label="$t('approvalDetails')"
    >
      <dl>
        <dt>{{ $t("approvalId") }}</dt>
        <dd>
          <code>{{ approval.id }}</code>
        </dd>
        <dt>{{ $t("state") }}</dt>
        <dd data-testid="approval-status">
          {{
            expired && approval.status === "pending"
              ? "expired"
              : approval.status
          }}
        </dd>
        <dt>{{ $t("approvalAction") }}</dt>
        <dd>{{ approval.action }}</dd>
        <dt>{{ $t("approvalResource") }}</dt>
        <dd>
          {{ approval.resourceType }} <code>{{ approval.resourceId }}</code>
        </dd>
        <dt>{{ $t("approvalRequester") }}</dt>
        <dd>
          <code>{{ approval.requesterId }}</code>
        </dd>
        <dt>{{ $t("approvalApprover") }}</dt>
        <dd>
          <code>{{ approval.approverId ?? "-" }}</code>
        </dd>
        <dt>{{ $t("reason") }}</dt>
        <dd>{{ approval.reason }}</dd>
        <dt>{{ $t("expires") }}</dt>
        <dd>{{ formatTimestamp(approval.expiresAt) }}</dd>
        <dt>{{ $t("approvalHash") }}</dt>
        <dd>
          <code data-testid="approval-hash">{{
            approval.requestHash ?? "-"
          }}</code>
        </dd>
      </dl>
      <h2>{{ $t("approvalContent") }}</h2>
      <pre data-testid="approval-summary">{{ summary ?? "-" }}</pre>
      <form
        v-if="approval.status === 'pending' && !expired"
        class="approval-decision"
        @submit.prevent="approve"
      >
        <label for="approval-reason">{{ $t("approvalDecisionReason") }}</label>
        <textarea
          id="approval-reason"
          v-model="reason"
          required
          maxlength="512"
          :disabled="submitting"
        />
        <label class="approval-reviewed"
          ><input v-model="reviewed" type="checkbox" :disabled="submitting" />{{
            $t("approvalReviewed")
          }}</label
        >
        <button type="submit" class="primary-button" :disabled="!canApprove">
          <Check :size="16" />{{ $t("approve") }}
        </button>
      </form>
    </section>
  </main>
</template>

<style scoped>
.approval-lookup {
  display: grid;
  grid-template-columns: auto minmax(0, 32rem) 36px;
  align-items: center;
  gap: 12px;
  margin: 24px 0;
}
.approval-lookup input,
.approval-decision textarea {
  min-width: 0;
  width: 100%;
  padding: 9px;
  border: 1px solid #cbd5e1;
  border-radius: 4px;
  font: inherit;
}
.approval-details {
  padding-top: 24px;
  border-top: 1px solid #dce2e9;
}
.approval-details dl {
  display: grid;
  grid-template-columns: 150px minmax(0, 1fr);
  gap: 12px 20px;
  margin: 0;
}
.approval-details dt {
  color: #64748b;
}
.approval-details dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.approval-details h2 {
  font-size: 16px;
  margin-top: 28px;
}
.approval-details pre {
  padding: 16px;
  background: #f3f5f7;
  border: 1px solid #dce2e9;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-size: 13px;
}
.approval-decision {
  display: grid;
  gap: 12px;
  max-width: 42rem;
  margin: 24px 0;
}
.approval-decision textarea {
  min-height: 80px;
  resize: vertical;
}
.approval-reviewed {
  display: flex;
  align-items: center;
  gap: 8px;
}
.approval-decision button {
  justify-self: start;
  display: flex;
  gap: 8px;
  align-items: center;
}
@media (max-width: 640px) {
  .approval-lookup {
    grid-template-columns: minmax(0, 1fr) 36px;
  }
  .approval-lookup label {
    grid-column: 1 / -1;
  }
  .approval-details dl {
    grid-template-columns: minmax(0, 1fr);
    gap: 6px;
  }
  .approval-details dd {
    margin-bottom: 12px;
  }
}
</style>
