<script setup lang="ts">
import { Cable, Gauge, Radio, Server, Users } from "@lucide/vue";
import { onMounted } from "vue";

import { useFleetStore } from "../shared/fleet";

const fleet = useFleetStore();
onMounted(async () => {
  await fleet.rebuild();
  fleet.connect();
});

function pathLabel(node: { path?: { mode: string; rttMs: number } }): string {
  if (!node.path) return "Unknown";
  return `${node.path.mode === "relay" ? "Relay" : node.path.mode === "direct" ? "Direct" : "Unknown"} · ${Math.round(node.path.rttMs)} ms`;
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
            <strong>{{ session.username }}</strong
            ><span>{{ session.clientIp }}</span>
          </div>
          <p v-if="fleet.sessions.length === 0">{{ $t("noSessions") }}</p>
        </div>
      </aside>
    </section>
  </main>
</template>
