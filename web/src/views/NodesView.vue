<script setup lang="ts">
import { Radio, Server, Users } from "@lucide/vue";
import type { NodeObservedState } from "@ocservia/api-client";
import { onMounted } from "vue";
import { useRouter } from "vue-router";

import { useFleetStore } from "../shared/fleet";

const fleet = useFleetStore();
const router = useRouter();

onMounted(async () => {
  if (!fleet.initialized) await fleet.rebuild();
  if (fleet.initialized) void fleet.connect();
});

function pathMode(node: NodeObservedState): "relay" | "direct" | "unknown" {
  if (node.path?.mode === "relay") return "relay";
  if (node.path?.mode === "direct") return "direct";
  return "unknown";
}

function pathRtt(node: NodeObservedState): string {
  return node.path ? String(Math.round(node.path.rttMs)) : "";
}

function openNode(nodeId: string): void {
  void router.push({ name: "node-detail", params: { nodeId } });
}
</script>

<template>
  <main class="overview nodes-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("fleet") }}</p>
        <h1>{{ $t("nodes") }}</h1>
      </div>
      <span class="health" :class="{ unavailable: fleet.unavailable }"
        ><i></i
        >{{
          $t(fleet.unavailable ? "systemsUnavailable" : "liveTelemetry")
        }}</span
      >
    </div>
    <section class="metrics" :aria-label="$t('fleetStatus')">
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
    <section class="fleet-list-panel">
      <div class="node-table-wrap">
        <table class="node-table fleet-table">
          <thead>
            <tr>
              <th>{{ $t("node") }}</th>
              <th>{{ $t("trust") }}</th>
              <th>{{ $t("connection") }}</th>
              <th>{{ $t("freshness") }}</th>
              <th>{{ $t("path") }}</th>
              <th>{{ $t("lastHeartbeat") }}</th>
              <th>{{ $t("agent") }}</th>
              <th>{{ $t("ocserv") }}</th>
              <th>{{ $t("sessions") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="node in fleet.nodes"
              :key="node.id"
              :class="{ selected: fleet.selected?.id === node.id }"
              tabindex="0"
              @click="openNode(node.id)"
              @keydown.enter="openNode(node.id)"
            >
              <td>
                <strong>{{ node.name }}</strong
                ><code>{{ node.id.slice(0, 8) }}</code>
              </td>
              <td>{{ $t(node.trustStatus) }}</td>
              <td>{{ $t(node.connectionState) }}</td>
              <td>
                <span class="state-dot" :class="node.freshness"></span
                >{{ $t(node.freshness) }}
              </td>
              <td>
                <span>{{ $t(pathMode(node)) }}</span
                ><small v-if="node.path"
                  >{{ pathRtt(node) }} {{ $t("milliseconds") }}</small
                >
              </td>
              <td>
                <time
                  v-if="node.lastHeartbeatAt"
                  :datetime="node.lastHeartbeatAt.toISOString()"
                  >{{ node.lastHeartbeatAt.toLocaleString() }}</time
                ><span v-else>{{ $t("notObserved") }}</span>
              </td>
              <td>{{ node.agentVersion ?? $t("notAvailable") }}</td>
              <td>{{ node.ocservVersion ?? $t("notAvailable") }}</td>
              <td>{{ node.sessionCount }}</td>
            </tr>
          </tbody>
        </table>
        <div
          v-if="
            fleet.loading || (!fleet.unavailable && fleet.nodes.length === 0)
          "
          class="empty-state"
        >
          <Server :size="24" /><span>{{
            fleet.loading ? $t("loading") : $t("noNodes")
          }}</span>
        </div>
        <div v-else-if="fleet.unavailable" class="empty-state" role="alert">
          <Server :size="24" /><span>{{ $t("systemsUnavailable") }}</span>
        </div>
      </div>
    </section>
  </main>
</template>
