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
  KeyRound,
  ListPlus,
} from "@lucide/vue";
import { computed, onMounted, ref } from "vue";

import { useFleetStore } from "../shared/fleet";

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
  kind: "create" | "disable" | "rotate" | "group";
  name: string;
  version: number;
}>();
const desiredName = ref("");
const sealedPassword = ref("");
const secretKeyId = ref("");
const groupMembers = ref("");
const desiredReason = ref("");
const usersState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "user"),
);
const groupsState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "group"),
);
const operationBusy = computed(() =>
  fleet.latestOperation
    ? ![
        "succeeded",
        "failed",
        "unknown",
        "expired",
        "rolled_back",
        "drifted",
        "superseded",
      ].includes(fleet.latestOperation.state)
    : false,
);
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
  kind: "create" | "disable" | "rotate" | "group",
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
      sealedPassword.value.trim(),
      secretKeyId.value.trim(),
      explanation,
    );
  else if (dialog.kind === "disable")
    await fleet.disableUser(dialog.name, dialog.version, explanation);
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
              }}</small></span
            >
            <span class="session-actions">
              <button
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
                type="button"
                class="danger"
                :disabled="
                  operationBusy ||
                  !item.desiredVersion ||
                  item.desiredEnabled === false
                "
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
              }}</small></span
            >
            <button
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
  </main>
</template>
