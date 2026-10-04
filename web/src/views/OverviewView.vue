<script setup lang="ts">
import {
  ListChecks,
  PackageCheck,
  Radio,
  Server,
  Users,
  Workflow,
} from "@lucide/vue";
import { computed, onBeforeUnmount, onMounted } from "vue";

import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import MetricCard from "../components/overview/MetricCard.vue";
import { operationTone } from "../features/operations/state-tone";
import { sourceState, sourceValue } from "../features/overview/source-state";
import { useFleetStore } from "../shared/fleet";
import { operationStatusKey } from "../shared/operation-status";
import {
  recentEventLimit,
  recentOperationWindow,
  useOverviewStore,
} from "../shared/overview";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const fleet = useFleetStore();
const overview = useOverviewStore();

const fleetState = computed(() =>
  sourceState(fleet.initialized, fleet.unavailable),
);
const operationsState = computed(() =>
  sourceState(overview.operationsLoaded, overview.operationsUnavailable),
);
const eventsState = computed(() =>
  sourceState(overview.eventsLoaded, overview.eventsUnavailable),
);
const fleetShown = computed(
  () => fleetState.value === "ready" || fleetState.value === "stale",
);
const staleNodes = computed(
  () => fleet.nodes.filter((node) => node.freshness === "stale").length,
);
const notableNodes = computed(() =>
  fleet.nodes
    .filter(
      (node) => node.connectionState !== "online" || node.freshness === "stale",
    )
    .slice(0, 5),
);

function nodeReference(nodeId: string): string {
  const node = fleet.nodes.find((candidate) => candidate.id === nodeId);
  return node ? node.name : nodeId.slice(0, 8);
}

function eventLabel(type: string): string {
  const labels: Record<string, string> = {
    connected: "eventConnected",
    disconnected: "eventDisconnected",
    command_result: "eventCommandResult",
    simulation_result: "eventCommandResult",
    heartbeat: "eventHeartbeat",
    telemetry: "eventTelemetry",
    path_changed: "eventPathChanged",
    error: "eventError",
  };
  return labels[type] ?? type;
}

function timeLabel(value: Date | string): string {
  if (typeof value !== "string")
    return Number.isFinite(value.getTime())
      ? value.toISOString().slice(11, 19) + " UTC"
      : String(value);
  if (!/^\d{4}-/.test(value)) return value;
  const date = new Date(value);
  return Number.isFinite(date.getTime())
    ? date.toISOString().slice(11, 19) + " UTC"
    : value;
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
    <PageHeader :eyebrow="$t('workspace')" :title="$t('overview')">
      <template #actions>
        <StatusBadge
          data-testid="overview-readiness"
          :tone="
            readiness.state === 'loading'
              ? 'neutral'
              : readiness.isReady
                ? 'success'
                : 'danger'
          "
          :label="
            $t(
              readiness.state === 'loading'
                ? 'loading'
                : readiness.isReady
                  ? 'allSystems'
                  : 'systemsUnavailable',
            )
          "
        />
      </template>
    </PageHeader>
    <section class="mb-6" aria-labelledby="overview-fleet-status">
      <div class="mb-3 flex flex-wrap items-baseline justify-between gap-2">
        <h2 id="overview-fleet-status" class="m-0 text-base font-semibold">
          {{ $t("fleetStatus") }}
        </h2>
        <p
          v-if="fleet.snapshotAt"
          class="text-muted-foreground m-0 text-xs"
          data-testid="overview-fleet-updated"
        >
          {{ $t("overviewUpdatedAt", { time: timeLabel(fleet.snapshotAt) }) }}
        </p>
      </div>
      <p
        v-if="fleetState === 'stale'"
        class="m-0 mb-3 text-sm text-amber-800"
        role="status"
      >
        {{ $t("fleetRefreshFailed") }}
      </p>
      <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
        <MetricCard
          :title="$t('controlPlane')"
          :icon="Server"
          value-testid="overview-control-plane"
          :value="
            $t(
              readiness.state === 'loading'
                ? 'loading'
                : readiness.isReady
                  ? 'ready'
                  : 'unavailable',
            )
          "
          :source="$t('overviewSourceReadiness')"
        />
        <MetricCard
          :title="$t('nodes')"
          :icon="Workflow"
          value-testid="overview-nodes"
          :value="sourceValue(fleetState, fleet.nodes.length)"
          :source="$t('overviewSourceNodes')"
        >
          <template v-if="fleetShown"
            >{{ fleet.online }} {{ $t("online") }} · {{ fleet.offline }}
            {{ $t("offline") }}</template
          >
          <template v-else-if="fleetState === 'unavailable'">{{
            $t("systemsUnavailable")
          }}</template>
        </MetricCard>
        <MetricCard
          :title="$t('observedSessions')"
          :icon="Users"
          value-testid="overview-sessions"
          :value="sourceValue(fleetState, fleet.sessionCount)"
          :source="$t('overviewSourceSessions')"
          :warning="
            fleetShown && staleNodes
              ? $t('overviewStaleReports', { count: staleNodes })
              : undefined
          "
        />
        <MetricCard
          :title="$t('overviewActiveOperations')"
          :icon="ListChecks"
          value-testid="overview-operations"
          :value="sourceValue(operationsState, overview.activeOperations)"
          :source="$t('overviewSourceOperations')"
          :warning="
            operationsState === 'stale' ? $t('overviewStaleSource') : undefined
          "
        >
          <template
            v-if="operationsState === 'ready' || operationsState === 'stale'"
            >{{ overview.unknownOperations }} {{ $t("unknown") }} ·
            {{
              $t("overviewRecentFailed", {
                count: overview.recentFailedOperations,
                window: recentOperationWindow,
              })
            }}</template
          >
          <template v-else-if="operationsState === 'unavailable'">{{
            $t("operationsUnavailable")
          }}</template>
        </MetricCard>
        <MetricCard
          :title="$t('lastObservedDirectPaths')"
          :icon="Radio"
          value-testid="overview-connectivity"
          :value="sourceValue(fleetState, fleet.direct)"
          :source="$t('overviewSourcePaths')"
        >
          <template v-if="fleetShown"
            >{{ fleet.direct }} {{ $t("direct") }} · {{ fleet.relay }}
            {{ $t("relay") }} · {{ $t("pathCountsIncludeOffline") }}</template
          >
        </MetricCard>
        <MetricCard
          :title="$t('agentVersions')"
          :icon="PackageCheck"
          value-testid="overview-agent-versions"
          :value="sourceValue(fleetState, fleet.agentCurrent)"
          :source="$t('overviewSourceAgents')"
        >
          <template v-if="fleetShown"
            >{{ fleet.agentCurrent }} {{ $t("current") }} ·
            {{ fleet.agentUpdateAvailable }} {{ $t("upgrade_available") }} ·
            {{ fleet.agentAhead }} {{ $t("ahead") }} · {{ fleet.agentUnknown }}
            {{ $t("unknown") }}</template
          >
        </MetricCard>
      </div>
    </section>
    <div class="grid items-start gap-x-6 lg:grid-cols-2">
      <SectionCard :title="$t('recentOperations')">
        <template #actions>
          <span class="text-muted-foreground text-xs">
            {{ $t("overviewRecentScope", { count: recentEventLimit }) }}
            <template v-if="overview.operationsAt">
              ·
              {{
                $t("overviewUpdatedAt", {
                  time: timeLabel(overview.operationsAt),
                })
              }}</template
            >
          </span>
        </template>
        <DataState
          v-if="operationsState === 'loading'"
          kind="loading"
          :message="$t('loading')"
        />
        <DataState
          v-else-if="operationsState === 'unavailable'"
          kind="error"
          :message="$t('operationsUnavailable')"
        />
        <template v-else>
          <p
            v-if="operationsState === 'stale'"
            class="m-0 text-sm text-amber-800"
            role="status"
          >
            {{ $t("overviewStaleSource") }}
          </p>
          <p
            v-if="overview.recentOperations.length === 0"
            class="text-muted-foreground m-0 text-sm"
          >
            {{ $t("noOperations") }}
          </p>
          <ol
            v-else
            class="divide-border m-0 grid list-none divide-y p-0"
            data-testid="overview-operations-list"
          >
            <li
              v-for="operation in overview.recentOperations"
              :key="operation.id"
              class="flex flex-wrap items-center gap-x-3 gap-y-1 py-2 text-sm first:pt-0 last:pb-0"
            >
              <StatusBadge
                :tone="operationTone(operation)"
                :label="$t(operationStatusKey(operation))"
              />
              <RouterLink
                v-if="operation.nodeId"
                :to="{
                  name: 'node-detail',
                  params: { nodeId: operation.nodeId },
                }"
                class="text-primary min-w-0 flex-1 truncate underline-offset-4 hover:underline"
                >{{ nodeReference(operation.nodeId) }}</RouterLink
              >
              <span v-else class="text-muted-foreground min-w-0 flex-1">{{
                $t("notAvailable")
              }}</span>
              <time
                class="text-muted-foreground text-xs"
                :datetime="operation.updatedAt"
                >{{ timeLabel(operation.updatedAt) }}</time
              >
            </li>
          </ol>
          <RouterLink
            :to="{ name: 'operations' }"
            class="text-primary justify-self-start text-sm underline-offset-4 hover:underline"
            >{{ $t("viewAllOperations") }}</RouterLink
          >
        </template>
      </SectionCard>
      <SectionCard :title="$t('recentEvents')">
        <template #actions>
          <span class="text-muted-foreground text-xs">
            {{ $t("overviewRecentScope", { count: recentEventLimit }) }}
            <template v-if="overview.eventsAt">
              ·
              {{
                $t("overviewUpdatedAt", { time: timeLabel(overview.eventsAt) })
              }}</template
            >
          </span>
        </template>
        <DataState
          v-if="eventsState === 'loading'"
          kind="loading"
          :message="$t('loading')"
        />
        <DataState
          v-else-if="eventsState === 'unavailable'"
          kind="error"
          :message="$t('eventsUnavailable')"
        />
        <template v-else>
          <p
            v-if="eventsState === 'stale'"
            class="m-0 text-sm text-amber-800"
            role="status"
          >
            {{ $t("overviewStaleSource") }}
          </p>
          <p
            v-if="overview.recentEvents.length === 0"
            class="text-muted-foreground m-0 text-sm"
          >
            {{ $t("noActivity") }}
          </p>
          <ol
            v-else
            class="divide-border m-0 grid list-none divide-y p-0"
            data-testid="overview-events"
          >
            <li
              v-for="event in overview.recentEvents"
              :key="event.id"
              class="flex flex-wrap items-center gap-x-3 gap-y-1 py-2 text-sm first:pt-0 last:pb-0"
            >
              <span class="font-medium">{{ $t(eventLabel(event.type)) }}</span>
              <RouterLink
                v-if="event.nodeId"
                :to="{ name: 'node-detail', params: { nodeId: event.nodeId } }"
                class="text-primary min-w-0 flex-1 truncate underline-offset-4 hover:underline"
                >{{ nodeReference(event.nodeId) }}</RouterLink
              >
              <span v-else class="min-w-0 flex-1" />
              <time
                class="text-muted-foreground text-xs"
                :datetime="event.occurredAt"
                >{{ timeLabel(event.occurredAt) }}</time
              >
            </li>
          </ol>
        </template>
      </SectionCard>
    </div>
    <SectionCard
      v-if="fleetShown && notableNodes.length > 0"
      :title="$t('offlineStaleNodes')"
    >
      <ol
        class="divide-border m-0 grid list-none divide-y p-0"
        data-testid="overview-notable-nodes"
      >
        <li
          v-for="node in notableNodes"
          :key="node.id"
          class="flex flex-wrap items-center gap-x-3 gap-y-1 py-2 text-sm first:pt-0 last:pb-0"
        >
          <RouterLink
            :to="{ name: 'node-detail', params: { nodeId: node.id } }"
            class="text-primary min-w-0 truncate font-medium underline-offset-4 hover:underline"
            >{{ node.name }}</RouterLink
          >
          <code class="text-muted-foreground text-xs">{{
            node.id.slice(0, 8)
          }}</code>
          <span class="ml-auto flex flex-wrap gap-2">
            <StatusBadge
              :tone="node.connectionState === 'online' ? 'success' : 'danger'"
              :label="$t(node.connectionState)"
            />
            <StatusBadge
              :tone="node.freshness === 'stale' ? 'warning' : 'neutral'"
              :label="$t(node.freshness)"
            />
          </span>
        </li>
      </ol>
    </SectionCard>
  </main>
</template>
