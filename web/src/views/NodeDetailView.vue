<script setup lang="ts">
import {
  ArrowLeft,
  ArrowUpCircle,
  Ban,
  Cable,
  CircleStop,
  Download,
  FileCheck2,
  Gauge,
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
  Users,
} from "@lucide/vue";
import type {
  ArtifactGrant,
  Certificate,
  ConfigPlan,
  NodeObservedState,
} from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { useI18n } from "vue-i18n";

import {
  applyConfigPlan,
  createCertificate,
  createCertificateP12,
  createConfigPlan,
  downloadCertificateArtifact,
  getCertificate,
  getConfigPlan,
  issueCertificate,
  listNodeCertificates,
  revokeCertificate,
} from "../api/client";
import {
  loadUserPolicy,
  policyToForm,
  saveUserPolicy,
  type UserPolicyForm,
} from "../adapters/user-policy";
import UserPolicyFields from "../upstream/UserPolicyFields.vue";
import { recoveryDialogKind } from "../shared/desired-recovery";
import { useFleetStore } from "../shared/fleet";
import { operationStatusKey } from "../shared/operation-status";
import { workspaceChangedEvent } from "../api/client";

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
const sealedPassword = ref("");
const secretKeyId = ref("");
const groupMembers = ref("");
const desiredReason = ref("");
const policyDialog = ref<{ username: string }>();
const policyForm = ref<UserPolicyForm>(policyToForm());
const policyReason = ref("");
const policyLoading = ref(false);
const policyError = ref("");
const configDialog = ref(false);
const configPlan = ref<ConfigPlan>();
const configError = ref("");
const configLoading = ref(false);
const configPort = ref(443);
const configMaxClients = ref(128);
const configRoute = ref("default");
const configCertificateSecretRefId = ref("");
const configPrivateKeySecretRefId = ref("");
const configReason = ref("");
const configApplyApproval = ref("");
const configApplyReason = ref("");
const certificateDialog = ref(false);
const certificate = ref<Certificate>();
const certificateGrant = ref<ArtifactGrant>();
const certificateCommonName = ref("");
const certificateDnsNames = ref("");
const certificateReason = ref("");
const certificateApproval = ref("");
const certificateError = ref("");
const certificateLoading = ref(false);
const usersState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "user"),
);
const groupsState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "group"),
);
const operationBusy = computed(() => fleet.operationTracking);

async function selectRouteNode(): Promise<void> {
  const sequence = ++detailSequence;
  const nodeId = routeNodeId.value;
  detailLoading.value = true;
  detailState.value = "loading";
  if (!nodeId) {
    detailLoading.value = false;
    detailState.value = "not-found";
    return;
  }
  if (!fleet.initialized) await fleet.rebuild();
  if (sequence !== detailSequence) return;
  await fleet.select(nodeId);
  if (sequence !== detailSequence) return;
  detailLoading.value = false;
  if (currentNode.value) {
    detailState.value = "loading";
    return;
  }
  detailState.value =
    fleet.selectionError === "notFound" ||
    (fleet.initialized &&
      fleet.nodes.length > 0 &&
      !fleet.nodes.some((node) => node.id === nodeId))
      ? "not-found"
      : "unavailable";
}

async function initialize(): Promise<void> {
  const sequence = ++detailSequence;
  detailLoading.value = true;
  detailState.value = "loading";
  if (!fleet.initialized) await fleet.rebuild();
  if (sequence !== detailSequence) return;
  if (fleet.initialized) void fleet.connect();
  await selectRouteNode();
}

function refreshForWorkspace(): void {
  void initialize();
}

onMounted(() => {
  window.addEventListener(workspaceChangedEvent, refreshForWorkspace);
  void initialize();
});
onBeforeUnmount(() => {
  detailSequence += 1;
  window.removeEventListener(workspaceChangedEvent, refreshForWorkspace);
});
watch(routeNodeId, () => void selectRouteNode());

function pathMode(node: NodeObservedState): string {
  if (node.path?.mode === "relay") return "relay";
  if (node.path?.mode === "direct") return "direct";
  return "unknown";
}

function pathRtt(node: NodeObservedState): string {
  return node.path ? String(Math.round(node.path.rttMs)) : "";
}

const upgradeEligible = computed(
  () =>
    Boolean(currentNode.value?.agentUpgradeEligible) &&
    Boolean(currentNode.value?.recommendedAgentVersion),
);

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
  sealedPassword.value = "";
  secretKeyId.value = "";
  groupMembers.value =
    kind === "group"
      ? (
          groupsState.value.find((item) => item.name === name)
            ?.desiredMembers ?? []
        ).join(", ")
      : "";
  desiredReason.value = "";
}

async function submitDesired(): Promise<void> {
  const dialog = desiredDialog.value;
  const explanation = desiredReason.value.trim();
  if (!dialog || !explanation) return;
  desiredDialog.value = undefined;
  if (dialog.kind === "create")
    await fleet.createUser(
      desiredName.value.trim(),
      dialog.version,
      sealedPassword.value.trim(),
      secretKeyId.value.trim(),
      explanation,
    );
  else if (dialog.kind === "disable")
    await fleet.disableUser(dialog.name, dialog.version, explanation);
  else if (dialog.kind === "enable")
    await fleet.enableUser(dialog.name, dialog.version, explanation);
  else if (dialog.kind === "rotate")
    await fleet.rotateUserPassword(
      dialog.name,
      dialog.version,
      sealedPassword.value.trim(),
      secretKeyId.value.trim(),
      explanation,
    );
  else
    await fleet.applyGroup(
      desiredName.value.trim(),
      dialog.version,
      groupMembers.value
        .split(",")
        .map((value) => value.trim())
        .filter(Boolean)
        .sort(),
      explanation,
    );
}

async function openPolicy(username: string): Promise<void> {
  const nodeId = currentNode.value?.id;
  if (!nodeId) return;
  policyDialog.value = { username };
  policyForm.value = policyToForm();
  policyReason.value = "";
  policyError.value = "";
  policyLoading.value = true;
  try {
    const loaded = await loadUserPolicy(nodeId, username);
    if (
      currentNode.value?.id === nodeId &&
      policyDialog.value?.username === username
    )
      policyForm.value = loaded;
  } catch (error) {
    policyError.value =
      error instanceof Error ? error.message : t("policyLoadFailed");
  } finally {
    policyLoading.value = false;
  }
}

async function submitPolicy(): Promise<void> {
  const nodeId = currentNode.value?.id;
  if (!nodeId || !policyDialog.value || !policyReason.value.trim()) return;
  const username = policyDialog.value.username;
  policyLoading.value = true;
  policyError.value = "";
  try {
    policyForm.value = await saveUserPolicy(
      nodeId,
      username,
      policyForm.value,
      policyReason.value.trim(),
    );
    policyDialog.value = undefined;
  } catch (error) {
    policyError.value =
      error instanceof Error ? error.message : t("policyUpdateFailed");
  } finally {
    policyLoading.value = false;
  }
}

function openConfigPlan(): void {
  configDialog.value = true;
  configPlan.value = undefined;
  configError.value = "";
  configReason.value = "";
  configApplyApproval.value = "";
  configApplyReason.value = "";
}

async function submitConfigPlan(): Promise<void> {
  const node = currentNode.value;
  if (!node || !configReason.value.trim()) return;
  configLoading.value = true;
  configError.value = "";
  try {
    let plan = await createConfigPlan(node.id, {
      expectedRevision: 0,
      template: {
        name: "node-baseline",
        directives: [
          { name: "auth", value: "plain[passwd=/etc/ocserv/ocpasswd]" },
          { name: "max-clients", value: String(configMaxClients.value) },
          { name: "route", value: configRoute.value.trim() },
          {
            name: "server-cert",
            secretRef: {
              secretRefId: configCertificateSecretRefId.value.trim(),
            },
          },
          {
            name: "server-key",
            secretRef: {
              secretRefId: configPrivateKeySecretRefId.value.trim(),
            },
          },
          { name: "socket-file", value: "/run/ocserv.socket" },
          { name: "tcp-port", value: String(configPort.value) },
        ],
      },
      ttlSeconds: 900,
      reason: configReason.value.trim(),
    });
    configPlan.value = plan;
    for (
      let attempt = 0;
      attempt < 30 &&
      !["succeeded", "failed", "rejected", "unknown", "expired"].includes(
        plan.state,
      );
      attempt += 1
    ) {
      await new Promise((resolve) => setTimeout(resolve, 500));
      plan = await getConfigPlan(plan.id);
      configPlan.value = plan;
    }
  } catch (error) {
    configError.value =
      error instanceof Error ? error.message : t("configPlanFailed");
  } finally {
    configLoading.value = false;
  }
}

async function submitConfigApply(): Promise<void> {
  if (
    !configPlan.value ||
    !configApplyApproval.value.trim() ||
    !configApplyReason.value.trim()
  )
    return;
  configLoading.value = true;
  configError.value = "";
  try {
    const operation = await applyConfigPlan(configPlan.value.id, {
      approvalId: configApplyApproval.value.trim(),
      reason: configApplyReason.value.trim(),
    });
    configDialog.value = false;
    await fleet.trackOperation(operation.id);
  } catch (error) {
    configError.value =
      error instanceof Error ? error.message : t("configApplyFailed");
  } finally {
    configLoading.value = false;
  }
}

async function openCertificate(): Promise<void> {
  certificateDialog.value = true;
  certificate.value = undefined;
  certificateGrant.value = undefined;
  certificateCommonName.value = currentNode.value?.name ?? "";
  certificateDnsNames.value = "";
  certificateReason.value = "";
  certificateApproval.value = "";
  certificateError.value = "";
  const node = currentNode.value;
  if (!node) return;
  certificateLoading.value = true;
  try {
    const records = await listNodeCertificates(node.id);
    if (currentNode.value?.id === node.id)
      certificate.value = records.find((record) => record.state !== "revoked");
  } catch (error) {
    certificateError.value =
      error instanceof Error ? error.message : t("certificateRecordsFailed");
  } finally {
    certificateLoading.value = false;
  }
}

async function submitCertificateRequest(): Promise<void> {
  const node = currentNode.value;
  if (
    !node ||
    !certificateCommonName.value.trim() ||
    !certificateReason.value.trim()
  )
    return;
  certificateLoading.value = true;
  certificateError.value = "";
  try {
    let value = await createCertificate(node.id, {
      expectedVersion: node.version,
      commonName: certificateCommonName.value.trim(),
      dnsNames: new Set(
        certificateDnsNames.value
          .split(",")
          .map((name) => name.trim())
          .filter(Boolean),
      ),
      keyBits: 3072,
      reason: certificateReason.value.trim(),
    });
    certificate.value = value;
    for (
      let attempt = 0;
      attempt < 30 && value.state === "csr_pending";
      attempt += 1
    ) {
      await new Promise((resolve) => setTimeout(resolve, 500));
      value = await getCertificate(value.id);
      certificate.value = value;
    }
  } catch (error) {
    certificateError.value =
      error instanceof Error ? error.message : t("certificateRequestFailed");
  } finally {
    certificateLoading.value = false;
  }
}

async function submitCertificateIssue(): Promise<void> {
  if (
    !certificate.value ||
    !certificateApproval.value.trim() ||
    !certificateReason.value.trim()
  )
    return;
  certificateLoading.value = true;
  certificateError.value = "";
  try {
    certificate.value = await issueCertificate(certificate.value.id, {
      approvalId: certificateApproval.value.trim(),
      reason: certificateReason.value.trim(),
    });
  } catch (error) {
    certificateError.value =
      error instanceof Error ? error.message : t("certificateIssueFailed");
  } finally {
    certificateLoading.value = false;
  }
}

async function createP12(): Promise<void> {
  const node = currentNode.value;
  if (
    !certificate.value ||
    !node ||
    !certificateReason.value.trim() ||
    !certificateApproval.value.trim()
  )
    return;
  certificateLoading.value = true;
  certificateError.value = "";
  try {
    certificateGrant.value = await createCertificateP12(certificate.value.id, {
      expectedVersion: node.version,
      certificateVersion: certificate.value.version,
      approvalId: certificateApproval.value.trim(),
      reason: certificateReason.value.trim(),
    });
    await fleet.trackOperation(certificateGrant.value.operation.id);
  } catch (error) {
    certificateError.value =
      error instanceof Error ? error.message : t("p12CreationFailed");
  } finally {
    certificateLoading.value = false;
  }
}

async function downloadP12(): Promise<void> {
  const grant = certificateGrant.value;
  if (!grant?.downloadToken) return;
  certificateLoading.value = true;
  certificateError.value = "";
  try {
    const blob = await downloadCertificateArtifact(
      grant.artifactId,
      grant.downloadToken,
    );
    const link = document.createElement("a");
    link.href = URL.createObjectURL(blob);
    link.download = "certificate.p12";
    link.click();
    URL.revokeObjectURL(link.href);
    const { downloadToken: _consumed, ...consumedGrant } = grant;
    certificateGrant.value = consumedGrant;
  } catch (error) {
    certificateError.value =
      error instanceof Error ? error.message : t("p12DownloadFailed");
  } finally {
    certificateLoading.value = false;
  }
}

async function revokeCurrentCertificate(): Promise<void> {
  const node = currentNode.value;
  if (
    !certificate.value ||
    !node ||
    !certificateReason.value.trim() ||
    !certificateApproval.value.trim()
  )
    return;
  certificateLoading.value = true;
  certificateError.value = "";
  try {
    const operation = await revokeCertificate(certificate.value.id, {
      expectedVersion: node.version,
      certificateVersion: certificate.value.version,
      approvalId: certificateApproval.value.trim(),
      reason: certificateReason.value.trim(),
    });
    await fleet.trackOperation(operation.id);
    certificate.value = await getCertificate(certificate.value.id);
  } catch (error) {
    certificateError.value =
      error instanceof Error ? error.message : t("certificateRevokeFailed");
  } finally {
    certificateLoading.value = false;
  }
}
</script>

<template>
  <main class="overview node-detail-view">
    <div class="page-heading">
      <div>
        <RouterLink class="back-link" to="/nodes"
          ><ArrowLeft :size="15" />{{ $t("backToNodes") }}</RouterLink
        >
        <p>{{ $t("nodeDetail") }}</p>
        <h1>{{ currentNode?.name ?? routeNodeId }}</h1>
      </div>
      <span class="health" :class="{ unavailable: fleet.unavailable }"
        ><i></i
        >{{
          $t(fleet.unavailable ? "systemsUnavailable" : "liveTelemetry")
        }}</span
      >
    </div>

    <div v-if="detailLoading" class="detail-state" role="status">
      <Server :size="24" /><span>{{ $t("nodeLoading") }}</span>
    </div>
    <div v-else-if="detailState === 'not-found'" class="detail-state">
      <Server :size="24" /><span>{{ $t("nodeNotFound") }}</span>
    </div>
    <div
      v-else-if="detailState === 'unavailable'"
      class="detail-state"
      role="alert"
    >
      <Server :size="24" /><span>{{ $t("nodeUnavailable") }}</span>
    </div>

    <template v-else-if="currentNode">
      <nav class="detail-nav" :aria-label="$t('nodeDetail')">
        <a href="#node-overview">{{ $t("nodeOverview") }}</a>
        <a href="#node-sessions">{{ $t("sessions") }}</a>
        <a href="#node-users-groups">{{ $t("usersAndGroups") }}</a>
        <a href="#node-configuration">{{ $t("configurationSection") }}</a>
        <a href="#node-certificates">{{ $t("certificatesSection") }}</a>
      </nav>

      <section class="node-detail node-detail-page">
        <section id="node-overview" class="detail-section">
          <header>
            <div>
              <span>{{ $t("observedState") }}</span>
              <h2>{{ currentNode.name }}</h2>
            </div>
            <span class="freshness-badge" :class="currentNode.freshness">{{
              $t(currentNode.freshness)
            }}</span>
          </header>
          <p
            v-if="currentNode.freshness === 'stale'"
            class="state-banner"
            role="status"
          >
            {{ $t("staleNode") }}
          </p>
          <div class="node-actions">
            <button
              type="button"
              :disabled="operationBusy"
              :title="$t('reloadOcserv')"
              @click="openAction('reload', '', $t('reloadOcserv'))"
            >
              <Power :size="15" />{{ $t("reload") }}
            </button>
            <button
              v-if="upgradeEligible"
              type="button"
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
              <ArrowUpCircle :size="15" />{{ $t("upgradeAgent") }}
            </button>
          </div>
          <div
            v-if="fleet.latestOperation"
            class="operation-status"
            aria-live="polite"
          >
            <span>{{ $t("latestOperation") }}</span>
            <strong :class="fleet.latestOperation.state">{{
              $t(operationStatusKey(fleet.latestOperation))
            }}</strong>
            <code v-if="fleet.latestOperation.agentUpgradeTargetVersion">{{
              fleet.latestOperation.agentUpgradeTargetVersion
            }}</code>
            <button
              v-if="
                fleet.operationTracking &&
                fleet.latestOperation.state === 'unknown'
              "
              type="button"
              class="icon-command"
              :title="$t('stopTrackingOperation')"
              :aria-label="$t('stopTrackingOperation')"
              @click="fleet.detachOperation"
            >
              <CircleStop :size="15" />
            </button>
          </div>
          <p v-if="fleet.operationError" class="operation-error" role="alert">
            {{ fleet.operationError }}
          </p>
          <dl>
            <div>
              <dt><Cable :size="15" />{{ $t("path") }}</dt>
              <dd>
                <span>{{ $t(pathMode(currentNode)) }}</span>
                <template v-if="currentNode.path">
                  · {{ pathRtt(currentNode) }} {{ $t("milliseconds") }}
                </template>
              </dd>
            </div>
            <div>
              <dt><Gauge :size="15" />{{ $t("ocserv") }}</dt>
              <dd>{{ currentNode.ocservVersion ?? $t("notAvailable") }}</dd>
            </div>
            <div>
              <dt><Server :size="15" />{{ $t("agent") }}</dt>
              <dd>
                {{ currentNode.agentVersion ?? $t("notAvailable") }}
                <span
                  v-if="currentNode.agentVersionState"
                  class="version-badge"
                  :class="currentNode.agentVersionState"
                  >{{ $t(currentNode.agentVersionState) }}</span
                >
              </dd>
            </div>
            <div>
              <dt>{{ $t("versionState") }}</dt>
              <dd>
                {{
                  currentNode.agentVersionState
                    ? $t(currentNode.agentVersionState)
                    : $t("notAvailable")
                }}
              </dd>
            </div>
            <div>
              <dt>{{ $t("recommendedAgentVersion") }}</dt>
              <dd>
                {{ currentNode.recommendedAgentVersion ?? $t("notAvailable") }}
              </dd>
            </div>
            <div>
              <dt>{{ $t("osRelease") }}</dt>
              <dd>{{ currentNode.osRelease ?? $t("notAvailable") }}</dd>
            </div>
            <div>
              <dt><Users :size="15" />{{ $t("sessions") }}</dt>
              <dd>{{ fleet.sessions.length }}</dd>
            </div>
            <div>
              <dt>{{ $t("trust") }}</dt>
              <dd>{{ $t(currentNode.trustStatus) }}</dd>
            </div>
            <div>
              <dt>{{ $t("connection") }}</dt>
              <dd>{{ $t(currentNode.connectionState) }}</dd>
            </div>
            <div>
              <dt>{{ $t("lastHeartbeat") }}</dt>
              <dd>
                {{
                  currentNode.lastHeartbeatAt?.toLocaleString() ??
                  $t("notObserved")
                }}
              </dd>
            </div>
          </dl>
        </section>

        <section id="node-sessions" class="detail-section">
          <header>
            <h2>{{ $t("sessions") }}</h2>
          </header>
          <div class="session-list">
            <h3>{{ $t("currentSessions") }}</h3>
            <div v-for="session in fleet.sessions" :key="session.id">
              <span class="session-identity"
                ><strong>{{ session.username }}</strong
                ><span>{{ session.clientIp }}</span></span
              >
              <span class="session-actions">
                <button
                  type="button"
                  :disabled="operationBusy || !currentNode.bootId"
                  :title="$t('disconnect')"
                  @click="
                    openAction('disconnect', session.id, $t('disconnect'))
                  "
                >
                  <LogOut :size="14" />
                </button>
                <button
                  type="button"
                  class="danger"
                  :disabled="operationBusy || !currentNode.bootId"
                  :title="$t('terminate')"
                  @click="openAction('terminate', session.id, $t('terminate'))"
                >
                  <ShieldOff :size="14" />
                </button>
              </span>
            </div>
            <p v-if="fleet.sessions.length === 0">{{ $t("noSessions") }}</p>
          </div>
          <div class="session-list ban-list">
            <h3>{{ $t("ipBans") }}</h3>
            <div v-for="ban in fleet.ipBans" :key="ban.ip">
              <span class="session-identity"
                ><strong>{{ ban.ip }}</strong></span
              >
              <span class="session-actions">
                <button
                  type="button"
                  :disabled="operationBusy"
                  :title="$t('removeBan')"
                  @click="openAction('unban', ban.ip, $t('removeBan'))"
                >
                  <Ban :size="14" />
                </button>
              </span>
            </div>
            <p v-if="fleet.ipBans.length === 0">{{ $t("noIpBans") }}</p>
          </div>
        </section>

        <section id="node-users-groups" class="detail-section">
          <header>
            <h2>{{ $t("usersAndGroups") }}</h2>
          </header>
          <div class="state-toolbar">
            <div class="segmented" role="tablist">
              <button
                type="button"
                :class="{ active: stateTab === 'users' }"
                @click="stateTab = 'users'"
              >
                {{ $t("users") }}
              </button>
              <button
                type="button"
                :class="{ active: stateTab === 'groups' }"
                @click="stateTab = 'groups'"
              >
                {{ $t("groups") }}
              </button>
            </div>
            <button
              v-if="stateTab === 'users'"
              type="button"
              class="icon-command"
              :title="$t('createUser')"
              @click="openDesired('create')"
            >
              <UserPlus :size="15" />
            </button>
            <button
              v-else
              type="button"
              class="icon-command"
              :title="$t('applyGroup')"
              @click="openDesired('group')"
            >
              <ListPlus :size="15" />
            </button>
          </div>
          <div class="desired-state-list" v-if="stateTab === 'users'">
            <div v-for="item in usersState" :key="item.name">
              <span
                ><strong>{{ item.name }}</strong
                ><small :class="item.convergence">{{
                  $t("convergence_" + item.convergence)
                }}</small
                ><small
                  v-if="item.recoveryRequired && !item.recoveryMutationKind"
                  class="drifted"
                  >{{ $t("manualReconciliationRequired") }}</small
                ></span
              >
              <span class="session-actions">
                <button
                  v-if="item.desiredVersion"
                  type="button"
                  :disabled="operationBusy"
                  :title="$t('quotaAndExpiry')"
                  @click="openPolicy(item.name)"
                >
                  <SlidersHorizontal :size="14" />
                </button>
                <button
                  v-if="recoveryDialogKind(item) === 'create'"
                  type="button"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('retryCreateUser')"
                  @click="
                    openDesired('create', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserPlus :size="14" />
                </button>
                <button
                  v-else-if="recoveryDialogKind(item) === 'rotate'"
                  type="button"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('retryRotatePassword')"
                  @click="
                    openDesired('rotate', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <KeyRound :size="14" />
                </button>
                <button
                  v-else-if="recoveryDialogKind(item) === 'disable'"
                  type="button"
                  class="danger"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('retryDisableUser')"
                  @click="
                    openDesired('disable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserX :size="14" />
                </button>
                <button
                  v-else-if="recoveryDialogKind(item) === 'enable'"
                  type="button"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('retryEnableUser')"
                  @click="
                    openDesired('enable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserCheck :size="14" />
                </button>
                <button
                  v-if="!item.recoveryRequired"
                  type="button"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('rotatePassword')"
                  @click="
                    openDesired('rotate', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <KeyRound :size="14" />
                </button>
                <button
                  v-if="!item.recoveryRequired && item.desiredEnabled === false"
                  type="button"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('enableUser')"
                  @click="
                    openDesired('enable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserCheck :size="14" />
                </button>
                <button
                  v-else-if="!item.recoveryRequired"
                  type="button"
                  class="danger"
                  :disabled="operationBusy || !item.desiredVersion"
                  :title="$t('disableUser')"
                  @click="
                    openDesired('disable', item.name, item.desiredVersion ?? 0)
                  "
                >
                  <UserX :size="14" />
                </button>
              </span>
            </div>
            <p v-if="usersState.length === 0">{{ $t("noUsers") }}</p>
          </div>
          <div class="desired-state-list" v-else>
            <div v-for="item in groupsState" :key="item.name">
              <span
                ><strong>{{ item.name }}</strong
                ><small :class="item.convergence">{{
                  $t("convergence_" + item.convergence)
                }}</small
                ><small
                  v-if="item.recoveryRequired && !item.recoveryMutationKind"
                  class="drifted"
                  >{{ $t("manualReconciliationRequired") }}</small
                ></span
              >
              <button
                v-if="recoveryDialogKind(item) === 'group'"
                type="button"
                class="icon-command"
                :disabled="operationBusy"
                :title="$t('retryApplyGroup')"
                @click="
                  openDesired('group', item.name, item.desiredVersion ?? 0)
                "
              >
                <ListPlus :size="14" />
              </button>
              <button
                v-else-if="!item.recoveryRequired"
                type="button"
                class="icon-command"
                :disabled="operationBusy"
                :title="$t('applyGroup')"
                @click="
                  openDesired('group', item.name, item.desiredVersion ?? 0)
                "
              >
                <ListPlus :size="14" />
              </button>
            </div>
            <p v-if="groupsState.length === 0">{{ $t("noGroups") }}</p>
          </div>
        </section>

        <section id="node-configuration" class="detail-section">
          <header>
            <h2>{{ $t("configurationSection") }}</h2>
          </header>
          <div class="detail-section-actions">
            <FileCheck2 :size="18" />
            <span>{{ $t("configPlan") }}</span>
            <button
              type="button"
              :disabled="operationBusy"
              :title="$t('configPlan')"
              @click="openConfigPlan"
            >
              {{ $t("plan") }}
            </button>
          </div>
        </section>

        <section id="node-certificates" class="detail-section">
          <header>
            <h2>{{ $t("certificatesSection") }}</h2>
          </header>
          <div class="detail-section-actions">
            <span>{{ $t("certificateLifecycle") }}</span>
            <button
              type="button"
              :disabled="operationBusy"
              :title="$t('certificateLifecycle')"
              @click="openCertificate"
            >
              {{ $t("certificate") }}
            </button>
          </div>
        </section>
      </section>
    </template>

    <div
      v-if="certificateDialog"
      class="dialog-backdrop"
      @click.self="certificateDialog = false"
    >
      <form class="operation-dialog" @submit.prevent="submitCertificateRequest">
        <header>
          <h2>{{ $t("certificateLifecycle") }}</h2>
          <code>{{ currentNode?.name }}</code>
        </header>
        <template v-if="!certificate">
          <label for="certificate-cn">{{ $t("commonName") }}</label>
          <input
            id="certificate-cn"
            v-model="certificateCommonName"
            maxlength="253"
            required
          />
          <label for="certificate-dns">{{ $t("dnsNames") }}</label>
          <input
            id="certificate-dns"
            v-model="certificateDnsNames"
            maxlength="4096"
          />
        </template>
        <template v-else>
          <div class="config-plan-result" aria-live="polite">
            <span class="freshness-badge" :class="certificate.state">{{
              certificate.state
            }}</span>
            <code>{{ certificate.id }}</code>
            <small v-if="certificate.notAfter"
              >{{ $t("expires") }}
              {{ certificate.notAfter.toLocaleString() }}</small
            >
          </div>
          <template
            v-if="
              certificate.state === 'csr_ready' ||
              certificate.state === 'signer_unavailable'
            "
          >
            <label for="certificate-approval">{{ $t("approvalId") }}</label>
            <input
              id="certificate-approval"
              v-model="certificateApproval"
              autocomplete="off"
              required
            />
          </template>
          <template v-if="certificateGrant">
            <label for="certificate-password">{{ $t("p12Password") }}</label>
            <input
              id="certificate-password"
              :value="certificateGrant.password"
              readonly
              autocomplete="off"
            />
            <small>{{ $t("oneTimeCredential") }}</small>
          </template>
        </template>
        <label for="certificate-reason">{{ $t("reason") }}</label>
        <textarea
          id="certificate-reason"
          v-model="certificateReason"
          maxlength="512"
          required
        ></textarea>
        <p v-if="certificateError" class="operation-error" role="alert">
          {{ certificateError }}
        </p>
        <footer>
          <button type="button" @click="certificateDialog = false">
            {{ $t("cancel") }}
          </button>
          <button
            v-if="!certificate"
            type="submit"
            class="primary"
            :disabled="certificateLoading || !certificateReason.trim()"
          >
            {{ $t("requestCsr") }}
          </button>
          <button
            v-else-if="
              certificate.state === 'csr_ready' ||
              certificate.state === 'signer_unavailable'
            "
            type="button"
            class="primary"
            :disabled="
              certificateLoading ||
              !certificateApproval.trim() ||
              !certificateReason.trim()
            "
            @click="submitCertificateIssue"
          >
            {{ $t("issueCertificate") }}
          </button>
          <template
            v-else-if="
              certificate.state === 'issued' || certificate.state === 'expiring'
            "
          >
            <button
              type="button"
              :disabled="certificateLoading || !certificateReason.trim()"
              @click="createP12"
            >
              <KeyRound :size="15" />{{ $t("createP12") }}
            </button>
            <button
              v-if="certificateGrant?.downloadToken"
              type="button"
              class="primary"
              :disabled="certificateLoading"
              @click="downloadP12"
            >
              <Download :size="15" />{{ $t("download") }}
            </button>
            <button
              type="button"
              class="danger"
              :disabled="certificateLoading || !certificateReason.trim()"
              @click="revokeCurrentCertificate"
            >
              {{ $t("revoke") }}
            </button>
          </template>
        </footer>
      </form>
    </div>

    <div
      v-if="pendingAction"
      class="dialog-backdrop"
      @click.self="pendingAction = undefined"
    >
      <form class="operation-dialog" @submit.prevent="submitAction">
        <header>
          <h2>{{ pendingAction.label }}</h2>
          <code v-if="pendingAction.target">{{ pendingAction.target }}</code>
        </header>
        <template v-if="pendingAction.kind === 'upgradeAgent'">
          <label for="upgrade-target">{{ $t("targetVersion") }}</label>
          <output id="upgrade-target" class="read-only-value">{{
            pendingAction.target
          }}</output>
          <p class="dialog-hint">{{ $t("upgradeTargetReadOnly") }}</p>
        </template>
        <label for="operation-reason">{{ $t("reason") }}</label>
        <textarea
          id="operation-reason"
          v-model="reason"
          maxlength="512"
          required
        ></textarea>
        <template
          v-if="
            pendingAction.kind === 'reload' ||
            pendingAction.kind === 'upgradeAgent'
          "
        >
          <label for="approval-id">{{ $t("approvalId") }}</label>
          <input
            id="approval-id"
            v-model="approvalId"
            autocomplete="off"
            required
          />
        </template>
        <footer>
          <button type="button" @click="pendingAction = undefined">
            {{ $t("cancel") }}
          </button>
          <button
            type="submit"
            class="primary"
            :disabled="
              !reason.trim() ||
              ((pendingAction.kind === 'reload' ||
                pendingAction.kind === 'upgradeAgent') &&
                !approvalId.trim())
            "
          >
            {{ $t("confirm") }}
          </button>
        </footer>
      </form>
    </div>

    <div
      v-if="desiredDialog"
      class="dialog-backdrop"
      @click.self="desiredDialog = undefined"
    >
      <form class="operation-dialog" @submit.prevent="submitDesired">
        <header>
          <h2>
            {{
              $t(
                desiredDialog.kind === "create"
                  ? "createUser"
                  : desiredDialog.kind === "disable"
                    ? "disableUser"
                    : desiredDialog.kind === "enable"
                      ? "enableUser"
                      : desiredDialog.kind === "rotate"
                        ? "rotatePassword"
                        : "applyGroup",
              )
            }}
          </h2>
          <code v-if="desiredDialog.name">{{ desiredDialog.name }}</code>
        </header>
        <template
          v-if="
            desiredDialog.kind === 'create' || desiredDialog.kind === 'group'
          "
          ><label for="desired-name">{{
            $t(desiredDialog.kind === "group" ? "group" : "user")
          }}</label
          ><input
            id="desired-name"
            v-model="desiredName"
            :disabled="
              desiredDialog.kind === 'create' && desiredDialog.version > 0
            "
            maxlength="64"
            required
        /></template>
        <template
          v-if="
            desiredDialog.kind === 'create' || desiredDialog.kind === 'rotate'
          "
          ><label for="secret-key-id">{{ $t("secretKeyId") }}</label
          ><input
            id="secret-key-id"
            v-model="secretKeyId"
            maxlength="128"
            autocomplete="off"
            required
          /><label for="sealed-password">{{ $t("sealedPassword") }}</label
          ><textarea
            id="sealed-password"
            v-model="sealedPassword"
            maxlength="5464"
            autocomplete="off"
            required
          ></textarea>
        </template>
        <template v-if="desiredDialog.kind === 'group'"
          ><label for="group-members">{{ $t("members") }}</label
          ><textarea
            id="group-members"
            v-model="groupMembers"
            maxlength="65535"
          ></textarea>
        </template>
        <label for="desired-reason">{{ $t("reason") }}</label
        ><textarea
          id="desired-reason"
          v-model="desiredReason"
          maxlength="512"
          required
        ></textarea>
        <footer>
          <button type="button" @click="desiredDialog = undefined">
            {{ $t("cancel") }}</button
          ><button
            type="submit"
            class="primary"
            :disabled="!desiredReason.trim()"
          >
            {{ $t("confirm") }}
          </button>
        </footer>
      </form>
    </div>

    <div
      v-if="policyDialog"
      class="dialog-backdrop"
      @click.self="policyDialog = undefined"
    >
      <form class="operation-dialog" @submit.prevent="submitPolicy">
        <header>
          <h2>{{ $t("quotaAndExpiry") }}</h2>
          <code>{{ policyDialog.username }}</code>
        </header>
        <UserPolicyFields v-model="policyForm" />
        <label for="policy-reason">{{ $t("reason") }}</label>
        <textarea
          id="policy-reason"
          v-model="policyReason"
          maxlength="512"
          required
        ></textarea>
        <p v-if="policyError" class="operation-error" role="alert">
          {{ policyError }}
        </p>
        <footer>
          <button type="button" @click="policyDialog = undefined">
            {{ $t("cancel") }}
          </button>
          <button
            type="submit"
            class="primary"
            :disabled="policyLoading || !policyReason.trim()"
          >
            {{ $t("confirm") }}
          </button>
        </footer>
      </form>
    </div>

    <div
      v-if="configDialog"
      class="dialog-backdrop"
      @click.self="configDialog = false"
    >
      <form
        class="operation-dialog config-plan-dialog"
        @submit.prevent="submitConfigPlan"
      >
        <header>
          <h2>{{ $t("configPlan") }}</h2>
          <code>{{ currentNode?.name }}</code>
        </header>
        <label for="config-port">{{ $t("tcpPort") }}</label>
        <input
          id="config-port"
          v-model.number="configPort"
          type="number"
          min="1"
          max="65535"
          required
        />
        <label for="config-clients">{{ $t("maxClients") }}</label>
        <input
          id="config-clients"
          v-model.number="configMaxClients"
          type="number"
          min="1"
          max="100000"
          required
        />
        <label for="config-route">{{ $t("route") }}</label>
        <input
          id="config-route"
          v-model="configRoute"
          maxlength="256"
          required
        />
        <label for="config-certificate-key">{{ $t("certificateRef") }}</label>
        <input
          id="config-certificate-key"
          v-model="configCertificateSecretRefId"
          maxlength="36"
          required
        />
        <label for="config-private-key">{{ $t("privateKeyRef") }}</label>
        <input
          id="config-private-key"
          v-model="configPrivateKeySecretRefId"
          maxlength="36"
          required
        />
        <label for="config-reason">{{ $t("reason") }}</label>
        <textarea
          id="config-reason"
          v-model="configReason"
          maxlength="512"
          required
        ></textarea>
        <div v-if="configPlan" class="config-plan-result" aria-live="polite">
          <span class="freshness-badge" :class="configPlan.validation">{{
            configPlan.validation
          }}</span>
          <code>{{ configPlan.candidateHash }}</code>
          <pre v-if="configPlan.diffRedacted">{{
            configPlan.diffRedacted
          }}</pre>
          <ul v-if="configPlan.warnings.length">
            <li v-for="warning in configPlan.warnings" :key="warning">
              {{ warning }}
            </li>
          </ul>
          <template v-if="configPlan.validation === 'valid'">
            <label for="config-apply-approval">{{ $t("approvalId") }}</label>
            <input
              id="config-apply-approval"
              v-model="configApplyApproval"
              required
            />
            <label for="config-apply-reason">{{ $t("reason") }}</label>
            <textarea
              id="config-apply-reason"
              v-model="configApplyReason"
              maxlength="512"
            ></textarea>
            <button
              type="button"
              class="primary"
              :disabled="
                configLoading ||
                !configApplyApproval.trim() ||
                !configApplyReason.trim()
              "
              @click="submitConfigApply"
            >
              {{ $t("apply") }}
            </button>
          </template>
        </div>
        <p v-if="configError" class="operation-error" role="alert">
          {{ configError }}
        </p>
        <footer>
          <button type="button" @click="configDialog = false">
            {{ $t("cancel") }}
          </button>
          <button
            type="submit"
            class="primary"
            :disabled="configLoading || !configReason.trim()"
          >
            {{ $t("plan") }}
          </button>
        </footer>
      </form>
    </div>
  </main>
</template>
