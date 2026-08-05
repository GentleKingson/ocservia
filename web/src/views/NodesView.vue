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
  </main>
</template>
