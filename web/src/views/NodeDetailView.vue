<script setup lang="ts">
import {
  ArrowUpCircle,
  Ban,
  CircleStop,
  Download,
  FileCheck2,
  KeyRound,
  ListPlus,
  LogOut,
  Power,
  Server,
  ShieldOff,
  SlidersHorizontal,
  UserCheck,
  UserPlus,
  UserX,
} from "@lucide/vue";
import { ResponseError, type NodeObservedState } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { useI18n } from "vue-i18n";
import { getUserPasswordSealingKey } from "../api/users";
import { sealUserPassword } from "../features/user-password";
import { formatTimestamp } from "../shared/timestamp";

import { workspaceContext } from "../api/workspace";
import { useNodeConfiguration } from "../features/configuration/useNodeConfiguration";
import { useNodeCertificates } from "../features/certificates/useNodeCertificates";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import DataState from "../components/common/DataState.vue";
import FormField from "../components/common/FormField.vue";
import OperationDialog from "../components/common/OperationDialog.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import NodeDetailHeader from "../components/nodes/NodeDetailHeader.vue";
import NodeDetailNav from "../components/nodes/NodeDetailNav.vue";
import NodeDetailSkeleton from "../components/nodes/NodeDetailSkeleton.vue";
import NodeObservedDetails from "../components/nodes/NodeObservedDetails.vue";
import NodeStatusSummary from "../components/nodes/NodeStatusSummary.vue";
import UserPolicyDialog from "../components/nodes/UserPolicyDialog.vue";
import {
  recoveryDialogKind,
  resourceStatusKey,
} from "../shared/desired-recovery";
import { useFleetStore } from "../shared/fleet";
import { operationTone } from "../features/operations/state-tone";
import { operationStatusKey } from "../shared/operation-status";
import { workspaceChangedEvent } from "../api/workspace";
import { createNodeWorkflow } from "../features/node-workflow";

const route = useRoute();
const fleet = useFleetStore();
const { t } = useI18n();
const routeNodeId = computed(() => {
  const value = route.params.nodeId;
  return Array.isArray(value) ? (value[0] ?? "") : value;
});
const currentNode = computed<NodeObservedState | undefined>(() =>
  fleet.selected?.id === routeNodeId.value ? fleet.selected : undefined,
);
const detailLoading = ref(true);
const detailState = ref<"loading" | "unavailable" | "not-found">("loading");
let detailSequence = 0;
watch(currentNode, (node) => {
  // A foreground retry can recover the initial detail read without navigation.
  if (node) detailState.value = "loading";
});

const pendingAction = ref<{
  kind: "disconnect" | "terminate" | "unban" | "reload" | "upgradeAgent";
  target: string;
  label: string;
}>();
const reason = ref("");
const approvalId = ref("");
const stateTab = ref<"users" | "groups">("users");
const desiredDialog = ref<{
  kind: "create" | "disable" | "enable" | "rotate" | "group";
  name: string;
  version: number;
}>();
const desiredName = ref("");
const desiredPassword = ref("");
const desiredLoading = ref(false);
const desiredError = ref("");
const groupMembers = ref("");
const desiredReason = ref("");
const policyDialog = ref<{ username: string }>();
const readReady = computed(
  () => !detailLoading.value && !fleet.selecting && !fleet.selectionError,
);
const {
  configDialog,
  currentConfigRevision,
  configSourceRevision,
  canSubmitConfigPlan,
  configPlan,
  configOperation,
  configError,
  configLoading,
  configPort,
  configUdpPort,
  configDevice,
  configNetwork,
  configDns,
  configCookieTimeout,
  configMaxSameClients,
  configMaxClients,
  configRoute,
  configCertificateSecretRefId,
  configPrivateKeySecretRefId,
  configReason,
  configApplyApproval,
  configApplyReason,
  openConfigPlan,
  submitConfigPlan,
  submitConfigApply,
} = useNodeConfiguration({
  currentNode,
  readReady,
  workspaceContext,
  trackOperation: (id) => fleet.trackOperation(id),
  t,
});
const {
  certificateDialog,
  certificate,
  certificateGrant,
  certificateOperation,
  certificateCommonName,
  certificateDnsNames,
  certificateReason,
  certificateApproval,
  certificateError,
  certificateLoading,
  openCertificate,
  submitCertificateRequest,
  submitCertificateIssue,
  createP12,
  downloadP12,
  revokeCurrentCertificate,
} = useNodeCertificates({
  currentNode,
  readReady,
  workspaceContext,
  trackOperation: (id) => fleet.trackOperation(id),
  t,
});
const usersState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "user"),
);
const groupsState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "group"),
);
const operationBusy = computed(() => fleet.operationTracking);
function closeNodeDialogs(): void {
  desiredDialog.value = undefined;
  pendingAction.value = undefined;
  configDialog.value = false;
  certificateDialog.value = false;
  policyDialog.value = undefined;
}

async function selectRouteNode(): Promise<void> {
  closeNodeDialogs();
  const sequence = ++detailSequence;
  const nodeId = routeNodeId.value;
  detailLoading.value = true;
  detailState.value = "loading";
  if (!nodeId) {
    detailLoading.value = false;
    detailState.value = "not-found";
    return;
  }
  await fleet.select(nodeId);
  if (sequence !== detailSequence) return;
  detailLoading.value = false;
  if (currentNode.value) {
    detailState.value = "loading";
    return;
  }
  detailState.value =
    fleet.selectionError === "notFound" ? "not-found" : "unavailable";
}

function refreshForWorkspace(): void {
  closeNodeDialogs();
  void selectRouteNode();
}

onMounted(() => {
  fleet.start();
  window.addEventListener(workspaceChangedEvent, refreshForWorkspace);
  void selectRouteNode();
});
onBeforeUnmount(() => {
  closeNodeDialogs();
  detailSequence += 1;
  fleet.stop();
  window.removeEventListener(workspaceChangedEvent, refreshForWorkspace);
});
watch(routeNodeId, () => void selectRouteNode(), { flush: "sync" });

const detailStatus = computed<{
  key: string;
  tone: "ok" | "error" | "muted";
}>(() => {
  if (detailLoading.value) return { key: "nodeLoading", tone: "muted" };
  if (fleet.unavailable || detailState.value === "unavailable")
    return { key: "systemsUnavailable", tone: "error" };
  if (!currentNode.value) return { key: "notObserved", tone: "muted" };
  return currentNode.value.freshness === "fresh"
    ? { key: "latestObservation", tone: "ok" }
    : { key: currentNode.value.freshness, tone: "muted" };
});

const upgradeEligible = computed(
  () =>
    Boolean(currentNode.value?.agentUpgradeEligible) &&
    Boolean(currentNode.value?.recommendedAgentVersion),
);

function actionAvailable(action: string): boolean {
  return (
    readReady.value &&
    currentNode.value?.effectiveActions?.[action]?.allowed === true
  );
}
function actionExplanation(action: string): string {
  const availability = currentNode.value?.effectiveActions?.[action];
  return availability?.allowed
    ? ""
    : t("action_" + (availability?.reason ?? "unknown"));
}

function openAction(
  kind: "disconnect" | "terminate" | "unban" | "reload" | "upgradeAgent",
  target: string,
  label: string,
): void {
  reason.value = "";
  approvalId.value = "";
  pendingAction.value = { kind, target, label };
}

async function submitAction(): Promise<void> {
  const action = pendingAction.value;
  const explanation = reason.value.trim();
  if (!action || !explanation) return;
  pendingAction.value = undefined;
  if (action.kind === "disconnect")
    await fleet.disconnectSession(action.target, explanation);
  else if (action.kind === "terminate")
    await fleet.terminateSession(action.target, explanation);
  else if (action.kind === "unban")
    await fleet.removeIpBan(action.target, explanation);
  else if (action.kind === "upgradeAgent")
    await fleet.upgradeAgent(
      action.target,
      explanation,
      approvalId.value.trim(),
    );
  else await fleet.reloadService(explanation, approvalId.value.trim());
}

function openDesired(
  kind: "create" | "disable" | "enable" | "rotate" | "group",
  name = "",
  version = 0,
): void {
  desiredDialog.value = { kind, name, version };
  desiredName.value = name;
  desiredWorkflow.cancel();
  desiredPassword.value = "";
  desiredError.value = "";
  desiredLoading.value = false;
  groupMembers.value =
    kind === "group"
      ? (
          groupsState.value.find((item) => item.name === name)
            ?.desiredMembers ?? []
        ).join(", ")
      : "";
  desiredReason.value = "";
}

const desiredWorkflow = createNodeWorkflow(
  () => currentNode.value?.id,
  () => Boolean(desiredDialog.value),
  workspaceContext,
);
watch(
  desiredDialog,
  (dialog) => {
    if (dialog) return;
    desiredWorkflow.cancel();
    desiredPassword.value = "";
    desiredError.value = "";
    desiredLoading.value = false;
  },
  { flush: "sync" },
);

async function submitDesired(): Promise<void> {
  const dialog = desiredDialog.value;
  const explanation = desiredReason.value.trim();
  const nodeId = currentNode.value?.id;
  if (!dialog || !explanation || !nodeId || desiredLoading.value) return;
  const name = desiredName.value.trim();
  const context = desiredWorkflow.begin(nodeId);
  desiredLoading.value = true;
  desiredError.value = "";
  // UTF-8 bytes are transient and never passed to the API or fleet store.
  const plaintext = new TextEncoder().encode(desiredPassword.value);
  desiredPassword.value = "";
  try {
    if (dialog.kind === "create" || dialog.kind === "rotate") {
      const key = await getUserPasswordSealingKey(nodeId, context.signal);
      if (!desiredWorkflow.isCurrent(context)) return;
      const sealed = await sealUserPassword(
        plaintext,
        key,
        context.workspace.id,
        nodeId,
      );
      if (!desiredWorkflow.isCurrent(context)) return;
      desiredDialog.value = undefined;
      if (dialog.kind === "create")
        await fleet.createUser(
          name,
          dialog.version,
          sealed.ciphertext,
          sealed.keyId,
          explanation,
        );
      else
        await fleet.rotateUserPassword(
          dialog.name,
          dialog.version,
          sealed.ciphertext,
          sealed.keyId,
          explanation,
        );
    } else {
      desiredDialog.value = undefined;
      if (dialog.kind === "disable")
        await fleet.disableUser(dialog.name, dialog.version, explanation);
      else if (dialog.kind === "enable")
        await fleet.enableUser(dialog.name, dialog.version, explanation);
      else
        await fleet.applyGroup(
          name,
          dialog.version,
          groupMembers.value
            .split(",")
            .map((value) => value.trim())
            .filter(Boolean)
            .sort(),
          explanation,
        );
    }
  } catch (error) {
    if (!desiredWorkflow.isCurrent(context)) return;
    const keys = [
      "passwordCryptoUnavailable",
      "passwordKeyChanged",
      "passwordTooLong",
    ];
    desiredError.value = t(
      error instanceof Error && keys.includes(error.message)
        ? error.message
        : error instanceof ResponseError && error.response.status === 403
          ? "passwordKeyForbidden"
          : error instanceof ResponseError && error.response.status === 409
            ? "passwordKeyChanged"
            : "passwordKeyUnavailable",
    );
  } finally {
    plaintext.fill(0);
    if (desiredWorkflow.isCurrent(context)) desiredLoading.value = false;
  }
}

function openPolicy(username: string): void {
  if (currentNode.value) policyDialog.value = { username };
}
function convergenceTone(key: string): string {
  if (key === "convergence_converged") return "text-success";
  return key === "convergence_drifted"
    ? "text-destructive"
    : "text-muted-foreground";
}
</script>

<template>
  <main>
    <NodeDetailHeader
      :title="
        currentNode?.name ??
        fleet.nodes?.find((node) => node.id === routeNodeId)?.name ??
        $t('nodeDetail')
      "
      :node-id="routeNodeId"
      :status="$t(detailStatus.key)"
      :tone="detailStatus.tone"
    />

    <NodeDetailSkeleton v-if="detailLoading" />
    <div
      v-else-if="detailState === 'not-found'"
      class="bg-card border-border text-muted-foreground grid min-h-56 place-content-center justify-items-center gap-2.5 rounded-lg border p-7 text-center text-sm"
    >
      <Server class="size-6" aria-hidden="true" /><span>{{
        $t("nodeNotFound")
      }}</span>
    </div>
    <DataState
      v-else-if="detailState === 'unavailable'"
      kind="error"
      :message="$t('nodeUnavailable')"
    />

    <template v-else-if="currentNode">
      <NodeStatusSummary
        :node="currentNode"
        :session-count="fleet.sessions.length"
      />
      <NodeDetailNav />
      <NodeObservedDetails id="node-overview" :node="currentNode">
        <div class="mb-4 grid gap-3">
          <p
            v-if="currentNode.freshness === 'stale'"
            class="m-0 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900"
            role="status"
          >
            {{ $t("staleNode") }}
          </p>
          <div class="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              :disabled="operationBusy || !actionAvailable('service.reload')"
              :title="actionExplanation('service.reload') || $t('reloadOcserv')"
              @click="openAction('reload', '', $t('reloadOcserv'))"
            >
              <Power aria-hidden="true" />{{ $t("reload") }}
            </Button>
            <Button
              v-if="upgradeEligible"
              type="button"
              variant="outline"
              size="sm"
              data-testid="upgrade-agent"
              :disabled="operationBusy"
              :title="$t('upgradeAgentTitle')"
              @click="
                openAction(
                  'upgradeAgent',
                  currentNode.recommendedAgentVersion ?? '',
                  $t('upgradeAgentTitle'),
                )
              "
            >
              <ArrowUpCircle aria-hidden="true" />{{ $t("upgradeAgent") }}
            </Button>
          </div>
          <p
            v-if="!actionAvailable('service.reload')"
            class="text-muted-foreground m-0 text-xs"
          >
            {{ $t("reload") }}: {{ actionExplanation("service.reload") }}
          </p>
          <div
            v-if="fleet.latestOperation"
            class="flex flex-wrap items-center gap-2 text-sm"
            data-testid="operation-status"
            aria-live="polite"
          >
            <span class="text-muted-foreground">{{
              $t("latestOperation")
            }}</span>
            <StatusBadge
              :tone="operationTone(fleet.latestOperation)"
              :label="$t(operationStatusKey(fleet.latestOperation))"
              data-testid="operation-state"
            />
            <code
              v-if="fleet.latestOperation.agentUpgradeTargetVersion"
              class="text-xs"
              >{{ fleet.latestOperation.agentUpgradeTargetVersion }}</code
            >
            <Button
              v-if="
                fleet.operationTracking &&
                fleet.latestOperation.state === 'unknown'
              "
              type="button"
              variant="ghost"
              size="icon-sm"
              :title="$t('stopTrackingOperation')"
              :aria-label="$t('stopTrackingOperation')"
              @click="fleet.detachOperation"
            >
              <CircleStop aria-hidden="true" />
            </Button>
          </div>
          <p
            v-if="fleet.operationError"
            class="text-destructive m-0 text-sm break-words"
            role="alert"
          >
            {{ fleet.operationError }}
          </p>
        </div>
      </NodeObservedDetails>

      <SectionCard id="node-sessions" :title="$t('sessions')">
        <div class="grid gap-2">
          <h3 class="m-0 text-sm font-semibold">
            {{ $t("currentSessions") }}
          </h3>
          <ul v-if="fleet.sessions.length" class="m-0 grid list-none p-0">
            <li
              v-for="session in fleet.sessions"
              :key="session.id"
              class="border-border flex items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
            >
              <span class="grid min-w-0 text-sm">
                <strong class="break-words">{{ session.username }}</strong>
                <span class="text-muted-foreground text-xs">{{
                  session.clientIp
                }}</span>
              </span>
              <span class="flex shrink-0 gap-1">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="operationBusy || !currentNode.bootId"
                  :title="$t('disconnect')"
                  :aria-label="$t('disconnect')"
                  @click="
                    openAction('disconnect', session.id, $t('disconnect'))
                  "
                >
                  <LogOut aria-hidden="true" />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  class="text-destructive hover:text-destructive"
                  :disabled="operationBusy || !currentNode.bootId"
                  :title="$t('terminate')"
                  :aria-label="$t('terminate')"
                  @click="openAction('terminate', session.id, $t('terminate'))"
                >
                  <ShieldOff aria-hidden="true" />
                </Button>
              </span>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">
            {{ $t("noSessions") }}
          </p>
        </div>
        <div class="grid gap-2">
          <h3 class="m-0 text-sm font-semibold">{{ $t("ipBans") }}</h3>
          <ul v-if="fleet.ipBans.length" class="m-0 grid list-none p-0">
            <li
              v-for="ban in fleet.ipBans"
              :key="ban.ip"
              class="border-border flex items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
            >
              <strong class="min-w-0 text-sm break-all">{{ ban.ip }}</strong>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                :disabled="operationBusy"
                :title="$t('removeBan')"
                :aria-label="$t('removeBan')"
                @click="openAction('unban', ban.ip, $t('removeBan'))"
              >
                <Ban aria-hidden="true" />
              </Button>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">
            {{ $t("noIpBans") }}
          </p>
        </div>
      </SectionCard>

      <SectionCard id="node-users-groups" :title="$t('usersAndGroups')">
        <template #actions>
          <div
            class="bg-muted inline-flex gap-0.5 rounded-md p-0.5"
            role="group"
            :aria-label="$t('usersAndGroups')"
          >
            <Button
              type="button"
              size="sm"
              :variant="stateTab === 'users' ? 'outline' : 'ghost'"
              :aria-pressed="stateTab === 'users'"
              @click="stateTab = 'users'"
            >
              {{ $t("users") }}
            </Button>
            <Button
              type="button"
              size="sm"
              :variant="stateTab === 'groups' ? 'outline' : 'ghost'"
              :aria-pressed="stateTab === 'groups'"
              @click="stateTab = 'groups'"
            >
              {{ $t("groups") }}
            </Button>
          </div>
          <Button
            v-if="stateTab === 'users'"
            type="button"
            variant="outline"
            size="icon-sm"
            :disabled="operationBusy || !actionAvailable('user.manage')"
            :title="actionExplanation('user.manage') || $t('createUser')"
            :aria-label="$t('createUser')"
            @click="openDesired('create')"
          >
            <UserPlus aria-hidden="true" />
          </Button>
          <Button
            v-else
            type="button"
            variant="outline"
            size="icon-sm"
            :disabled="operationBusy || !actionAvailable('group.manage')"
            :title="actionExplanation('group.manage') || $t('applyGroup')"
            :aria-label="$t('applyGroup')"
            @click="openDesired('group')"
          >
            <ListPlus aria-hidden="true" />
          </Button>
        </template>
        <p
          v-if="
            !actionAvailable(
              stateTab === 'users' ? 'user.manage' : 'group.manage',
            )
          "
          class="text-muted-foreground m-0 text-xs"
        >
          {{
            actionExplanation(
              stateTab === "users" ? "user.manage" : "group.manage",
            )
          }}
        </p>
        <template v-if="stateTab === 'users'">
          <ul v-if="usersState.length" class="m-0 grid list-none p-0">
            <li
              v-for="item in usersState"
              :key="item.name"
              class="border-border flex flex-wrap items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
            >
              <span class="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                <strong class="break-words">{{ item.name }}</strong>
                <Badge
                  variant="outline"
                  :class="convergenceTone(resourceStatusKey(item))"
                  >{{ $t(resourceStatusKey(item)) }}</Badge
                >
                <Badge
                  v-if="item.recoveryRequired && !item.recoveryMutationKind"
                  variant="outline"
                  class="text-destructive"
                  >{{ $t("manualReconciliationRequired") }}</Badge
                >
              </span>
              <span class="flex shrink-0 gap-1">
                <Button
                  v-if="item.desiredVersion"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('quotaAndExpiry')"
                  :aria-label="$t('quotaAndExpiry')"
                  @click="openPolicy(item.name)"
                >
                  <SlidersHorizontal aria-hidden="true" />
                </Button>
                <Button
                  v-if="recoveryDialogKind(item) === 'create'"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('retryCreateUser')"
                  :aria-label="$t('retryCreateUser')"
                  @click="
                    openDesired('create', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserPlus aria-hidden="true" />
                </Button>
                <Button
                  v-else-if="recoveryDialogKind(item) === 'rotate'"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('retryRotatePassword')"
                  :aria-label="$t('retryRotatePassword')"
                  @click="
                    openDesired('rotate', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <KeyRound aria-hidden="true" />
                </Button>
                <Button
                  v-else-if="recoveryDialogKind(item) === 'disable'"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  class="text-destructive hover:text-destructive"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('retryDisableUser')"
                  :aria-label="$t('retryDisableUser')"
                  @click="
                    openDesired('disable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserX aria-hidden="true" />
                </Button>
                <Button
                  v-else-if="recoveryDialogKind(item) === 'enable'"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('retryEnableUser')"
                  :aria-label="$t('retryEnableUser')"
                  @click="
                    openDesired('enable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserCheck aria-hidden="true" />
                </Button>
                <Button
                  v-if="!item.recoveryRequired"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('rotatePassword')"
                  :aria-label="$t('rotatePassword')"
                  @click="
                    openDesired('rotate', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <KeyRound aria-hidden="true" />
                </Button>
                <Button
                  v-if="!item.recoveryRequired && item.desiredEnabled === false"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('enableUser')"
                  :aria-label="$t('enableUser')"
                  @click="
                    openDesired('enable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserCheck aria-hidden="true" />
                </Button>
                <Button
                  v-else-if="!item.recoveryRequired"
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  class="text-destructive hover:text-destructive"
                  :disabled="
                    operationBusy ||
                    !actionAvailable('user.manage') ||
                    !item.desiredVersion
                  "
                  :title="$t('disableUser')"
                  :aria-label="$t('disableUser')"
                  @click="
                    openDesired('disable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserX aria-hidden="true" />
                </Button>
              </span>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">
            {{ $t("noUsers") }}
          </p>
        </template>
        <template v-else>
          <ul v-if="groupsState.length" class="m-0 grid list-none p-0">
            <li
              v-for="item in groupsState"
              :key="item.name"
              class="border-border flex flex-wrap items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
            >
              <span class="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                <strong class="break-words">{{ item.name }}</strong>
                <Badge
                  variant="outline"
                  :class="convergenceTone(resourceStatusKey(item))"
                  >{{ $t(resourceStatusKey(item)) }}</Badge
                >
                <Badge
                  v-if="item.recoveryRequired && !item.recoveryMutationKind"
                  variant="outline"
                  class="text-destructive"
                  >{{ $t("manualReconciliationRequired") }}</Badge
                >
              </span>
              <Button
                v-if="recoveryDialogKind(item) === 'group'"
                type="button"
                variant="ghost"
                size="icon-sm"
                :disabled="operationBusy || !actionAvailable('group.manage')"
                :title="$t('retryApplyGroup')"
                :aria-label="$t('retryApplyGroup')"
                @click="
                  openDesired('group', item.name, item.desiredVersion ?? 0)
                "
              >
                <ListPlus aria-hidden="true" />
              </Button>
              <Button
                v-else-if="!item.recoveryRequired"
                type="button"
                variant="ghost"
                size="icon-sm"
                :disabled="operationBusy || !actionAvailable('group.manage')"
                :title="$t('applyGroup')"
                :aria-label="$t('applyGroup')"
                @click="
                  openDesired('group', item.name, item.desiredVersion ?? 0)
                "
              >
                <ListPlus aria-hidden="true" />
              </Button>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">
            {{ $t("noGroups") }}
          </p>
        </template>
      </SectionCard>

      <SectionCard id="node-configuration" :title="$t('configurationSection')">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <span class="inline-flex items-center gap-2 text-sm">
            <FileCheck2
              class="text-muted-foreground size-4"
              aria-hidden="true"
            />
            {{ $t("configPlan") }}
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            :disabled="
              operationBusy ||
              currentConfigRevision === undefined ||
              !actionAvailable('config.plan')
            "
            :title="$t('configPlan')"
            @click="openConfigPlan"
          >
            {{ $t("plan") }}
          </Button>
        </div>
        <p
          v-if="!actionAvailable('config.plan')"
          class="text-muted-foreground m-0 text-xs"
        >
          {{ actionExplanation("config.plan") }}
        </p>
      </SectionCard>

      <SectionCard id="node-certificates" :title="$t('certificatesSection')">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <span class="text-sm">{{ $t("certificateLifecycle") }}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            :disabled="operationBusy || !actionAvailable('certificate.read')"
            :title="
              actionExplanation('certificate.read') ||
              $t('certificateLifecycle')
            "
            @click="openCertificate"
          >
            {{ $t("certificate") }}
          </Button>
        </div>
        <p
          v-if="!actionAvailable('certificate.issue')"
          class="text-muted-foreground m-0 text-xs"
        >
          {{ actionExplanation("certificate.issue") }}
        </p>
      </SectionCard>
    </template>

    <OperationDialog
      v-if="certificateDialog"
      :title="$t('certificateLifecycle')"
      :subject="currentNode?.name"
      :error="certificateError"
      @close="certificateDialog = false"
      @submit="submitCertificateRequest"
    >
      <p
        v-if="!actionAvailable('certificate.issue')"
        class="text-muted-foreground m-0 text-xs"
      >
        {{ $t("requestCsr") }}: {{ actionExplanation("certificate.issue") }}
      </p>
      <template v-if="!certificate && actionAvailable('certificate.issue')">
        <FormField id="certificate-cn" :label="$t('commonName')">
          <Input
            id="certificate-cn"
            v-model="certificateCommonName"
            maxlength="253"
            required
          />
        </FormField>
        <FormField id="certificate-dns" :label="$t('dnsNames')">
          <Input
            id="certificate-dns"
            v-model="certificateDnsNames"
            maxlength="4096"
          />
        </FormField>
      </template>
      <template v-else-if="certificate">
        <div
          class="bg-muted/50 border-border flex min-w-0 flex-wrap items-center gap-2 rounded-md border p-3 text-sm"
          aria-live="polite"
        >
          <Badge
            variant="outline"
            :class="
              certificate.state === 'issued'
                ? 'text-success'
                : certificate.state === 'expiring'
                  ? 'text-amber-800'
                  : undefined
            "
            >{{ certificate.state }}</Badge
          >
          <code class="min-w-0 text-xs break-all">{{ certificate.id }}</code>
          <small v-if="certificate.notAfter" class="text-muted-foreground"
            >{{ $t("expires") }}
            {{ formatTimestamp(certificate.notAfter) }}</small
          >
        </div>
        <FormField
          v-if="
            certificate.state === 'csr_ready' ||
            certificate.state === 'signer_unavailable' ||
            certificate.state === 'issued' ||
            certificate.state === 'expiring'
          "
          id="certificate-approval"
          :label="$t('approvalId')"
          :help="$t('approvalIdHelp')"
          v-slot="{ describedBy }"
        >
          <Input
            id="certificate-approval"
            v-model="certificateApproval"
            :aria-describedby="describedBy"
            autocomplete="off"
            required
          />
        </FormField>
        <FormField
          v-if="certificateGrant?.password"
          id="certificate-password"
          :label="$t('p12Password')"
          :help="$t('oneTimeCredential')"
          v-slot="{ describedBy }"
        >
          <Input
            id="certificate-password"
            class="font-mono"
            :model-value="certificateGrant.password"
            :aria-describedby="describedBy"
            readonly
            autocomplete="off"
          />
        </FormField>
      </template>
      <FormField id="certificate-reason" :label="$t('reason')">
        <Textarea
          id="certificate-reason"
          v-model="certificateReason"
          maxlength="512"
          required
        />
      </FormField>
      <p
        v-if="certificateOperation || certificate?.operationId"
        class="m-0 flex flex-wrap items-center gap-2 text-sm"
      >
        {{ $t("operationId") }}:
        <code class="text-xs break-all">{{
          certificateOperation?.id ?? certificate?.operationId
        }}</code>
        <span v-if="certificateOperation">{{
          $t(operationStatusKey(certificateOperation))
        }}</span>
      </p>
      <template #footer>
        <Button
          v-if="!certificate"
          type="submit"
          :disabled="
            certificateLoading ||
            !actionAvailable('certificate.issue') ||
            !certificateReason.trim()
          "
        >
          {{ $t("requestCsr") }}
        </Button>
        <Button
          v-else-if="
            certificate.state === 'csr_ready' ||
            certificate.state === 'signer_unavailable'
          "
          type="button"
          :disabled="
            certificateLoading ||
            !actionAvailable('certificate.issue') ||
            !certificateApproval.trim() ||
            !certificateReason.trim()
          "
          @click="submitCertificateIssue"
        >
          {{ $t("issueCertificate") }}
        </Button>
        <template
          v-else-if="
            certificate.state === 'issued' || certificate.state === 'expiring'
          "
        >
          <Button
            type="button"
            variant="outline"
            :disabled="
              certificateLoading ||
              !actionAvailable('certificate.private_key.export') ||
              Boolean(certificateGrant?.downloadToken) ||
              !certificateReason.trim()
            "
            @click="createP12"
          >
            <KeyRound aria-hidden="true" />{{ $t("createP12") }}
          </Button>
          <Button
            v-if="certificateGrant?.downloadToken"
            type="button"
            :disabled="certificateLoading"
            @click="downloadP12"
          >
            <Download aria-hidden="true" />{{ $t("download") }}
          </Button>
          <Button
            type="button"
            variant="destructive"
            :disabled="
              certificateLoading ||
              !actionAvailable('certificate.revoke') ||
              !certificateReason.trim()
            "
            @click="revokeCurrentCertificate"
          >
            {{ $t("revoke") }}
          </Button>
        </template>
      </template>
    </OperationDialog>

    <OperationDialog
      v-if="pendingAction"
      :title="pendingAction.label"
      :subject="pendingAction.target || currentNode?.name"
      @close="pendingAction = undefined"
      @submit="submitAction"
    >
      <FormField
        v-if="pendingAction.kind === 'upgradeAgent'"
        id="upgrade-target"
        :label="$t('targetVersion')"
        :help="$t('upgradeTargetReadOnly')"
      >
        <output id="upgrade-target" class="font-mono text-sm">{{
          pendingAction.target
        }}</output>
      </FormField>
      <FormField id="operation-reason" :label="$t('reason')">
        <Textarea
          id="operation-reason"
          v-model="reason"
          maxlength="512"
          required
        />
      </FormField>
      <FormField
        v-if="
          pendingAction.kind === 'reload' ||
          pendingAction.kind === 'upgradeAgent'
        "
        id="approval-id"
        :label="$t('approvalId')"
        :help="$t('approvalIdHelp')"
        v-slot="{ describedBy }"
      >
        <Input
          id="approval-id"
          v-model="approvalId"
          :aria-describedby="describedBy"
          autocomplete="off"
          required
        />
      </FormField>
      <template #footer>
        <Button
          type="submit"
          :variant="
            pendingAction.kind === 'terminate' ? 'destructive' : 'default'
          "
          :disabled="
            !reason.trim() ||
            ((pendingAction.kind === 'reload' ||
              pendingAction.kind === 'upgradeAgent') &&
              !approvalId.trim())
          "
        >
          {{ $t("confirm") }}
        </Button>
      </template>
    </OperationDialog>

    <OperationDialog
      v-if="desiredDialog"
      :title="
        $t(
          desiredDialog.kind === 'create'
            ? 'createUser'
            : desiredDialog.kind === 'disable'
              ? 'disableUser'
              : desiredDialog.kind === 'enable'
                ? 'enableUser'
                : desiredDialog.kind === 'rotate'
                  ? 'rotatePassword'
                  : 'applyGroup',
        )
      "
      :subject="desiredDialog.name || currentNode?.name"
      :error="desiredError"
      @close="desiredDialog = undefined"
      @submit="submitDesired"
    >
      <FormField
        v-if="desiredDialog.kind === 'create' || desiredDialog.kind === 'group'"
        id="desired-name"
        :label="$t(desiredDialog.kind === 'group' ? 'group' : 'user')"
      >
        <Input
          id="desired-name"
          v-model="desiredName"
          :disabled="
            desiredDialog.kind === 'create' && desiredDialog.version > 0
          "
          maxlength="64"
          required
        />
      </FormField>
      <FormField
        v-if="
          desiredDialog.kind === 'create' || desiredDialog.kind === 'rotate'
        "
        id="desired-password"
        :label="$t('password')"
        :help="$t('passwordSealingHelp')"
        v-slot="{ describedBy }"
      >
        <Input
          id="desired-password"
          v-model="desiredPassword"
          type="password"
          autocomplete="new-password"
          :aria-describedby="describedBy"
          :disabled="desiredLoading"
          required
        />
      </FormField>
      <FormField
        v-if="desiredDialog.kind === 'group'"
        id="group-members"
        :label="$t('members')"
      >
        <Textarea id="group-members" v-model="groupMembers" maxlength="65535" />
      </FormField>
      <FormField id="desired-reason" :label="$t('reason')">
        <Textarea
          id="desired-reason"
          v-model="desiredReason"
          maxlength="512"
          required
        />
      </FormField>
      <template #footer>
        <Button
          type="submit"
          :variant="
            desiredDialog.kind === 'disable' ? 'destructive' : 'default'
          "
          :disabled="
            desiredLoading ||
            !desiredReason.trim() ||
            ((desiredDialog.kind === 'create' ||
              desiredDialog.kind === 'rotate') &&
              !desiredPassword)
          "
        >
          {{ $t("confirm") }}
        </Button>
      </template>
    </OperationDialog>

    <UserPolicyDialog
      v-if="policyDialog"
      :key="policyDialog.username"
      :node-id="currentNode?.id"
      :username="policyDialog.username"
      @close="policyDialog = undefined"
    />

    <OperationDialog
      v-if="configDialog"
      wide
      :title="$t('configPlan')"
      :subject="currentNode?.name"
      :error="configError"
      @close="configDialog = false"
      @submit="submitConfigPlan"
    >
      <template #description>
        <span class="mt-2 block" role="note">{{
          $t("configTemplateSource")
        }}</span>
      </template>
      <div class="text-muted-foreground grid gap-1 text-xs">
        <p class="m-0">
          {{ $t("configSourceRevision") }}:
          <code>{{ configSourceRevision }}</code>
        </p>
        <p class="m-0">{{ $t("configRiskFields") }}</p>
      </div>
      <div class="grid gap-4 sm:grid-cols-2">
        <FormField id="config-port" :label="$t('tcpPort')">
          <Input
            id="config-port"
            v-model.number="configPort"
            type="number"
            min="1"
            max="65535"
            required
          />
        </FormField>
        <FormField id="config-clients" :label="$t('maxClients')">
          <Input
            id="config-clients"
            v-model.number="configMaxClients"
            type="number"
            min="1"
            max="65535"
            required
          />
        </FormField>
        <FormField id="config-udp-port" :label="$t('udpPort')">
          <Input
            id="config-udp-port"
            v-model.number="configUdpPort"
            type="number"
            min="0"
            max="65535"
            required
          />
        </FormField>
        <FormField id="config-device" :label="$t('vpnDevice')">
          <Input
            id="config-device"
            v-model="configDevice"
            maxlength="15"
            required
          />
        </FormField>
        <FormField id="config-network" :label="$t('ipv4Network')">
          <Input
            id="config-network"
            v-model="configNetwork"
            maxlength="18"
            required
          />
        </FormField>
        <FormField id="config-dns" :label="$t('dnsServer')">
          <Input id="config-dns" v-model="configDns" maxlength="15" required />
        </FormField>
        <FormField id="config-same-clients" :label="$t('maxSameClients')">
          <Input
            id="config-same-clients"
            v-model.number="configMaxSameClients"
            type="number"
            min="1"
            :max="configMaxClients"
            required
          />
        </FormField>
        <FormField id="config-cookie-timeout" :label="$t('cookieTimeout')">
          <Input
            id="config-cookie-timeout"
            v-model.number="configCookieTimeout"
            type="number"
            min="60"
            max="86400"
            required
          />
        </FormField>
        <FormField id="config-route" :label="$t('route')">
          <Input
            id="config-route"
            v-model="configRoute"
            maxlength="256"
            required
          />
        </FormField>
        <FormField id="config-certificate-key" :label="$t('certificateRef')">
          <Input
            id="config-certificate-key"
            v-model="configCertificateSecretRefId"
            maxlength="36"
            required
          />
        </FormField>
        <FormField id="config-private-key" :label="$t('privateKeyRef')">
          <Input
            id="config-private-key"
            v-model="configPrivateKeySecretRefId"
            maxlength="36"
            required
          />
        </FormField>
      </div>
      <FormField id="config-reason" :label="$t('reason')">
        <Textarea
          id="config-reason"
          v-model="configReason"
          maxlength="512"
          required
        />
      </FormField>
      <div
        v-if="configPlan"
        class="bg-muted/50 border-border grid min-w-0 gap-3 rounded-md border p-3"
        data-testid="config-plan-result"
        aria-live="polite"
      >
        <Badge
          variant="outline"
          :class="
            configPlan.validation === 'valid'
              ? 'text-success'
              : 'text-destructive'
          "
          >{{ configPlan.validation }}</Badge
        >
        <dl class="m-0 grid gap-2 text-sm">
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">
              {{ $t("configPlanId") }}
            </dt>
            <dd class="m-0">
              <code class="text-xs break-all">{{ configPlan.id }}</code>
            </dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">
              {{ $t("candidateHash") }}
            </dt>
            <dd class="m-0">
              <code class="text-xs break-all">{{
                configPlan.candidateHash
              }}</code>
            </dd>
          </div>
          <div v-if="configPlan.materializedHash" class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">
              {{ $t("materializedHash") }}
            </dt>
            <dd class="m-0">
              <code class="text-xs break-all">{{
                configPlan.materializedHash
              }}</code>
            </dd>
          </div>
          <div class="grid min-w-0 gap-1">
            <dt class="text-muted-foreground text-xs">
              {{ $t("configSourceRevision") }}
            </dt>
            <dd class="m-0">
              <code class="text-xs">{{ configPlan.expectedRevision }}</code>
            </dd>
          </div>
        </dl>
        <p class="text-muted-foreground m-0 text-xs">
          {{ $t("configCandidateSource") }}
        </p>
        <pre
          v-if="configPlan.diffRedacted"
          class="bg-background border-border m-0 max-h-80 overflow-auto rounded-md border p-3 text-xs"
          >{{ configPlan.diffRedacted }}</pre>
        <ul
          v-if="configPlan.warnings.length"
          class="m-0 list-disc pl-5 text-sm text-amber-900"
        >
          <li v-for="(warning, index) in configPlan.warnings" :key="index">
            {{ warning }}
          </li>
        </ul>
        <p
          v-if="!actionAvailable('config.apply')"
          class="text-muted-foreground m-0 text-xs"
        >
          {{ $t("apply") }}: {{ actionExplanation("config.apply") }}
        </p>
        <template v-if="configPlan.validation === 'valid'">
          <FormField
            id="config-apply-approval"
            :label="$t('approvalId')"
            :help="$t('approvalIdHelp')"
            v-slot="{ describedBy }"
          >
            <Input
              id="config-apply-approval"
              v-model="configApplyApproval"
              :aria-describedby="describedBy"
              required
            />
          </FormField>
          <FormField id="config-apply-reason" :label="$t('reason')">
            <Textarea
              id="config-apply-reason"
              v-model="configApplyReason"
              maxlength="512"
            />
          </FormField>
          <Button
            type="button"
            class="w-fit"
            :disabled="
              configLoading ||
              !canSubmitConfigPlan ||
              !actionAvailable('config.apply') ||
              !configApplyApproval.trim() ||
              !configApplyReason.trim()
            "
            @click="submitConfigApply"
          >
            {{ $t("apply") }}
          </Button>
        </template>
      </div>
      <p
        v-if="configOperation || configPlan?.operationId"
        class="m-0 flex flex-wrap items-center gap-2 text-sm"
      >
        {{ $t("operationId") }}:
        <code class="text-xs break-all">{{
          configOperation?.id ?? configPlan?.operationId
        }}</code>
        <span v-if="configOperation">{{
          $t(operationStatusKey(configOperation))
        }}</span>
      </p>
      <template #footer>
        <Button
          type="submit"
          :variant="configPlan ? 'outline' : 'default'"
          :disabled="
            configLoading ||
            !canSubmitConfigPlan ||
            !actionAvailable('config.plan') ||
            !configReason.trim()
          "
        >
          {{ $t("plan") }}
        </Button>
      </template>
    </OperationDialog>
  </main>
</template>
