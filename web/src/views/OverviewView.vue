<script setup lang="ts">
import {
  Clock3,
  ListChecks,
  PackageCheck,
  Radio,
  Server,
  Users,
  Workflow,
} from "@lucide/vue";
import type { Operation } from "@ocservia/api-client";
import { computed, onBeforeUnmount, onMounted } from "vue";

import { useFleetStore } from "../shared/fleet";
import { useOverviewStore } from "../shared/overview";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const fleet = useFleetStore();
const overview = useOverviewStore();

const fleetLoading = computed(
  () => fleet.loading && !fleet.initialized && !fleet.unavailable,
);
const notableNodes = computed(() =>
  fleet.nodes
    .filter(
      (node) => node.connectionState !== "online" || node.freshness === "stale",
    )
    .slice(0, 5),
);

function nodeReference(nodeId: string | undefined): string {
  if (!nodeId) return "";
  const node = fleet.nodes.find((candidate) => candidate.id === nodeId);
  return node ? node.name : nodeId.slice(0, 8);
}

function operationLabel(state: Operation["state"]): string {
  return "operation_" + state;
}

function eventLabel(type: string): string {
  const labels: Record<string, string> = {
    connected: "eventConnected",
    disconnected: "eventDisconnected",
    command_result: "eventCommandResult",
    simulation_result: "eventCommandResult",
    heartbeat: "eventHeartbeat",
    error: "eventError",
  };
  return labels[type] ?? type;
}

function timeLabel(value: Date): string {
  return value.toLocaleTimeString();
}

onMounted(async () => {
  overview.start();
  if (!fleet.initialized) await fleet.rebuild();
  if (fleet.initialized) void fleet.connect();
});
onBeforeUnmount(() => {
  overview.stop();
});
</script>

<template>
  <main class="overview">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("overview") }}</h1>
      </div>
      <span class="health" :class="{ unavailable: !readiness.isReady }"
        ><i></i
        >{{ $t(readiness.isReady ? "allSystems" : "systemsUnavailable") }}</span
      >
    </div>
    <section class="metrics overview-metrics" :aria-label="$t('fleetStatus')">
      <article>
        <div>
          <span>{{ $t("controlPlane") }}</span>
          <strong>{{
            $t(
              readiness.state === "loading"
                ? "loading"
                : readiness.isReady
                  ? "ready"
                  : "unavailable",
            )
          }}</strong>
        </div>
        <Server :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("nodes") }}</span>
          <strong data-testid="overview-nodes">{{
            fleetLoading ? "…" : fleet.unavailable ? "–" : fleet.nodes.length
          }}</strong>
          <small>
            <template v-if="fleetLoading">{{ $t("loading") }}</template>
            <template v-else-if="fleet.unavailable">{{
              $t("systemsUnavailable")
            }}</template>
            <template v-else
              >{{ fleet.online }} {{ $t("online") }} · {{ fleet.offline }}
              {{ $t("offline") }}</template
            >
          </small>
        </div>
        <Workflow :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("sessions") }}</span>
          <strong data-testid="overview-sessions">{{
            fleetLoading ? "…" : fleet.unavailable ? "–" : fleet.sessionCount
          }}</strong>
        </div>
        <Users :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("operations") }}</span>
          <strong data-testid="overview-operations">{{
            overview.operationsLoading && !overview.operationsLoaded
              ? "…"
              : overview.operationsUnavailable
                ? "–"
                : overview.activeOperations
          }}</strong>
          <small>
            <template
              v-if="overview.operationsLoading && !overview.operationsLoaded"
              >{{ $t("loading") }}</template
            >
            <template v-else-if="overview.operationsUnavailable">{{
              $t("operationsUnavailable")
            }}</template>
            <template v-else
              >{{ overview.unknownOperations }} {{ $t("unknown") }} ·
              {{ overview.recentFailedOperations }} {{ $t("failed") }}</template
            >
          </small>
        </div>
        <ListChecks :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("connectivity") }}</span>
          <strong data-testid="overview-connectivity">{{
            fleetLoading ? "…" : fleet.unavailable ? "–" : fleet.direct
          }}</strong>
          <small>
            <template v-if="fleetLoading">{{ $t("loading") }}</template>
            <template v-else-if="fleet.unavailable">{{
              $t("systemsUnavailable")
            }}</template>
            <template v-else
              >{{ fleet.direct }} {{ $t("direct") }} · {{ fleet.relay }}
              {{ $t("relay") }}</template
            >
          </small>
        </div>
        <Radio :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("agentVersions") }}</span>
          <strong data-testid="overview-agent-versions">{{
            fleetLoading ? "…" : fleet.unavailable ? "–" : fleet.agentCurrent
          }}</strong>
          <small>
            <template v-if="fleetLoading">{{ $t("loading") }}</template>
            <template v-else-if="fleet.unavailable">{{
              $t("systemsUnavailable")
            }}</template>
            <template v-else
              >{{ fleet.agentUpdateAvailable }} {{ $t("upgrade_available") }} ·
              {{ fleet.agentUnknown }} {{ $t("unknown") }}</template
            >
          </small>
        </div>
        <PackageCheck :size="20" />
      </article>
    </section>
    <div class="overview-panels">
      <section class="activity-panel" :aria-label="$t('recentOperations')">
        <header class="activity-header">
          <h2>{{ $t("recentOperations") }}</h2>
        </header>
        <div
          v-if="overview.operationsLoading && !overview.operationsLoaded"
          class="empty-state"
        >
          <ListChecks :size="24" /><span>{{ $t("loading") }}</span>
        </div>
        <div
          v-else-if="overview.operationsUnavailable"
          class="empty-state"
          role="alert"
        >
          <ListChecks :size="24" /><span>{{
            $t("operationsUnavailable")
          }}</span>
        </div>
        <div
          v-else-if="overview.recentOperations.length === 0"
          class="empty-state"
        >
          <ListChecks :size="24" /><span>{{ $t("noOperations") }}</span>
        </div>
        <ol v-else class="event-list" data-testid="overview-operations-list">
          <li
            v-for="operation in overview.recentOperations"
            :key="operation.id"
          >
            <span class="event-type" :class="operation.state">{{
              $t(operationLabel(operation.state))
            }}</span>
            <code>{{
              operation.nodeId
                ? nodeReference(operation.nodeId)
                : $t("notAvailable")
            }}</code>
            <time :datetime="operation.updatedAt.toISOString()">{{
              timeLabel(operation.updatedAt)
            }}</time>
          </li>
        </ol>
      </section>
      <section class="activity-panel" :aria-label="$t('recentEvents')">
        <header class="activity-header">
          <h2>{{ $t("recentEvents") }}</h2>
        </header>
        <div
          v-if="overview.eventsLoading && !overview.eventsLoaded"
          class="empty-state"
        >
          <Clock3 :size="24" /><span>{{ $t("loading") }}</span>
        </div>
        <div
          v-else-if="overview.eventsUnavailable"
          class="empty-state"
          role="alert"
        >
          <Clock3 :size="24" /><span>{{ $t("eventsUnavailable") }}</span>
        </div>
        <div v-else-if="overview.recentEvents.length === 0" class="empty-state">
          <Clock3 :size="24" /><span>{{ $t("noActivity") }}</span>
        </div>
        <ol v-else class="event-list" data-testid="overview-events">
          <li v-for="event in overview.recentEvents" :key="event.id">
            <span class="event-type">{{ $t(eventLabel(event.type)) }}</span>
            <code>{{ nodeReference(event.nodeId) }}</code>
            <time :datetime="event.occurredAt.toISOString()">{{
              timeLabel(event.occurredAt)
            }}</time>
          </li>
        </ol>
      </section>
    </div>
    <section
      v-if="!fleetLoading && !fleet.unavailable && notableNodes.length > 0"
      class="activity-panel"
      :aria-label="$t('offlineStaleNodes')"
    >
      <header class="activity-header">
        <h2>{{ $t("offlineStaleNodes") }}</h2>
      </header>
      <ol class="event-list" data-testid="overview-notable-nodes">
        <li v-for="node in notableNodes" :key="node.id">
          <span class="event-type">{{ node.name }}</span>
          <code>{{ node.id.slice(0, 8) }}</code>
          <span>{{ $t(node.connectionState) }} · {{ $t(node.freshness) }}</span>
        </li>
      </ol>
    </section>
  </main>
</template>
