<script setup lang="ts">
import {
  Ban,
  Cable,
  Gauge,
  LogOut,
  Power,
  Radio,
  Server,
  ShieldOff,
  Users,
  UserPlus,
  UserX,
  UserCheck,
  KeyRound,
  ListPlus,
  CircleStop,
  SlidersHorizontal,
  FileCheck2,
} from "@lucide/vue";
import type { ConfigPlan } from "@ocservia/api-client";
import { computed, onMounted, ref } from "vue";

import { useFleetStore } from "../shared/fleet";
import { recoveryDialogKind } from "../shared/desired-recovery";
import {
  loadUserPolicy,
  policyToForm,
  saveUserPolicy,
  type UserPolicyForm,
} from "../adapters/user-policy";
import UserPolicyFields from "../upstream/UserPolicyFields.vue";
import {
  applyConfigPlan,
  createConfigPlan,
  getConfigPlan,
} from "../api/client";

const fleet = useFleetStore();
const pendingAction = ref<{
  kind: "disconnect" | "terminate" | "unban" | "reload";
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
const configSecretProvider = ref("node");
const configCertificateKey = ref("tls/server-certificate");
const configPrivateKey = ref("tls/server-private-key");
const configReason = ref("");
const configApplyApproval = ref("");
const configApplyReason = ref("");
const usersState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "user"),
);
const groupsState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "group"),
);
const operationBusy = computed(() => fleet.operationTracking);
onMounted(async () => {
  await fleet.rebuild();
  void fleet.connect();
});

function pathLabel(node: { path?: { mode: string; rttMs: number } }): string {
  if (!node.path) return "Unknown";
  return `${node.path.mode === "relay" ? "Relay" : node.path.mode === "direct" ? "Direct" : "Unknown"} · ${Math.round(node.path.rttMs)} ms`;
}

function openAction(
  kind: "disconnect" | "terminate" | "unban" | "reload",
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
  if (!fleet.selected) return;
  const nodeId = fleet.selected.id;
  policyDialog.value = { username };
  policyForm.value = policyToForm();
  policyReason.value = "";
  policyError.value = "";
  policyLoading.value = true;
  try {
    const loaded = await loadUserPolicy(nodeId, username);
    if (
      fleet.selected?.id === nodeId &&
      policyDialog.value?.username === username
    )
      policyForm.value = loaded;
  } catch (error) {
    policyError.value =
      error instanceof Error ? error.message : "Policy load failed";
  } finally {
    policyLoading.value = false;
  }
}

async function submitPolicy(): Promise<void> {
  if (!fleet.selected || !policyDialog.value || !policyReason.value.trim())
    return;
  const nodeId = fleet.selected.id;
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
      error instanceof Error ? error.message : "Policy update failed";
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
  if (!fleet.selected || !configReason.value.trim()) return;
  configLoading.value = true;
  configError.value = "";
  try {
    let plan = await createConfigPlan(fleet.selected.id, {
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
              provider: configSecretProvider.value.trim(),
              key: configCertificateKey.value.trim(),
            },
          },
          {
            name: "server-key",
            secretRef: {
              provider: configSecretProvider.value.trim(),
              key: configPrivateKey.value.trim(),
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
      error instanceof Error ? error.message : "Configuration plan failed";
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
      error instanceof Error ? error.message : "Configuration apply failed";
  } finally {
    configLoading.value = false;
  }
}
</script>

<template>
  <main class="overview nodes-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("nodes") }}</h1>
      </div>
      <span class="health" :class="{ unavailable: fleet.unavailable }"
        ><i></i
        >{{
          $t(fleet.unavailable ? "systemsUnavailable" : "liveTelemetry")
        }}</span
      >
    </div>
    <section class="metrics" aria-label="Fleet status">
      <article>
        <div>
          <span>{{ $t("onlineNodes") }}</span
          ><strong>{{ fleet.online }} / {{ fleet.nodes.length }}</strong>
        </div>
        <Server :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("relayPaths") }}</span
          ><strong>{{ fleet.relay }}</strong>
        </div>
        <Radio :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("activeSessions") }}</span
          ><strong>{{ fleet.sessionCount }}</strong>
        </div>
        <Users :size="20" />
      </article>
    </section>
    <section class="fleet-layout">
      <div class="node-table-wrap">
        <table class="node-table">
          <thead>
            <tr>
              <th>{{ $t("node") }}</th>
              <th>{{ $t("freshness") }}</th>
              <th>{{ $t("path") }}</th>
              <th>{{ $t("ocserv") }}</th>
              <th>{{ $t("sessions") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="node in fleet.nodes"
              :key="node.id"
              :class="{ selected: fleet.selected?.id === node.id }"
              @click="fleet.select(node.id)"
            >
              <td>
                <strong>{{ node.name }}</strong
                ><code>{{ node.id.slice(0, 8) }}</code>
              </td>
              <td>
                <span class="state-dot" :class="node.freshness"></span
                >{{ $t(node.freshness) }}
              </td>
              <td>{{ pathLabel(node) }}</td>
              <td>{{ node.ocservVersion ?? "—" }}</td>
              <td>{{ node.sessionCount }}</td>
            </tr>
          </tbody>
        </table>
        <div
          v-if="!fleet.loading && fleet.nodes.length === 0"
          class="empty-state"
        >
          <Server :size="24" /><span>{{ $t("noNodes") }}</span>
        </div>
      </div>
      <aside class="node-detail" v-if="fleet.selected">
        <header>
          <div>
            <span>{{ $t("observedState") }}</span>
            <h2>{{ fleet.selected.name }}</h2>
          </div>
          <span class="freshness-badge" :class="fleet.selected.freshness">{{
            $t(fleet.selected.freshness)
          }}</span>
        </header>
        <div class="node-actions">
          <button
            type="button"
            :disabled="operationBusy"
            :title="$t('configPlan')"
            @click="openConfigPlan"
          >
            <FileCheck2 :size="15" />{{ $t("plan") }}
          </button>
          <button
            type="button"
            :disabled="operationBusy"
            :title="$t('reloadOcserv')"
            @click="openAction('reload', '', $t('reloadOcserv'))"
          >
            <Power :size="15" />{{ $t("reload") }}
          </button>
        </div>
        <div
          v-if="fleet.latestOperation"
          class="operation-status"
          aria-live="polite"
        >
          <span>{{ $t("latestOperation") }}</span>
          <strong :class="fleet.latestOperation.state">{{
            $t(`operation_${fleet.latestOperation.state}`)
          }}</strong>
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
            <dd>{{ pathLabel(fleet.selected) }}</dd>
          </div>
          <div>
            <dt><Gauge :size="15" />{{ $t("ocserv") }}</dt>
            <dd>{{ fleet.selected.ocservVersion ?? "—" }}</dd>
          </div>
          <div>
            <dt><Server :size="15" />{{ $t("agent") }}</dt>
            <dd>{{ fleet.selected.agentVersion ?? "—" }}</dd>
          </div>
          <div>
            <dt><Users :size="15" />{{ $t("sessions") }}</dt>
            <dd>{{ fleet.sessions.length }}</dd>
          </div>
        </dl>
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
                $t(`convergence_${item.convergence}`)
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
                $t(`convergence_${item.convergence}`)
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
              @click="openDesired('group', item.name, item.desiredVersion ?? 0)"
            >
              <ListPlus :size="14" />
            </button>
            <button
              v-else-if="!item.recoveryRequired"
              type="button"
              class="icon-command"
              :disabled="operationBusy"
              :title="$t('applyGroup')"
              @click="openDesired('group', item.name, item.desiredVersion ?? 0)"
            >
              <ListPlus :size="14" />
            </button>
          </div>
          <p v-if="groupsState.length === 0">{{ $t("noGroups") }}</p>
        </div>
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
                :disabled="operationBusy || !fleet.selected.bootId"
                :title="$t('disconnect')"
                @click="openAction('disconnect', session.id, $t('disconnect'))"
              >
                <LogOut :size="14" />
              </button>
              <button
                type="button"
                class="danger"
                :disabled="operationBusy || !fleet.selected.bootId"
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
      </aside>
    </section>
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
        <label for="operation-reason">{{ $t("reason") }}</label>
        <textarea
          id="operation-reason"
          v-model="reason"
          maxlength="512"
          required
        ></textarea>
        <template v-if="pendingAction.kind === 'reload'">
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
              (pendingAction.kind === 'reload' && !approvalId.trim())
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
          <code>{{ fleet.selected?.name }}</code>
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
        <label for="config-secret-provider">{{ $t("secretProvider") }}</label>
        <input
          id="config-secret-provider"
          v-model="configSecretProvider"
          maxlength="64"
          required
        />
        <label for="config-certificate-key">{{ $t("certificateRef") }}</label>
        <input
          id="config-certificate-key"
          v-model="configCertificateKey"
          maxlength="256"
          required
        />
        <label for="config-private-key">{{ $t("privateKeyRef") }}</label>
        <input
          id="config-private-key"
          v-model="configPrivateKey"
          maxlength="256"
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
